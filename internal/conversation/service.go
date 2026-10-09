package conversation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/mcp"
	"stable/internal/permission"
	"stable/internal/platform/ipc"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
	"stable/internal/workspace"
)

type Deps struct {
	Store               *store.Store
	Provider            decision.StructuredProvider
	ChatProvider        decision.ChatProvider
	Runner              agent.Runner
	Delegator           agent.Delegator
	Agents              agentcatalog.Catalog
	AgentTasks          *AgentTaskCoordinator
	ForkProvider        llm.Provider
	ForkExecutorFactory agent.ExecutorFactory
	ForkToolSchemas     []llm.ToolSchema
	ExecutorFactory     agent.ExecutorFactory
	ToolSchemas         []llm.ToolSchema
	ProviderName        string
	Model               string
	RunnerError         string
	CandidateCheckers   []candidate.Checker
	PermissionService   *permission.PermissionService
	ProviderCredential  string
	Temporal            string
	ProjectRoot         string
	RunRoot             string
	// WorkspaceStateRoot is a service-owned directory outside the project.
	// Empty disables worktree lifecycle operations.
	WorkspaceStateRoot     string
	WorkspaceLifecycleHost *execution.WorkspaceLifecycleToolHost
	SocketPath             string
	// ContextWindowTokens overrides the model context window used for
	// compaction; zero resolves to the sessioncontext default.
	ContextWindowTokens int
	// Snapshots is the candidate snapshot store used by snapshot_list and
	// snapshot_rewind; nil makes both report snapshots as unavailable.
	Snapshots *candidate.SnapshotStore
	// Skills, when set, enables the M07-A skill feature: the skill_invoke,
	// skill_reload and skill_list ops, the load_skill tool provider and the
	// per-run skill inventory injection. Nil keeps the skill surface closed.
	Skills    *SkillGate
	Hooks     *HookGate
	MCP       *mcp.Manager
	Refresher core.DependencyRefresher
	PollEvery time.Duration // goal status poll interval; 0 defaults to 2s
}

// Service is the persistent chat session: it owns the unix socket, fans out
// messages to every connected client, and translates user input into goal
// events. Client connections are thin terminals; all state lives here and in
// the store.
type Service struct {
	deps              Deps
	lifeCtx           context.Context
	ln                net.Listener
	mu                sync.Mutex
	clients           map[chan ServerMsg]*clientSubscription
	statuses          map[string]core.GoalStatus
	activeRuns        map[string]string
	activeRequests    map[string]agent.ExecutionRequest
	runDone           map[string]chan struct{}
	activeForkRuns    map[string]*forkRunState
	closing           bool
	notifiedApprovals map[string]bool
	eventMu           sync.Mutex
	// askMu guards the per-session count of runs blocked inside the question
	// adapter (see AskAdapter) and the poll-loop delivery dedup state below.
	askMu sync.Mutex
	// askWaiters counts, per session, the in-flight AskAdapter waits so
	// replyQuestion can tell a live waiting run from a leftover question.
	askWaiters map[string]int
	// notifiedQuestions and notifiedPlanApprovals remember which pending
	// dialog was already delivered to a session client, so the poll loop
	// pushes each pending question and plan approval once instead of
	// re-sending the full set every tick. Entries are pruned when the
	// underlying request is no longer pending.
	notifiedQuestions     map[string]bool
	notifiedPlanApprovals map[string]bool
	// planMu guards the plan mode runtime state below. Plan state and plan
	// approvals are service-lifetime memory: a restart drops sessions back to
	// the default mode and clears pending dialogs (the session log keeps the
	// transitions auditable).
	planMu        sync.Mutex
	planStates    map[string]*PlanState
	planApprovals map[string]*PlanApproval
	// skills is the M07-A skill gate copied from deps at Serve; nil closes
	// the skill surface. The gate itself also keeps a service reference (set
	// by Bind) so its event appends share the service event mutex.
	skills          *SkillGate
	hooks           *HookGate
	mcp             *mcp.Manager
	mcpMu           sync.Mutex
	mcpInstructions map[string]bool
	teamScheduler   *teamScheduler
	// teamMemberStateAppender is an optional per-service persistence seam used
	// to exercise recovery of a failed state append. Nil uses the session log.
	teamMemberStateAppender func(root string, team teams.Team, runID, actor string, member teams.Member) error
	workspaceMu             sync.Mutex
	workspaceAdmissionMu    sync.Mutex
	workspaceTransitionMu   sync.Mutex
	workspaces              map[string]*workspace.LifecycleService
	workspaceRuns           map[string]workspaceLeadRun
}

