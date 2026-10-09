package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

func (s *Service) executeWorkspaceLifecycleTool(ctx context.Context, request agent.ExecutionRequest, lease *workspace.WriterLease, call llm.ToolUse) (agent.ToolOutcome, error) {
	outcome := agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolDenied, IsError: true}
	if request.Work.SessionID == "" || request.RunID == "" || request.TeamTurn != nil || request.TeamCoordinator || request.TeamUser || request.Work.Kind != agent.WorkSession && request.Work.Kind != agent.WorkGoal {
		outcome.Content = "Error: workspace lifecycle tools require an ordinary session/goal lead run"
		return outcome, nil
	}
	active, err := s.activeRunRequest(request.Work.SessionID, request.RunID)
	if err != nil || active.RunID != request.RunID || active.Work != request.Work {
		outcome.Content = "Error: workspace lifecycle tools require the active lead run"
		return outcome, nil
	}
	started, found, err := sessionlog.FindRunStart(currentProjectRoot(s.deps.ProjectRoot), request.Work.SessionID, request.RunID)
	if err != nil || !found || started.TeamID != "" || started.AgentTaskID != "" || started.OriginRunID != "" || !workRefMatchesRun(request.Work, started) {
		outcome.Content = "Error: workspace lifecycle tools are unavailable to child or team runs"
		return outcome, nil
	}
	projectRoot, scope, err := s.workspaceScope(ctx, ClientMsg{SessionID: request.Work.SessionID, WorkKind: string(request.Work.Kind), GoalID: request.Work.GoalID, WorkItemID: request.Work.WorkItemID})
	if err != nil {
		outcome.Content = "Error: workspace scope is unavailable"
		return outcome, nil
	}
	manager, err := s.workspaceService(projectRoot)
	if err != nil {
		outcome.Content = "Error: workspace lifecycle is unavailable"
		return outcome, nil
	}

	transition, err := s.workspaceTransitionFromTool(ctx, request, lease, call, scope, manager)
	if err != nil {
		outcome.Content = "Error: " + boundedWorkspaceToolError(err)
		return outcome, nil
	}
	outcome.Status, outcome.IsError = agent.ToolSucceeded, false
	outcome.Content = "Workspace lifecycle operation durably scheduled for run completion; the current run authority and workspace binding remain unchanged. Transition: " + transition.ID
	return outcome, nil
}

