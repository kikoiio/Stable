package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"stable/internal/planfile"
	"stable/internal/sessionlog"
)

func (s *Service) sessionProjectRoot(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if root := s.ephemeralRoots[sessionID]; root != "" {
		return root
	}
	if root := s.remoteRoots[sessionID]; root != "" {
		return root
	}
	return s.deps.ProjectRoot
}

func (s *Service) sessionIsEphemeral(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ephemeralRoots[sessionID] != ""
}

func (s *Service) discardSession(c ClientMsg) ([]ServerMsg, error) {
	root, err := sessionRoot(c.ProjectRoot)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if active := s.activeRuns; active != nil {
		for _, sessionID := range active {
			if sessionID == c.SessionID {
				s.mu.Unlock()
				return nil, errors.New("cannot discard a session with an active run")
			}
		}
	}
	boundRoot := s.ephemeralRoots[c.SessionID]
	s.mu.Unlock()
	if boundRoot == "" || boundRoot != root {
		return nil, errors.New("session is not an ephemeral session bound to this project")
	}
	replay, err := sessionlog.Replay(root, c.SessionID)
	if err != nil {
		return nil, err
	}
	if !replay.Session.Ephemeral {
		return nil, errors.New("refusing to discard a persistent session")
	}
	if err = removeEphemeralPlan(root, c.SessionID); err != nil {
		return nil, err
	}
	var questionIDs, planApprovalIDs, permissionApprovalIDs []string
	for _, event := range replay.Events {
		if event.Type == sessionlog.EventQuestion {
			var question sessionlog.PendingQuestion
			if decodeSessionData(event.Data, &question) == nil {
				questionIDs = append(questionIDs, question.QuestionID)
			}
		}
	}
	s.planMu.Lock()
	for id, approval := range s.planApprovals {
		if approval.SessionID == c.SessionID {
			planApprovalIDs = append(planApprovalIDs, id)
		}
	}
	s.planMu.Unlock()
	if s.deps.Store != nil {
		if approvals, listErr := s.pendingApprovals(context.Background(), c.SessionID); listErr == nil {
			for _, approval := range approvals {
				permissionApprovalIDs = append(permissionApprovalIDs, approval.ID)
			}
		}
	}
	if s.deps.Store != nil {
		if err = s.deps.Store.DeleteEphemeralSessionRecords(context.Background(), c.SessionID); err != nil {
			return nil, err
		}
	}
	if err = sessionlog.DeleteEphemeral(root, c.SessionID); err != nil {
		return nil, err
	}
	s.planMu.Lock()
	delete(s.planStates, c.SessionID)
	for id, approval := range s.planApprovals {
		if approval.SessionID == c.SessionID {
			delete(s.planApprovals, id)
		}
	}
	s.planMu.Unlock()
	s.askMu.Lock()
	delete(s.askWaiters, c.SessionID)
	for _, id := range questionIDs {
		delete(s.notifiedQuestions, id)
	}
	for _, id := range planApprovalIDs {
		delete(s.notifiedPlanApprovals, id)
	}
	s.askMu.Unlock()
	s.mu.Lock()
	delete(s.ephemeralRoots, c.SessionID)
	for _, id := range permissionApprovalIDs {
		delete(s.notifiedApprovals, id)
	}
	s.mu.Unlock()
	s.mcpMu.Lock()
	delete(s.mcpInstructions, c.SessionID)
	s.mcpMu.Unlock()
	if s.hooks != nil {
		s.hooks.DiscardSession(c.SessionID)
	}
	if s.skills != nil {
		s.skills.DiscardSession(c.SessionID)
	}
	return []ServerMsg{{Type: "discarded", SessionID: c.SessionID}}, nil
}

// removeEphemeralPlan is kept local to the ephemeral lifecycle so print does
// not leave a plan artifact after its transcript has been discarded.
func removeEphemeralPlan(root, sessionID string) error {
	path, err := planfile.PlanPath(root, sessionID)
	if err != nil {
		return err
	}
	for _, dir := range []string{filepath.Join(root, ".stable"), filepath.Dir(path)} {
		info, statErr := os.Lstat(dir)
		if os.IsNotExist(statErr) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("plan directory is unsafe")
		}
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
