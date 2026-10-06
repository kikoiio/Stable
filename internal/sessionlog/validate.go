package sessionlog

import (
	"errors"
	"fmt"
)

// scanState summarizes validated history so a newly observed event can be
// checked for ownership and ordering without re-decoding every caller side.
type scanState struct {
	openCalls     map[string]uint64            // call ID -> session seq of the call
	runs          map[string]RunStarted        // run ID -> start record
	runSeq        map[string]uint64            // run ID -> last observed run seq
	runEventAt    map[string]map[uint64]uint64 // run ID -> run seq -> session seq
	snapshots     map[string]SnapshotRef       // snapshot ID -> recorded ref
	questions     map[string]string            // question ID -> current status
	planApprovals map[string]string            // request ID -> last recorded status
	todoRev       int                          // last observed todo snapshot revision
	skillInv      bool                         // a skill inventory snapshot already exists
}

func scanEvents(events []Event) scanState {
	st := scanState{
		openCalls:     map[string]uint64{},
		runs:          map[string]RunStarted{},
		runSeq:        map[string]uint64{},
		runEventAt:    map[string]map[uint64]uint64{},
		snapshots:     map[string]SnapshotRef{},
		questions:     map[string]string{},
		planApprovals: map[string]string{},
	}
	for _, e := range events {
		switch e.Type {
		case EventToolCall:
			var call ToolCall
			if decodeData(e.Data, &call) == nil && call.CallID != "" {
				st.openCalls[call.CallID] = e.Seq
			}
		case EventToolResult:
			var result ToolResult
			if decodeData(e.Data, &result) == nil {
				delete(st.openCalls, result.CallID)
			}
		case EventRunStarted:
			var started RunStarted
			if decodeData(e.Data, &started) == nil {
				st.runs[started.RunID] = started
			}
		case EventRunEvent:
			var runEvent RunEvent
			if decodeData(e.Data, &runEvent) == nil {
				st.runSeq[runEvent.RunID] = runEvent.RunSeq
				m := st.runEventAt[runEvent.RunID]
				if m == nil {
					m = map[uint64]uint64{}
					st.runEventAt[runEvent.RunID] = m
				}
				m[runEvent.RunSeq] = e.Seq
			}
		case EventSnapshot:
			var snap SnapshotRef
			if decodeData(e.Data, &snap) == nil {
				st.snapshots[snap.SnapshotID] = snap
			}
		case EventQuestion:
			var question PendingQuestion
			if decodeData(e.Data, &question) == nil {
				st.questions[question.QuestionID] = question.Status
			}
		case EventReply:
			var reply QuestionReply
			if decodeData(e.Data, &reply) == nil {
				st.questions[reply.QuestionID] = QuestionReplied
			}
		case EventPlanApproval:
			var record PlanApprovalRecord
			if decodeData(e.Data, &record) == nil {
				st.planApprovals[record.RequestID] = record.Status
			}
		case EventTodo:
			var update TodoUpdate
			if decodeData(e.Data, &update) == nil {
				st.todoRev = update.Revision
			}
		case EventSkillInventory:
			var inv SkillInventory
			if decodeData(e.Data, &inv) == nil {
				st.skillInv = true
			}
		}
	}
	return st
}

