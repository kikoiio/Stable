package conversation

import (
	"encoding/json"
	"errors"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

func (s *Service) publishDelegationEvent(runID string, event agent.DelegationEvent) error {
	s.mu.Lock()
	forkRun := s.activeForkRuns[runID]
	_, parentRun := s.activeRuns[runID]
	allowTestFallback := s.activeRuns == nil && s.activeForkRuns == nil
	s.mu.Unlock()
	if forkRun != nil {
		return s.appendForkProgress(forkRun, runID, event)
	}
	if (!parentRun && !allowTestFallback) || s.deps.Runner == nil {
		return errors.New("parent run is not active")
	}
	publisher, ok := s.deps.Runner.(interface {
		PublishDelegation(string, agent.DelegationEvent) error
	})
	if !ok {
		return errors.New("run event publisher does not support delegation events")
	}
	return publisher.PublishDelegation(runID, event)
}

func (s *Service) appendForkProgress(state *forkRunState, runID string, event agent.DelegationEvent) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.eventMu.Lock()
	state.runSeq++
	runEvent := sessionlog.RunEvent{
		ID: id, RunID: runID, SessionID: state.sessionID, RunSeq: state.runSeq,
		At: time.Now().UTC(), Kind: string(agent.EventDelegation), Payload: payload,
	}
	stored, err := sessionlog.Append(s.deps.ProjectRoot, state.sessionID, sessionlog.EventRunEvent, runEvent)
	s.eventMu.Unlock()
	if err == nil {
		s.broadcastRun(ServerMsg{Type: "run_event", RunID: runID, RunEvent: &runEvent, Cursor: stored.Seq}, state.sessionID, runID, stored.Seq)
	}
	return err
}