func (s *Service) workspaceTransitionFromTool(ctx context.Context, request agent.ExecutionRequest, lease *workspace.WriterLease, call llm.ToolUse, scope workspace.Scope, manager *workspace.LifecycleService) (sessionlog.WorkspaceToolTransition, error) {
	var transition sessionlog.WorkspaceToolTransition
	transition.ID, _ = sessionlog.NewID()
	if transition.ID == "" {
		return transition, errors.New("could not allocate transition identity")
	}
	transition.SessionID = request.Work.SessionID
	transition.RunID, transition.CallID = request.RunID, call.ID
	transition.WorkKind, transition.GoalID, transition.WorkItemID = string(request.Work.Kind), request.Work.GoalID, request.Work.WorkItemID
	transition.Status = sessionlog.WorkspaceToolTransitionPending
	transition.CreatedAt, transition.UpdatedAt = time.Now().UTC(), time.Now().UTC()

	switch call.Name {
	case "enter_worktree":
		var args struct {
			WorkspaceID string `json:"workspace_id,omitempty"`
			Label       string `json:"label,omitempty"`
		}
		if err := decodeWorkspaceToolArgs(call.Arguments, &args); err != nil {
			return transition, err
		}
		transition.Action = "enter"
		if args.WorkspaceID != "" {
			if args.Label != "" || !workspace.ValidID(args.WorkspaceID) {
				return transition, workspace.ErrOwnership
			}
			transition.WorkspaceID = args.WorkspaceID
			if _, err := manager.Get(ctx, scope, args.WorkspaceID); err != nil {
				return transition, err
			}
		} else {
			if lease != nil {
				return transition, workspace.ErrUnavailable
			}
			if args.Label == "" {
				args.Label = "lead workspace"
			}
			if err := workspace.ValidateLabel(args.Label); err != nil {
				return transition, err
			}
			transition.Label = args.Label
			transition.WorkspaceID, _ = sessionlog.NewID()
			if transition.WorkspaceID == "" {
				return transition, errors.New("could not allocate workspace identity")
			}
		}
	case "exit_worktree":
		var args struct{}
		if err := decodeWorkspaceToolArgs(call.Arguments, &args); err != nil {
			return transition, err
		}
		transition.Action = "exit"
	case "worktree_export":
		var args struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := decodeWorkspaceToolArgs(call.Arguments, &args); err != nil || !workspace.ValidID(args.WorkspaceID) {
			return transition, workspace.ErrOwnership
		}
		snapshot, getErr := manager.Get(ctx, scope, args.WorkspaceID)
		if getErr != nil {
			return transition, getErr
		}
		if snapshot.WriterRunID == request.RunID && (lease == nil || lease.RunID != request.RunID || lease.WorkspaceID != snapshot.ID || lease.Generation != snapshot.Generation || !lease.Scope.SameOwner(scope)) {
			return transition, workspace.ErrOwnership
		}
		transition.Action, transition.WorkspaceID = "export", args.WorkspaceID
	default:
		return transition, errors.New("unsupported workspace lifecycle tool")
	}
	if err := s.appendWorkspaceTransition(transition); err != nil {
		return transition, err
	}
	if transition.Action == "enter" && transition.Label != "" {
		var authority permission.Authority
		if err := json.Unmarshal(request.PermissionBounds, &authority); err != nil || authority.RunID != request.RunID || authority.SessionID != request.Work.SessionID || authority.GoalID != request.Work.GoalID || authority.WorkItemID != request.Work.WorkItemID {
			failed, persistErr := s.finishWorkspaceTransition(transition, sessionlog.WorkspaceToolTransitionFailed, "lead authority did not match workspace scope", "")
			return failed, errors.Join(workspace.ErrOwnership, persistErr)
		}
		scope.Authority, scope.OriginRunID = authority, request.RunID
		if _, err := manager.CreateWithID(ctx, scope, transition.Label, transition.WorkspaceID); err != nil {
			_, persistErr := s.finishWorkspaceTransition(transition, sessionlog.WorkspaceToolTransitionFailed, boundedWorkspaceToolError(err), "")
			return transition, errors.Join(err, persistErr)
		}
	}
	return transition, nil
}

func decodeWorkspaceToolArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return workspace.ErrOwnership
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return workspace.ErrOwnership
	}
	return nil
}

func (s *Service) appendWorkspaceTransition(transition sessionlog.WorkspaceToolTransition) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	_, err := sessionlog.Append(s.deps.ProjectRoot, transition.SessionID, sessionlog.EventWorkspaceToolTransition, transition)
	return err
}

func (s *Service) finishWorkspaceTransition(transition sessionlog.WorkspaceToolTransition, status, reason, candidateID string) (sessionlog.WorkspaceToolTransition, error) {
	transition.Status = status
	transition.Error = reason
	transition.CandidateID = candidateID
	transition.UpdatedAt = time.Now().UTC()
	err := s.appendWorkspaceTransition(transition)
	return transition, err
}

func boundedWorkspaceToolError(err error) string {
	if err == nil {
		return "operation failed"
	}
	text := strings.ToValidUTF8(err.Error(), "?")
	if len(text) > 1000 {
		text = text[:1000]
	}
	return text
}

func workspaceTransitionTerminal(events []sessionlog.Event, runID string) bool {
	for _, event := range events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if decodeSessionData(event.Data, &runEvent) == nil && runEvent.RunID == runID && runEvent.Kind == "terminal" {
			return true
		}
	}
	return false
}