// checkBoundary validates a compaction boundary against history. selfSeq is
// the sequence number the boundary event itself will occupy.
func checkBoundary(b Boundary, st scanState, selfSeq uint64) error {
	if b.FromSeq == 0 || b.ToSeq < b.FromSeq || b.Summary == "" {
		return errors.New("boundary requires a valid range and summary")
	}
	var cutoff uint64 // session seq of the last covered event
	switch b.EffectiveScope() {
	case BoundaryScopeSession:
		if b.RunID != "" {
			return errors.New("session scope boundary must not name a run")
		}
		cutoff = b.ToSeq
	case BoundaryScopeRun:
		if b.RunID == "" {
			return errors.New("run scope boundary requires run_id")
		}
		if st.runs[b.RunID].RunID == "" {
			return errors.New("boundary run does not exist in session")
		}
		at, ok := st.runEventAt[b.RunID][b.ToSeq]
		if !ok || b.ToSeq > st.runSeq[b.RunID] {
			return errors.New("boundary range exceeds observed run events")
		}
		cutoff = at
	default:
		return fmt.Errorf("boundary has invalid scope %q", b.Scope)
	}
	if cutoff >= selfSeq {
		return errors.New("boundary range covers the boundary itself")
	}
	for _, seq := range st.openCalls {
		if seq <= cutoff {
			return errors.New("boundary splits a tool call and its result")
		}
	}
	return nil
}

func checkSnapshot(sessionID string, s SnapshotRef, st scanState) error {
	if s.SnapshotID == "" || s.CandidateID == "" || s.Digest == "" || s.CreatedAt.IsZero() {
		return errors.New("snapshot event requires snapshot_id, candidate_id, digest, and created_at")
	}
	if s.SessionID != sessionID {
		return errors.New("snapshot session does not match log")
	}
	if s.RunID != "" && st.runs[s.RunID].RunID == "" {
		return errors.New("snapshot run does not exist in session")
	}
	if _, dup := st.snapshots[s.SnapshotID]; dup {
		return errors.New("snapshot ID already recorded in session")
	}
	return nil
}

func checkRewind(r RewindRecord, st scanState) error {
	if r.SnapshotID == "" || r.CandidateID == "" || r.CreatedAt.IsZero() {
		return errors.New("rewind event requires snapshot_id, candidate_id, and created_at")
	}
	switch r.Status {
	case RewindPending, RewindCompleted, RewindFailed:
	default:
		return fmt.Errorf("rewind has invalid status %q", r.Status)
	}
	snap, ok := st.snapshots[r.SnapshotID]
	if !ok {
		return errors.New("rewind targets an unknown snapshot")
	}
	if snap.CandidateID != r.CandidateID {
		return errors.New("rewind candidate does not match snapshot owner")
	}
	return nil
}

func checkQuestion(sessionID string, q PendingQuestion, st scanState) error {
	if q.QuestionID == "" || q.WorkRef == "" || q.RunID == "" || q.Prompt == "" || q.CreatedAt.IsZero() {
		return errors.New("question event requires question_id, work_ref, run_id, prompt, and created_at")
	}
	if q.SessionID != sessionID {
		return errors.New("question session does not match log")
	}
	if q.Status != QuestionPending {
		return errors.New("question must start pending")
	}
	if st.runs[q.RunID].RunID == "" {
		return errors.New("question run does not exist in session")
	}
	if _, dup := st.questions[q.QuestionID]; dup {
		return errors.New("question ID already exists in session")
	}
	return nil
}

func checkReply(r QuestionReply, st scanState) error {
	if r.QuestionID == "" || r.ReplyText == "" || r.RepliedAt.IsZero() {
		return errors.New("reply event requires question_id, reply_text, and replied_at")
	}
	status, ok := st.questions[r.QuestionID]
	if !ok {
		return errors.New("reply targets an unknown question")
	}
	if status != QuestionPending {
		return errors.New("question is already answered")
	}
	return nil
}

func checkPlanMode(m PlanMode) error {
	if m.At.IsZero() {
		return errors.New("plan mode event requires a time")
	}
	switch m.Mode {
	case PlanModePlan, PlanModeDefault:
	default:
		return fmt.Errorf("plan mode has invalid mode %q", m.Mode)
	}
	switch m.Reason {
	case PlanModeReasonUserToggle, PlanModeReasonPlanApproved, PlanModeReasonPlanCancelled:
	default:
		return fmt.Errorf("plan mode has invalid reason %q", m.Reason)
	}
	return nil
}

