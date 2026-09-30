package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type GoalStatus string

const (
	GoalActive     GoalStatus = "active"
	GoalWaiting    GoalStatus = "waiting"
	GoalNeedsHuman GoalStatus = "needs_human"
	GoalVerified   GoalStatus = "verified"
)

type AgentStatus string
type ComputerStatus string

type Criterion struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

type Goal struct {
	ID                   string      `json:"id"`
	Objective            string      `json:"objective"`
	Criteria             []Criterion `json:"criteria"`
	AllowedRoot          string      `json:"allowed_root"`
	ArtifactPath         string      `json:"artifact_path"`
	CheckIntervalSeconds int         `json:"check_interval_seconds"`
	AllowedCapabilities  []string    `json:"allowed_capabilities"`
	Status               GoalStatus  `json:"status"`
	CurrentArtifactID    string      `json:"current_artifact_id"`
	CriteriaRevision     int         `json:"criteria_revision"`
	Revision             int64       `json:"revision"`
	Reason               string      `json:"reason"`
	CreatedAt            time.Time   `json:"created_at"`
}

type AgentInstance struct {
	ID             string      `json:"id"`
	GoalID         string      `json:"goal_id"`
	Status         AgentStatus `json:"status"`
	LastDecisionID string      `json:"last_decision_id"`
	NextWakeAt     *time.Time  `json:"next_wake_at,omitempty"`
}

type ComputerSession struct {
	ID                string         `json:"id"`
	GoalID            string         `json:"goal_id"`
	Status            ComputerStatus `json:"status"`
	Generation        int64          `json:"generation"`
	OpenedArtifactID  string         `json:"opened_artifact_id"`
	LastObservationID string         `json:"last_observation_id"`
	RuntimeHandle     string         `json:"runtime_handle"`
}

type ArtifactVersion struct {
	ID        string    `json:"id"`
	GoalID    string    `json:"goal_id"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
}

type Event struct {
	ID         string          `json:"id"`
	GoalID     string          `json:"goal_id"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	ReceivedAt time.Time       `json:"received_at"`
	Status     string          `json:"status"`
}

type Observation struct {
	ID                string          `json:"id"`
	GoalID            string          `json:"goal_id"`
	EventID           string          `json:"event_id"`
	ArtifactID        string          `json:"artifact_id"`
	ComputerSessionID string          `json:"computer_session_id"`
	Facts             json.RawMessage `json:"facts"`
	ObservedAt        time.Time       `json:"observed_at"`
}

type ProposedAction struct {
	Kind               string          `json:"kind"`
	Capability         string          `json:"capability"`
	Target             string          `json:"target"`
	Parameters         json.RawMessage `json:"parameters"`
	ExpectedArtifactID string          `json:"expected_artifact_id"`
	Reason             string          `json:"reason"`
}

type Decision struct {
	ID            string         `json:"id"`
	AgentID       string         `json:"agent_id"`
	ObservationID string         `json:"observation_id"`
	Proposal      ProposedAction `json:"proposal"`
	ModelRunID    string         `json:"model_run_id"`
	ModelCallID   string         `json:"model_call_id,omitempty"`
	ModelInfo     string         `json:"model_info,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

type ModelDescriptor struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Host     string `json:"host"`
}

type ModelCall struct {
	ID                string     `json:"id"`
	GoalID            string     `json:"goal_id"`
	ObservationID     string     `json:"observation_id"`
	Provider          string     `json:"provider"`
	Model             string     `json:"model"`
	Host              string     `json:"host"`
	ProviderRequestID string     `json:"provider_request_id,omitempty"`
	Status            string     `json:"status"`
	ErrorKind         string     `json:"error_kind,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
}

type ActionRecord struct {
	ID                   string          `json:"id"`
	DecisionID           string          `json:"decision_id"`
	ExpectedArtifactID   string          `json:"expected_artifact_id"`
	DesiredPostcondition json.RawMessage `json:"desired_postcondition"`
	Status               string          `json:"status"`
	ResultArtifactID     string          `json:"result_artifact_id"`
	Reason               string          `json:"reason"`
}

