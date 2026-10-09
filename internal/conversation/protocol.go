package conversation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/mcp"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
	"stable/internal/workspace"
)

// ClientMsg is one line of JSON sent from a chat client to the session service.
type ClientMsg struct {
	Op                   string                  `json:"op"`             // existing session operations plus run_start | run_subscribe | run_cancel
	Goal                 string                  `json:"goal,omitempty"` // focused goal (required for say/reply)
	Text                 string                  `json:"text,omitempty"` // natural-language content
	ID                   string                  `json:"id,omitempty"`   // proposal ID for confirm/reject
	ProjectRoot          string                  `json:"project_root,omitempty"`
	SessionID            string                  `json:"session_id,omitempty"`
	RunID                string                  `json:"run_id,omitempty"`
	AfterSeq             uint64                  `json:"after_seq,omitempty"`
	Run                  *agent.ExecutionRequest `json:"run,omitempty"`
	CandidateID          string                  `json:"candidate_id,omitempty"`
	DecisionID           string                  `json:"decision_id,omitempty"`
	PreviewDigest        string                  `json:"preview_digest,omitempty"`
	CandidateDigest      string                  `json:"candidate_digest,omitempty"`
	FormalDigest         string                  `json:"formal_digest,omitempty"`
	AcceptanceMode       string                  `json:"acceptance_mode,omitempty"`
	Confirmed            []string                `json:"confirmed_findings,omitempty"`
	ApprovalID           string                  `json:"approval_id,omitempty"`
	ApprovalChoice       string                  `json:"approval_choice,omitempty"`
	SnapshotID           string                  `json:"snapshot_id,omitempty"`
	QuestionID           string                  `json:"question_id,omitempty"`
	SkillName            string                  `json:"skill_name,omitempty"`
	SkillArgs            string                  `json:"skill_args,omitempty"`
	AgentName            string                  `json:"agent_name,omitempty"`
	TaskID               string                  `json:"task_id,omitempty"`
	TaskTitle            *string                 `json:"task_title,omitempty"`
	TaskDescription      *string                 `json:"task_description,omitempty"`
	TaskStatus           *string                 `json:"task_status,omitempty"`
	TaskAssignee         *string                 `json:"task_assignee,omitempty"`
	TaskBlockedBy        *[]string               `json:"task_blocked_by,omitempty"`
	ExpectedRevision     uint64                  `json:"expected_revision,omitempty"`
	TeamID               string                  `json:"team_id,omitempty"`
	TeamName             string                  `json:"team_name,omitempty"`
	TeamRecipient        string                  `json:"team_recipient,omitempty"`
	TeamToken            string                  `json:"team_token,omitempty"`
	TeamBroadcast        bool                    `json:"team_broadcast,omitempty"`
	TeamRequestID        string                  `json:"team_request_id,omitempty"`
	TeamDecision         string                  `json:"team_decision,omitempty"`
	TeamFeedback         string                  `json:"team_feedback,omitempty"`
	TeamMemberID         string                  `json:"team_member_id,omitempty"`
	TeamMemberName       string                  `json:"team_member_name,omitempty"`
	TeamPlanRequired     bool                    `json:"team_plan_required,omitempty"`
	TeamAcceptRoleChange bool                    `json:"team_accept_role_change,omitempty"`
	WorktreeGeneration   uint64                  `json:"worktree_generation,omitempty"`
	WorktreePreviewID    string                  `json:"worktree_preview_id,omitempty"`
	ConflictChoices      map[string]string       `json:"conflict_choices,omitempty"`
	ConflictAfter        string                  `json:"conflict_after,omitempty"`
	Isolation            string                  `json:"isolation,omitempty"`
	WorkKind             string                  `json:"work_kind,omitempty"`
	GoalID               string                  `json:"goal_id,omitempty"`
	WorkItemID           string                  `json:"work_item_id,omitempty"`
	CoordinatorOn        bool                    `json:"coordinator_on,omitempty"`
	Background           bool                    `json:"background,omitempty"`
	WaitMS               int                     `json:"wait_ms,omitempty"`
	TimeoutMS            int                     `json:"timeout_ms,omitempty"`
	Model                string                  `json:"model,omitempty"`
	Limit                int                     `json:"limit,omitempty"`
}