type workspaceLeadRun struct {
	lease   workspace.WriterLease
	manager *workspace.LifecycleService
}

type clientSubscription struct {
	ch            chan ServerMsg
	sessionID     string
	runID         string
	pendingCursor uint64
}

func Serve(ctx context.Context, deps Deps) (*Service, error) {
	if deps.PollEvery <= 0 {
		deps.PollEvery = 2 * time.Second
	}
	if err := recoverAgentTaskRuns(deps.ProjectRoot); err != nil {
		return nil, fmt.Errorf("recover agent tasks: %w", err)
	}
	if err := recoverDelegationRuns(deps.ProjectRoot); err != nil {
		return nil, fmt.Errorf("recover interrupted delegations: %w", err)
	}
	if err := recoverTeamRuns(deps.ProjectRoot); err != nil {
		return nil, fmt.Errorf("recover interrupted team turns: %w", err)
	}
	ln, err := ipc.ListenPrivate(deps.SocketPath, true)
	if err != nil {
		return nil, err
	}
	s := &Service{deps: deps, lifeCtx: ctx, ln: ln, clients: map[chan ServerMsg]*clientSubscription{}, statuses: map[string]core.GoalStatus{}, activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{}, runDone: map[string]chan struct{}{}, activeForkRuns: map[string]*forkRunState{}, notifiedApprovals: map[string]bool{}, skills: deps.Skills, hooks: deps.Hooks, mcp: deps.MCP, mcpInstructions: map[string]bool{}, workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{}}
	if deps.WorkspaceLifecycleHost != nil {
		deps.WorkspaceLifecycleHost.Bind(s.executeWorkspaceLifecycleTool)
	}
	if deps.WorkspaceStateRoot != "" {
		if err := s.recoverWorkspaceToolTransitions(); err != nil {
			return nil, fmt.Errorf("recover workspace lifecycle tool transitions: %w", err)
		}
	}
	s.teamScheduler = newTeamScheduler(s)
	if deps.AgentTasks != nil {
		deps.AgentTasks.Bind(s)
	}
	if deps.Skills != nil {
		deps.Skills.Bind(s)
	}
	if deps.Hooks != nil {
		deps.Hooks.Bind(s)
	}
	go s.pollGoals(ctx)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go s.acceptLoop(ctx)
	return s, nil
}

func (s *Service) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if ctx.Err() == nil && !closing {
				continue
			}
			return
		}
		go s.serveConn(ctx, conn)
	}
}

func (s *Service) Close() error {
	s.workspaceAdmissionMu.Lock()
	s.mu.Lock()
	s.closing = true
	for _, run := range s.activeForkRuns {
		run.cancel()
	}
	runIDs := make([]string, 0, len(s.activeRuns))
	done := make([]<-chan struct{}, 0, len(s.runDone))
	for runID := range s.activeRuns {
		runIDs = append(runIDs, runID)
	}
	for _, finished := range s.runDone {
		done = append(done, finished)
	}
	s.mu.Unlock()
	s.workspaceAdmissionMu.Unlock()
	var closeErr error
	if err := s.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		closeErr = errors.Join(closeErr, err)
	}
	if s.deps.Runner != nil {
		for _, runID := range runIDs {
			closeErr = errors.Join(closeErr, s.deps.Runner.Cancel(runID))
		}
	}
	if s.deps.AgentTasks != nil {
		s.deps.AgentTasks.Close()
	}
	if s.teamScheduler != nil {
		s.teamScheduler.close()
		s.teamScheduler.mu.Lock()
		for _, writer := range s.teamScheduler.workspaceRuns {
			if writer.done != nil {
				done = append(done, writer.done)
			}
		}
		s.teamScheduler.mu.Unlock()
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, finished := range done {
		select {
		case <-finished:
		case <-waitCtx.Done():
			// A live run may still own a workspace writer. Leave its manager
			// and durable lease open so restart recovery can mark it interrupted.
			return errors.Join(closeErr, fmt.Errorf("wait for active runs before closing workspace managers: %w", waitCtx.Err()))
		}
	}
	if s.hooks != nil {
		s.hooks.Close()
	}
	s.workspaceMu.Lock()
	for key, manager := range s.workspaces {
		closeErr = errors.Join(closeErr, manager.Close(context.Background()))
		delete(s.workspaces, key)
	}
	s.workspaceMu.Unlock()
	return closeErr
}

func (s *Service) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	updates := make(chan ServerMsg, 64)
	sub := &clientSubscription{ch: updates}
	s.mu.Lock()
	s.clients[updates] = sub
	s.mu.Unlock()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for msg := range updates {
			if err := encodeServer(conn, msg); err != nil {
				return
			}
		}
	}()

	s.readLoop(ctx, conn, updates)
	s.mu.Lock()
	delete(s.clients, updates)
	s.mu.Unlock()
	close(updates) // unblocks the writer
	<-writerDone
}

