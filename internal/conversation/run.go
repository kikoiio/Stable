package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/planfile"
	"stable/internal/prompt"
	"stable/internal/redact"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
	"stable/internal/workspace"
)

func (s *Service) startRun(ctx context.Context, msg ClientMsg, updates chan ServerMsg) error {
	if s.deps.Runner == nil {
		if s.deps.RunnerError != "" {
			return fmt.Errorf("streaming agent is unavailable: %s", s.deps.RunnerError)
		}
		return errors.New("streaming agent is not configured")
	}
	request := *msg.Run
	if request.RunID == "" {
		id, err := sessionlog.NewID()
		if err != nil {
			return err
		}
		request.RunID = id
	}
	if request.ProviderName == "" {
		request.ProviderName = s.deps.ProviderName
	}
	if request.Model == "" {
		request.Model = s.deps.Model
	}
	request.Intent = redactRunCredential(request.Intent, s.deps.ProviderCredential)
	for i := range request.Messages {
		request.Messages[i].Content = redactRunCredential(request.Messages[i].Content, s.deps.ProviderCredential)
	}
	if request.Work.SessionID != msg.SessionID {
		return errors.New("run session does not match request session")
	}
	if s.mcp != nil {
		if err := s.ensureMCPFresh(msg.SessionID); err != nil {
			return fmt.Errorf("refresh MCP configuration: %w", err)
		}
	}
	if err := agent.ValidateRequest(request); err != nil {
		return err
	}
	plan := s.PlanStateOf(msg.SessionID)
	mode, planFilePath := planRunAuthority(plan, request.Work.Kind)
	authority, err := BuildAuthority(ctx, s.deps.Store, s.deps.ProjectRoot, request, mode, planFilePath)
	if err != nil {
		return fmt.Errorf("build trusted run authority: %w", err)
	}
	request.AllowedScope = []string{authority.AllowedRoot}
	request.PermissionBounds, err = json.Marshal(authority)
	if err != nil {
		return fmt.Errorf("encode trusted run authority: %w", err)
	}
	if _, err := sessionlog.SessionPath(s.deps.ProjectRoot, msg.SessionID); err != nil {
		return err
	}
	// Run starts and workspace binding changes share this admission lock. Keep
	// it through lease acquisition, RunStarted persistence, Runner.Start, and
	// active-run registration so an Enter/Exit cannot slip into the gap.
	s.workspaceAdmissionMu.Lock()
	if err := ctx.Err(); err != nil {
		s.workspaceAdmissionMu.Unlock()
		return err
	}
	admissionHeld := true
	defer func() {
		if admissionHeld {
			s.workspaceAdmissionMu.Unlock()
		}
	}()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return workspace.ErrUnavailable
	}
	var teamCoordinatorMode bool
	var teamCoordinatorTeamID string
	if request.Work.Kind == agent.WorkSession {
		coordinator, modeErr := teamCoordinatorModeForSession(s.deps.ProjectRoot, msg.SessionID)
		err = modeErr
		if err != nil {
			return err
		}
		teamCoordinatorMode, teamCoordinatorTeamID = coordinator.Enabled, coordinator.TeamID
		if coordinator.Enabled {
			if coordinator.TeamID == "" {
				return errors.New("team coordinator mode has no bound team")
			}
			team, teamErr := s.getTeamForSession(ctx, currentProjectRoot(s.deps.ProjectRoot), msg.SessionID, coordinator.TeamID, &request.Work)
			if teamErr != nil || team.Status != teams.TeamOpen {
				return errors.New("team coordinator binding is no longer authorized")
			}
		}
		request.TeamCoordinator = coordinator.Enabled
		request.TeamCoordinatorTeamID = coordinator.TeamID
	}
	var leadLease *workspace.WriterLease
	var leadManager *workspace.LifecycleService
	var trustedFactory agent.ExecutorFactory
	leaseTransferred := false
	defer func() {
		if leadLease != nil && !leaseTransferred {
			_, _ = leadManager.ReleaseCompletedWriter(context.Background(), *leadLease)
		}
	}()
	if (request.Work.Kind == agent.WorkSession || request.Work.Kind == agent.WorkGoal) && s.deps.WorkspaceStateRoot != "" {
		// Derive workspace ownership from the validated run WorkRef, not from
		// optional client routing fields. Bindings are exact to that WorkRef.
		workspaceMsg := ClientMsg{
			SessionID: request.Work.SessionID, WorkKind: string(request.Work.Kind),
			GoalID: request.Work.GoalID, WorkItemID: request.Work.WorkItemID,
		}
		projectRoot, scope, scopeErr := s.workspaceScope(ctx, workspaceMsg)
		if scopeErr != nil {
			return scopeErr
		}
		if scope.Work != request.Work {
			return workspace.ErrOwnership
		}
		leadManager, err = s.workspaceService(projectRoot)
		if err != nil {
			return err
		}
		boundID, bindingErr := leadManager.Binding(scope)
		if bindingErr != nil {
			return bindingErr
		}
		if boundID != "" {
			if mode == permission.ModePlan || teamCoordinatorMode || request.TeamTurn != nil || request.TeamUser {
				return workspace.ErrOwnership
			}
			if _, ok := s.deps.Runner.(agent.TrustedExecutorRunner); !ok {
				return errors.New("workspace-bound runs require trusted per-run executor support")
			}
			scope.Authority = authority
			scope.OriginRunID = request.RunID
			lease, acquireErr := leadManager.AcquireLeadWriter(ctx, scope, boundID, request.RunID)
			if acquireErr != nil {
				return acquireErr
			}
			leadLease = &lease
			base := s.deps.ExecutorFactory
			if base == nil {
				return errors.New("workspace-bound runs require a trusted executor factory")
			}
			trustedFactory = execution.WorkspaceWriterExecutorFactory(base, lease, leadManager)
			writerSchemas := execution.WorkspaceWriterToolSchemas()
			if trustedFactory == nil || len(writerSchemas) != 6 {
				return errors.New("workspace writer executor surface is unavailable")
			}
			request.ToolSchemas = writerSchemas
			request.AllowedScope = []string{lease.Authority.AllowedRoot}
			request.PermissionBounds, err = json.Marshal(lease.Authority)
			if err != nil {
				return fmt.Errorf("encode trusted workspace writer authority: %w", err)
			}
		}
	}
	work := sessionlog.RunStarted{RunID: request.RunID, WorkKind: string(request.Work.Kind), GoalID: request.Work.GoalID, WorkItemID: request.Work.WorkItemID, Intent: request.Intent}
	if leadLease != nil {
		work.WorkspaceID = leadLease.WorkspaceID
		work.WorkspaceGeneration = leadLease.Generation
	}
	// The skill inventory is refreshed before the event mutex is taken: the
	// gate journals its events under the same mutex, so calling it here keeps
	// the lock order gate → event and avoids a re-entrant acquire.
	var skillPrefix []llm.Message
	if request.Work.Kind == agent.WorkSession && s.skills != nil && leadLease == nil {
		snapshotText, deltaText, invErr := s.skills.SkillInventory(ctx, msg.SessionID)
		if invErr != nil {
			return invErr
		}
		if snapshotText != "" {
			skillPrefix = append(skillPrefix, llm.Message{Role: "user", Content: snapshotText})
		}
		if deltaText != "" {
			skillPrefix = append(skillPrefix, llm.Message{Role: "user", Content: deltaText})
		}
	}
	var mcpPrefix []llm.Message
	if request.Work.Kind == agent.WorkSession && s.mcp != nil && leadLease == nil {
		if instructions := s.mcp.Instructions(); instructions != "" {
			s.mcpMu.Lock()
			if !s.mcpInstructions[msg.SessionID] {
				s.mcpInstructions[msg.SessionID] = true
				mcpPrefix = append(mcpPrefix, llm.Message{Role: "user", Content: "MCP Server Instructions\n" + instructions})
			}
			s.mcpMu.Unlock()
		}
	}
	var hookPrefix []llm.Message
	if s.hooks != nil && leadLease == nil {
		if notice := s.hooks.DrainNotifications(msg.SessionID); notice != "" {
			hookPrefix = append(hookPrefix, llm.Message{Role: "user", Content: notice})
		}
	}
	s.eventMu.Lock()
	if request.Work.Kind == agent.WorkSession {
		coordinatorMode, modeErr := teamCoordinatorModeForSession(s.deps.ProjectRoot, msg.SessionID)
		if modeErr != nil {
			s.eventMu.Unlock()
			return modeErr
		}
		if coordinatorMode.Enabled != teamCoordinatorMode || coordinatorMode.TeamID != teamCoordinatorTeamID {
			s.eventMu.Unlock()
			return errors.New("session run mode changed during admission")
		}
		if coordinatorMode.Enabled {
			request.TeamCoordinator = true
			request.TeamCoordinatorTeamID = coordinatorMode.TeamID
			request.ToolSchemas = execution.TeamCoordinatorToolSchemas(s.deps.ToolSchemas)
			if len(request.ToolSchemas) == 0 {
				s.eventMu.Unlock()
				return errors.New("team coordinator mode has no configured team tools")
			}
		}
	}
	taskPrefix, taskErr := s.agentTaskNotifications(request)
	if taskErr != nil {
		s.eventMu.Unlock()
		return taskErr
	}
	if request.Work.Kind != agent.WorkSession {
		request.Messages = append(taskPrefix, request.Messages...)
	}
	if request.Work.Kind == agent.WorkGoal && leadLease != nil {
		request.Messages = append([]llm.Message{{
			Role:    "system",
			Content: "你是 Stable 的受控工作区 agent。读、搜、列只能访问本次绑定工作区的只读基线；写、编辑和命令只能影响本次绑定工作区。正式工程保持不变，变更需稍后单独导出为候选并经用户审核。工具路径使用工作区相对路径。",
		}}, request.Messages...)
	}
	if request.Work.Kind == agent.WorkSession {
		history := sessionConversationMessages(s.deps.ProjectRoot, msg.SessionID)
		systemPrompt := "你是 Stable 的通用 agent。读、搜、列只能访问正式工程只读视图；写、编辑只能写入本次运行的候选区。工具路径使用工作区相对路径。工具结果代表真实受控执行结果。"
		if leadLease != nil {
			systemPrompt = "你是 Stable 的受控工作区 agent。读、搜、列只能访问本次绑定工作区的只读基线；写、编辑和命令只能影响本次绑定工作区。正式工程保持不变，变更需稍后单独导出为候选并经用户审核。工具路径使用工作区相对路径。"
		}
		if request.TeamCoordinator {
			systemPrompt += " 当前运行处于团队协调器模式，仅协调已绑定团队 " + request.TeamCoordinatorTeamID + "：仅使用团队消息、请求和任务板工具；不得操作其它团队，不得调用项目文件、命令、MCP、网络、委派、后台任务或 session todo 工具。"
		}
		prefix := []llm.Message{{Role: "system", Content: systemPrompt}}
		// The skill inventory text is per-run context like the plan reminder:
		// it rides after the system prefix (before the replayed history, so
		// the stable snapshot sits at a fixed offset) and is deliberately not
		// appended to the session log — the skill events keep it auditable.
		prefix = append(prefix, skillPrefix...)
		prefix = append(prefix, mcpPrefix...)
		prefix = append(prefix, hookPrefix...)
		prefix = append(prefix, taskPrefix...)
		if plan.Mode == sessionlog.PlanModePlan && plan.PlanPath != "" {
			// The plan workflow reminder is per-turn context, not session
			// history: it is inserted after the replayed conversation and
			// immediately before the newest user message of this request, so
			// the model reads it as the last instruction before acting, and
			// it is deliberately not appended to the session log. The run
			// counter drives the cadence: the full workflow text opens the
			// first plan run and repeats every fifth run, the compact
			// reminder fills the turns in between.
			plan = s.recordPlanRun(msg.SessionID)
			exists, existsErr := planfile.Exists(s.deps.ProjectRoot, msg.SessionID)
			if existsErr != nil {
				s.eventMu.Unlock()
				return fmt.Errorf("check plan file: %w", existsErr)
			}
			reminder := prompt.BuildPlanModeReminder(plan.PlanPath, exists, int(plan.Runs))
			prefix = append(prefix, history...)
			prefix = append(prefix, llm.Message{Role: "user", Content: reminder})
			request.Messages = append(prefix, request.Messages...)
		} else {
			request.Messages = append(prefix, append(history, request.Messages...)...)
		}
		for i := range request.Messages {
			request.Messages[i].Content = redactRunCredential(request.Messages[i].Content, s.deps.ProviderCredential)
		}
		if len(request.Messages) > 0 {
			last := request.Messages[len(request.Messages)-1]
			if last.Role == "user" && last.Content != "" {
				if _, err := sessionlog.Append(s.deps.ProjectRoot, msg.SessionID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Text: last.Content, Kind: "text"}); err != nil {
					s.eventMu.Unlock()
					return err
				}
			}
		}
	}
	if _, err := sessionlog.Append(s.deps.ProjectRoot, msg.SessionID, sessionlog.EventRunStarted, work); err != nil {
		s.eventMu.Unlock()
		return err
	}
	s.mu.Lock()
	if sub := s.clients[updates]; sub != nil {
		sub.sessionID, sub.runID = msg.SessionID, request.RunID
	}
	s.mu.Unlock()
	s.eventMu.Unlock()
	if request.Work.Kind != agent.WorkSession && len(hookPrefix) > 0 {
		request.Messages = append(hookPrefix, request.Messages...)
	}
	var handle *agent.RunHandle
	if trustedFactory != nil {
		handle, err = s.deps.Runner.(agent.TrustedExecutorRunner).StartWithExecutorFactory(ctx, request, trustedFactory)
	} else {
		handle, err = s.deps.Runner.Start(ctx, request)
	}
	if err != nil {
		eventID, idErr := sessionlog.NewID()
		if idErr == nil {
			failed := sessionlog.RunEvent{ID: eventID, RunID: request.RunID, SessionID: request.Work.SessionID, RunSeq: 1, At: time.Now().UTC(), Kind: string(agent.EventTerminal), Payload: map[string]any{"status": agent.RunFailed, "reason": "runner could not start"}}
			s.eventMu.Lock()
			stored, appendErr := sessionlog.Append(s.deps.ProjectRoot, msg.SessionID, sessionlog.EventRunEvent, failed)
			s.eventMu.Unlock()
			if appendErr == nil {
				s.broadcastRun(ServerMsg{Type: "run_event", RunID: request.RunID, RunEvent: &failed, Cursor: stored.Seq}, msg.SessionID, request.RunID, stored.Seq)
			}
		}
		return err
	}
	s.mu.Lock()
	if s.activeRuns == nil {
		s.activeRuns = map[string]string{}
	}
	if s.activeRequests == nil {
		s.activeRequests = map[string]agent.ExecutionRequest{}
	}
	s.activeRuns[request.RunID] = request.Work.SessionID
	s.activeRequests[request.RunID] = request
	runDone := make(chan struct{})
	if s.runDone == nil {
		s.runDone = map[string]chan struct{}{}
	}
	s.runDone[request.RunID] = runDone
	if leadLease != nil {
		s.workspaceRuns[request.RunID] = workspaceLeadRun{lease: *leadLease, manager: leadManager}
		leaseTransferred = true
	}
	s.mu.Unlock()
	s.workspaceAdmissionMu.Unlock()
	admissionHeld = false
	if s.hooks != nil {
		// Start the runner first so run_start child progress can be published
		// into its durable parent event stream. The consumer starts after the
		// synchronous run_start hooks have completed.
		s.hooks.RunStartRun(ctx, s.hookAgentParent(request), msg.SessionID, request.Intent)
	}
	s.broadcastRun(ServerMsg{Type: "run_started", RunID: request.RunID}, msg.SessionID, request.RunID, 0)
	go s.consumeRun(request, handle, leadLease, leadManager, runDone)
	return nil
}

