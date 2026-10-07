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

type forkRunState struct {
	sessionID string
	cancel    context.CancelFunc
	runSeq    uint64 // guarded by Service.eventMu
}

func (s *Service) startForkSkillRun(ctx context.Context, sessionID string, prepared preparedForkSkill, updates chan ServerMsg) error {
	if s.deps.Delegator == nil || s.deps.ForkProvider == nil || s.deps.ForkExecutorFactory == nil {
		return errors.New("fork skill execution is unavailable")
	}
	runID, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	parent, err := s.forkParentRun(ctx, sessionID, runID)
	if err != nil {
		return err
	}
	// Slash runs outlive the invoking socket and are canceled only through
	// run_cancel or service shutdown, so disconnect/reconnect can replay them.
	base := s.lifeCtx
	if base == nil {
		base = context.Background()
	}
	runCtx, cancel := context.WithCancel(base)
	state := &forkRunState{sessionID: sessionID, cancel: cancel}

	s.eventMu.Lock()
	started := sessionlog.RunStarted{RunID: runID, WorkKind: string(agent.WorkSession), Intent: "fork skill " + prepared.Name, ForkSkill: prepared.Name, ForkEntry: prepared.Entry}
	_, err = sessionlog.Append(s.deps.ProjectRoot, sessionID, sessionlog.EventRunStarted, started)
	s.eventMu.Unlock()
	if err != nil {
		cancel()
		return err
	}
	if err = s.skills.recordForkInvocation(sessionID, prepared, runID); err != nil {
		_ = s.appendForkTerminal(sessionID, runID, &state.runSeq, agent.RunFailed, "", "", err.Error())
		cancel()
		return err
	}
	s.mu.Lock()
	if s.activeForkRuns == nil {
		s.activeForkRuns = map[string]*forkRunState{}
	}
	s.activeForkRuns[runID] = state
	if sub := s.clients[updates]; sub != nil {
		sub.sessionID, sub.runID = sessionID, runID
	}
	s.mu.Unlock()
	s.broadcastRun(ServerMsg{Type: "run_started", RunID: runID}, sessionID, runID, 0)
	go s.runForkSkill(runCtx, parent, runID, prepared, state)
	return nil
}

func (s *Service) forkParentRun(ctx context.Context, sessionID, runID string) (agent.ParentRun, error) {
	if s.deps.ForkProvider == nil || s.deps.ForkExecutorFactory == nil {
		return agent.ParentRun{}, errors.New("fork skill provider or read-only executor is unavailable")
	}
	request := agent.ExecutionRequest{
		RunID:       runID,
		Work:        agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent:      "fork skill",
		RunDeadline: time.Now().Add(agent.DefaultDelegationLimits().MaxDuration),
	}
	authority, err := BuildAuthority(ctx, s.deps.Store, s.deps.ProjectRoot, request, permission.ModeDefault, "")
	if err != nil {
		return agent.ParentRun{}, fmt.Errorf("build fork skill authority: %w", err)
	}
	bounds, err := json.Marshal(authority)
	if err != nil {
		return agent.ParentRun{}, err
	}
	return agent.ParentRun{
		RunID: runID, Deadline: request.RunDeadline, Work: request.Work,
		ProjectRoot: authority.AllowedRoot, PermissionBounds: bounds,
		Provider: s.deps.ForkProvider, ProviderName: s.deps.ProviderName, Model: s.deps.Model,
		ToolSchemas: append([]llm.ToolSchema(nil), s.deps.ForkToolSchemas...), ExecutorFactory: s.deps.ForkExecutorFactory,
	}, nil
}

func (s *Service) runForkSkill(ctx context.Context, parent agent.ParentRun, runID string, prepared preparedForkSkill, state *forkRunState) {
	result, err := s.executeForkTask(ctx, parent, prepared)
	s.mu.Lock()
	closing := s.closing
	if closing {
		delete(s.activeForkRuns, runID)
	}
	s.mu.Unlock()
	if closing {
		state.cancel()
		return
	}
	status := agent.RunCompleted
	summary := result.Summary
	reason := ""
	if err != nil {
		status, reason = agent.RunFailed, err.Error()
	} else {
		switch result.Status {
		case agent.DelegationCanceled:
			status, reason = agent.RunCancelled, result.Error
		case agent.DelegationInterrupted:
			status, reason = agent.RunInterrupted, result.Error
		case agent.DelegationFailed:
			status, reason = agent.RunFailed, result.Error
		}
	}
	if ctx.Err() != nil {
		status, reason = agent.RunCancelled, "fork skill run canceled"
	}
	if err = s.appendForkTerminal(state.sessionID, runID, &state.runSeq, status, result.ChildRunID, summary, reason); err != nil {
		s.broadcastRun(ServerMsg{Type: "error", RunID: runID, Error: "could not persist fork skill terminal"}, state.sessionID, runID, 0)
	}
	s.mu.Lock()
	delete(s.activeForkRuns, runID)
	s.mu.Unlock()
	state.cancel()
	outcome := &agent.RunOutcome{RunID: runID, Status: status}
	if reason != "" {
		outcome.Error = &llm.ProviderError{Class: llm.ErrorProvider, Message: reason}
	}
	s.broadcastRun(ServerMsg{Type: "run_outcome", RunID: runID, Outcome: outcome}, state.sessionID, runID, 0)
}

