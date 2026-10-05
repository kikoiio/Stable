package conversation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/permission"
	"stable/internal/sessionlog"
)

// ClientMsg is one line of JSON sent from a chat client to the session service.
type ClientMsg struct {
	Op              string                  `json:"op"`             // existing session operations plus run_start | run_subscribe | run_cancel
	Goal            string                  `json:"goal,omitempty"` // focused goal (required for say/reply)
	Text            string                  `json:"text,omitempty"` // natural-language content
	ID              string                  `json:"id,omitempty"`   // proposal ID for confirm/reject
	ProjectRoot     string                  `json:"project_root,omitempty"`
	SessionID       string                  `json:"session_id,omitempty"`
	RunID           string                  `json:"run_id,omitempty"`
	AfterSeq        uint64                  `json:"after_seq,omitempty"`
	Run             *agent.ExecutionRequest `json:"run,omitempty"`
	CandidateID     string                  `json:"candidate_id,omitempty"`
	DecisionID      string                  `json:"decision_id,omitempty"`
	PreviewDigest   string                  `json:"preview_digest,omitempty"`
	CandidateDigest string                  `json:"candidate_digest,omitempty"`
	FormalDigest    string                  `json:"formal_digest,omitempty"`
	AcceptanceMode  string                  `json:"acceptance_mode,omitempty"`
	Confirmed       []string                `json:"confirmed_findings,omitempty"`
	ApprovalID      string                  `json:"approval_id,omitempty"`
	ApprovalChoice  string                  `json:"approval_choice,omitempty"`
	SnapshotID      string                  `json:"snapshot_id,omitempty"`
	QuestionID      string                  `json:"question_id,omitempty"`
	Limit           int                     `json:"limit,omitempty"`
}

// ServerMsg is one line of JSON pushed from the session service to clients.
type ServerMsg struct {
	Type       string                         `json:"type"` // message | proposal | goal_update | error | done
	Message    *core.SessionMessage           `json:"message,omitempty"`
	Proposal   *core.CriteriaProposal         `json:"proposal,omitempty"`
	Goal       *core.Goal                     `json:"goal,omitempty"`
	Error      string                         `json:"error,omitempty"`
	Session    *sessionlog.SessionInfo        `json:"session,omitempty"`
	Sessions   []sessionlog.SessionInfo       `json:"sessions,omitempty"`
	Transcript *sessionlog.Transcript         `json:"transcript,omitempty"`
	Goals      []core.Goal                    `json:"goals,omitempty"`
	RunID      string                         `json:"run_id,omitempty"`
	RunEvent   *sessionlog.RunEvent           `json:"run_event,omitempty"`
	Outcome    *agent.RunOutcome              `json:"outcome,omitempty"`
	Cursor     uint64                         `json:"cursor,omitempty"`
	Review     *candidate.Review              `json:"review,omitempty"`
	Receipt    *candidate.Receipt             `json:"receipt,omitempty"`
	Approval   *permission.ApprovalPrompt     `json:"approval,omitempty"`
	Approvals  []permission.ApprovalPrompt    `json:"approvals,omitempty"`
	Decision   *permission.PermissionDecision `json:"decision,omitempty"`
	Search     *sessionlog.SearchResult       `json:"search,omitempty"`
	Snapshots  []sessionlog.SnapshotRef       `json:"snapshots,omitempty"`
	Rewind     *sessionlog.RewindRecord       `json:"rewind,omitempty"`
	Questions  []sessionlog.PendingQuestion   `json:"questions,omitempty"`
	Reply      *sessionlog.QuestionReply      `json:"reply,omitempty"`
	// Tasks is the full task-list snapshot pushed by every todo_update
	// event (TodoProvider onChange).
	Tasks []sessionlog.TaskSnapshot `json:"tasks,omitempty"`
	// Plan carries the restored plan state of a session_load response.
	Plan *PlanState `json:"plan,omitempty"`
	// PlanState is the session plan state pushed by plan_mode, plan_resolve,
	// and the plan_approval_resolved broadcast.
	PlanState *PlanState `json:"plan_state,omitempty"`
	// PlanApprovals is the pending plan approval list pushed by
	// plan_approval_pending / plan_approval_resolved.
	PlanApprovals []PlanApprovalRef `json:"plan_approvals,omitempty"`
}

func validOp(op string) bool {
	switch op {
	case "session_list", "session_create", "session_load", "session_search", "chat", "say", "create_goal", "confirm", "reject", "reply", "history", "status", "run_start", "run_subscribe", "run_cancel", "review_get", "review_accept", "approval_list", "approval_resolve", "approval_cancel", "snapshot_list", "snapshot_rewind", "question_list", "plan_mode", "plan_resolve":
		return true
	}
	return false
}

func decodeClient(r io.Reader) (ClientMsg, error) {
	var m ClientMsg
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("invalid message: %w", err)
	}
	if err := validateClient(m); err != nil {
		return m, err
	}
	return m, nil
}