// redactRunCredential strips the provider credential from text before it
// reaches the model or the session log. A credential too short for
// redact.Redact to identify safely is still replaced literally: leaking it
// is worse than the mangling risk of a short replacement.
func redactRunCredential(text, credential string) string {
	redacted, err := redact.Redact(text, []string{credential})
	if err != nil {
		return strings.ReplaceAll(text, credential, redact.Placeholder)
	}
	return redacted
}

func sessionConversationMessages(root, sessionID string) []llm.Message {
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		return nil
	}
	covered, boundarySeq := sessionlog.CoveredSeqs(transcript.Events)
	var messages []llm.Message
	runAssistant := map[string]int{}
	pendingToolCalls := map[string]bool{}
	for _, event := range transcript.Events {
		if covered[event.Seq] {
			continue
		}
		switch event.Type {
		case sessionlog.EventBoundary:
			// The latest effective boundary replaces its covered range with
			// the summary; older boundaries are superseded. Post-compaction
			// deltas of the same run start a fresh assistant message.
			if event.Seq != boundarySeq {
				continue
			}
			var b sessionlog.Boundary
			if decodeSessionData(event.Data, &b) != nil {
				continue
			}
			messages = append(messages, llm.Message{Role: "assistant", Content: "Earlier conversation summary: " + b.Summary})
			if b.EffectiveScope() == sessionlog.BoundaryScopeRun {
				delete(runAssistant, b.RunID)
			}
		case sessionlog.EventMessage:
			var msg sessionlog.Message
			if decodeSessionData(event.Data, &msg) == nil && (msg.Role == "user" || msg.Role == "assistant") && msg.Text != "" {
				messages = append(messages, llm.Message{Role: msg.Role, Content: msg.Text})
			}
		case sessionlog.EventToolCall:
			var call sessionlog.ToolCall
			if decodeSessionData(event.Data, &call) == nil && call.CallID != "" && call.Name != "" && !pendingToolCalls[call.CallID] {
				input, inputErr := json.Marshal(call.Input)
				tool := llm.ToolUse{ID: call.CallID, Name: call.Name}
				if inputErr == nil && string(input) != "null" {
					tool.Arguments = input
				}
				messages = append(messages, llm.Message{Role: "assistant", ToolUses: []llm.ToolUse{tool}})
				pendingToolCalls[call.CallID] = true
			}
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			if decodeSessionData(event.Data, &result) == nil && result.CallID != "" && pendingToolCalls[result.CallID] {
				content, ok := result.Result.(string)
				if !ok {
					encoded, encodeErr := json.Marshal(result.Result)
					if encodeErr != nil {
						continue
					}
					content = string(encoded)
				}
				messages = append(messages, llm.Message{Role: "user", ToolResults: []llm.ToolResultPart{{ToolUseID: result.CallID, Content: content, IsError: result.Error != ""}}})
				delete(pendingToolCalls, result.CallID)
			}
		case sessionlog.EventRunEvent:
			var runEvent sessionlog.RunEvent
			if decodeSessionData(event.Data, &runEvent) != nil {
				continue
			}
			switch runEvent.Kind {
			case "text_delta":
				var payload struct {
					Text string `json:"text"`
				}
				if decodeSessionData(runEvent.Payload, &payload) != nil || payload.Text == "" {
					continue
				}
				if index, ok := runAssistant[runEvent.RunID]; ok {
					messages[index].Content += payload.Text
				} else {
					runAssistant[runEvent.RunID] = len(messages)
					messages = append(messages, llm.Message{Role: "assistant", Content: payload.Text})
				}
			}
		}
	}
	if len(messages) > 20 {
		messages = append([]llm.Message(nil), messages[len(messages)-20:]...)
	}
	return messages
}