// ServerMsg is one line of JSON pushed from the session service to clients.
type ServerMsg struct {
	Agents     *agentcatalog.Snapshot         `json:"agents,omitempty"`
	AgentTask  *agent.AgentTaskSnapshot       `json:"agent_task,omitempty"`
	AgentTasks []agent.AgentTaskSnapshot      `json:"agent_tasks,omitempty"`
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
	// SkillReport carries skill activation errors, /skills reload counts and
	// the one-shot skill-delta notice pushed by skill_invoke / skill_reload /
	// skill_delta.
	SkillReport *SkillReport `json:"skill_report,omitempty"`
	// Skills and SkillActivated carry the skill_list response: the current
	// catalog infos and the session's activated skill names.
	Skills         []sessionlog.SkillInfo `json:"skills,omitempty"`
	SkillActivated []string               `json:"skill_activated,omitempty"`
	Teams          []teams.Team           `json:"teams,omitempty"`
	Team           *teams.Team            `json:"team,omitempty"`
	TeamTasks      []teams.Task           `json:"team_tasks,omitempty"`
	TeamTask       *teams.Task            `json:"team_task,omitempty"`
	TeamMessages   []teams.Message        `json:"team_messages,omitempty"`
	TeamMessage    *teams.Message         `json:"team_message,omitempty"`
	TeamRequests   []teams.Request        `json:"team_requests,omitempty"`
	TeamRequest    *teams.Request         `json:"team_request,omitempty"`
	Worktrees      []workspace.Snapshot   `json:"worktrees,omitempty"`
	Worktree       *workspace.Snapshot    `json:"worktree,omitempty"`
	TeamMember     *teams.Member          `json:"team_member,omitempty"`
	CoordinatorOn  bool                   `json:"coordinator_on,omitempty"`
	HookList       *HookListMsg           `json:"hook_list,omitempty"`
	HookReport     *HookReportMsg         `json:"hook_report,omitempty"`
	MCPList        *MCPListMsg            `json:"mcp_list,omitempty"`
	MCPReport      *MCPReportMsg          `json:"mcp_report,omitempty"`
}

// HookSummary is one loaded hook in the merged view.
type HookSummary struct {
	ID     string `json:"id"`
	Event  string `json:"event"`
	Action string `json:"action"`
	Source string `json:"source"`
	Reject bool   `json:"reject,omitempty"`
	Once   bool   `json:"once,omitempty"`
	Async  bool   `json:"async,omitempty"`
}

type HookListMsg struct {
	Hooks      []HookSummary `json:"hooks"`
	Rejections []string      `json:"rejections,omitempty"`
}

type HookReportMsg struct {
	Before int `json:"before"`
	After  int `json:"after"`
}

type MCPListMsg struct {
	Servers    []mcp.ServerStatus `json:"servers"`
	Rejections []string           `json:"rejections,omitempty"`
}

