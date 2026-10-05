package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"stable/internal/agent"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

// BuildAuthority derives trusted run bounds from the on-disk session and
// persisted goal. Client-provided AllowedScope and PermissionBounds are never
// used to widen the returned authority. mode selects the permission mode of
// the run (plan/accept-edits semantics are decided by the conversation plan
// state); planFilePath is the session's plan file, non-empty only for runs
// that start in plan mode, and is copied into Authority.PlanFilePath so the
// policy can exempt exactly that file from the write restrictions.
func BuildAuthority(ctx context.Context, state *store.Store, projectRoot string, request agent.ExecutionRequest, mode permission.Mode, planFilePath string) (permission.Authority, error) {
	var out permission.Authority
	root, err := sessionlog.ProjectRoot(projectRoot)
	if err != nil {
		return out, err
	}
	if err = sessionlog.ValidateID(request.Work.SessionID); err != nil {
		return out, err
	}
	if request.RunID == "" || strings.ContainsAny(request.RunID, "/\\") || request.RunID == "." || request.RunID == ".." {
		return out, errors.New("invalid run ID")
	}
	sessionPath, err := sessionlog.SessionPath(root, request.Work.SessionID)
	if err != nil {
		return out, err
	}
	if _, err = os.Stat(sessionPath); err != nil {
		return out, errors.New("run session does not exist")
	}
	if planFilePath != "" {
		if request.Work.Kind != agent.WorkSession {
			return out, errors.New("plan authority requires a session run")
		}
		planFilePath = filepath.Clean(planFilePath)
	}
	authorizedRoot := root
	var goalID, workID string
	var capabilities []string
	switch request.Work.Kind {
	case agent.WorkSession:
		if request.Work.GoalID != "" || request.Work.WorkItemID != "" {
			return out, errors.New("session authority cannot include a goal")
		}
	case agent.WorkGoal:
		if state == nil {
			return out, errors.New("authority store is unavailable")
		}
		if !validComponent(request.Work.GoalID) || !validComponent(request.Work.WorkItemID) {
			return out, errors.New("invalid goal work attribution")
		}
		snapshot, err := state.GetGoalSnapshot(ctx, request.Work.GoalID)
		if err != nil {
			return out, err
		}
		if snapshot.Goal.SourceSessionID != request.Work.SessionID {
			return out, errors.New("goal does not belong to this session")
		}
		authorizedRoot, err = filepath.EvalSymlinks(snapshot.Goal.AllowedRoot)
		if err != nil {
			return out, err
		}
		capabilities = append([]string(nil), snapshot.Goal.AllowedCapabilities...)
		goalID, workID = request.Work.GoalID, request.Work.WorkItemID
	default:
		return out, errors.New("unknown work kind")
	}
	formalRoot, err := filepath.Abs(authorizedRoot)
	if err != nil {
		return out, err
	}
	allowed := formalRoot
	if len(request.AllowedScope) > 1 {
		return out, errors.New("multiple client scope roots are not supported")
	}
	if len(request.AllowedScope) == 1 {
		requested, resolveErr := filepath.EvalSymlinks(request.AllowedScope[0])
		if resolveErr != nil {
			return out, errors.New("requested scope cannot be resolved")
		}
		if !isWithin(formalRoot, requested) {
			return out, errors.New("client scope cannot widen the authorized root")
		}
		allowed = requested
	}
	allowed, err = filepath.Abs(allowed)
	if err != nil {
		return out, err
	}
	if st, e := os.Lstat(allowed); e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return out, errors.New("authorized project root is invalid")
	}
	// Candidate directories are siblings of the formal root so the complete
	// project tree can be exchanged atomically on the same filesystem.
	parent := filepath.Dir(formalRoot)
	var namespace string
	if goalID != "" {
		namespace = "goal-" + goalID + "-" + workID
	} else {
		namespace = "session-" + request.Work.SessionID + "-" + request.RunID
	}
	candidateRoot := filepath.Join(parent, ".stable-candidates", namespace)
	if _, err = os.Lstat(candidateRoot); err == nil {
		return out, errors.New("candidate run directory already exists")
	} else if !os.IsNotExist(err) {
		return out, err
	}
	return permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, GoalID: goalID, WorkItemID: workID, AllowedRoot: allowed, CandidateRoot: candidateRoot, FormalRoot: formalRoot, Mode: mode, PlanFilePath: planFilePath, Capabilities: capabilities}, nil
}

func validComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsRune(value, 0)
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
