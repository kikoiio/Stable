package conversation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/permission"
	"stable/internal/planfile"
	"stable/internal/sessionlog"
)

// PlanState is the session-scoped plan mode runtime state. It lives in
// service memory only — a restart drops every session back to the default
// mode — while the session log keeps the transitions auditable.
type PlanState struct {
	// Mode is "plan" or "default".
	Mode string `json:"mode"`
	// PlanPath is the session's plan file inside the project. It is filled
	// when the session enters plan mode and kept while the session lives.
	PlanPath string `json:"plan_path,omitempty"`
	// ExecutionMode selects the permission mode of runs started after a plan
	// was approved: "acceptEdits" (in-scope writes auto-accepted) or "default"
	// (per-write confirmation). Empty until the first approval. It only takes
	// effect while Mode is default; plan mode always runs with ModePlan.
	ExecutionMode string `json:"execution_mode,omitempty"`
	// Runs counts the runs started while the session was in plan mode. It
	// drives the recurring plan workflow reminder cadence.
	Runs int64 `json:"runs"`
}

// Post-approval execution modes recorded in PlanState.ExecutionMode.
const (
	PlanExecutionAcceptEdits = "acceptEdits"
	PlanExecutionDefault     = "default"
)

// plan_resolve choices. auto/manual approve the plan, feedback rejects it
// with correction text, cancel dismisses the approval dialog.
const (
	PlanResolveAuto     = "auto"
	PlanResolveManual   = "manual"
	PlanResolveFeedback = "feedback"
	PlanResolveCancel   = "cancel"
)