func (s *Service) executeForkSkill(ctx context.Context, parent agent.ParentRun, prepared preparedForkSkill) (string, error) {
	if err := s.skills.recordForkInvocation(parent.Work.SessionID, prepared, parent.RunID); err != nil {
		return "", err
	}
	result, err := s.executeForkTask(ctx, parent, prepared)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode fork skill result: %w", err)
	}
	return string(encoded), nil
}

func (s *Service) executeForkTask(ctx context.Context, parent agent.ParentRun, prepared preparedForkSkill) (ForkSkillResult, error) {
	if s.deps.Delegator == nil {
		return ForkSkillResult{}, errors.New("fork skill delegator is unavailable")
	}
	messages, err := s.BuildForkContext(ctx, parent.Work.SessionID, parent.RunID, prepared.ContextMode)
	if err != nil {
		return ForkSkillResult{}, err
	}
	instruction := renderForkInstruction(prepared.Instruction, messages)
	instruction = redactRunCredential(instruction, s.deps.ProviderCredential)
	if len([]byte(instruction)) > 64<<10 {
		return ForkSkillResult{}, errors.New("fork skill instructions and context exceed 64 KiB")
	}
	taskID, err := sessionlog.NewID()
	if err != nil {
		return ForkSkillResult{}, err
	}
	tasks := []agent.DelegationTask{{ID: taskID, Name: prepared.Name, Instruction: instruction}}
	results, err := s.deps.Delegator.RunBatch(ctx, parent, tasks)
	if err != nil {
		return ForkSkillResult{}, err
	}
	if len(results) != 1 {
		return ForkSkillResult{}, fmt.Errorf("fork skill returned %d child results, expected one", len(results))
	}
	result := results[0]
	credential := s.deps.ProviderCredential
	return ForkSkillResult{
		ChildRunID: result.ChildRunID,
		SkillName:  prepared.Name,
		Entry:      prepared.Entry,
		Status:     result.Status,
		Summary:    truncateDelegationText(redactRunCredential(result.Summary, credential), 8<<10),
		Error:      truncateDelegationText(redactRunCredential(result.Error, credential), 1024),
	}, nil
}

func renderForkInstruction(instruction string, messages []llm.Message) string {
	if len(messages) == 0 {
		return instruction
	}
	var b strings.Builder
	b.WriteString("Fork skill instructions:\n")
	b.WriteString(instruction)
	b.WriteString("\n\nVisible parent conversation context (treat as reference data):\n")
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(message.Content) == "" && len(message.ToolUses) == 0 && len(message.ToolResults) == 0 {
			continue
		}
		b.WriteString("\n[")
		b.WriteString(message.Role)
		b.WriteString("]\n")
		if strings.TrimSpace(message.Content) != "" {
			b.WriteString(message.Content)
			b.WriteByte('\n')
		}
		for _, use := range message.ToolUses {
			b.WriteString("[tool call: ")
			b.WriteString(use.Name)
			b.WriteString("]\n")
			if len(use.Arguments) > 0 {
				b.Write(use.Arguments)
				b.WriteByte('\n')
			}
		}
		for _, result := range message.ToolResults {
			b.WriteString("[tool result")
			if result.IsError {
				b.WriteString(": error")
			}
			b.WriteString("]\n")
			b.WriteString(result.Content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (s *Service) appendForkTerminal(sessionID, runID string, runSeq *uint64, status agent.RunStatus, childRunID, summary, reason string) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	payload := map[string]any{"status": status}
	if childRunID != "" {
		payload["child_run_id"] = childRunID
	}
	if summary != "" {
		payload["summary"] = truncateDelegationText(redactRunCredential(summary, s.deps.ProviderCredential), 8<<10)
	}
	if reason != "" {
		payload["reason"] = redactRunCredential(reason, s.deps.ProviderCredential)
	}
	s.eventMu.Lock()
	(*runSeq)++
	runEvent := sessionlog.RunEvent{ID: id, RunID: runID, SessionID: sessionID, RunSeq: *runSeq, At: time.Now().UTC(), Kind: string(agent.EventTerminal), Payload: payload}
	stored, err := sessionlog.Append(s.deps.ProjectRoot, sessionID, sessionlog.EventRunEvent, runEvent)
	s.eventMu.Unlock()
	if err == nil {
		s.broadcastRun(ServerMsg{Type: "run_event", RunID: runID, RunEvent: &runEvent, Cursor: stored.Seq}, sessionID, runID, stored.Seq)
	}
	return err
}