// planApprovalTerminal reports whether the status ends a request lifecycle.
func planApprovalTerminal(status string) bool {
	switch status {
	case PlanApprovalApprovedAuto, PlanApprovalApprovedManual, PlanApprovalFeedback, PlanApprovalCancelled:
		return true
	}
	return false
}

func checkPlanApproval(r PlanApprovalRecord, st scanState) error {
	if r.RequestID == "" || r.RunID == "" || r.PlanPath == "" || r.CreatedAt.IsZero() {
		return errors.New("plan approval event requires request_id, run_id, plan_path, and created_at")
	}
	switch {
	case r.Status == PlanApprovalSubmitted:
		if !r.ResolvedAt.IsZero() {
			return errors.New("submitted plan approval cannot carry a resolution time")
		}
	case planApprovalTerminal(r.Status):
		if r.ResolvedAt.IsZero() {
			return errors.New("resolved plan approval requires a resolution time")
		}
	default:
		return fmt.Errorf("plan approval has invalid status %q", r.Status)
	}
	status, seen := st.planApprovals[r.RequestID]
	if !seen {
		if r.Status != PlanApprovalSubmitted {
			return errors.New("plan approval must start submitted")
		}
		return nil
	}
	if status != PlanApprovalSubmitted {
		return errors.New("plan approval request is already resolved")
	}
	if r.Status == PlanApprovalSubmitted {
		return errors.New("plan approval request is already submitted")
	}
	return nil
}

func checkTodoUpdate(u TodoUpdate, st scanState) error {
	if u.Revision <= 0 {
		return errors.New("todo update requires a positive revision")
	}
	if u.Revision <= st.todoRev {
		return errors.New("todo revision must strictly increase")
	}
	if len(u.Tasks) > MaxTodoTasks {
		return fmt.Errorf("todo update carries %d tasks, limit is %d", len(u.Tasks), MaxTodoTasks)
	}
	return nil
}

func checkSkillList(entries []SkillInfo, what string) error {
	if len(entries) > MaxSkillListEntries {
		return fmt.Errorf("%s carries %d entries, limit is %d", what, len(entries), MaxSkillListEntries)
	}
	for _, info := range entries {
		if info.Name == "" {
			return fmt.Errorf("%s requires a name for every skill", what)
		}
	}
	return nil
}

func checkSkillInventory(inv SkillInventory, st scanState) error {
	if st.skillInv {
		return errors.New("session already has a skill inventory")
	}
	return checkSkillList(inv.Skills, "skill inventory")
}

func checkSkillDelta(d SkillDelta) error {
	return checkSkillList(d.Added, "skill delta")
}

func checkSkillInvoked(i SkillInvoked) error {
	if i.Name == "" {
		return errors.New("skill invocation requires a name")
	}
	switch i.Entry {
	case SkillEntrySlash, SkillEntryTool:
	default:
		return fmt.Errorf("skill invocation has invalid entry %q", i.Entry)
	}
	return nil
}

