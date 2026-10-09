package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

// AgentTaskCoordinator is bound once, after the service and shared pool exist.
// Only live tasks are kept in memory; queries project the durable session log.
type AgentTaskCoordinator struct {
	mu              sync.Mutex
	service         *Service
	active          map[string]*agentTaskState
	canceledParents map[string]bool
}
type agentTaskState struct {
	snapshot        agent.AgentTaskSnapshot
	work            agent.WorkRef
	cancel          context.CancelFunc
	done            chan struct{}
	runnerDone      chan struct{}
	roleInstruction string
	persistErr      error
	workspace       *workspace.LifecycleService
	writerLease     *workspace.WriterLease
}

func NewAgentTaskCoordinator() *AgentTaskCoordinator {
	return &AgentTaskCoordinator{active: map[string]*agentTaskState{}, canceledParents: map[string]bool{}}
}
func (c *AgentTaskCoordinator) Bind(s *Service) { c.mu.Lock(); c.service = s; c.mu.Unlock() }
func (c *AgentTaskCoordinator) host() (*Service, error) {
	c.mu.Lock()
	s := c.service
	c.mu.Unlock()
	if s == nil {
		return nil, errors.New("agent task service is unavailable")
	}
	return s, nil
}
func (c *AgentTaskCoordinator) Run(ctx context.Context, parent agent.ParentRun, req agent.AgentTaskRequest) (agent.AgentTaskSnapshot, error) {
	return c.run(ctx, parent, req, false)
}
func (c *AgentTaskCoordinator) run(ctx context.Context, parent agent.ParentRun, req agent.AgentTaskRequest, direct bool) (agent.AgentTaskSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return agent.AgentTaskSnapshot{}, err
	}
	s, err := c.host()
	if err != nil {
		return agent.AgentTaskSnapshot{}, err
	}
	submitter, ok := s.deps.Delegator.(agent.TaskSubmitter)
	if !ok || s.deps.Agents == nil {
		return agent.AgentTaskSnapshot{}, errors.New("agent tasks are unavailable")
	}
	if strings.TrimSpace(req.Instruction) == "" || !utf8.ValidString(req.Instruction) || req.Timeout < 0 || len(req.Model) > 256 || strings.IndexFunc(req.Model, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 || !utf8.ValidString(req.Model) {
		return agent.AgentTaskSnapshot{}, errors.New("agent task requires an instruction and nonnegative timeout")
	}
	def, ok := s.deps.Agents.Resolve(req.AgentName)
	if !ok {
		return agent.AgentTaskSnapshot{}, errors.New("unknown agent role")
	}
	if parent.Provider == nil || parent.ExecutorFactory == nil || parent.Work.SessionID == "" {
		return agent.AgentTaskSnapshot{}, errors.New("agent parent execution context is unavailable")
	}
	var authority permission.Authority
	if json.Unmarshal(parent.PermissionBounds, &authority) != nil || authority.RunID != parent.RunID || authority.SessionID != parent.Work.SessionID || authority.AllowedRoot != parent.ProjectRoot || authority.GoalID != parent.Work.GoalID || authority.WorkItemID != parent.Work.WorkItemID {
		return agent.AgentTaskSnapshot{}, errors.New("invalid agent parent authority")
	}
	runID, err := sessionlog.NewID()
	if err != nil {
		return agent.AgentTaskSnapshot{}, err
	}
	taskID, err := sessionlog.NewID()
	if err != nil {
		return agent.AgentTaskSnapshot{}, err
	}
	instruction := def.Instruction + "\n\nAssigned task:\n" + req.Instruction
	task := agent.DelegationTask{ID: taskID, Name: def.Name, Instruction: instruction}
	encoded, _ := json.Marshal([]agent.DelegationTask{task})
	if len(encoded) > 64<<10 {
		return agent.AgentTaskSnapshot{}, errors.New("agent task input exceeds 64 KiB")
	}
	isolation := req.Isolation
	if isolation == "" {
		isolation = def.Isolation
	}
	if isolation != "" && isolation != "none" && isolation != "worktree" {
		return agent.AgentTaskSnapshot{}, errors.New("unsupported agent task isolation")
	}
	origin := parent.RunID
	// Direct slash invocations carry a trusted synthetic authority but no origin run.
	if direct {
		origin = ""
	}
	if isolation == "worktree" && (def.Name == "explore" || def.Name == "plan" || (!direct && origin == "")) {
		return agent.AgentTaskSnapshot{}, errors.New("worktree isolation requires a persisted lead run and a writable agent role")
	}
	var manager *workspace.LifecycleService
	var writerLease *workspace.WriterLease
	if isolation == "worktree" {
		if !direct {
			active, activeErr := s.activeRunRequest(parent.Work.SessionID, parent.RunID)
			started, found, logErr := sessionlog.FindRunStart(currentProjectRoot(s.deps.ProjectRoot), parent.Work.SessionID, parent.RunID)
			if activeErr != nil || active.Work != parent.Work || active.TeamTurn != nil || active.TeamUser || logErr != nil || !found || started.TeamID != "" || started.AgentTaskID != "" || started.OriginRunID != "" {
				return agent.AgentTaskSnapshot{}, workspace.ErrOwnership
			}
		}
		projectRoot, scope, scopeErr := s.workspaceScope(ctx, ClientMsg{SessionID: parent.Work.SessionID, WorkKind: string(parent.Work.Kind), GoalID: parent.Work.GoalID, WorkItemID: parent.Work.WorkItemID})
		if scopeErr != nil || filepath.Clean(projectRoot) != filepath.Clean(parent.ProjectRoot) {
			return agent.AgentTaskSnapshot{}, workspace.ErrOwnership
		}
		if scope.Authority = authority; scope.ValidateAuthority() != nil {
			return agent.AgentTaskSnapshot{}, workspace.ErrOwnership
		}
		scope.OriginRunID = origin
		scope.OriginTaskID = taskID
		manager, err = s.workspaceService(projectRoot)
		if err != nil {
			return agent.AgentTaskSnapshot{}, err
		}
		created, createErr := manager.Create(ctx, scope, def.Name)
		if createErr != nil {
			return agent.AgentTaskSnapshot{}, createErr
		}
		lease, acquireErr := manager.AcquireWriter(ctx, scope, created.ID, runID)
		if acquireErr != nil {
			_, _ = manager.RemoveClean(context.Background(), scope, created.ID)
			return agent.AgentTaskSnapshot{}, acquireErr
		}
		writerLease = &lease
		// Keep the acquired workspace identity on the trusted delegation
		// context as well as in the executor closure and durable RunStarted
		// event. ChildRunInput is the execution boundary used by synchronous,
		// background, and definition-isolated named agents; carrying the lease
		// generation there lets every runner attribute work to the exact
		// workspace generation it is authorized to write.
		parent.WorkspaceID = lease.WorkspaceID
		parent.WorkspaceGeneration = lease.Generation
		parent.ExecutorFactory = execution.WorkspaceWriterExecutorFactory(parent.ExecutorFactory, lease, manager)
		if parent.ExecutorFactory == nil {
			_, _ = manager.ReleaseCompletedWriter(context.Background(), lease)
			return agent.AgentTaskSnapshot{}, errors.New("workspace writer executor is unavailable")
		}
		parent.ProjectRoot = lease.Authority.AllowedRoot
		parent.PermissionBounds, err = json.Marshal(lease.Authority)
		if err != nil {
			_, _ = manager.ReleaseCompletedWriter(context.Background(), lease)
			return agent.AgentTaskSnapshot{}, err
		}
	}
	parent.RunID = runID
	parent.ChildRunID = runID
	if writerLease == nil {
		authority.RunID = runID
		parent.PermissionBounds, err = json.Marshal(authority)
		if err != nil {
			return agent.AgentTaskSnapshot{}, err
		}
	}
	if def.Model != "" && def.Model != "inherit" {
		parent.Model = def.Model
	}
	if req.Model != "" && req.Model != "inherit" {
		parent.Model = req.Model
	}
	if parent.Budget.MaxToolRounds <= 0 || def.MaxTurns < parent.Budget.MaxToolRounds {
		parent.Budget.MaxToolRounds = def.MaxTurns
	}
	if req.Timeout > 0 && (parent.Budget.MaxDuration <= 0 || req.Timeout < parent.Budget.MaxDuration) {
		parent.Budget.MaxDuration = req.Timeout
	}
	allowed := map[string]bool{}
	for _, name := range def.EffectiveToolsForIsolation(isolation) {
		allowed[name] = true
	}
	parentTools := make(map[string]bool, len(parent.ToolSchemas))
	for _, schema := range parent.ToolSchemas {
		parentTools[schema.Name] = true
	}
	for name := range allowed {
		if !parentTools[name] {
			delete(allowed, name)
		}
	}
	schemas := []llm.ToolSchema{}
	for _, schema := range parent.ToolSchemas {
		if allowed[schema.Name] {
			schemas = append(schemas, schema)
		}
	}
	parent.ToolSchemas = schemas
	parent.RoleInstruction = def.Instruction
	parent.ExecutorFactory = roleExecutorFactory{inner: parent.ExecutorFactory, allowed: allowed}
	base := ctx
	background := req.Background || def.Background
	if background {
		base = s.lifeCtx
		if base == nil {
			base = context.Background()
		}
		parent.Deadline = time.Time{}
	}
	duration := agent.DefaultDelegationLimits().MaxDuration
	if parent.Budget.MaxDuration > 0 && parent.Budget.MaxDuration < duration {
		duration = parent.Budget.MaxDuration
	}
	runCtx, cancel := context.WithTimeout(base, duration)
	state := &agentTaskState{snapshot: agent.AgentTaskSnapshot{ID: taskID, RunID: runID, OriginRunID: origin, SessionID: parent.Work.SessionID, AgentName: def.Name, Name: def.Name, Status: agent.DelegationQueued}, work: parent.Work, cancel: cancel, done: make(chan struct{}), runnerDone: make(chan struct{}), roleInstruction: def.Instruction, workspace: manager, writerLease: writerLease}
	if writerLease != nil {
		state.snapshot.WorkspaceID = writerLease.WorkspaceID
		state.snapshot.WorkspaceGeneration = writerLease.Generation
	}
	c.mu.Lock()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if ctx.Err() != nil || closing || (origin != "" && c.canceledParents[parent.Work.SessionID+"/"+origin]) {
		c.mu.Unlock()
		cancel()
		if writerLease != nil {
			_, _ = manager.ReleaseCompletedWriter(context.Background(), *writerLease)
		}
		return agent.AgentTaskSnapshot{}, errors.New("agent parent was canceled or service is closing")
	}
	c.active[taskID] = state
	c.mu.Unlock()
	started := sessionlog.RunStarted{RunID: runID, WorkKind: string(parent.Work.Kind), GoalID: parent.Work.GoalID, WorkItemID: parent.Work.WorkItemID, Intent: "agent task " + def.Name, AgentTaskID: taskID, AgentName: def.Name, WorkspaceID: state.snapshot.WorkspaceID, WorkspaceGeneration: state.snapshot.WorkspaceGeneration, OriginRunID: origin, OriginCallID: parent.ToolCallID}
	s.eventMu.Lock()
	_, err = sessionlog.Append(s.deps.ProjectRoot, parent.Work.SessionID, sessionlog.EventRunStarted, started)
	s.eventMu.Unlock()
	if err != nil {
		if writerLease != nil {
			_, _ = manager.ReleaseCompletedWriter(context.Background(), *writerLease)
		}
		c.release(taskID, state)
		return agent.AgentTaskSnapshot{}, err
	}
	handle, err := submitter.SubmitTask(runCtx, parent, task)
	if err != nil {
		if writerLease != nil {
			_, _ = manager.ReleaseCompletedWriter(context.Background(), *writerLease)
		}
		persistErr := s.appendAgentTaskTerminal(state, agent.RunFailed, "", err.Error())
		c.release(taskID, state)
		if persistErr != nil {
			return agent.AgentTaskSnapshot{}, fmt.Errorf("agent submission failed; terminal persistence failed: %w", persistErr)
		}
		return agent.AgentTaskSnapshot{}, err
	}
	go func() {
		result := <-handle.Results
		close(state.runnerDone)
		status := agent.RunCompleted
		switch result.Status {
		case agent.DelegationFailed:
			status = agent.RunFailed
		case agent.DelegationCanceled:
			status = agent.RunCancelled
		case agent.DelegationInterrupted:
			status = agent.RunInterrupted
		}
		if state.writerLease != nil {
			if _, releaseErr := state.workspace.ReleaseCompletedWriter(context.Background(), *state.writerLease); releaseErr != nil {
				status = agent.RunFailed
				if result.Error == "" {
					result.Error = "workspace writer could not be safely released: " + releaseErr.Error()
				}
			}
		}
		if err := s.appendAgentTaskTerminal(state, status, result.Summary, result.Error); err != nil {
			state.persistErr = err
			s.broadcastRun(ServerMsg{Type: "error", RunID: runID, Error: "could not persist agent task terminal"}, parent.Work.SessionID, "", 0)
		}
		c.release(taskID, state)
	}()
	if background {
		return c.Output(ctx, parent, taskID, 0)
	}
	select {
	case <-state.done:
		if state.persistErr != nil {
			return agent.AgentTaskSnapshot{}, fmt.Errorf("could not persist agent task terminal: %w", state.persistErr)
		}
		return c.Output(ctx, parent, taskID, 0)
	case <-ctx.Done():
		cancel()
		<-state.done
		if state.persistErr != nil {
			return agent.AgentTaskSnapshot{}, state.persistErr
		}
		return c.Output(context.Background(), parent, taskID, 0)
	}
}

