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

	"stable/internal/agent"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

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
	defer s.workspaceMu.Unlock()
	if manager := s.workspaces[root]; manager != nil {
		return manager, nil
	}
	digest := sha256.Sum256([]byte(filepath.Clean(root)))
	projectID := "p" + hex.EncodeToString(digest[:16])
	layout, err := workspace.NewLayout(s.deps.WorkspaceStateRoot, root, projectID)
	if err != nil {
		return nil, err
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{IdleGuard: s, Stopper: s.deps.AgentTasks, Exporter: workspaceCandidateExporter{service: s}})
	if err != nil {
		return nil, err
	}
	s.workspaces[root] = manager
	return manager, nil
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
