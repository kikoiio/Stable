package goalrun

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"stable/internal/artifact"
	"stable/internal/core"
	"stable/internal/store"
)

// Capabilities every goal is allowed to use; the policy layer still gates
// each proposal against this list.
var DefaultCapabilities = []string{"computer.ensure_open", "kicad.repair_connection", "kicad.run_erc", "inspect_design"}

type Spec struct {
	ID                   string
	Objective            string
	Criteria             []core.Criterion
	CheckIntervalSeconds int
}

// Definition is the non-interactive goal definition file consumed by
// `agentctl create --from`; all fields are explicit.
type Definition struct {
	Objective            string           `json:"objective"`
	Criteria             []core.Criterion `json:"criteria"`
	CheckIntervalSeconds int              `json:"check_interval_seconds"`
}

func LoadDefinition(path string) (Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d Definition
	if err = dec.Decode(&d); err != nil {
		return Definition{}, fmt.Errorf("goal definition %s: %w", path, err)
	}
	if dec.More() {
		return Definition{}, fmt.Errorf("goal definition %s: trailing data", path)
	}
	if d.Objective == "" {
		return Definition{}, fmt.Errorf("goal definition %s: objective required", path)
	}
	if len(d.Criteria) == 0 {
		return Definition{}, fmt.Errorf("goal definition %s: criteria required", path)
	}
	if err = core.ValidateCriteria(d.Criteria); err != nil {
		return Definition{}, fmt.Errorf("goal definition %s: %w", path, err)
	}
	return d, nil
}

func (d Definition) Spec(id string) Spec {
	return Spec{ID: id, Objective: d.Objective, Criteria: d.Criteria, CheckIntervalSeconds: d.CheckIntervalSeconds}
}

// Create copies the bundled sensor fixture into an authorized run directory,
// records the goal, and starts its workflow. The store must already be open;
// spec fields are required explicitly — nothing here supplies defaults for
// objective or criteria.
func Create(ctx context.Context, s *store.Store, runRoot, temporalAddress, projectRoot string, spec Spec) (core.Goal, error) {
	id := spec.ID
	if id == "" {
		id = RandomID("goal")
	}
	if !ValidID(id) {
		return core.Goal{}, errors.New("goal ID must contain only letters, digits, dash, underscore")
	}
	if spec.Objective == "" {
		return core.Goal{}, errors.New("objective required")
	}
	if len(spec.Criteria) == 0 {
		return core.Goal{}, errors.New("criteria required")
	}
	if err := core.ValidateCriteria(spec.Criteria); err != nil {
		return core.Goal{}, err
	}
	interval := spec.CheckIntervalSeconds
	if interval == 0 {
		interval = 30
	}
	if interval < 0 {
		return core.Goal{}, errors.New("interval must be positive")
	}
	dir := filepath.Join(runRoot, id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return core.Goal{}, err
	}
	project, err := filepath.Abs(projectRoot)
	if err != nil {
		return core.Goal{}, err
	}
	fixture := filepath.Join(project, "fixtures/sensor_board")
	for _, name := range []string{"sensor.kicad_sch", "sensor.kicad_pro"} {
		target := filepath.Join(dir, name)
		if _, err = os.Stat(target); errors.Is(err, os.ErrNotExist) {
			if err = copyExclusive(filepath.Join(fixture, name), target); err != nil {
				return core.Goal{}, err
			}
		} else if err != nil {
			return core.Goal{}, err
		}
	}
	artifactPath := filepath.Join(dir, "sensor.kicad_sch")
	artifacts, err := artifact.New(runRoot)
	if err != nil {
		return core.Goal{}, err
	}
	digest, err := artifacts.Digest(ctx, artifactPath)
	if err != nil {
		return core.Goal{}, err
	}
	goal := core.Goal{ID: id, Objective: spec.Objective, Criteria: spec.Criteria,
		AllowedRoot: dir, ArtifactPath: artifactPath, CheckIntervalSeconds: interval,
		AllowedCapabilities: DefaultCapabilities, Status: core.GoalActive, CurrentArtifactID: digest}
	snap, err := s.CreateGoal(ctx, goal)
	if err != nil {
		return core.Goal{}, err
	}
	if err = startWorkflow(ctx, temporalAddress, id); err != nil {
		return core.Goal{}, fmt.Errorf("goal %s persisted; Temporal connection: %w", id, err)
	}
	return snap.Goal, nil
}

func startWorkflow(ctx context.Context, address, id string) error {
	connection, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		return err
	}
	defer connection.Close()
	_, err = connection.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: core.TaskQueue}, core.GoalWorkflow, id)
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &already) {
		return nil
	}
	return err
}

// signalStarter is the subset of client.Client WakeGoal needs; tests inject a
// fake so both workflow states can be exercised without a Temporal server.
type signalStarter interface {
	SignalWithStartWorkflow(ctx context.Context, workflowID, signalName string, signalArg any, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// WakeGoal delivers one persisted event to the goal's workflow: a running
// workflow only receives the signal, while an already-finished one is started
// as a new run under the same goal ID and signalled with it. The event ID
// never changes, so duplicate delivery collapses onto the stored event row.
func WakeGoal(ctx context.Context, temporalAddress, goalID, eventID string) error {
	connection, err := client.Dial(client.Options{HostPort: temporalAddress})
	if err != nil {
		return err
	}
	defer connection.Close()
	return wake(ctx, connection, goalID, eventID)
}

func wake(ctx context.Context, conn signalStarter, goalID, eventID string) error {
	_, err := conn.SignalWithStartWorkflow(ctx, goalID, core.GoalEventSignal, eventID,
		client.StartWorkflowOptions{
			ID:        goalID,
			TaskQueue: core.TaskQueue,
			// A completed run must not block restarting the same goal ID.
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}, core.GoalWorkflow, goalID)
	return err
}

func copyExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func RandomID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

func ValidID(id string) bool {
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return id != ""
}
