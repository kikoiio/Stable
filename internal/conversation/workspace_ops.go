package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

const workspaceQueryTimeout = 30 * time.Second

func isWorkspaceQuery(op string) bool {
	switch op {
	case "worktree_list", "worktree_get", "worktree_preview":
		return true
	// worktree_discard_preview persists a user-bound discard decision and is
	// therefore a lifecycle mutation rather than a read-only query.
	default:
		return false
	}
}

func withWorkspaceQuery[T any](ctx context.Context, op string, query func(context.Context) (T, error)) (T, error) {
	if !isWorkspaceQuery(op) {
		return query(ctx)
	}
	queryCtx, cancel := context.WithTimeout(ctx, workspaceQueryTimeout)
	defer cancel()
	return query(queryCtx)
}

// CanSwitchWorkspace keeps persisted workspace bindings stable while any run
// from the owning session may still be using its authority. Switching a
// binding never changes process cwd or rewrites an already-created authority.
func (s *Service) CanSwitchWorkspace(ctx context.Context, scope workspace.Scope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	for runID, sessionID := range s.activeRuns {
		if sessionID == scope.SessionID {
			s.mu.Unlock()
			return workspace.ErrUnavailable
		}
		if request, ok := s.activeRequests[runID]; ok && request.Work == scope.Work {
			s.mu.Unlock()
			return workspace.ErrUnavailable
		}
	}
	s.mu.Unlock()
	if tasks := s.deps.AgentTasks; tasks != nil {
		tasks.mu.Lock()
		defer tasks.mu.Unlock()
		for _, task := range tasks.active {
			if task.work.SessionID == scope.SessionID {
				return workspace.ErrUnavailable
			}
		}
	}
	return nil
}

// LockWorkspaceBinding shares one admission boundary between lifecycle
// mutations and run start. The caller keeps the returned unlock function
// until its persisted binding operation or RunStarted fact is complete.
func (s *Service) LockWorkspaceBinding(ctx context.Context, scope workspace.Scope) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.workspaceAdmissionMu.Lock()
	if err := ctx.Err(); err != nil {
		s.workspaceAdmissionMu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		s.workspaceAdmissionMu.Unlock()
		return nil, workspace.ErrUnavailable
	}
	return s.workspaceAdmissionMu.Unlock, nil
}