func validateClient(m ClientMsg) error {
	if !validOp(m.Op) {
		return fmt.Errorf("unknown op %q", m.Op)
	}
	switch m.Op {
	case "session_list", "session_create":
		if m.ProjectRoot == "" {
			return fmt.Errorf("op %s requires project_root", m.Op)
		}
	case "session_load":
		if m.ProjectRoot == "" || m.SessionID == "" {
			return fmt.Errorf("op session_load requires project_root and session_id")
		}
	case "session_search":
		if m.ProjectRoot == "" || m.Text == "" {
			return fmt.Errorf("op session_search requires project_root and text")
		}
	case "snapshot_list", "question_list":
		if m.SessionID == "" {
			return fmt.Errorf("op %s requires session_id", m.Op)
		}
		if m.Op == "snapshot_list" && m.CandidateID == "" {
			return fmt.Errorf("op snapshot_list requires candidate_id")
		}
	case "snapshot_rewind":
		if m.SessionID == "" || m.CandidateID == "" || m.SnapshotID == "" || m.CandidateDigest == "" {
			return fmt.Errorf("op snapshot_rewind requires session, candidate, snapshot and the expected candidate digest")
		}
	case "chat":
		if m.Text == "" {
			return fmt.Errorf("op chat requires text")
		}
	case "say":
		if m.Goal == "" || m.Text == "" {
			return fmt.Errorf("op say requires goal and text")
		}
	case "reply":
		// The session-scoped reply answers one explicit pending question; the
		// legacy goal-scoped reply stays a queued workflow message.
		if m.QuestionID != "" {
			if m.SessionID == "" || m.Text == "" {
				return fmt.Errorf("op reply with question_id requires session_id and text")
			}
		} else if m.Goal == "" || m.Text == "" {
			return fmt.Errorf("op reply requires goal and text")
		}
	case "create_goal":
		if m.Text == "" {
			return fmt.Errorf("op create_goal requires text")
		}
	case "confirm", "reject":
		if m.ID == "" {
			return fmt.Errorf("op %s requires proposal ID", m.Op)
		}
	case "run_start":
		if m.SessionID == "" || m.Run == nil || m.Run.Work.SessionID != m.SessionID || m.Run.Intent == "" {
			return fmt.Errorf("op run_start requires matching session_id and run request")
		}
	case "run_subscribe":
		if m.SessionID == "" {
			return fmt.Errorf("op run_subscribe requires session_id")
		}
	case "run_cancel":
		if m.SessionID == "" || m.RunID == "" {
			return fmt.Errorf("op run_cancel requires session_id and run_id")
		}
	case "review_get":
		if m.CandidateID == "" || m.SessionID == "" {
			return fmt.Errorf("op review_get requires candidate_id and session_id")
		}
	case "review_accept":
		if m.CandidateID == "" || m.SessionID == "" || m.DecisionID == "" || m.PreviewDigest == "" || m.CandidateDigest == "" || m.FormalDigest == "" || m.AcceptanceMode == "" {
			return fmt.Errorf("op review_accept requires candidate, decision, preview, version and mode")
		}
		if m.AcceptanceMode != "normal" && m.AcceptanceMode != "force" {
			return fmt.Errorf("invalid acceptance mode")
		}
		seen := map[string]bool{}
		for _, id := range m.Confirmed {
			if id == "" || seen[id] {
				return fmt.Errorf("invalid confirmed finding ID")
			}
			seen[id] = true
		}
	case "approval_list":
		if m.SessionID == "" {
			return fmt.Errorf("op approval_list requires session_id")
		}
	case "approval_resolve":
		if m.SessionID == "" || m.ApprovalID == "" || m.ApprovalChoice == "" {
			return fmt.Errorf("op approval_resolve requires session_id, approval_id and choice")
		}
		if m.ApprovalChoice != "allow_once" && m.ApprovalChoice != "save_rule" && m.ApprovalChoice != "deny" {
			return fmt.Errorf("invalid approval choice")
		}
	case "approval_cancel":
		if m.SessionID == "" || m.ApprovalID == "" {
			return fmt.Errorf("op approval_cancel requires session_id and approval_id")
		}
	case "plan_mode":
		if m.SessionID == "" {
			return fmt.Errorf("op plan_mode requires session_id")
		}
	case "plan_resolve":
		if m.SessionID == "" {
			return fmt.Errorf("op plan_resolve requires session_id")
		}
		switch m.ApprovalChoice {
		case PlanResolveAuto, PlanResolveManual, PlanResolveCancel:
		case PlanResolveFeedback:
			if strings.TrimSpace(m.Text) == "" {
				return fmt.Errorf("op plan_resolve with feedback choice requires text")
			}
		default:
			return fmt.Errorf("invalid plan approval choice")
		}
	}
	return nil
}

func encodeServer(w io.Writer, m ServerMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	out := bufio.NewWriter(w)
	if _, err = out.Write(append(b, '\n')); err != nil {
		return err
	}
	return out.Flush()
}