type Evidence struct {
	ID          string    `json:"id"`
	GoalID      string    `json:"goal_id"`
	CriterionID string    `json:"criterion_id"`
	ArtifactID  string    `json:"artifact_id"`
	Kind        string    `json:"kind"`
	Result      string    `json:"result"`
	ReportPath  string    `json:"report_path"`
	CreatedAt   time.Time `json:"created_at"`
}

type GoalSnapshot struct {
	Goal         Goal            `json:"goal"`
	Agent        AgentInstance   `json:"agent"`
	Session      ComputerSession `json:"session"`
	Events       []Event         `json:"events"`
	Observations []Observation   `json:"observations"`
	Decisions    []Decision      `json:"decisions"`
	ModelCalls   []ModelCall     `json:"model_calls"`
	Actions      []ActionRecord  `json:"actions"`
	Evidence     []Evidence      `json:"evidence"`
}

type CapabilityDescriptor struct {
	Name              string          `json:"name"`
	ParameterSchema   json.RawMessage `json:"parameter_schema"`
	AllowedTargetRoot string          `json:"allowed_target_root"`
	PostconditionKind string          `json:"postcondition_kind"`
}

type CapabilityRequest struct {
	ProtocolVersion    int             `json:"protocol_version"`
	OperationID        string          `json:"operation_id"`
	Kind               string          `json:"kind"`
	GoalID             string          `json:"goal_id"`
	ExpectedArtifactID string          `json:"expected_artifact_id"`
	Payload            json.RawMessage `json:"payload"`
}

type CapabilityResult struct {
	ProtocolVersion  int             `json:"protocol_version"`
	OperationID      string          `json:"operation_id"`
	Status           string          `json:"status"`
	ActualArtifactID string          `json:"actual_artifact_id"`
	EvidencePaths    []string        `json:"evidence_paths"`
	Postcondition    json.RawMessage `json:"postcondition"`
	ErrorCode        string          `json:"error_code"`
}

type ComputerObservation struct {
	SessionID        string         `json:"session_id"`
	Generation       int64          `json:"generation"`
	Status           ComputerStatus `json:"status"`
	OpenedArtifactID string         `json:"opened_artifact_id"`
	WindowIdentity   string         `json:"window_identity"`
	ScreenshotPath   string         `json:"screenshot_path"`
	RuntimeHandle    string         `json:"runtime_handle"`
	ObservedAt       time.Time      `json:"observed_at"`
}

type DecisionContext struct {
	Goal          Goal                   `json:"goal"`
	Agent         AgentInstance          `json:"agent"`
	Observation   Observation            `json:"observation"`
	ValidEvidence []Evidence             `json:"valid_evidence"`
	Capabilities  []CapabilityDescriptor `json:"capabilities"`
	Conversation  []SessionMessage       `json:"conversation,omitempty"`
}

const (
	MessageRoleUser   = "user"
	MessageRoleAgent  = "agent"
	MessageRoleSystem = "system"
)

const (
	MessageKindText             = "text"
	MessageKindQuestion         = "question"
	MessageKindReply            = "reply"
	MessageKindCriteriaProposal = "criteria_proposal"
	MessageKindCriteriaConfirm  = "criteria_confirm"
)