func (c *AgentTaskCoordinator) release(id string, state *agentTaskState) {
	c.mu.Lock()
	delete(c.active, id)
	close(state.done)
	c.mu.Unlock()
	state.cancel()
}
func taskSnapshot(sessionID string, r sessionlog.AgentTaskRecord) agent.AgentTaskSnapshot {
	status := agent.DelegationStatus(r.Delegation.Status)
	if status == "" {
		status = agent.DelegationQueued
		switch r.RunStatus {
		case "failed":
			status = agent.DelegationFailed
		case "interrupted":
			status = agent.DelegationInterrupted
		case "cancelled":
			status = agent.DelegationCanceled
		case "completed":
			status = agent.DelegationSucceeded
		}
	}
	reason := r.Delegation.Error
	if reason == "" && r.RunStatus != "completed" {
		reason = r.TerminalReason
	}
	return agent.AgentTaskSnapshot{ID: r.Started.AgentTaskID, RunID: r.Started.RunID, OriginRunID: r.Started.OriginRunID, SessionID: sessionID, AgentName: r.Started.AgentName, Name: r.Started.AgentName, WorkspaceID: r.Started.WorkspaceID, WorkspaceGeneration: r.Started.WorkspaceGeneration, Status: status, Stage: r.Delegation.Stage, Summary: r.Delegation.Summary, Error: reason, Cursor: r.LastSeq}
}
func (c *AgentTaskCoordinator) Output(ctx context.Context, parent agent.ParentRun, id string, wait time.Duration) (agent.AgentTaskSnapshot, error) {
	s, err := c.host()
	if err != nil {
		return agent.AgentTaskSnapshot{}, err
	}
	snapshot, err := s.getAgentTask(parent.Work.SessionID, id)
	if err != nil {
		return snapshot, err
	}
	if wait <= 0 {
		return snapshot, nil
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	c.mu.Lock()
	state := c.active[id]
	c.mu.Unlock()
	if state == nil {
		return snapshot, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-state.done:
		if state.persistErr != nil {
			return snapshot, fmt.Errorf("could not persist agent task terminal: %w", state.persistErr)
		}
	case <-timer.C:
	case <-ctx.Done():
		return snapshot, ctx.Err()
	}
	return s.getAgentTask(parent.Work.SessionID, id)
}
func (c *AgentTaskCoordinator) Stop(ctx context.Context, parent agent.ParentRun, id string) (agent.AgentTaskSnapshot, error) {
	snapshot, err := c.Output(ctx, parent, id, 0)
	if err != nil {
		return snapshot, err
	}
	c.mu.Lock()
	state := c.active[id]
	if state != nil && state.snapshot.SessionID == parent.Work.SessionID {
		state.cancel()
	}
	c.mu.Unlock()
	return snapshot, nil // Cancellation requested; the persisted state changes only after exit.
}
func (c *AgentTaskCoordinator) CancelParent(sessionID, runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.canceledParents[sessionID+"/"+runID] = true
	for _, state := range c.active {
		if state.snapshot.SessionID == sessionID && state.snapshot.OriginRunID == runID {
			state.cancel()
		}
	}
}
func (c *AgentTaskCoordinator) Close() {
	c.mu.Lock()
	pending := make([]<-chan struct{}, 0, len(c.active))
	for _, state := range c.active {
		state.cancel()
		pending = append(pending, state.done)
	}
	c.mu.Unlock()
	// The workspace manager closes after this method returns. Wait until each
	// runner has stopped and its writer lease has been settled first.
	for _, done := range pending {
		<-done
	}
}
func (s *Service) getAgentTask(sessionID, id string) (agent.AgentTaskSnapshot, error) {
	transcript, err := sessionlog.Replay(s.deps.ProjectRoot, sessionID)
	if err != nil {
		return agent.AgentTaskSnapshot{}, err
	}
	records, err := sessionlog.AgentTasks(transcript)
	if err != nil {
		return agent.AgentTaskSnapshot{}, err
	}
	for _, r := range records {
		if r.Started.AgentTaskID == id {
			return taskSnapshot(sessionID, r), nil
		}
	}
	return agent.AgentTaskSnapshot{}, errors.New("agent task does not belong to requested session")
}
func (s *Service) listAgentTasks(sessionID string, after uint64, limit int) ([]agent.AgentTaskSnapshot, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	transcript, err := sessionlog.Replay(s.deps.ProjectRoot, sessionID)
	if err != nil {
		return nil, err
	}
	records, err := sessionlog.AgentTasks(transcript)
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].LastSeq < records[j].LastSeq })
	result := []agent.AgentTaskSnapshot{}
	for _, r := range records {
		if r.LastSeq > after {
			result = append(result, taskSnapshot(sessionID, r))
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}
func (s *Service) appendAgentTaskTerminal(state *agentTaskState, status agent.RunStatus, summary, reason string) error {
	s.eventMu.Lock()
	transcript, err := sessionlog.Replay(s.deps.ProjectRoot, state.snapshot.SessionID)
	if err != nil {
		s.eventMu.Unlock()
		return err
	}
	records, err := sessionlog.AgentTasks(transcript)
	if err != nil {
		s.eventMu.Unlock()
		return err
	}
	var record sessionlog.AgentTaskRecord
	for _, r := range records {
		if r.Started.AgentTaskID == state.snapshot.ID {
			record = r
			break
		}
	}
	if record.RunStatus != "" {
		s.eventMu.Unlock()
		return nil
	}
	id, err := sessionlog.NewID()
	if err != nil {
		s.eventMu.Unlock()
		return err
	}
	runEvent := sessionlog.RunEvent{ID: id, RunID: state.snapshot.RunID, SessionID: state.snapshot.SessionID, RunSeq: record.LastRunSeq + 1, At: time.Now().UTC(), Kind: string(agent.EventTerminal), Payload: map[string]any{"status": status, "summary": truncateDelegationText(redactRunCredential(hideRoleBody(summary, state.roleInstruction), s.deps.ProviderCredential), 8<<10), "reason": truncateDelegationText(redactRunCredential(hideRoleBody(reason, state.roleInstruction), s.deps.ProviderCredential), 1024)}}
	stored, err := sessionlog.Append(s.deps.ProjectRoot, state.snapshot.SessionID, sessionlog.EventRunEvent, runEvent)
	s.eventMu.Unlock()
	if err != nil {
		return err
	}
	s.broadcastRun(ServerMsg{Type: "run_event", RunID: state.snapshot.RunID, RunEvent: &runEvent, Cursor: stored.Seq}, state.snapshot.SessionID, state.snapshot.RunID, stored.Seq)
	snapshot, getErr := s.getAgentTask(state.snapshot.SessionID, state.snapshot.ID)
	if getErr == nil {
		s.broadcastRun(ServerMsg{Type: "agent_task_update", AgentTask: &snapshot, RunID: state.snapshot.RunID, Cursor: stored.Seq}, state.snapshot.SessionID, "", stored.Seq)
	}
	return getErr
}

// Definitions constrain dispatch as well as the schema sent to the model.
type roleExecutorFactory struct {
	inner   agent.ExecutorFactory
	allowed map[string]bool
}

func (f roleExecutorFactory) ForRun(req agent.ExecutionRequest) (agent.RunExecutor, error) {
	inner, err := f.inner.ForRun(req)
	if err != nil {
		return nil, err
	}
	return roleExecutor{inner: inner, allowed: f.allowed}, nil
}

type roleExecutor struct {
	inner   agent.RunExecutor
	allowed map[string]bool
}

func (e roleExecutor) Execute(ctx context.Context, call llm.ToolUse) (agent.ToolOutcome, error) {
	if !e.allowed[call.Name] {
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolDenied, IsError: true, Content: "Error: tool is not allowed by the agent role"}, nil
	}
	return e.inner.Execute(ctx, call)
}

