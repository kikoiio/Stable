package conversation

import (
	"encoding/json"
	"errors"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

func (s *Service) publishDelegationEvent(runID string, event agent.DelegationEvent) error {
	if s.deps.AgentTasks != nil && s.deps.AgentTasks.ownsRun(runID, event.SessionID) {
		if err := s.appendParentDelegationEvent(runID, event); err != nil {
			return err
		}
		snapshot, err := s.findAgentRun(event.SessionID, runID)
		if err != nil {
			return err
		}
		s.broadcastRun(ServerMsg{Type: "agent_task_update", AgentTask: &snapshot, RunID: runID, Cursor: snapshot.Cursor}, event.SessionID, "", snapshot.Cursor)
		return nil
	}
	s.mu.Lock()
	forkRun := s.activeForkRuns[runID]
	_, parentRun := s.activeRuns[runID]
	allowTestFallback := s.activeRuns == nil && s.activeForkRuns == nil
	s.mu.Unlock()
	if forkRun != nil {
		return s.appendForkProgress(forkRun, runID, event)
	}
	if (!parentRun && !allowTestFallback && event.SessionID == "") || s.deps.Runner == nil {
		return errors.New("parent run is not active")
	}
	publisher, ok := s.deps.Runner.(interface {
		PublishDelegation(string, agent.DelegationEvent) error
	})
	if !ok {
		if event.SessionID == "" {
			return errors.New("run event publisher does not support delegation events")
		}
		return s.appendParentDelegationEvent(runID, event)
	}
	if err := publisher.PublishDelegation(runID, event); err == nil {
		return nil
	}
	// run_end hooks execute after the streaming runner has closed its live
	// sink. Preserve their child progress in the completed parent's durable
	// event sequence instead of treating the closed sink as a lost event.
	if !parentRun && event.SessionID == "" {
		return errors.New("parent run is not active")
	}
	return s.appendParentDelegationEvent(runID, event)
}

func (s *Service) appendParentDelegationEvent(runID string, event agent.DelegationEvent) error {
	s.mu.Lock()
	sessionID := s.activeRuns[runID]
	s.mu.Unlock()
	if sessionID == "" {
		sessionID = event.SessionID
	}
	if sessionID == "" {
		return errors.New("parent run is not active")
	}
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.eventMu.Lock()
	transcript, replayErr := sessionlog.Replay(s.deps.ProjectRoot, sessionID)
	if replayErr != nil {
		s.eventMu.Unlock()
		return replayErr
	}
	var lastSeq uint64
	for _, item := range transcript.Events {
		if item.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if decodeSessionData(item.Data, &runEvent) == nil && runEvent.RunID == runID && runEvent.RunSeq > lastSeq {
			lastSeq = runEvent.RunSeq
		}
	}
	updatedAt := event.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	runEvent := sessionlog.RunEvent{
		ID: id, RunID: runID, SessionID: sessionID, RunSeq: lastSeq + 1,
		At: updatedAt, Kind: string(agent.EventDelegation), Payload: json.RawMessage(payload),
	}
	stored, appendErr := sessionlog.Append(s.deps.ProjectRoot, sessionID, sessionlog.EventRunEvent, runEvent)
	s.eventMu.Unlock()
	if appendErr == nil {
		s.broadcastRun(ServerMsg{Type: "run_event", RunID: runID, RunEvent: &runEvent, Cursor: stored.Seq}, sessionID, runID, stored.Seq)
	}
	return appendErr
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
		At: time.Now().UTC(), Kind: string(agent.EventDelegation), Payload: json.RawMessage(payload),
	}
	stored, err := sessionlog.Append(s.deps.ProjectRoot, state.sessionID, sessionlog.EventRunEvent, runEvent)
	s.eventMu.Unlock()
	if err == nil {
		s.broadcastRun(ServerMsg{Type: "run_event", RunID: runID, RunEvent: &runEvent, Cursor: stored.Seq}, state.sessionID, runID, stored.Seq)
	}
	return err
}