// StopWorkspaceWriter is used only by the lifecycle manager after it has
// durably recorded a stopping intent. It waits for consumeRun to observe the
// actual Runner.Done signal and finish lease settlement.
func (s *Service) StopWorkspaceWriter(ctx context.Context, lease workspace.WriterLease) error {
	s.mu.Lock()
	lead, ok := s.workspaceRuns[lease.RunID]
	done := s.runDone[lease.RunID]
	if ok && (lead.lease.WorkspaceID != lease.WorkspaceID || lead.lease.Generation != lease.Generation || !lead.lease.Scope.SameOwner(lease.Scope)) {
		s.mu.Unlock()
		return workspace.ErrOwnership
	}
	s.mu.Unlock()
	if !ok {
		if s.teamScheduler != nil {
			s.teamScheduler.mu.Lock()
			teamRun, teamOK := s.teamScheduler.workspaceRuns[lease.RunID]
			s.teamScheduler.mu.Unlock()
			if teamOK {
				if teamRun.lease.WorkspaceID != lease.WorkspaceID || teamRun.lease.Generation != lease.Generation || !teamRun.lease.Scope.SameOwner(lease.Scope) {
					return workspace.ErrOwnership
				}
				if teamRun.cancel == nil || teamRun.done == nil {
					return workspace.ErrUnavailable
				}
				teamRun.cancel()
				select {
				case <-teamRun.done:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if s.deps.AgentTasks != nil {
			return s.deps.AgentTasks.StopWorkspaceWriter(ctx, lease)
		}
		return workspace.ErrUnavailable
	}
	if s.deps.Runner == nil {
		return workspace.ErrUnavailable
	}
	if err := s.deps.Runner.Cancel(lease.RunID); err != nil {
		return err
	}
	if done == nil {
		return workspace.ErrUnavailable
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) workspaceService(formalRoot string) (*workspace.LifecycleService, error) {
	if s.deps.WorkspaceStateRoot == "" {
		return nil, workspace.ErrUnavailable
	}
	root, err := filepath.EvalSymlinks(formalRoot)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	s.workspaceMu.Lock()
	if manager := s.workspaces[root]; manager != nil {
		s.workspaceMu.Unlock()
		if err := s.replayAcceptedWorkspaceRoots(context.Background(), manager, root); err != nil {
			return nil, err
		}
		return manager, nil
	}
	digest := sha256.Sum256([]byte(filepath.Clean(root)))
	projectID := "p" + hex.EncodeToString(digest[:16])
	layout, err := workspace.NewLayout(s.deps.WorkspaceStateRoot, root, projectID)
	if err != nil {
		return nil, err
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{IdleGuard: s, Stopper: s, Exporter: workspaceCandidateExporter{service: s}})
	if err != nil {
		s.workspaceMu.Unlock()
		return nil, err
	}
	s.workspaces[root] = manager
	s.workspaceMu.Unlock()
	if err := s.replayAcceptedWorkspaceRoots(context.Background(), manager, root); err != nil {
		return nil, err
	}
	return manager, nil
}

func (s *Service) replayAcceptedWorkspaceRoots(ctx context.Context, manager *workspace.LifecycleService, formalRoot string) error {
	if manager == nil || s.deps.Store == nil {
		return nil
	}
	transitions, err := s.deps.Store.AcceptedProjectRootTransitions(ctx, formalRoot)
	if err != nil {
		return err
	}
	if len(transitions) == 0 {
		return nil
	}
	currentToken, err := candidate.CaptureRootIdentity(formalRoot)
	if err != nil {
		return err
	}
	target, err := workspace.RootIdentityFromToken(currentToken)
	if err != nil {
		return err
	}
	// Walk the durable acceptance chain backward from the root currently at
	// the formal path. Rebinding each exact predecessor to this verified final
	// identity is safe after a crash in the middle of an earlier replay.
	seen := make(map[string]bool, len(transitions))
	for range transitions {
		if seen[currentToken] {
			return workspace.ErrOwnership
		}
		seen[currentToken] = true
		matched := false
		for _, transition := range transitions {
			if transition.TargetIdentity == currentToken {
				currentToken = transition.ExpectedIdentity
				matched = true
				break
			}
		}
		if !matched {
			break
		}
		expected, parseErr := workspace.RootIdentityFromToken(currentToken)
		if parseErr != nil {
			return parseErr
		}
		if err := manager.RebindAcceptedFormalRoot(ctx, expected, target); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) workspaceScope(ctx context.Context, msg ClientMsg) (string, workspace.Scope, error) {
	work := agent.WorkRef{Kind: agent.WorkKind(msg.WorkKind), SessionID: msg.SessionID, GoalID: msg.GoalID, WorkItemID: msg.WorkItemID}
	if work.Kind == "" {
		work.Kind = agent.WorkSession
	}
	_, trusted, err := s.scopeForWork(ctx, currentProjectRoot(s.deps.ProjectRoot), work)
	if err != nil {
		return "", workspace.Scope{}, workspace.ErrOwnership
	}
	// scopeForWork's first result is the session log root; the validated scope
	// carries the exact formal root bound to the persisted session/Goal.
	projectRoot := trusted.ProjectRoot
	digest := sha256.Sum256([]byte(filepath.Clean(projectRoot)))
	scope := workspace.Scope{ProjectID: "p" + hex.EncodeToString(digest[:16]), SessionID: msg.SessionID, Work: work}
	if err := scope.Validate(); err != nil {
		return "", workspace.Scope{}, err
	}
	return projectRoot, scope, nil
}

func (s *Service) handleWorkspaceRequest(ctx context.Context, msg ClientMsg) (ServerMsg, error) {
	return withWorkspaceQuery(ctx, msg.Op, func(queryCtx context.Context) (ServerMsg, error) {
		return s.handleWorkspaceRequestWithContext(queryCtx, msg)
	})
}

func (s *Service) handleWorkspaceRequestWithContext(ctx context.Context, msg ClientMsg) (ServerMsg, error) {
	if err := ctx.Err(); err != nil {
		return ServerMsg{}, err
	}
	projectRoot, scope, err := s.workspaceScope(ctx, msg)
	if err != nil {
		return ServerMsg{}, err
	}
	manager, err := s.workspaceService(projectRoot)
	if err != nil {
		return ServerMsg{}, err
	}
	var snapshot workspace.Snapshot
	switch msg.Op {
	case "worktree_create":
		request, requestErr := s.activeRunRequest(msg.SessionID, msg.RunID)
		if requestErr != nil || request.Work != scope.Work {
			return ServerMsg{}, workspace.ErrOwnership
		}
		run, found, runErr := sessionlog.FindRunStart(currentProjectRoot(s.deps.ProjectRoot), msg.SessionID, msg.RunID)
		if runErr != nil || !found || !workRefMatchesRun(request.Work, run) || run.TeamID != "" || run.AgentTaskID != "" || run.OriginRunID != "" || request.TeamTurn != nil || request.TeamUser {
			return ServerMsg{}, workspace.ErrOwnership
		}
		var authority permission.Authority
		if json.Unmarshal(request.PermissionBounds, &authority) != nil || authority.RunID != request.RunID || authority.SessionID != request.Work.SessionID || authority.GoalID != request.Work.GoalID || authority.WorkItemID != request.Work.WorkItemID {
			return ServerMsg{}, workspace.ErrOwnership
		}
		scope.Authority = authority
		scope.OriginRunID = request.RunID
		snapshot, err = manager.Create(ctx, scope, msg.Text)
	case "worktree_list":
		var snapshots []workspace.Snapshot
		snapshots, err = manager.List(ctx, scope, msg.AfterSeq, msg.Limit)
		if err != nil {
			return ServerMsg{}, err
		}
		return ServerMsg{Type: "worktree_list", Worktrees: snapshots, Cursor: msg.AfterSeq + uint64(len(snapshots))}, nil
	case "worktree_get":
		snapshot, err = manager.Get(ctx, scope, msg.ID)
	case "worktree_enter":
		snapshot, err = manager.Enter(ctx, scope, msg.ID)
	case "worktree_exit":
		snapshot, err = manager.Exit(ctx, scope)
	case "worktree_preview":
		if msg.ConflictAfter == "" {
			snapshot, err = manager.Preview(ctx, scope, msg.ID)
		} else {
			snapshot, err = manager.ConflictPage(ctx, scope, msg.ID, msg.ConflictAfter)
		}
	case "worktree_resolve":
		snapshot, err = manager.ResolveUser(ctx, scope, msg.ID, strconv.Itoa(os.Getuid()), msg.WorktreePreviewID, msg.WorktreeGeneration, msg.ConflictChoices)
	case "worktree_discard_preview":
		snapshot, err = manager.PreviewDiscardUser(ctx, scope, msg.ID, strconv.Itoa(os.Getuid()))
	case "worktree_discard":
		snapshot, err = manager.RemoveDiscardUser(ctx, scope, msg.ID, strconv.Itoa(os.Getuid()), msg.DecisionID, msg.PreviewDigest, msg.WorktreeGeneration)
	case "worktree_export":
		snapshot, err = manager.Export(ctx, scope, msg.ID)
	case "worktree_keep":
		snapshot, err = manager.Keep(ctx, scope, msg.ID)
	case "worktree_remove":
		snapshot, err = manager.RemoveClean(ctx, scope, msg.ID)
	default:
		return ServerMsg{}, fmt.Errorf("unsupported worktree operation")
	}
	if err != nil {
		return ServerMsg{}, err
	}
	return ServerMsg{Type: "worktree", Worktree: &snapshot}, nil
}