var _ agent.AgentTaskService = (*AgentTaskCoordinator)(nil)

func (c *AgentTaskCoordinator) ownsRun(runID, sessionID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c.active {
		if state.snapshot.RunID == runID && state.snapshot.SessionID == sessionID {
			return true
		}
	}
	return false
}
func (c *AgentTaskCoordinator) ForgetParent(sessionID, runID string) {
	c.mu.Lock()
	delete(c.canceledParents, sessionID+"/"+runID)
	c.mu.Unlock()
}
func (s *Service) findAgentRun(sessionID, runID string) (agent.AgentTaskSnapshot, error) {
	transcript, err := sessionlog.Replay(s.deps.ProjectRoot, sessionID)
	if err != nil {
		return agent.AgentTaskSnapshot{}, err
	}
	records, err := sessionlog.AgentTasks(transcript)
	if err != nil {
		return agent.AgentTaskSnapshot{}, err
	}
	for _, r := range records {
		if r.Started.RunID == runID {
			return taskSnapshot(sessionID, r), nil
		}
	}
	return agent.AgentTaskSnapshot{}, errors.New("not an agent task run")
}

func hideRoleBody(text, body string) string {
	return agent.SanitizeRoleOutput(text, body)
}
func (c *AgentTaskCoordinator) sanitizeEvent(runID string, event agent.DelegationEvent) agent.DelegationEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c.active {
		if state.snapshot.RunID == runID {
			event.Summary = hideRoleBody(event.Summary, state.roleInstruction)
			event.Error = hideRoleBody(event.Error, state.roleInstruction)
			break
		}
	}
	return event
}

// StopWorkspaceWriter waits for the runner, whose tool calls wait for their
// sandbox process trees. It does not wait for lease settlement: the workspace
// manager may hold its lifecycle lock while it invokes this callback.
func (c *AgentTaskCoordinator) StopWorkspaceWriter(ctx context.Context, lease workspace.WriterLease) error {
	if c == nil {
		return workspace.ErrUnavailable
	}
	c.mu.Lock()
	var found *agentTaskState
	for _, state := range c.active {
		if state.writerLease != nil && state.writerLease.WorkspaceID == lease.WorkspaceID && state.writerLease.RunID == lease.RunID && state.writerLease.Generation == lease.Generation && state.work == lease.Scope.Work && state.writerLease.Scope.SameOwner(lease.Scope) {
			found = state
			state.cancel()
			break
		}
	}
	c.mu.Unlock()
	if found == nil {
		return workspace.ErrUnavailable
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case <-found.runnerDone:
		return nil
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
}

var _ workspace.WriterStopper = (*AgentTaskCoordinator)(nil)