// validateOwnedAppend checks boundary, snapshot, rewind, question, reply,
// plan mode, plan approval, todo, and skill events before they are appended.
// selfSeq is the sequence number the new event will occupy.
func validateOwnedAppend(sessionID, typ string, data any, events []Event, selfSeq uint64) error {
	st := scanEvents(events)
	switch typ {
	case EventBoundary:
		var b Boundary
		if err := decodeData(data, &b); err != nil {
			return errors.New("compaction boundary has invalid shape")
		}
		return checkBoundary(b, st, selfSeq)
	case EventSnapshot:
		var s SnapshotRef
		if err := decodeData(data, &s); err != nil {
			return errors.New("snapshot event has invalid shape")
		}
		return checkSnapshot(sessionID, s, st)
	case EventRewind:
		var r RewindRecord
		if err := decodeData(data, &r); err != nil {
			return errors.New("rewind event has invalid shape")
		}
		return checkRewind(r, st)
	case EventQuestion:
		var q PendingQuestion
		if err := decodeData(data, &q); err != nil {
			return errors.New("question event has invalid shape")
		}
		return checkQuestion(sessionID, q, st)
	case EventReply:
		var r QuestionReply
		if err := decodeData(data, &r); err != nil {
			return errors.New("reply event has invalid shape")
		}
		return checkReply(r, st)
	case EventPlanMode:
		var m PlanMode
		if err := decodeData(data, &m); err != nil {
			return errors.New("plan mode event has invalid shape")
		}
		return checkPlanMode(m)
	case EventPlanApproval:
		var r PlanApprovalRecord
		if err := decodeData(data, &r); err != nil {
			return errors.New("plan approval event has invalid shape")
		}
		return checkPlanApproval(r, st)
	case EventTodo:
		var u TodoUpdate
		if err := decodeData(data, &u); err != nil {
			return errors.New("todo update event has invalid shape")
		}
		return checkTodoUpdate(u, st)
	case EventSkillInventory:
		var inv SkillInventory
		if err := decodeData(data, &inv); err != nil {
			return errors.New("skill inventory event has invalid shape")
		}
		return checkSkillInventory(inv, st)
	case EventSkillDelta:
		var d SkillDelta
		if err := decodeData(data, &d); err != nil {
			return errors.New("skill delta event has invalid shape")
		}
		return checkSkillDelta(d)
	case EventSkillInvoked:
		var i SkillInvoked
		if err := decodeData(data, &i); err != nil {
			return errors.New("skill invoked event has invalid shape")
		}
		return checkSkillInvoked(i)
	case EventHookFired:
		var h HookFired
		if err := decodeData(data, &h); err != nil {
			return errors.New("hook fired event has invalid shape")
		}
		return checkHookFired(h)
	case EventHookReload:
		var h HookReload
		if err := decodeData(data, &h); err != nil {
			return errors.New("hook reload event has invalid shape")
		}
		return checkHookReload(h)
	case EventMCPReload:
		var r MCPReload
		if err := decodeData(data, &r); err != nil {
			return errors.New("mcp reload event has invalid shape")
		}
		return checkMCPReload(r)
	case EventMCPServer:
		var s MCPServer
		if err := decodeData(data, &s); err != nil {
			return errors.New("mcp server event has invalid shape")
		}
		return checkMCPServer(s)
	}
	return nil
}

func checkHookFired(h HookFired) error {
	if h.HookID == "" || h.Event == "" || h.Action == "" {
		return errors.New("hook fired event is missing hook_id, event, or action")
	}
	if len(h.Output) > MaxHookOutput {
		return fmt.Errorf("hook output exceeds %d bytes", MaxHookOutput)
	}
	return nil
}

func checkHookReload(h HookReload) error {
	if h.Before < 0 || h.After < 0 {
		return errors.New("hook reload counts must be non-negative")
	}
	return nil
}

func checkMCPReload(r MCPReload) error {
	if r.Before < 0 || r.After < 0 {
		return errors.New("mcp reload counts must be non-negative")
	}
	for _, rejection := range r.Rejections {
		if len(rejection) > MaxMCPOutput {
			return fmt.Errorf("mcp rejection exceeds %d bytes", MaxMCPOutput)
		}
	}
	switch r.Trigger {
	case "auto", "manual":
	default:
		return fmt.Errorf("mcp reload has invalid trigger %q", r.Trigger)
	}
	return nil
}

func checkMCPServer(s MCPServer) error {
	if s.Name == "" || s.Source == "" {
		return errors.New("mcp server event is missing name or source")
	}
	if s.ToolCount < 0 {
		return errors.New("mcp server tool count must be non-negative")
	}
	switch s.State {
	case "connected", "disconnected", "reload-failed":
	default:
		return fmt.Errorf("mcp server has invalid state %q", s.State)
	}
	if len(s.Error) > MaxMCPOutput {
		return fmt.Errorf("mcp error exceeds %d bytes", MaxMCPOutput)
	}
	return nil
}