func (s *Service) consumeRun(request agent.ExecutionRequest, handle *agent.RunHandle, lease *workspace.WriterLease, manager *workspace.LifecycleService, runDone chan struct{}) {
	defer close(runDone)
	var textOut strings.Builder
	for event := range handle.Events {
		if event.Kind == agent.EventTextDelta {
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(event.Payload, &payload)
			textOut.WriteString(payload.Text)
		}
		persisted := sessionlog.RunEvent{
			ID: event.ID, RunID: event.RunID, SessionID: event.SessionID,
			RunSeq: event.RunSeq, At: event.At, Kind: string(event.Kind), Payload: event.Payload,
		}
		s.eventMu.Lock()
		stored, err := sessionlog.Append(s.deps.ProjectRoot, request.Work.SessionID, sessionlog.EventRunEvent, persisted)
		if err == nil && event.Kind == agent.EventCompactionBoundary {
			// The conversation service is the only session log writer: the
			// runner-issued boundary becomes a run-scope compaction boundary
			// in stream order, right after its run event.
			var boundary agent.ContextBoundary
			if json.Unmarshal(event.Payload, &boundary) != nil {
				err = errors.New("compaction boundary has invalid payload")
			} else {
				_, err = sessionlog.Append(s.deps.ProjectRoot, request.Work.SessionID, sessionlog.EventBoundary, sessionlog.Boundary{
					FromSeq: boundary.FromSeq, ToSeq: boundary.ToSeq, Summary: boundary.Summary,
					Scope: sessionlog.BoundaryScopeRun, RunID: boundary.RunID,
				})
			}
		}
		if err == nil && event.Kind == agent.EventToolExecResult {
			// Snapshots created at tool write boundaries ride the tool result
			// payload; each becomes a session snapshot event, bound to its
			// run and candidate, in stream order.
			var outcome agent.ToolOutcome
			if json.Unmarshal(event.Payload, &outcome) != nil {
				err = errors.New("tool result has invalid payload")
			} else {
				for _, snap := range outcome.Snapshots {
					if _, err = sessionlog.Append(s.deps.ProjectRoot, request.Work.SessionID, sessionlog.EventSnapshot, sessionlog.SnapshotRef{
						SnapshotID: snap.SnapshotID, SessionID: request.Work.SessionID, CandidateID: snap.CandidateID,
						RunID: snap.RunID, Label: snap.Label, Digest: snap.Digest, CreatedAt: snap.CreatedAt,
					}); err != nil {
						break
					}
				}
			}
		}
		s.eventMu.Unlock()
		if err != nil {
			_ = s.deps.Runner.Cancel(request.RunID)
			s.broadcastRun(ServerMsg{Type: "error", RunID: request.RunID, Error: "could not persist streaming event"}, request.Work.SessionID, request.RunID, 0)
			for range handle.Events { /* drain after cancellation so the runner cannot block on its event channel */
			}
			break
		}
		s.broadcastRun(ServerMsg{Type: "run_event", RunID: request.RunID, RunEvent: &persisted, Cursor: stored.Seq}, request.Work.SessionID, request.RunID, stored.Seq)
	}
	outcome := <-handle.Done
	if s.hooks != nil {
		message := textOut.String()
		if outcome.Status != agent.RunCompleted {
			summary := string(outcome.Status)
			if outcome.Error != nil && outcome.Error.Message != "" {
				summary += ": " + outcome.Error.Message
			}
			message = summary
		}
		if len([]rune(message)) > sessionlog.MaxHookOutput {
			message = string([]rune(message)[:sessionlog.MaxHookOutput])
		}
		s.hooks.RunEndRun(s.hookAgentServiceContext(), s.hookAgentParent(request), request.Work.SessionID, string(outcome.Status), message)
	}
	if lease != nil {
		if _, err := manager.ReleaseCompletedWriter(context.Background(), *lease); err != nil {
			s.broadcastRun(ServerMsg{Type: "error", RunID: request.RunID, Error: "could not settle workspace writer: " + err.Error()}, request.Work.SessionID, request.RunID, 0)
		}
	} else if err := s.finalizeRunCandidate(context.Background(), request); err != nil {
		s.broadcastRun(ServerMsg{Type: "error", RunID: request.RunID, Error: "could not finalize candidate: " + err.Error()}, request.Work.SessionID, request.RunID, 0)
	}
	s.broadcastRun(ServerMsg{Type: "run_outcome", RunID: request.RunID, Outcome: &outcome}, request.Work.SessionID, request.RunID, 0)
	s.eventMu.Lock()
	s.mu.Lock()
	delete(s.activeRuns, request.RunID)
	delete(s.activeRequests, request.RunID)
	delete(s.runDone, request.RunID)
	delete(s.workspaceRuns, request.RunID)
	s.mu.Unlock()
	s.eventMu.Unlock()
	if s.deps.AgentTasks != nil {
		s.deps.AgentTasks.ForgetParent(request.Work.SessionID, request.RunID)
	}
}

