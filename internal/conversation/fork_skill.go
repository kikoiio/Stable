package conversation

import (
	"context"
	"encoding/json"
	"fmt"

	"stable/internal/agent"
	"stable/internal/llm"
)

type ForkContextMode string

const (
	ForkContextNone   ForkContextMode = "none"
	ForkContextRecent ForkContextMode = "recent"
	ForkContextFull   ForkContextMode = "full"
)

type ForkSkillInvocation struct {
	SessionID        string
	RunID            string
	ParentRunID      string
	SkillName        string
	SkillSource      string
	Entry            string
	Instruction      string
	ContextMode      ForkContextMode
	ContextMessages  []llm.Message
	Provider         llm.Provider
	ProviderName     string
	Model            string
	ProjectRoot      string
	PermissionBounds json.RawMessage
}

type ForkSkillResult struct {
	ChildRunID string                 `json:"child_run_id,omitempty"`
	SkillName  string                 `json:"skill_name"`
	Entry      string                 `json:"entry"`
	Status     agent.DelegationStatus `json:"status"`
	Summary    string                 `json:"summary,omitempty"`
	Error      string                 `json:"error,omitempty"`
}

type ForkContextSource interface {
	Build(ctx context.Context, sessionID, parentRunID string, mode ForkContextMode) ([]llm.Message, error)
}

func (s *Service) BuildForkContext(ctx context.Context, sessionID, parentRunID string, mode ForkContextMode) ([]llm.Message, error) {
	source := NewForkContextSource(s.deps.ProjectRoot, s.deps.ContextWindowTokens)
	s.eventMu.Lock()
	messages, err := source.Build(ctx, sessionID, parentRunID, mode)
	s.eventMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("build fork skill context: %w", err)
	}
	return messages, nil
}