// PlanApproval is one plan approval request tracked by the conversation
// service. A request is submitted once and resolved exactly once with one of
// the sessionlog.PlanApproval* terminal statuses; the session log carries the
// same lifecycle as sessionlog.PlanApprovalRecord events.
type PlanApproval struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	RunID      string    `json:"run_id"`
	PlanPath   string    `json:"plan_path"`
	Status     string    `json:"status"`
	Feedback   string    `json:"feedback,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
}

// PlanApprovalRef is the client-facing projection of a pending plan approval.
// It strips the internal lifecycle bookkeeping (status, feedback, resolution
// time): the approval dialog only needs to know what to display and that
// plan_resolve answers the session's single pending request.
type PlanApprovalRef struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	RunID     string    `json:"run_id"`
	PlanPath  string    `json:"plan_path"`
	CreatedAt time.Time `json:"created_at"`
}

// PlanStateOf returns a snapshot of the session plan state. Unknown sessions
// default to the default mode.
func (s *Service) PlanStateOf(sessionID string) PlanState {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	return s.planStateLocked(sessionID)
}

// planStateLocked reads the plan state; planMu must be held.
func (s *Service) planStateLocked(sessionID string) PlanState {
	if s.planStates != nil {
		if state, ok := s.planStates[sessionID]; ok && state != nil {
			return *state
		}
	}
	return PlanState{Mode: sessionlog.PlanModeDefault}
}

// applyPlanState rewrites the plan state of one session under the lock.
func (s *Service) applyPlanState(sessionID string, mutate func(PlanState) PlanState) PlanState {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	state := mutate(s.planStateLocked(sessionID))
	if s.planStates == nil {
		s.planStates = map[string]*PlanState{}
	}
	stored := state
	s.planStates[sessionID] = &stored
	return state
}

// initPlanState seeds the default plan state of a fresh session.
func (s *Service) initPlanState(sessionID string) {
	s.applyPlanState(sessionID, func(PlanState) PlanState {
		return PlanState{Mode: sessionlog.PlanModeDefault}
	})
}

// SetPlanMode switches the plan mode of one session, creates or reuses its
// plan file when entering plan mode, appends the plan_mode event, and returns
// the new state. reason distinguishes a user toggle from a transition caused
// by the plan approval and is recorded verbatim for the transcript.
func (s *Service) SetPlanMode(sessionID, mode, reason string) (PlanState, error) {
	if err := validatePlanModeTransition(mode, reason); err != nil {
		return PlanState{}, err
	}
	root, err := s.boundProjectRoot()
	if err != nil {
		return PlanState{}, err
	}
	// Fail closed on unknown sessions before the plan file is created, so a
	// typo'd session id cannot leave a stray file behind.
	if _, err = sessionlog.Replay(root, sessionID); err != nil {
		return PlanState{}, err
	}
	var planPath string
	if mode == sessionlog.PlanModePlan {
		path, _, ensureErr := planfile.Ensure(root, sessionID)
		if ensureErr != nil {
			return PlanState{}, fmt.Errorf("prepare plan file: %w", ensureErr)
		}
		planPath = path
	}
	if err = s.appendPlanModeEvent(sessionID, mode, reason); err != nil {
		return PlanState{}, err
	}
	return s.applyPlanState(sessionID, func(current PlanState) PlanState {
		next := current
		next.Mode = mode
		if planPath != "" {
			next.PlanPath = planPath
		}
		return next
	}), nil
}

func validatePlanModeTransition(mode, reason string) error {
	switch mode {
	case sessionlog.PlanModePlan, sessionlog.PlanModeDefault:
	default:
		return fmt.Errorf("invalid plan mode %q", mode)
	}
	switch reason {
	case sessionlog.PlanModeReasonUserToggle, sessionlog.PlanModeReasonPlanApproved, sessionlog.PlanModeReasonPlanCancelled:
	default:
		return fmt.Errorf("invalid plan mode reason %q", reason)
	}
	return nil
}

func (s *Service) appendPlanModeEvent(sessionID, mode, reason string) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if _, err := sessionlog.Append(s.deps.ProjectRoot, sessionID, sessionlog.EventPlanMode, sessionlog.PlanMode{Mode: mode, Reason: reason, At: time.Now().UTC()}); err != nil {
		return fmt.Errorf("record plan mode event: %w", err)
	}
	return nil
}

// SubmitPlanApproval registers the plan approval request of one session and
// appends the submitted event. A session holds at most one pending request:
// a second submission is rejected instead of silently replacing the dialog
// the user may already be looking at.
func (s *Service) SubmitPlanApproval(sessionID, runID, planPath string) (PlanApproval, error) {
	root, err := s.boundProjectRoot()
	if err != nil {
		return PlanApproval{}, err
	}
	if runID == "" || planPath == "" {
		return PlanApproval{}, errors.New("plan approval requires a run and a plan path")
	}
	id, err := sessionlog.NewID()
	if err != nil {
		return PlanApproval{}, err
	}
	now := time.Now().UTC()
	approval := PlanApproval{ID: id, SessionID: sessionID, RunID: runID, PlanPath: planPath, Status: sessionlog.PlanApprovalSubmitted, CreatedAt: now}
	s.planMu.Lock()
	if s.pendingPlanApprovalLocked(sessionID) != nil {
		s.planMu.Unlock()
		return PlanApproval{}, errors.New("已有待审批计划")
	}
	if s.planApprovals == nil {
		s.planApprovals = map[string]*PlanApproval{}
	}
	entry := approval
	s.planApprovals[approval.ID] = &entry
	s.planMu.Unlock()

	s.eventMu.Lock()
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventPlanApproval, sessionlog.PlanApprovalRecord{
		RequestID: approval.ID, RunID: runID, PlanPath: planPath, Status: sessionlog.PlanApprovalSubmitted, CreatedAt: now,
	})
	s.eventMu.Unlock()
	if err != nil {
		s.planMu.Lock()
		delete(s.planApprovals, approval.ID)
		s.planMu.Unlock()
		return PlanApproval{}, fmt.Errorf("record plan approval submission: %w", err)
	}
	s.broadcastPlanApproval(sessionID, ServerMsg{Type: "plan_approval_pending", PlanApprovals: []PlanApprovalRef{planApprovalRef(approval)}})
	return approval, nil
}

// ResolvePlanApproval applies the user decision to the session's pending plan
// approval. The terminal plan_approval event, the plan-mode transition of an
// approval, and an ordinary user message summarizing the decision (so it
// enters the model context) are appended to the session log in that order,
// the plan state is updated, and session clients are notified. Feedback text
// must be redacted by the caller before it reaches this method.
func (s *Service) ResolvePlanApproval(sessionID, choice, feedback string) (PlanApproval, PlanState, string, error) {
	root, err := s.boundProjectRoot()
	if err != nil {
		return PlanApproval{}, PlanState{}, "", err
	}
	status := planApprovalStatusForChoice(choice)
	if status == "" {
		return PlanApproval{}, PlanState{}, "", fmt.Errorf("invalid plan approval choice %q", choice)
	}
	s.planMu.Lock()
	pending := s.pendingPlanApprovalLocked(sessionID)
	s.planMu.Unlock()
	if pending == nil {
		return PlanApproval{}, PlanState{}, "", errors.New("没有待审批计划")
	}
	resolvedAt := time.Now().UTC()
	s.eventMu.Lock()
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventPlanApproval, sessionlog.PlanApprovalRecord{
		RequestID: pending.ID, RunID: pending.RunID, PlanPath: pending.PlanPath,
		Status: status, Feedback: feedback, CreatedAt: pending.CreatedAt, ResolvedAt: resolvedAt,
	})
	s.eventMu.Unlock()
	if err != nil {
		return PlanApproval{}, PlanState{}, "", fmt.Errorf("record plan approval resolution: %w", err)
	}
	// The request is terminal in the log from here on: drop it from the
	// pending set even if the remaining appends fail, so the session never
	// keeps a ghost dialog the log already resolved.
	s.planMu.Lock()
	if entry := s.planApprovals[pending.ID]; entry != nil {
		entry.Status = status
		entry.Feedback = feedback
		entry.ResolvedAt = resolvedAt
	}
	s.planMu.Unlock()

	approved := status == sessionlog.PlanApprovalApprovedAuto || status == sessionlog.PlanApprovalApprovedManual
	if approved {
		if err = s.appendPlanModeEvent(sessionID, sessionlog.PlanModeDefault, sessionlog.PlanModeReasonPlanApproved); err != nil {
			return PlanApproval{}, PlanState{}, "", err
		}
	}
	text := planResolutionMessage(status, feedback)
	s.eventMu.Lock()
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Kind: "text", Text: text})
	s.eventMu.Unlock()
	if err != nil {
		return PlanApproval{}, PlanState{}, "", fmt.Errorf("record plan resolution message: %w", err)
	}
	state := s.applyPlanState(sessionID, func(current PlanState) PlanState {
		next := current
		if approved {
			next.Mode = sessionlog.PlanModeDefault
			if status == sessionlog.PlanApprovalApprovedAuto {
				next.ExecutionMode = PlanExecutionAcceptEdits
			} else {
				next.ExecutionMode = PlanExecutionDefault
			}
		}
		return next
	})
	updated := *pending
	updated.Status = status
	updated.Feedback = feedback
	updated.ResolvedAt = resolvedAt
	s.broadcastPlanApproval(sessionID, ServerMsg{Type: "plan_approval_resolved", PlanState: &state, PlanApprovals: []PlanApprovalRef{}})
	return updated, state, text, nil
}

// CancelPlanApproval resolves the pending approval of one session as
// cancelled. It backs the sink's context-cancellation path: a run that stops
// waiting must not leave a dangling dialog behind.
func (s *Service) CancelPlanApproval(sessionID, requestID string) error {
	s.planMu.Lock()
	pending := s.pendingPlanApprovalLocked(sessionID)
	s.planMu.Unlock()
	if pending == nil || pending.ID != requestID {
		return errors.New("没有待审批计划")
	}
	_, _, _, err := s.ResolvePlanApproval(sessionID, PlanResolveCancel, "")
	return err
}

// PlanApprovalStatus reports the lifecycle status of one request; ok is false
// for unknown requests.
func (s *Service) PlanApprovalStatus(requestID string) (status, feedback string, ok bool) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	approval, found := s.planApprovals[requestID]
	if !found || approval == nil {
		return "", "", false
	}
	return approval.Status, approval.Feedback, true
}

// pendingPlanApprovalLocked returns the pending request of one session, if
// any; planMu must be held.
func (s *Service) pendingPlanApprovalLocked(sessionID string) *PlanApproval {
	for _, approval := range s.planApprovals {
		if approval != nil && approval.SessionID == sessionID && approval.Status == sessionlog.PlanApprovalSubmitted {
			pending := *approval
			return &pending
		}
	}
	return nil
}

func planApprovalStatusForChoice(choice string) string {
	switch choice {
	case PlanResolveAuto:
		return sessionlog.PlanApprovalApprovedAuto
	case PlanResolveManual:
		return sessionlog.PlanApprovalApprovedManual
	case PlanResolveFeedback:
		return sessionlog.PlanApprovalFeedback
	case PlanResolveCancel:
		return sessionlog.PlanApprovalCancelled
	}
	return ""
}

// planResolutionMessage is the ordinary user message inserted into the
// session context when a plan approval reaches its terminal state.
func planResolutionMessage(status, feedback string) string {
	switch status {
	case sessionlog.PlanApprovalApprovedAuto:
		return "计划已批准，后续按自动接受模式执行"
	case sessionlog.PlanApprovalApprovedManual:
		return "计划已批准，后续按逐次确认模式执行"
	case sessionlog.PlanApprovalFeedback:
		return "用户要求继续修改计划：" + feedback
	case sessionlog.PlanApprovalCancelled:
		return "计划审批已取消"
	}
	return ""
}

func planApprovalRef(approval PlanApproval) PlanApprovalRef {
	return PlanApprovalRef{ID: approval.ID, SessionID: approval.SessionID, RunID: approval.RunID, PlanPath: approval.PlanPath, CreatedAt: approval.CreatedAt}
}

// broadcastPlanApproval fans a plan approval update out to the clients bound
// to the session, mirroring the permission approval fan-out.
func (s *Service) broadcastPlanApproval(sessionID string, msg ServerMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, sub := range s.clients {
		if sub.sessionID != sessionID {
			continue
		}
		select {
		case ch <- msg:
		default:
		}
	}
}

// recordPlanRun bumps the plan-mode run counter of a session and returns the
// state the run starts with. Non-plan sessions are returned unchanged.
func (s *Service) recordPlanRun(sessionID string) PlanState {
	return s.applyPlanState(sessionID, func(current PlanState) PlanState {
		if current.Mode != sessionlog.PlanModePlan {
			return current
		}
		next := current
		next.Runs++
		return next
	})
}

// planRunAuthority maps the session plan state onto the permission bounds of
// a new run. Plan mode wins over the post-approval execution mode — the plan
// gate is only left through a plan approval or a user toggle — and goal runs
// always keep the default mode.
func planRunAuthority(state PlanState, workKind agent.WorkKind) (permission.Mode, string) {
	if workKind != agent.WorkSession {
		return permission.ModeDefault, ""
	}
	if state.Mode == sessionlog.PlanModePlan {
		return permission.ModePlan, state.PlanPath
	}
	if state.ExecutionMode == PlanExecutionAcceptEdits {
		return permission.ModeAcceptEdits, ""
	}
	return permission.ModeDefault, ""
}

// PlanApprovalSink adapts the plan approval service to the execution.PlanSink
// interface: exit_plan_mode submissions become session approval requests and
// the calling run blocks, polling the request status, until the user decides
// or the run context is cancelled. The factory call site wires it with
// execution.WithPlanSink.
type PlanApprovalSink struct {
	Service *Service
	// PollEvery is the request status poll interval; zero defaults to 500ms.
	PollEvery time.Duration
}

// NewPlanApprovalSink builds the plan approval sink for the executor factory.
func NewPlanApprovalSink(service *Service) *PlanApprovalSink {
	return &PlanApprovalSink{Service: service}
}

// SubmitPlan implements execution.PlanSink.
func (p *PlanApprovalSink) SubmitPlan(ctx context.Context, sessionID, runID, planPath string) (string, error) {
	approval, err := p.Service.SubmitPlanApproval(sessionID, runID, planPath)
	if err != nil {
		return "", err
	}
	interval := p.PollEvery
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// The run is gone: drop the dialog instead of leaving it pending.
			_ = p.Service.CancelPlanApproval(sessionID, approval.ID)
			return "", execution.PlanCancelledError{}
		case <-ticker.C:
		}
		status, feedback, ok := p.Service.PlanApprovalStatus(approval.ID)
		if !ok {
			continue
		}
		switch status {
		case sessionlog.PlanApprovalApprovedAuto:
			return execution.PlanChoiceAuto, nil
		case sessionlog.PlanApprovalApprovedManual:
			return execution.PlanChoiceManual, nil
		case sessionlog.PlanApprovalFeedback:
			return "", execution.PlanFeedbackError{Text: feedback}
		case sessionlog.PlanApprovalCancelled:
			return "", execution.PlanCancelledError{}
		}
	}
}