type MCPReportMsg struct {
	Before     int      `json:"before"`
	After      int      `json:"after"`
	Rejections []string `json:"rejections,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// SkillReport kinds for the ServerMsg SkillReport payload.
const (
	SkillReportError  = "error"
	SkillReportReload = "reload"
	SkillReportDelta  = "delta"
)

// SkillReport is the M07-A skill feedback message: activation errors, reload
// count changes and one-shot delta notices for newly added skills.
type SkillReport struct {
	Kind      string   `json:"kind"` // error | reload | delta
	SessionID string   `json:"session_id,omitempty"`
	Name      string   `json:"name,omitempty"`
	Error     string   `json:"error,omitempty"`
	Before    int      `json:"before,omitempty"`
	After     int      `json:"after,omitempty"`
	Added     []string `json:"added,omitempty"`
}

func validOp(op string) bool {
	switch op {
	case "agent_list", "agent_reload", "agent_task_start", "agent_task_list", "agent_task_get", "agent_task_cancel", "session_list", "session_create", "session_load", "session_search", "chat", "say", "create_goal", "confirm", "reject", "reply", "history", "status", "run_start", "run_subscribe", "run_cancel", "review_get", "review_accept", "approval_list", "approval_resolve", "approval_cancel", "snapshot_list", "snapshot_rewind", "question_list", "plan_mode", "plan_resolve", "skill_invoke", "skill_reload", "skill_list", "hooks_list", "hooks_reload", "mcp_list", "mcp_reload", "team_create", "team_list", "team_get", "team_close", "team_coordinator", "team_member_spawn", "team_member_resume", "team_member_stop", "team_send", "team_messages", "team_request_list", "team_request_respond", "team_shutdown_request", "team_task_create", "team_task_get", "team_task_list", "team_task_update", "worktree_create", "worktree_list", "worktree_get", "worktree_enter", "worktree_exit", "worktree_keep", "worktree_export", "worktree_remove", "worktree_preview", "worktree_resolve", "worktree_discard_preview", "worktree_discard":
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
	case "agent_list", "agent_reload", "agent_task_start", "agent_task_list", "agent_task_get", "agent_task_cancel":
		if m.Isolation != "" && (m.Op != "agent_task_start" || (m.Isolation != "none" && m.Isolation != "worktree")) {
			return fmt.Errorf("isolation is only supported for agent_task_start with none or worktree")
		}
		if m.SessionID == "" {
			return fmt.Errorf("op %s requires session_id", m.Op)
		}
		if m.Op == "agent_task_start" && (m.AgentName == "" || strings.TrimSpace(m.Text) == "") {
			return fmt.Errorf("agent_task_start requires agent_name and text")
		}
		if (m.Op == "agent_task_get" || m.Op == "agent_task_cancel") && m.TaskID == "" {
			return fmt.Errorf("op %s requires task_id", m.Op)
		}
		if m.WaitMS < 0 || m.WaitMS > 30000 || m.TimeoutMS < 0 || m.TimeoutMS > 180000 || m.Limit < 0 || m.Limit > 100 {
			return fmt.Errorf("agent task bounds are invalid")
		}
		if m.Run != nil || m.RunID != "" || m.ProjectRoot != "" {
			return fmt.Errorf("agent operations use server-owned execution scope")
		}
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
	case "team_coordinator":
		if sessionlog.ValidateID(m.SessionID) != nil || m.Run != nil || m.RunID != "" || m.ProjectRoot != "" {
			return fmt.Errorf("team_coordinator requires only a valid session scope")
		}
	case "worktree_create", "worktree_list", "worktree_get", "worktree_enter", "worktree_exit", "worktree_keep", "worktree_export", "worktree_remove", "worktree_preview", "worktree_resolve", "worktree_discard_preview", "worktree_discard":
		if sessionlog.ValidateID(m.SessionID) != nil || m.Run != nil || m.ProjectRoot != "" || m.RunID != "" && sessionlog.ValidateID(m.RunID) != nil {
			return fmt.Errorf("op %s requires a valid session scope and server-owned project root", m.Op)
		}
		if m.WorkKind == "" {
			m.WorkKind = "session"
		}
		if m.WorkKind != "session" && m.WorkKind != "goal" || m.WorkKind == "session" && (m.GoalID != "" || m.WorkItemID != "") || m.WorkKind == "goal" && (!validComponent(m.GoalID) || !validComponent(m.WorkItemID)) {
			return fmt.Errorf("invalid worktree work scope")
		}
		if m.Op == "worktree_create" {
			if m.RunID == "" || strings.TrimSpace(m.Text) == "" || m.ID != "" {
				return fmt.Errorf("worktree_create requires active run_id and label in text")
			}
		} else if m.Op == "worktree_list" || m.Op == "worktree_exit" {
			if m.ID != "" || m.Text != "" || m.Limit < 0 || m.Limit > 100 {
				return fmt.Errorf("invalid worktree_list request")
			}
		} else if sessionlog.ValidateID(m.ID) != nil || m.Text != "" {
			return fmt.Errorf("op %s requires worktree id", m.Op)
		}
		if m.Op == "worktree_resolve" {
			if sessionlog.ValidateID(m.WorktreePreviewID) != nil || m.WorktreeGeneration == 0 || len(m.ConflictChoices) == 0 || len(m.ConflictChoices) > 100 {
				return fmt.Errorf("worktree_resolve requires a current preview, generation and bounded per-path choices")
			}
			bytes := 0
			for path, choice := range m.ConflictChoices {
				clean, err := workspace.CleanRelative(path)
				bytes += len(path)
				if err != nil || clean != path || workspace.ProtectedRoot(path) || len(path) > 4096 || bytes > 64<<10 || (choice != workspace.UseFormal && choice != workspace.UseWorkspace) {
					return fmt.Errorf("invalid worktree conflict choice")
				}
			}
		} else if len(m.ConflictChoices) != 0 || m.WorktreePreviewID != "" {
			return fmt.Errorf("conflict decisions are only accepted by worktree_resolve")
		}
		if m.Op == "worktree_discard" {
			if sessionlog.ValidateID(m.DecisionID) != nil || len(m.PreviewDigest) != 64 || m.WorktreeGeneration == 0 {
				return fmt.Errorf("worktree_discard requires a service-issued decision, digest and generation")
			}
		}
		if m.ConflictAfter != "" && m.Op != "worktree_preview" {
			return fmt.Errorf("conflict cursor is only accepted by worktree_preview")
		}
	case "team_create", "team_list", "team_get", "team_close":
		if m.SessionID == "" {
			return fmt.Errorf("op %s requires session_id", m.Op)
		}
		if m.Run != nil || m.ProjectRoot != "" {
			return fmt.Errorf("team operations use the persisted run scope and server-bound project root")
		}
		if m.Op == "team_list" && (m.Limit < 0 || m.Limit > teams.MaxPageSize) {
			return fmt.Errorf("team_list page size exceeds the maximum")
		}
		if m.Op != "team_list" && m.Limit != 0 {
			return fmt.Errorf("limit is only accepted by team_list")
		}
		if m.Op == "team_create" && (m.TeamName == "" || m.RunID == "") {
			return fmt.Errorf("op team_create requires team_name and active lead run_id")
		}
		if (m.Op == "team_get" || m.Op == "team_close") && m.TeamID == "" {
			return fmt.Errorf("op %s requires team_id", m.Op)
		}
	case "team_member_spawn", "team_member_resume":
		if sessionlog.ValidateID(m.SessionID) != nil || sessionlog.ValidateID(m.RunID) != nil || teams.ValidateID(m.TeamID) != nil || m.Run != nil || m.ProjectRoot != "" {
			return fmt.Errorf("op %s requires active lead run and team scope", m.Op)
		}
		if m.Op == "team_member_spawn" {
			if m.TeamAcceptRoleChange {
				return fmt.Errorf("team role change acceptance is only valid when resuming a member")
			}
			_, memberNameErr := teams.NormalizeMemberName(m.TeamMemberName)
			_, agentNameErr := teams.NormalizeName(m.AgentName)
			if teams.ValidateText(m.TeamMemberName, teams.MaxNameBytes, true) != nil || memberNameErr != nil || teams.ValidateText(m.AgentName, teams.MaxNameBytes, true) != nil || agentNameErr != nil || teams.ValidateText(m.Text, teams.MaxInputBytes, true) != nil || m.TeamMemberID != "" {
				return fmt.Errorf("team_member_spawn requires member name, role and bounded instruction")
			}
		} else if teams.ValidateID(m.TeamMemberID) != nil || m.TeamMemberName != "" || m.AgentName != "" || m.Text != "" || m.TeamPlanRequired {
			return fmt.Errorf("team_member_resume requires only member_id")
		}
	case "team_member_stop":
		if sessionlog.ValidateID(m.SessionID) != nil || teams.ValidateID(m.TeamID) != nil || teams.ValidateID(m.TeamMemberID) != nil || m.RunID != "" || m.Run != nil || m.ProjectRoot != "" {
			return fmt.Errorf("team_member_stop requires session, team and member scope")
		}
	case "team_task_create", "team_task_get", "team_task_list", "team_task_update":
		if sessionlog.ValidateID(m.SessionID) != nil || (m.RunID != "" && sessionlog.ValidateID(m.RunID) != nil) || teams.ValidateID(m.TeamID) != nil || m.Run != nil || m.ProjectRoot != "" {
			return fmt.Errorf("op %s requires session and team scope", m.Op)
		}
		switch m.Op {
		case "team_task_create":
			if m.TaskTitle == nil || teams.ValidateText(*m.TaskTitle, teams.MaxTaskTitleBytes, true) != nil || (m.TaskDescription != nil && teams.ValidateText(*m.TaskDescription, teams.MaxTaskDescriptionBytes, false) != nil) || m.TaskStatus != nil || m.TaskID != "" || m.ExpectedRevision != 0 || m.Limit != 0 {
				return fmt.Errorf("team_task_create requires a bounded title and valid task data")
			}
			if m.TaskAssignee != nil && *m.TaskAssignee != "" && teams.ValidateID(*m.TaskAssignee) != nil {
				return fmt.Errorf("invalid task assignee")
			}
			if m.TaskBlockedBy != nil {
				if len(*m.TaskBlockedBy) > teams.MaxTaskDependencies {
					return fmt.Errorf("too many task dependencies")
				}
				for _, id := range *m.TaskBlockedBy {
					if teams.ValidateID(id) != nil {
						return fmt.Errorf("invalid task dependency ID")
					}
				}
			}
		case "team_task_get":
			if teams.ValidateID(m.TaskID) != nil || m.TaskTitle != nil || m.TaskDescription != nil || m.TaskStatus != nil || m.TaskAssignee != nil || m.TaskBlockedBy != nil || m.ExpectedRevision != 0 || m.Limit != 0 {
				return fmt.Errorf("team_task_get requires only task_id")
			}
		case "team_task_list":
			if m.TaskID != "" || m.TaskTitle != nil || m.TaskDescription != nil || m.TaskStatus != nil || m.TaskAssignee != nil || m.TaskBlockedBy != nil || m.ExpectedRevision != 0 || m.Limit < 0 || m.Limit > teams.MaxPageSize {
				return fmt.Errorf("team_task_list accepts no task fields")
			}
		case "team_task_update":
			if teams.ValidateID(m.TaskID) != nil || m.ExpectedRevision == 0 || (m.TaskTitle == nil && m.TaskDescription == nil && m.TaskStatus == nil && m.TaskAssignee == nil && m.TaskBlockedBy == nil) || m.Limit != 0 {
				return fmt.Errorf("team_task_update requires task_id, expected_revision and a patch")
			}
			if (m.TaskTitle != nil && teams.ValidateText(*m.TaskTitle, teams.MaxTaskTitleBytes, true) != nil) || (m.TaskDescription != nil && teams.ValidateText(*m.TaskDescription, teams.MaxTaskDescriptionBytes, false) != nil) {
				return fmt.Errorf("team task patch text is invalid")
			}
			if m.TaskStatus != nil && *m.TaskStatus != string(teams.TaskPending) && *m.TaskStatus != string(teams.TaskInProgress) && *m.TaskStatus != string(teams.TaskCompleted) {
				return fmt.Errorf("invalid team task status")
			}
			if m.TaskAssignee != nil && *m.TaskAssignee != "" && teams.ValidateID(*m.TaskAssignee) != nil {
				return fmt.Errorf("invalid task assignee")
			}
			if m.TaskBlockedBy != nil {
				if len(*m.TaskBlockedBy) > teams.MaxTaskDependencies {
					return fmt.Errorf("too many task dependencies")
				}
				for _, id := range *m.TaskBlockedBy {
					if teams.ValidateID(id) != nil {
						return fmt.Errorf("invalid task dependency ID")
					}
				}
			}
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
	case "skill_invoke":
		if m.SessionID == "" || strings.TrimSpace(m.SkillName) == "" {
			return fmt.Errorf("op skill_invoke requires session_id and skill_name")
		}
	case "skill_list":
		if m.SessionID == "" {
			return fmt.Errorf("op skill_list requires session_id")
		}
	case "skill_reload":
		// The reload rescans the catalog service-wide; no fields required.
	case "mcp_list", "mcp_reload":
		if m.SessionID == "" {
			return fmt.Errorf("op %s requires session_id", m.Op)
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
	case "team_send":
		if sessionlog.ValidateID(m.SessionID) != nil || (m.RunID != "" && sessionlog.ValidateID(m.RunID) != nil) || teams.ValidateID(m.TeamID) != nil || m.TeamToken == "" || teams.ValidateText(m.Text, teams.MaxMessageBytes, true) != nil || m.Run != nil || m.ProjectRoot != "" {
			return fmt.Errorf("op team_send requires session, team, token and bounded message text")
		}
		if m.TeamBroadcast && m.TeamRecipient != "" {
			return fmt.Errorf("op team_send broadcast cannot specify a recipient")
		}
		if !m.TeamBroadcast && m.TeamRecipient == "" {
			return fmt.Errorf("op team_send requires a recipient unless broadcasting")
		}
		if teams.ValidateText(m.TeamToken, 256, true) != nil {
			return fmt.Errorf("op team_send token is invalid")
		}
	case "team_messages":
		if sessionlog.ValidateID(m.SessionID) != nil || (m.RunID != "" && sessionlog.ValidateID(m.RunID) != nil) || teams.ValidateID(m.TeamID) != nil || m.Limit < 0 || m.Limit > teams.MaxPageSize || m.Run != nil || m.ProjectRoot != "" {
			return fmt.Errorf("op team_messages requires session, team and bounded page size")
		}
	case "team_request_list", "team_request_respond", "team_shutdown_request":
		if sessionlog.ValidateID(m.SessionID) != nil || teams.ValidateID(m.TeamID) != nil || m.Run != nil || m.ProjectRoot != "" {
			return fmt.Errorf("op %s requires session and team scope", m.Op)
		}
		switch m.Op {
		case "team_request_respond":
			if teams.ValidateID(m.TeamRequestID) != nil || m.ExpectedRevision == 0 || (m.TeamDecision != string(teams.RequestApproved) && m.TeamDecision != string(teams.RequestRejected) && m.TeamDecision != string(teams.RequestDeferred)) || teams.ValidateText(m.TeamFeedback, teams.MaxFeedbackBytes, false) != nil {
				return fmt.Errorf("team_request_respond requires a current request revision and bounded response")
			}
		case "team_shutdown_request":
			if teams.ValidateID(m.TeamMemberID) != nil {
				return fmt.Errorf("team_shutdown_request requires member_id")
			}
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
