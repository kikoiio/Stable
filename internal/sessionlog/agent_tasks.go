package sessionlog

import (
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"
)

// AgentTaskDelegation is the persisted task lifecycle payload. It mirrors the
// delegation event without importing agent, which already depends on sessionlog.
type AgentTaskDelegation struct {
	SessionID string    `json:"session_id,omitempty"`
	BatchID   string    `json:"batch_id"`
	TaskID    string    `json:"task_id"`
	TaskName  string    `json:"task_name"`
	Status    string    `json:"status"`
	Stage     string    `json:"stage,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AgentTaskRecord projects one task from durable events. LastSeq and
// TerminalSeq are session cursors, while LastRunSeq is the independent run
// cursor. A task with only run_started is visible before its first queued event.
type AgentTaskRecord struct {
	Started        RunStarted
	Delegation     AgentTaskDelegation
	StartSeq       uint64
	LastSeq        uint64
	LastRunSeq     uint64
	TerminalSeq    uint64
	RunStatus      string
	TerminalReason string
}

// AgentTasks folds the complete, uncompacted transcript. It rejects malformed
// identities and lifecycle transitions rather than publishing invented state.
// Use Replay, not ReplayAfter, because task ownership precedes the cursor tail.
func AgentTasks(t Transcript) ([]AgentTaskRecord, error) {
	st, err := scanAgentTasks(t.Session.ID, t.Events)
	if err != nil {
		return nil, err
	}
	out := make([]AgentTaskRecord, 0, len(st.order))
	for _, id := range st.order {
		out = append(out, st.tasks[id])
	}
	return out, nil
}

// AgentTaskNotifications returns validated handoff references in event order.
// Consumers must verify that the destination run exists before marking a
// reference delivered; an absent destination is intentionally recoverable.
func AgentTaskNotifications(t Transcript) ([]AgentTaskNotification, error) {
	st, err := scanAgentTasks(t.Session.ID, t.Events)
	if err != nil {
		return nil, err
	}
	return append([]AgentTaskNotification(nil), st.notifications...), nil
}

type agentTaskState struct {
	runs          map[string]RunStarted
	tasks         map[string]AgentTaskRecord
	order         []string
	notifications []AgentTaskNotification
	openCalls     map[string]ToolCall
	usedCalls     map[string]string
	currentParent string
}

func newAgentTaskState() *agentTaskState {
	return &agentTaskState{runs: map[string]RunStarted{}, tasks: map[string]AgentTaskRecord{}, openCalls: map[string]ToolCall{}, usedCalls: map[string]string{}}
}

func scanAgentTasks(sessionID string, events []Event) (*agentTaskState, error) {
	st := newAgentTaskState()
	for _, event := range events {
		if event.SessionID != "" && event.SessionID != sessionID {
			return nil, errors.New("agent task event belongs to a different session")
		}
		if err := st.observe(sessionID, event); err != nil {
			return nil, fmt.Errorf("agent task event at seq %d: %w", event.Seq, err)
		}
	}
	return st, nil
}

func sameAgentTaskWork(a, b RunStarted) bool {
	return a.WorkKind == b.WorkKind && a.GoalID == b.GoalID && a.WorkItemID == b.WorkItemID
}

func validAgentIdentity(value string, max int) bool {
	if len(value) == 0 || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if i > 0 && (r == '_' || r == '-') {
			continue
		}
		return false
	}
	return true
}

func (st *agentTaskState) checkStarted(start RunStarted) error {
	if start.AgentTaskID == "" {
		if start.AgentName != "" || start.OriginRunID != "" || start.OriginCallID != "" {
			return errors.New("agent source fields require agent_task_id")
		}
	} else {
		if !validAgentIdentity(start.AgentTaskID, 128) || !validAgentIdentity(start.AgentName, 64) {
			return errors.New("agent task requires bounded task ID and agent name")
		}
		if !validAgentIdentity(start.RunID, 128) {
			return errors.New("agent task run ID is invalid")
		}
		if start.ForkSkill != "" || start.ForkEntry != "" {
			return errors.New("agent task run cannot also be a fork skill")
		}
		if _, ok := st.tasks[start.AgentTaskID]; ok {
			return errors.New("agent task ID already exists in session")
		}
		if start.OriginRunID != "" {
			if !validAgentIdentity(start.OriginRunID, 128) || start.OriginRunID == start.RunID {
				return errors.New("agent task origin run ID is invalid")
			}
			origin, ok := st.runs[start.OriginRunID]
			if !ok || !sameAgentTaskWork(start, origin) {
				return errors.New("agent task origin run does not own the same work")
			}
		}
		if start.OriginCallID != "" {
			if start.OriginRunID == "" || len(start.OriginCallID) > 256 || !utf8.ValidString(start.OriginCallID) {
				return errors.New("agent task origin call requires a parent run and bounded UTF-8 call ID")
			}
			for _, r := range start.OriginCallID {
				if unicode.IsControl(r) {
					return errors.New("agent task origin call ID contains control characters")
				}
			}
			call, ok := st.openCalls[start.OriginCallID]
			if !ok || call.Name != "run_agent" || call.RunID != start.OriginRunID {
				return errors.New("agent task origin call must be an open run_agent call owned by the origin run")
			}
			if st.usedCalls[start.OriginCallID] != "" {
				return errors.New("agent task origin call already owns a task")
			}
		}
	}
	// References recorded before run_started must not acquire a different
	// work owner when their destination eventually starts.
	for _, notification := range st.notifications {
		if notification.DestinationRunID == start.RunID {
			task := st.tasks[notification.TaskID]
			if start.AgentTaskID != "" || !sameAgentTaskWork(task.Started, start) {
				return errors.New("agent task notification destination does not own the same work")
			}
			for _, prior := range st.notifications {
				if prior.TaskID == notification.TaskID && prior.TerminalSeq == notification.TerminalSeq && prior.DestinationRunID != start.RunID {
					if _, delivered := st.runs[prior.DestinationRunID]; delivered {
						return errors.New("agent task terminal notification already handed off")
					}
				}
			}
		}
	}
	return nil
}

func agentTaskTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "canceled", "interrupted":
		return true
	default:
		return false
	}
}

func taskRunStatus(status string) string {
	switch status {
	case "succeeded":
		return "completed"
	case "canceled":
		return "cancelled"
	default:
		return status
	}
}

func (st *agentTaskState) checkRunEvent(sessionID string, run RunEvent) error {
	start, ok := st.runs[run.RunID]
	if !ok || start.AgentTaskID == "" {
		return nil // Existing delegation and hook runs retain their contracts.
	}
	task := st.tasks[start.AgentTaskID]
	if run.SessionID != sessionID {
		return errors.New("agent task run event belongs to a different session")
	}
	if task.RunStatus != "" {
		return errors.New("agent task run event follows run terminal")
	}
	switch run.Kind {
	case "delegation_event":
		var d AgentTaskDelegation
		if err := decodeData(run.Payload, &d); err != nil {
			return errors.New("agent task delegation has invalid shape")
		}
		if d.TaskID != start.AgentTaskID || !validAgentIdentity(d.BatchID, 128) || (d.SessionID != "" && d.SessionID != sessionID) {
			return errors.New("agent task delegation does not match its task owner")
		}
		if d.TaskName == "" || len(d.TaskName) > 256 || len(d.Stage) > 256 || len(d.Summary) > 8<<10 || len(d.Error) > 1024 || !utf8.ValidString(d.TaskName+d.Stage+d.Summary+d.Error) {
			return errors.New("agent task delegation text exceeds its bounds")
		}
		if task.Delegation.BatchID != "" && task.Delegation.BatchID != d.BatchID {
			return errors.New("agent task delegation batch cannot change")
		}
		if task.TerminalSeq != 0 {
			return errors.New("agent task delegation follows terminal task state")
		}
		switch d.Status {
		case "queued":
			if task.Delegation.Status != "" {
				return errors.New("agent task can only be queued once")
			}
		case "running":
			if task.Delegation.Status != "queued" && task.Delegation.Status != "running" {
				return errors.New("agent task running event requires a queued task")
			}
		case "succeeded", "failed", "canceled", "interrupted":
			// Submission failures and startup recovery can close a run before
			// the first queued event was persisted.
		default:
			return errors.New("agent task delegation has invalid status")
		}
	case "terminal":
		var terminal struct {
			Status  string `json:"status"`
			Summary string `json:"summary,omitempty"`
			Reason  string `json:"reason,omitempty"`
		}
		if decodeData(run.Payload, &terminal) != nil {
			return errors.New("agent task run terminal has invalid shape")
		}
		if len(terminal.Summary) > 8<<10 || len(terminal.Reason) > 1024 || !utf8.ValidString(terminal.Summary+terminal.Reason) {
			return errors.New("agent task run terminal text exceeds its bounds")
		}
		if task.Delegation.Status != "" && !agentTaskTerminal(task.Delegation.Status) {
			return errors.New("agent task run terminal precedes child terminal")
		}
		if task.Delegation.Status != "" && terminal.Status != taskRunStatus(task.Delegation.Status) {
			return errors.New("agent task run terminal contradicts child terminal")
		}
		if task.Delegation.Status == "" && terminal.Status != "failed" && terminal.Status != "cancelled" && terminal.Status != "interrupted" {
			return errors.New("agent task without child events cannot complete successfully")
		}
	}
	return nil
}

func (st *agentTaskState) checkNotification(n AgentTaskNotification) error {
	if !validAgentIdentity(n.TaskID, 128) || !validAgentIdentity(n.DestinationRunID, 128) || n.TerminalSeq == 0 {
		return errors.New("agent task notification requires task, terminal sequence, and destination")
	}
	task, ok := st.tasks[n.TaskID]
	if !ok || task.TerminalSeq != n.TerminalSeq || !agentTaskTerminal(task.Delegation.Status) {
		return errors.New("agent task notification must reference the task's child terminal sequence")
	}
	if destination, exists := st.runs[n.DestinationRunID]; exists && (destination.AgentTaskID != "" || !sameAgentTaskWork(task.Started, destination)) {
		return errors.New("agent task notification destination does not own the same work")
	}
	for _, prior := range st.notifications {
		if prior.TaskID != n.TaskID || prior.TerminalSeq != n.TerminalSeq {
			continue
		}
		if prior.DestinationRunID == n.DestinationRunID {
			return errors.New("duplicate agent task notification")
		}
		if _, delivered := st.runs[prior.DestinationRunID]; delivered {
			return errors.New("agent task terminal notification already handed off")
		}
	}
	return nil
}

func (st *agentTaskState) observe(sessionID string, e Event) error {
	switch e.Type {
	case EventToolCall:
		var call ToolCall
		if decodeData(e.Data, &call) != nil {
			return errors.New("agent task tool call has invalid shape")
		}
		if call.CallID != "" {
			if _, pending := st.openCalls[call.CallID]; pending {
				return errors.New("duplicate pending tool call ID in agent task projection")
			}
			// Call IDs may be reused after the preceding call/result pair has
			// closed. The new call instance owns its own single-task association.
			delete(st.usedCalls, call.CallID)
			if call.RunID == "" {
				call.RunID = st.currentParent
			}
			st.openCalls[call.CallID] = call
		}
	case EventToolResult:
		var result ToolResult
		if decodeData(e.Data, &result) != nil {
			return errors.New("agent task tool result has invalid shape")
		}
		delete(st.openCalls, result.CallID)
	case EventRunStarted:
		var start RunStarted
		if decodeData(e.Data, &start) != nil {
			return errors.New("agent task run_started has invalid shape")
		}
		if _, duplicate := st.runs[start.RunID]; duplicate {
			return errors.New("duplicate run ID in task projection")
		}
		if err := st.checkStarted(start); err != nil {
			return err
		}
		st.runs[start.RunID] = start
		if start.AgentTaskID != "" {
			st.order = append(st.order, start.AgentTaskID)
			st.tasks[start.AgentTaskID] = AgentTaskRecord{Started: start, StartSeq: e.Seq, LastSeq: e.Seq}
			if start.OriginCallID != "" {
				st.usedCalls[start.OriginCallID] = start.AgentTaskID
			}
		} else {
			st.currentParent = start.RunID
		}
	case EventRunEvent:
		var run RunEvent
		if decodeData(e.Data, &run) != nil {
			return errors.New("agent task run_event has invalid shape")
		}
		if err := st.checkRunEvent(sessionID, run); err != nil {
			return err
		}
		start := st.runs[run.RunID]
		if start.AgentTaskID == "" {
			return nil
		}
		task := st.tasks[start.AgentTaskID]
		task.LastSeq, task.LastRunSeq = e.Seq, run.RunSeq
		switch run.Kind {
		case "delegation_event":
			_ = decodeData(run.Payload, &task.Delegation)
			if agentTaskTerminal(task.Delegation.Status) {
				task.TerminalSeq = e.Seq
			}
		case "terminal":
			var terminal struct {
				Status string `json:"status"`
				Reason string `json:"reason,omitempty"`
			}
			_ = decodeData(run.Payload, &terminal)
			task.RunStatus = terminal.Status
			task.TerminalReason = terminal.Reason
		}
		st.tasks[start.AgentTaskID] = task
	case EventAgentTaskNotification:
		var n AgentTaskNotification
		if decodeData(e.Data, &n) != nil {
			return errors.New("agent task notification has invalid shape")
		}
		if err := st.checkNotification(n); err != nil {
			return err
		}
		st.notifications = append(st.notifications, n)
	}
	return nil
}