func (s *Service) finalizeRunCandidate(ctx context.Context, request agent.ExecutionRequest) error {
	var authority permission.Authority
	if err := json.Unmarshal(request.PermissionBounds, &authority); err != nil || authority.CandidateRoot == "" {
		return nil
	}
	candidateRoot, err := filepath.Abs(authority.CandidateRoot)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(candidateRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	formalRoot := authority.FormalRoot
	if formalRoot == "" {
		formalRoot = authority.AllowedRoot
	}
	candidateID := filepath.Base(candidateRoot)
	var record *store.CandidateRecord
	if s.deps.Store != nil {
		loaded, getErr := s.deps.Store.GetCandidate(ctx, candidateID)
		if getErr == nil {
			record = &loaded
		} else if !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}
	}
	expectedGoalID := request.Work.GoalID
	if expectedGoalID == "" {
		expectedGoalID = "session-" + request.Work.SessionID
	}
	if record != nil {
		if record.GoalID != expectedGoalID || record.ActionID != "tool-run-"+request.RunID || filepath.Clean(record.Candidate.FormalRoot) != filepath.Clean(formalRoot) || filepath.Clean(record.Candidate.CandidateRoot) != filepath.Clean(candidateRoot) {
			return errors.New("candidate ownership does not match run")
		}
		formalRoot = record.Candidate.FormalRoot
	}
	if record != nil && record.Candidate.Status == "blocked" {
		// A failed snapshot checkpoint blocked this candidate during the
		// run. Keep it blocked: it must not be frozen ready, accepted, or
		// silently cleaned up.
		return nil
	}
	policy := candidate.ManifestPolicyLegacy
	if record != nil && record.Candidate.ManifestPolicy != "" {
		policy = record.Candidate.ManifestPolicy
	}
	_, candidateDigest, err := candidate.BuildManifestForPolicy(candidateRoot, policy)
	if err != nil {
		return err
	}
	baselineDigest := ""
	if record != nil {
		baselineDigest = record.Candidate.BaselineDigest
	} else {
		_, baselineDigest, err = candidate.BuildManifestForPolicy(formalRoot, policy)
		if err != nil {
			return err
		}
	}
	if candidateDigest == baselineDigest {
		if record != nil {
			if _, execErr := s.deps.Store.DB().ExecContext(ctx, "DELETE FROM candidate_reviews WHERE candidate_id=?", candidateID); execErr != nil {
				return execErr
			}
			if _, execErr := s.deps.Store.DB().ExecContext(ctx, "DELETE FROM candidates WHERE id=?", candidateID); execErr != nil {
				return execErr
			}
		}
		return os.RemoveAll(candidateRoot)
	}
	if record == nil {
		return errors.New("changed candidate is not registered")
	}
	frozen, err := candidate.FreezeCandidate(record.Candidate, nil, ctx)
	if err != nil {
		return err
	}
	if frozen.CandidateDigest != candidateDigest {
		return errors.New("candidate changed while finalizing")
	}
	return s.deps.Store.TransitionCandidate(ctx, candidateID, "running", "ready", frozen.CandidateDigest)
}

