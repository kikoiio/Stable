package agent

import (
	"context"
	"time"
)

type AgentTaskRequest struct {
	AgentName   string        `json:"agent_name"`
	Instruction string        `json:"instruction"`
	Model       string        `json:"model,omitempty"`
	Background  bool          `json:"background,omitempty"`
	Timeout     time.Duration `json:"timeout,omitempty"`
}

type AgentTaskSnapshot struct {
	ID          string           `json:"id"`
	RunID       string           `json:"run_id"`
	OriginRunID string           `json:"origin_run_id,omitempty"`
	SessionID   string           `json:"session_id"`
	AgentName   string           `json:"agent_name"`
	Name        string           `json:"name"`
	Status      DelegationStatus `json:"status"`
	Stage       string           `json:"stage,omitempty"`
	Summary     string           `json:"summary,omitempty"`
	Error       string           `json:"error,omitempty"`
	Cursor      uint64           `json:"cursor"`
}

type AgentTaskService interface {
	Run(context.Context, ParentRun, AgentTaskRequest) (AgentTaskSnapshot, error)
	Output(context.Context, ParentRun, string, time.Duration) (AgentTaskSnapshot, error)
	Stop(context.Context, ParentRun, string) (AgentTaskSnapshot, error)
}