func (s *Service) readLoop(ctx context.Context, conn net.Conn, updates chan ServerMsg) error {
	dec := jsonDecoder(conn)
	for {
		var c ClientMsg
		if err := dec.Decode(&c); err != nil {
			return err
		}
		if err := validateClient(c); err != nil {
			updates <- ServerMsg{Type: "error", Error: err.Error()}
			updates <- ServerMsg{Type: "done"}
			continue
		}
		switch c.Op {
		case "agent_list", "agent_reload", "agent_task_start", "agent_task_list", "agent_task_get", "agent_task_cancel":
			msgs, err := s.handleAgentRequest(ctx, c)
			for _, msg := range msgs {
				updates <- msg
			}
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "run_start":
			if err := s.startRun(ctx, c, updates); err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			}
			continue
		case "skill_invoke":
			// On success the stream stays open: startRun binds this connection
			// to the run, so run events flow to the invoking client until the
			// outcome closes it. On failure the activation error is a plain
			// error + done, so both streaming and one-shot clients terminate.
			if err := s.invokeSkill(ctx, c, updates); err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
				updates <- ServerMsg{Type: "done"}
			}
			continue
		case "run_subscribe":
			if err := s.subscribeRun(ctx, c, updates); err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			}
			continue
		case "run_cancel":
			if err := s.cancelRun(c, updates); err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "done", RunID: c.RunID}
			}
			continue
		case "team_create", "team_list", "team_get", "team_close", "team_coordinator", "team_member_spawn", "team_member_resume", "team_member_stop", "team_send", "team_messages", "team_request_list", "team_request_respond", "team_shutdown_request", "team_task_create", "team_task_get", "team_task_list", "team_task_update":
			msg, err := s.handleTeamRequest(ctx, c)
			if err != nil {
				if errors.Is(err, teams.ErrRevisionConflict) && msg.TeamTask != nil {
					updates <- msg
				}
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- msg
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "worktree_create", "worktree_list", "worktree_get", "worktree_enter", "worktree_exit", "worktree_preview", "worktree_keep", "worktree_export", "worktree_remove", "worktree_resolve", "worktree_discard_preview", "worktree_discard":
			msg, err := s.handleWorkspaceRequest(ctx, c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- msg
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "review_get":
			review, err := s.reviewCandidate(ctx, c.CandidateID, c.SessionID)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "review", Review: &review}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "review_accept":
			receipt, err := s.acceptReviewedCandidate(ctx, c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "acceptance", Receipt: &receipt}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "approval_list":
			approvals, err := s.pendingApprovals(ctx, c.SessionID)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				prompts := make([]permission.ApprovalPrompt, 0, len(approvals))
				for _, approval := range approvals {
					prompts = append(prompts, permission.Prompt(approval))
				}
				updates <- ServerMsg{Type: "approvals", Approvals: prompts}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "approval_resolve":
			decision, err := s.resolveApproval(ctx, c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "permission_decision", Decision: &decision}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "approval_cancel":
			if err := s.cancelApproval(ctx, c); err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "approval_cancelled", RunID: c.RunID}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "session_search":
			result, err := s.searchSessions(c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "search", Search: &result}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "snapshot_list":
			snapshots, err := s.listSnapshots(c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "snapshots", Snapshots: snapshots}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "snapshot_rewind":
			record, err := s.rewindSnapshot(ctx, c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "rewind", Rewind: &record}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "question_list":
			questions, err := s.listQuestions(c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "questions", Questions: questions}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "session_load":
			s.mu.Lock()
			if sub := s.clients[updates]; sub != nil {
				sub.sessionID, sub.runID = c.SessionID, ""
			}
			s.mu.Unlock()
		}
		msgs, err := s.handle(ctx, c)
		for i := range msgs {
			s.broadcast(msgs[i])
		}
		if err != nil {
			updates <- ServerMsg{Type: "error", Error: err.Error()}
		}
		// Queue completion after this client's broadcast messages so one-shot
		// clients can distinguish a finished request from a slow model response.
		updates <- ServerMsg{Type: "done"}
	}
}

func (s *Service) broadcast(msg ServerMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- msg:
		default: // drop for slow clients; history replay covers reconnects
		}
	}
}