func (s *Service) subscribeRun(ctx context.Context, msg ClientMsg, updates chan ServerMsg) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	foundRun := msg.RunID == ""
	var replayOutcome *agent.RunOutcome
	if msg.RunID != "" {
		full, err := sessionlog.Replay(s.deps.ProjectRoot, msg.SessionID)
		if err != nil {
			return err
		}
		for _, event := range full.Events {
			if event.Type == sessionlog.EventRunStarted {
				var started sessionlog.RunStarted
				if decodeSessionData(event.Data, &started) == nil && started.RunID == msg.RunID {
					foundRun = true
				}
			}
			if event.Type == sessionlog.EventRunEvent {
				var runEvent sessionlog.RunEvent
				if decodeSessionData(event.Data, &runEvent) == nil && runEvent.RunID == msg.RunID && runEvent.Kind == string(agent.EventTerminal) {
					var terminal struct {
						Status agent.RunStatus    `json:"status"`
						Error  *llm.ProviderError `json:"error"`
					}
					if decodeSessionData(runEvent.Payload, &terminal) == nil {
						replayOutcome = &agent.RunOutcome{RunID: msg.RunID, Status: terminal.Status, Error: terminal.Error}
					}
				}
			}
		}
	}
	transcript, err := sessionlog.ReplayAfter(s.deps.ProjectRoot, msg.SessionID, msg.AfterSeq)
	if err != nil {
		return err
	}
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if err := decodeSessionData(event.Data, &runEvent); err != nil {
			return err
		}
		if msg.RunID != "" && runEvent.RunID != msg.RunID {
			continue
		}
		if runEvent.SessionID != msg.SessionID {
			continue
		}
		copy := runEvent
		if err := writeSub(ctx, updates, ServerMsg{Type: "run_event", RunID: runEvent.RunID, RunEvent: &copy, Cursor: event.Seq}); err != nil {
			return err
		}
	}
	if !foundRun {
		return fmt.Errorf("run %q was not found in session", msg.RunID)
	}
	s.mu.Lock()
	if sub := s.clients[updates]; sub != nil {
		sub.sessionID, sub.runID = msg.SessionID, msg.RunID
	}
	s.mu.Unlock()
	if s.deps.Store != nil {
		approvals, err := s.pendingApprovals(ctx, msg.SessionID)
		if err != nil {
			return err
		}
		for i := range approvals {
			a := permission.Prompt(approvals[i])
			if err = writeSub(ctx, updates, ServerMsg{Type: "approval_pending", Approval: &a}); err != nil {
				return err
			}
		}
	}
	if replayOutcome != nil {
		if err := writeSub(ctx, updates, ServerMsg{Type: "run_outcome", RunID: msg.RunID, Outcome: replayOutcome}); err != nil {
			return err
		}
	}
	_ = ctx
	return nil
}