type SessionMessage struct {
	ID        string          `json:"id"`
	GoalID    string          `json:"goal_id,omitempty"`
	Role      string          `json:"role"`
	Kind      string          `json:"kind"`
	Text      string          `json:"text"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	// Delivered marks whether a decision round has consumed the message.
	Delivered bool `json:"delivered"`
}

const (
	ProposalPending    = "proposed"
	ProposalConfirmed  = "confirmed"
	ProposalRejected   = "rejected"
	ProposalSuperseded = "superseded"
)

type CriteriaProposal struct {
	ID        string      `json:"id"`
	GoalID    string      `json:"goal_id,omitempty"`
	Status    string      `json:"status"`
	Criteria  []Criterion `json:"criteria"`
	RawText   string      `json:"raw_text"`
	CreatedAt time.Time   `json:"created_at"`
}

// Acceptance-criteria vocabulary. The kernel verifies exactly these kinds;
// anything outside must be rejected before it reaches a goal.
const (
	CriterionKindERCClean          = "kicad.erc_clean"
	CriterionKindConnectionPresent = "sensor.connection_present"
)

// Conversation-driven event kinds carried by the regular event pipeline.
const (
	EventKindUserMessage    = "user_message"
	EventKindHumanReply     = "human_reply"
	EventKindCriteriaUpdate = "criteria_updated"
)

// The only connection endpoints the bundled sensor fixture can verify.
const (
	SensorEndpointA = "RT1.2"
	SensorEndpointB = "J1.2"
)

func ValidateCriterion(c Criterion) error {
	switch c.Kind {
	case CriterionKindERCClean:
		var payload struct {
			MaxViolations int `json:"max_violations"`
		}
		if err := strictUnmarshal(c.Payload, &payload); err != nil {
			return fmt.Errorf("criterion %s: %w", c.ID, err)
		}
		if payload.MaxViolations < 0 {
			return fmt.Errorf("criterion %s: max_violations must be >= 0", c.ID)
		}
	case CriterionKindConnectionPresent:
		var payload struct {
			EndpointA string `json:"endpoint_a"`
			EndpointB string `json:"endpoint_b"`
		}
		if err := strictUnmarshal(c.Payload, &payload); err != nil {
			return fmt.Errorf("criterion %s: %w", c.ID, err)
		}
		if payload.EndpointA != SensorEndpointA || payload.EndpointB != SensorEndpointB {
			return fmt.Errorf("criterion %s: unsupported endpoints %q-%q", c.ID, payload.EndpointA, payload.EndpointB)
		}
	default:
		return fmt.Errorf("criterion %s: unsupported kind %q", c.ID, c.Kind)
	}
	return nil
}

func ValidateCriteria(criteria []Criterion) error {
	seen := map[string]bool{}
	for _, c := range criteria {
		if c.ID == "" {
			return fmt.Errorf("criterion ID required")
		}
		if seen[c.ID] {
			return fmt.Errorf("criterion %s: duplicate ID", c.ID)
		}
		seen[c.ID] = true
		if err := ValidateCriterion(c); err != nil {
			return err
		}
	}
	return nil
}

func strictUnmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return fmt.Errorf("payload required")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after JSON value")
	}
	return nil
}

type DecisionMaker interface {
	Decide(context.Context, DecisionContext) (ProposedAction, error)
}

type ModelDecisionOutput struct {
	Proposal          ProposedAction
	ProviderRequestID string
}
type AuditedDecisionMaker interface {
	DecisionMaker
	Descriptor() ModelDescriptor
	DecideModel(context.Context, DecisionContext) (ModelDecisionOutput, error)
}
type ModelCallStore interface {
	StartModelCall(context.Context, ModelCall) error
	FinishModelCall(context.Context, string, string, string, string) error
}

type ActionPolicy interface {
	Check(Goal, Observation, ProposedAction) error
}

type Capability interface {
	Observe(context.Context, CapabilityRequest) (CapabilityResult, error)
	Execute(context.Context, CapabilityRequest) (CapabilityResult, error)
	Verify(context.Context, CapabilityRequest) (CapabilityResult, error)
}

type ComputerProvider interface {
	EnsureOpen(context.Context, ComputerSession, ArtifactVersion) (ComputerObservation, error)
	Observe(context.Context, ComputerSession) (ComputerObservation, error)
	Recover(context.Context, ComputerSession, ArtifactVersion) (ComputerObservation, error)
}

type ArtifactStore interface {
	Snapshot(context.Context, string, string) (ArtifactVersion, error)
	Digest(context.Context, string) (string, error)
}

type StateStore interface {
	CreateGoal(context.Context, Goal) (GoalSnapshot, error)
	GetGoalSnapshot(context.Context, string) (GoalSnapshot, error)
	UpdateStatus(context.Context, string, int64, GoalStatus, string) (Goal, error)
	InsertEventIfAbsent(context.Context, Event) (Event, bool, error)
	PendingEvents(context.Context) ([]Event, error)
	SetEventStatus(context.Context, string, string) error
	RecordObservation(context.Context, Observation) error
	RecordDecision(context.Context, Decision) error
	ReserveAction(context.Context, ActionRecord) (ActionRecord, error)
	SetActionResult(context.Context, string, string, string, string) error
	RecordEvidence(context.Context, Evidence) error
	UpsertSession(context.Context, ComputerSession) error
	GoalIDForDecision(context.Context, string) (string, error)
	SetCurrentArtifact(context.Context, string, string) error
	InvalidateEvidence(context.Context, string, string) error
}
