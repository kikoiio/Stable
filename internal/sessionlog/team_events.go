package sessionlog

import (
	"time"

	"stable/internal/teams"
)

const EventTeam = "team_event"

const (
	TeamCreated          = "team_created"
	TeamClosing          = "team_closing"
	TeamClosed           = "team_closed"
	TeamMemberAdded      = "member_added"
	TeamMemberState      = "member_state"
	TeamTurnIntent       = "member_turn_intent"
	TeamTurnAccepted     = "member_turn_accepted"
	TeamTurnAborted      = "member_turn_aborted"
	TeamTurnTerminal     = "member_turn_terminal"
	TeamMessageSent      = "message_sent"
	TeamMessageHandoff   = "message_handoff"
	TeamTaskCreated      = "task_created"
	TeamTaskUpdated      = "task_updated"
	TeamRequestCreated   = "request_created"
	TeamRequestResponded = "request_responded"
	TeamRequestExpired   = "request_expired"
	TeamLeadHandoff      = "lead_handoff"
)

// TeamEvent is a closed union. Exactly one payload is selected by Kind. Scope,
// sender and actor are derived by the service; role bodies never enter a fact.
// Revision is the next team revision; changed objects also carry their own CAS
// revision. Token/ArgsDigest identify a service-validated mutation request.
type TeamEvent struct {
	ID          string         `json:"id"`
	TeamID      string         `json:"team_id"`
	SessionID   string         `json:"session_id"`
	Kind        string         `json:"kind"`
	Revision    uint64         `json:"revision"`
	ActorID     string         `json:"actor_id"`
	ActorRunID  string         `json:"actor_run_id,omitempty"`
	OperationID string         `json:"operation_id,omitempty"`
	Token       string         `json:"token,omitempty"`
	ArgsDigest  string         `json:"args_digest,omitempty"`
	Team        *teams.Team    `json:"team,omitempty"`
	Member      *teams.Member  `json:"member,omitempty"`
	Message     *teams.Message `json:"message,omitempty"`
	Task        *teams.Task    `json:"task,omitempty"`
	Request     *teams.Request `json:"request,omitempty"`
	Turn        *TurnFact      `json:"turn,omitempty"`
	Handoff     *HandoffFact   `json:"handoff,omitempty"`
}

type TurnFact struct {
	ID                  string        `json:"id"`
	MemberID            string        `json:"member_id"`
	RunID               string        `json:"run_id"`
	TaskID              string        `json:"task_id"`
	WorkspaceID         string        `json:"workspace_id,omitempty"`
	WorkspaceGeneration uint64        `json:"workspace_generation,omitempty"`
	OriginRunID         string        `json:"origin_run_id,omitempty"`
	OriginCallID        string        `json:"origin_call_id,omitempty"`
	MessageIDs          []string      `json:"message_ids,omitempty"`
	Status              string        `json:"status"`
	Elapsed             time.Duration `json:"elapsed,omitempty"`
	Summary             string        `json:"summary,omitempty"`
	Error               string        `json:"error,omitempty"`
}

type HandoffFact struct {
	MessageID         string `json:"message_id,omitempty"`
	RecipientID       string `json:"recipient_id"`
	DestinationRunID  string `json:"destination_run_id"`
	DestinationTurnID string `json:"destination_turn_id,omitempty"`
	RetryOfTurnID     string `json:"retry_of_turn_id,omitempty"`
	TerminalSeq       uint64 `json:"terminal_seq,omitempty"`
}

// TeamProjection folds raw durable facts regardless of transcript compaction.
// IDs are opaque and map keys are IDs, never user display names.
type TeamProjection struct {
	Teams           map[string]teams.Team
	Members         map[string]teams.Member
	Turns           map[string]TurnFact
	Messages        map[string]teams.Message
	Tasks           map[string]teams.Task
	Requests        map[string]teams.Request
	Handoffs        []HandoffFact
	LastSeq         map[string]uint64
	TurnTerminalSeq map[string]uint64
}