func (s *Service) cancelRun(msg ClientMsg, updates chan ServerMsg) error {
	if s.deps.AgentTasks != nil {
		if task, err := s.findAgentRun(msg.SessionID, msg.RunID); err == nil {
			_, err = s.deps.AgentTasks.Stop(context.Background(), agent.ParentRun{Work: agent.WorkRef{SessionID: msg.SessionID}}, task.ID)
			return err
		}
	}
	s.mu.Lock()
	sessionID, active := s.activeRuns[msg.RunID]
	forkRun := s.activeForkRuns[msg.RunID]
	s.mu.Unlock()
	if forkRun != nil {
		if forkRun.sessionID != msg.SessionID {
			return errors.New("run does not belong to requested session")
		}
		forkRun.cancel()
		return nil
	}
	if !active {
		return nil
	}
	if sessionID != msg.SessionID {
		return errors.New("run does not belong to requested session")
	}
	if s.deps.Runner == nil {
		return errors.New("streaming agent is not configured")
	}
	if s.deps.AgentTasks != nil {
		s.deps.AgentTasks.CancelParent(msg.SessionID, msg.RunID)
	}
	if s.teamScheduler != nil {
		for _, cancel := range s.teamScheduler.invalidateParentRun(msg.RunID) {
			cancel()
		}
	}
	return s.deps.Runner.Cancel(msg.RunID)
}

func (s *Service) broadcastRun(msg ServerMsg, sessionID, runID string, cursor uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range s.clients {
		if sub.sessionID != sessionID || (msg.Type != "agent_task_update" && sub.runID != "" && sub.runID != runID) {
			continue
		}
		if sub.pendingCursor != 0 {
			resync := ServerMsg{Type: "resync", RunID: runID, Cursor: sub.pendingCursor}
			select {
			case sub.ch <- resync:
				sub.pendingCursor = 0
			default:
				if cursor > sub.pendingCursor {
					sub.pendingCursor = cursor
				}
				continue
			}
		}
		select {
		case sub.ch <- msg:
		default:
			if cursor > 0 && sub.pendingCursor == 0 {
				sub.pendingCursor = cursor - 1
			}
		}
	}
}

func writeSub(ctx context.Context, ch chan ServerMsg, msg ServerMsg) error {
	select {
	case ch <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func decodeSessionData(data any, target any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}
