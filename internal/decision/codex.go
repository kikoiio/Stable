package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"proactive-agent/internal/core"
)

var ErrUnavailable = errors.New("model decision unavailable")

type Codex struct {
	CLI        string
	SchemaPath string
	Workdir    string
	Timeout    time.Duration
	Attempts   int
}

func (c *Codex) Decide(ctx context.Context, in core.DecisionContext) (core.ProposedAction, error) {
	p, _, err := c.DecideDetailed(ctx, in)
	return p, err
}

func (c *Codex) DecideDetailed(ctx context.Context, in core.DecisionContext) (core.ProposedAction, string, error) {
	limit := c.Attempts
	if limit <= 0 {
		limit = 2
	}
	if limit > 3 {
		limit = 3
	}
	var last error
	for attempt := 0; attempt < limit; attempt++ {
		proposal, runID, err := c.once(ctx, in)
		if err == nil {
			return proposal, runID, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return core.ProposedAction{}, "", fmt.Errorf("%w after %d attempt(s): %v", ErrUnavailable, limit, last)
}

func (c *Codex) once(ctx context.Context, in core.DecisionContext) (core.ProposedAction, string, error) {
	var empty core.ProposedAction
	cli := c.CLI
	if cli == "" {
		cli = "codex"
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	schema, err := filepath.Abs(c.SchemaPath)
	if err != nil {
		return empty, "", err
	}
	if _, err = os.Stat(schema); err != nil {
		return empty, "", err
	}
	dir, err := os.MkdirTemp("", "proactive-decision-*")
	if err != nil {
		return empty, "", err
	}
	defer os.RemoveAll(dir)
	output := filepath.Join(dir, "proposal.json")
	workdir := c.Workdir
	if workdir == "" {
		workdir = "."
	}
	prompt, err := makePrompt(in)
	if err != nil {
		return empty, "", err
	}
	args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "--sandbox", "read-only", "--output-schema", schema, "-o", output, prompt}
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Dir = workdir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		return empty, "", fmt.Errorf("codex exec: %w: %s", err, tail(stderr.String(), 2000))
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return empty, "", err
	}
	if err = json.Unmarshal(data, &empty); err != nil {
		return core.ProposedAction{}, "", fmt.Errorf("invalid model JSON: %w", err)
	}
	if err = Validate(empty); err != nil {
		return core.ProposedAction{}, "", err
	}
	runID := fmt.Sprintf("codex-%d", time.Now().UTC().UnixNano())
	return empty, runID, nil
}

func makePrompt(in core.DecisionContext) (string, error) {
	compact := struct {
		Goal          core.Goal                   `json:"goal"`
		Agent         core.AgentInstance          `json:"agent"`
		Observation   core.Observation            `json:"observation"`
		ValidEvidence []core.Evidence             `json:"valid_evidence"`
		Capabilities  []core.CapabilityDescriptor `json:"capabilities"`
	}{in.Goal, in.Agent, in.Observation, in.ValidEvidence, in.Capabilities}
	data, err := json.Marshal(compact)
	if err != nil {
		return "", err
	}
	return "Choose exactly one next action for this local, single-goal agent. Return only a JSON object matching the schema. " +
		"Use current observed facts and evidence. If the KiCad computer session is absent or stale, choose open_computer. " +
		"If the supported sensor connection is missing and the computer is open, choose execute_capability with capability kicad.repair_connection. " +
		"If the connection is present, choose observe so the coordinator can run ERC. " +
		"For unsupported faults, choose ask_human. Never claim verification from a screenshot or old ERC. " +
		"Use the observation artifact ID as expected_artifact_id for actions that touch the design. " +
		"The target must be within the goal's allowed_root. Include a short concrete reason. Context: " + string(data), nil
}

func Validate(p core.ProposedAction) error {
	switch p.Kind {
	case "observe", "execute_capability", "open_computer", "wait", "ask_human":
	default:
		return fmt.Errorf("unknown action kind %q", p.Kind)
	}
	if strings.TrimSpace(p.Reason) == "" {
		return errors.New("model reason missing")
	}
	if len(p.Parameters) == 0 || !json.Valid(p.Parameters) {
		return errors.New("parameters missing or invalid")
	}
	if p.Kind == "execute_capability" || p.Kind == "open_computer" {
		if p.Target == "" || p.ExpectedArtifactID == "" {
			return errors.New("target and expected artifact required")
		}
	}
	if p.Kind == "execute_capability" && p.Capability == "" {
		return errors.New("capability missing")
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