// pollGoals pushes goal status changes to connected clients so terminals see
// progress without running goal status themselves.
func (s *Service) pollGoals(ctx context.Context) {
	tick := time.NewTicker(s.deps.PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.deliverPermissionWakes(ctx)
		s.pushPendingApprovals(ctx)
		s.pushPendingQuestions()
		s.pushPendingPlanApprovals()
		if s.deps.Store == nil {
			continue
		}
		goals, err := s.deps.Store.ListGoals(ctx)
		if err != nil {
			continue
		}
		s.mu.Lock()
		for i := range goals {
			g := goals[i]
			if prev, ok := s.statuses[g.ID]; !ok || prev != g.Status {
				s.statuses[g.ID] = g.Status
				s.mu.Unlock()
				s.broadcast(ServerMsg{Type: "goal_update", Goal: &g})
				s.mu.Lock()
			}
		}
		s.mu.Unlock()
	}
}

func (s *Service) pushPendingApprovals(ctx context.Context) {
	if s.deps.Store == nil {
		return
	}
	s.mu.Lock()
	seen := map[string]bool{}
	for _, sub := range s.clients {
		if sub.sessionID != "" {
			seen[sub.sessionID] = true
		}
	}
	s.mu.Unlock()
	for sessionID := range seen {
		requests, err := s.pendingApprovals(ctx, sessionID)
		if err != nil {
			continue
		}
		for _, request := range requests {
			s.pushApproval(ctx, request.ID, sessionID)
		}
	}
}

// pushPendingQuestions mirrors pushPendingApprovals for the ask_user
// lifecycle: sessions with bound clients get their still-pending questions
// pushed once, tracked by question id so the poll never re-sends the same
// dialog. Delivery is re-attempted while the send fails, and the dedup
// entries are dropped once the question is answered.
func (s *Service) pushPendingQuestions() {
	s.mu.Lock()
	seen := map[string]bool{}
	for _, sub := range s.clients {
		if sub.sessionID != "" {
			seen[sub.sessionID] = true
		}
	}
	s.mu.Unlock()
	for sessionID := range seen {
		questions, err := s.listQuestions(ClientMsg{SessionID: sessionID})
		if err != nil {
			continue
		}
		pending := map[string]bool{}
		s.mu.Lock()
		var fresh []sessionlog.PendingQuestion
		for _, question := range questions {
			if question.Status == sessionlog.QuestionReplied {
				continue
			}
			pending[question.QuestionID] = true
			if !s.notifiedQuestions[question.QuestionID] {
				fresh = append(fresh, question)
			}
		}
		for id := range s.notifiedQuestions {
			if !pending[id] {
				delete(s.notifiedQuestions, id)
			}
		}
		s.mu.Unlock()
		if len(fresh) > 0 {
			s.pushQuestions(sessionID, fresh)
		}
	}
}

// pushPendingPlanApprovals mirrors pushPendingQuestions for the plan approval
// lifecycle. The pending set is service-lifetime memory (a restart clears
// pending dialogs), matching the PlanApprovalService; the broadcast reuses
// the plan_approvals message shape.
func (s *Service) pushPendingPlanApprovals() {
	s.mu.Lock()
	seen := map[string]bool{}
	for _, sub := range s.clients {
		if sub.sessionID != "" {
			seen[sub.sessionID] = true
		}
	}
	s.mu.Unlock()
	for sessionID := range seen {
		s.planMu.Lock()
		pending := map[string]bool{}
		var refs []PlanApprovalRef
		for _, approval := range s.planApprovals {
			if approval == nil || approval.SessionID != sessionID || approval.Status != sessionlog.PlanApprovalSubmitted {
				continue
			}
			pending[approval.ID] = true
			refs = append(refs, planApprovalRef(*approval))
		}
		s.planMu.Unlock()
		if len(refs) == 0 {
			s.mu.Lock()
			for id := range s.notifiedPlanApprovals {
				delete(s.notifiedPlanApprovals, id)
			}
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		var fresh []PlanApprovalRef
		for _, ref := range refs {
			if !s.notifiedPlanApprovals[ref.ID] {
				fresh = append(fresh, ref)
			}
		}
		s.mu.Unlock()
		if len(fresh) == 0 {
			continue
		}
		msg := ServerMsg{Type: "plan_approvals", PlanApprovals: fresh}
		s.mu.Lock()
		delivered := false
		for ch, sub := range s.clients {
			if sub.sessionID != sessionID {
				continue
			}
			select {
			case ch <- msg:
				delivered = true
			default:
			}
		}
		if delivered {
			if s.notifiedPlanApprovals == nil {
				s.notifiedPlanApprovals = map[string]bool{}
			}
			for _, ref := range fresh {
				s.notifiedPlanApprovals[ref.ID] = true
			}
		}
		s.mu.Unlock()
	}
}