func (s *Service) applyWorkspaceTransition(ctx context.Context, transition sessionlog.WorkspaceToolTransition) (string, error) {
	projectRoot, scope, err := s.workspaceScope(ctx, ClientMsg{SessionID: transition.SessionID, WorkKind: transition.WorkKind, GoalID: transition.GoalID, WorkItemID: transition.WorkItemID})
	if err != nil {
		return "", err
	}
	manager, err := s.workspaceService(projectRoot)
	if err != nil {
		return "", err
	}
	switch transition.Action {
	case "enter":
		boundID, err := manager.Binding(scope)
		if err != nil {
			return "", err
		}
		if boundID != "" && boundID != transition.WorkspaceID {
			if _, err := manager.Exit(ctx, scope); err != nil {
				return "", err
			}
		}
		_, err = manager.Enter(ctx, scope, transition.WorkspaceID)
		return "", err
	case "exit":
		_, err := manager.Exit(ctx, scope)
		return "", err
	case "export":
		snapshot, err := manager.Export(ctx, scope, transition.WorkspaceID)
		if err != nil {
			return "", err
		}
		if snapshot.CandidateID == "" {
			return "", errors.New("workspace export produced no candidate")
		}
		return snapshot.CandidateID, nil
	default:
		return "", fmt.Errorf("unsupported workspace transition action")
	}
}

func latestWorkspaceTransitions(events []sessionlog.Event) map[string]sessionlog.WorkspaceToolTransition {
	latest := map[string]sessionlog.WorkspaceToolTransition{}
	for _, event := range events {
		if event.Type != sessionlog.EventWorkspaceToolTransition {
			continue
		}
		var transition sessionlog.WorkspaceToolTransition
		if decodeSessionData(event.Data, &transition) == nil && transition.ID != "" {
			latest[transition.ID] = transition
		}
	}
	return latest
}

func workspaceLifecycleToolName(action string) string {
	switch action {
	case "enter":
		return "enter_worktree"
	case "exit":
		return "exit_worktree"
	case "export":
		return "worktree_export"
	default:
		return ""
	}
}

func workspaceTransitionToolResult(events []sessionlog.Event, transition sessionlog.WorkspaceToolTransition) (sessionlog.ToolResult, bool, error) {
	pendingIndex := -1
	for index, event := range events {
		if event.Type != sessionlog.EventWorkspaceToolTransition {
			continue
		}
		var old sessionlog.WorkspaceToolTransition
		if decodeSessionData(event.Data, &old) == nil && old.ID == transition.ID && old.Status == sessionlog.WorkspaceToolTransitionPending {
			pendingIndex = index
		}
	}
	if pendingIndex < 0 {
		return sessionlog.ToolResult{}, false, fmt.Errorf("workspace transition %s has no pending event", transition.ID)
	}
	callIndex := -1
	var matchedCall sessionlog.ToolCall
	for index := 0; index < pendingIndex; index++ {
		event := events[index]
		if event.Type != sessionlog.EventToolCall {
			continue
		}
		var call sessionlog.ToolCall
		if decodeSessionData(event.Data, &call) == nil && call.CallID == transition.CallID {
			callIndex, matchedCall = index, call
		}
	}
	if callIndex < 0 || matchedCall.RunID != transition.RunID || matchedCall.Name != workspaceLifecycleToolName(transition.Action) {
		return sessionlog.ToolResult{}, false, fmt.Errorf("workspace transition %s has mismatched lead tool call", transition.ID)
	}
	var matchedResult sessionlog.ToolResult
	resultCount := 0
	for index := callIndex + 1; index < len(events); index++ {
		event := events[index]
		if event.Type == sessionlog.EventToolCall {
			var call sessionlog.ToolCall
			if decodeSessionData(event.Data, &call) == nil && call.CallID == transition.CallID {
				break
			}
		}
		if event.Type != sessionlog.EventToolResult {
			continue
		}
		var result sessionlog.ToolResult
		if decodeSessionData(event.Data, &result) == nil && result.CallID == transition.CallID {
			resultCount++
			matchedResult = result
		}
	}
	if resultCount > 1 {
		return sessionlog.ToolResult{}, false, fmt.Errorf("workspace transition %s has duplicate paired tool results", transition.ID)
	}
	return matchedResult, resultCount == 1, nil
}

