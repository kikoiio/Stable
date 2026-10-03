package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
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
	if err := agent.ValidateRequest(request); err != nil {
		return err
	}
	authority, err := BuildAuthority(ctx, s.deps.Store, s.deps.ProjectRoot, request, permission.ModeDefault)
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
	work := sessionlog.RunStarted{RunID: request.RunID, WorkKind: string(request.Work.Kind), GoalID: request.Work.GoalID, WorkItemID: request.Work.WorkItemID, Intent: request.Intent}
	s.eventMu.Lock()
	if request.Work.Kind == agent.WorkSession {
		history := sessionConversationMessages(s.deps.ProjectRoot, msg.SessionID)
		request.Messages = append([]llm.Message{{Role: "system", Content: "你是 Stable 的通用 agent。直接响应用户请求；工具调用会由后续受控执行流程处理，当前阶段不要声称已执行任何工具或修改。"}}, append(history, request.Messages...)...)
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

	handle, err := s.deps.Runner.Start(ctx, request)
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
	s.activeRuns[request.RunID] = request.Work.SessionID
	s.mu.Unlock()
	s.broadcastRun(ServerMsg{Type: "run_started", RunID: request.RunID}, msg.SessionID, request.RunID, 0)
	go s.consumeRun(request, handle)
	return nil
}

func redactRunCredential(text, credential string) string {
	if credential == "" {
		return text
	}
	return strings.ReplaceAll(text, credential, "[credential redacted]")
}

func sessionConversationMessages(root, sessionID string) []llm.Message {
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		return nil
	}
	var messages []llm.Message
	runAssistant := map[string]int{}
	for _, event := range transcript.Events {
		switch event.Type {
		case sessionlog.EventMessage:
			var msg sessionlog.Message
			if decodeSessionData(event.Data, &msg) == nil && (msg.Role == "user" || msg.Role == "assistant") && msg.Text != "" {
				messages = append(messages, llm.Message{Role: msg.Role, Content: msg.Text})
			}
		case sessionlog.EventRunEvent:
			var runEvent sessionlog.RunEvent
			if decodeSessionData(event.Data, &runEvent) != nil || runEvent.Kind != "text_delta" {
				continue
			}
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
	if len(messages) > 20 {
		messages = append([]llm.Message(nil), messages[len(messages)-20:]...)
	}
	return messages
}

func (s *Service) consumeRun(request agent.ExecutionRequest, handle *agent.RunHandle) {
	for event := range handle.Events {
		persisted := sessionlog.RunEvent{
			ID: event.ID, RunID: event.RunID, SessionID: event.SessionID,
			RunSeq: event.RunSeq, At: event.At, Kind: string(event.Kind), Payload: event.Payload,
		}
		s.eventMu.Lock()
		stored, err := sessionlog.Append(s.deps.ProjectRoot, request.Work.SessionID, sessionlog.EventRunEvent, persisted)
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
	s.broadcastRun(ServerMsg{Type: "run_outcome", RunID: request.RunID, Outcome: &outcome}, request.Work.SessionID, request.RunID, 0)
	s.mu.Lock()
	delete(s.activeRuns, request.RunID)
	s.mu.Unlock()
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
	s.mu.Lock()
	sessionID, active := s.activeRuns[msg.RunID]
	s.mu.Unlock()
	if !active {
		return nil
	}
	if sessionID != msg.SessionID {
		return errors.New("run does not belong to requested session")
	}
	if s.deps.Runner == nil {
		return errors.New("streaming agent is not configured")
	}
	return s.deps.Runner.Cancel(msg.RunID)
}

func (s *Service) broadcastRun(msg ServerMsg, sessionID, runID string, cursor uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range s.clients {
		if sub.sessionID != sessionID || (sub.runID != "" && sub.runID != runID) {
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