func (s *Service) reconcileWorkspaceToolTransitions(ctx context.Context, sessionID string, interruptedOnRestart bool) error {
	s.workspaceTransitionMu.Lock()
	defer s.workspaceTransitionMu.Unlock()
	return s.reconcileWorkspaceToolTransitionsLocked(ctx, sessionID, interruptedOnRestart)
}

func (s *Service) reconcileWorkspaceToolTransitionsLocked(ctx context.Context, sessionID string, interruptedOnRestart bool) error {
	transcript, err := sessionlog.Replay(s.deps.ProjectRoot, sessionID)
	if err != nil {
		return err
	}
	terminalRuns := map[string]bool{}
	for _, event := range transcript.Events {
		switch event.Type {
		case sessionlog.EventRunEvent:
			var runEvent sessionlog.RunEvent
			if decodeSessionData(event.Data, &runEvent) == nil && runEvent.Kind == "terminal" {
				terminalRuns[runEvent.RunID] = true
			}
		}
	}
	transitions := latestWorkspaceTransitions(transcript.Events)
	for _, transition := range transitions {
		if transition.Status != sessionlog.WorkspaceToolTransitionPending {
			continue
		}
		if !terminalRuns[transition.RunID] {
			if interruptedOnRestart {
				if _, err := s.finishWorkspaceTransition(transition, sessionlog.WorkspaceToolTransitionInterrupted, "service restarted before lead run terminal; binding was not changed", ""); err != nil {
					return err
				}
			}
			continue
		}
		result, hasResult, evidenceErr := workspaceTransitionToolResult(transcript.Events, transition)
		if evidenceErr != nil {
			return evidenceErr
		}
		if !hasResult || result.Error != "" {
			if _, err := s.finishWorkspaceTransition(transition, sessionlog.WorkspaceToolTransitionInterrupted, "lead run reached terminal without a successful lifecycle tool result; transition was not applied", ""); err != nil {
				return err
			}
			continue
		}
		candidateID, applyErr := s.applyWorkspaceTransition(ctx, transition)
		if applyErr != nil {
			// Busy/closed services keep a retryable pending intent. Stable
			// ownership, conflict, or snapshot failures become an explicit
			// terminal fact instead of retrying the same unusable request forever.
			if errors.Is(applyErr, workspace.ErrUnavailable) || errors.Is(applyErr, workspace.ErrClosed) || errors.Is(applyErr, context.Canceled) || errors.Is(applyErr, context.DeadlineExceeded) {
				return fmt.Errorf("apply workspace transition %s: %w", transition.ID, applyErr)
			}
			if _, persistErr := s.finishWorkspaceTransition(transition, sessionlog.WorkspaceToolTransitionFailed, boundedWorkspaceToolError(applyErr), ""); persistErr != nil {
				return persistErr
			}
			continue
		}
		transition.CandidateID = candidateID
		if _, err := s.finishWorkspaceTransition(transition, sessionlog.WorkspaceToolTransitionApplied, "", candidateID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recoverWorkspaceToolTransitions() error {
	sessions, err := sessionlog.List(s.deps.ProjectRoot)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if err := s.reconcileWorkspaceToolTransitions(context.Background(), session.ID, true); err != nil {
			// Operational failure leaves the pending record intact for another
			// reconciliation attempt; malformed history still fails Replay above.
			continue
		}
	}
	return nil
}
