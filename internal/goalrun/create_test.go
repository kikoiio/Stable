package goalrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"stable/internal/core"
	"stable/internal/store"
)

func fixtureDir(t *testing.T) string {
	t.Helper()
	board := filepath.Join(t.TempDir(), "fixtures", "sensor_board")
	if err := os.MkdirAll(board, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(board, "sensor.kicad_sch"), []byte("(kicad_sch)"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(board, "sensor.kicad_pro"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(board))
}

func criteria() []core.Criterion {
	return []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)}}
}

func TestCreatePersistsGoalAndCopiesFixture(t *testing.T) {
	ctx := context.Background()
	runRoot := t.TempDir()
	db := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	spec := Spec{ID: "demo1", Objective: "repair sensor", Criteria: criteria(), CheckIntervalSeconds: 30}
	// Unit tests have no Temporal server: persistence and fixture isolation are
	// asserted via the documented partial-failure contract; workflow start is
	// covered by e2e.
	_, err = Create(ctx, s, runRoot, "127.0.0.1:1", fixtureDir(t), spec)
	if err == nil || !strings.Contains(err.Error(), "persisted; Temporal connection") {
		t.Fatalf("expected persistence-then-temporal failure, got %v", err)
	}
	g, err := s.GetGoalSnapshot(ctx, "demo1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Goal.ID != "demo1" || g.Goal.Objective != "repair sensor" || g.Goal.Status != core.GoalActive {
		t.Fatalf("goal: %+v", g.Goal)
	}
	if len(g.Goal.Criteria) != 1 || g.Goal.CriteriaRevision != 0 {
		t.Fatalf("criteria: %+v", g.Goal)
	}
	if len(g.Goal.AllowedCapabilities) != len(DefaultCapabilities) {
		t.Fatalf("capabilities: %v", g.Goal.AllowedCapabilities)
	}
	// The fixture must live inside the authorized run directory.
	for _, name := range []string{"sensor.kicad_sch", "sensor.kicad_pro"} {
		if _, err = os.Stat(filepath.Join(runRoot, "demo1", name)); err != nil {
			t.Fatalf("copy missing: %v", err)
		}
	}
	if g.Goal.CurrentArtifactID == "" {
		t.Fatal("no artifact digest recorded")
	}
}

func TestCreateRejectsIncompleteSpecs(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runRoot, project := t.TempDir(), fixtureDir(t)
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{"no objective", Spec{ID: "a", Criteria: criteria()}, "objective required"},
		{"no criteria", Spec{ID: "a", Objective: "x"}, "criteria required"},
		{"bad id", Spec{ID: "bad id!", Objective: "x", Criteria: criteria()}, "goal ID"},
		{"bad criteria", Spec{ID: "a", Objective: "x", Criteria: []core.Criterion{{ID: "c", Kind: "kicad.beautiful"}}}, "unsupported kind"},
		{"negative interval", Spec{ID: "a", Objective: "x", Criteria: criteria(), CheckIntervalSeconds: -1}, "interval"},
	}
	for _, tc := range cases {
		if _, err = Create(ctx, s, runRoot, "127.0.0.1:1", project, tc.spec); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v", tc.name, err)
		}
	}
}

func TestCreateMissingFixtureFails(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = Create(ctx, s, t.TempDir(), "127.0.0.1:1", t.TempDir(), Spec{ID: "a", Objective: "x", Criteria: criteria()}); err == nil {
		t.Fatal("missing fixture accepted")
	}
}

func TestValidID(t *testing.T) {
	if !ValidID("goal-1_a") || ValidID("") || ValidID("bad id") || ValidID("slash/") {
		t.Fatal("ValidID")
	}
	if got := RandomID("goal"); len(got) <= len("goal-") {
		t.Fatalf("RandomID: %q", got)
	}
}

type wakeCall struct {
	workflowID string
	signalName string
	signalArg  any
	options    client.StartWorkflowOptions
	workflow   interface{}
	args       []interface{}
}

// fakeSignalStarter records SignalWithStartWorkflow calls; running reports
// whether the goal ID currently has an open workflow run.
type fakeSignalStarter struct {
	running bool
	err     error
	calls   []wakeCall
}

func (f *fakeSignalStarter) SignalWithStartWorkflow(_ context.Context, workflowID, signalName string, signalArg any, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error) {
	f.calls = append(f.calls, wakeCall{workflowID, signalName, signalArg, options, workflow, args})
	if f.err != nil {
		return nil, f.err
	}
	return nil, nil
}

func checkWakeCall(t *testing.T, c wakeCall, goalID, eventID string) {
	t.Helper()
	if c.workflowID != goalID || c.options.ID != goalID {
		t.Fatalf("workflow ID: %q options %+v", c.workflowID, c.options)
	}
	if c.signalName != core.GoalEventSignal || c.signalArg != eventID {
		t.Fatalf("signal: %q %v", c.signalName, c.signalArg)
	}
	if c.options.TaskQueue != core.TaskQueue {
		t.Fatalf("task queue: %q", c.options.TaskQueue)
	}
	if reflect.ValueOf(c.workflow).Pointer() != reflect.ValueOf(core.GoalWorkflow).Pointer() {
		t.Fatalf("workflow: %v", c.workflow)
	}
	if len(c.args) != 1 || c.args[0] != goalID {
		t.Fatalf("workflow args: %v", c.args)
	}
}

// A running workflow only receives the signal; nothing restarts it.
func TestWakeRunningGoalSignalsOnly(t *testing.T) {
	fake := &fakeSignalStarter{running: true}
	if err := wake(context.Background(), fake, "goal-1", "evt-1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("running goal woke %d times", len(fake.calls))
	}
	checkWakeCall(t, fake.calls[0], "goal-1", "evt-1")
}

// A finished workflow is restarted under the same goal ID and signalled with
// the same event; the reuse policy must allow a completed run to reopen.
func TestWakeFinishedGoalStartsNewRun(t *testing.T) {
	fake := &fakeSignalStarter{running: false}
	if err := wake(context.Background(), fake, "goal-1", "evt-9"); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("finished goal woke %d times", len(fake.calls))
	}
	c := fake.calls[0]
	checkWakeCall(t, c, "goal-1", "evt-9")
	if c.options.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE {
		t.Fatalf("reuse policy blocks restart: %v", c.options.WorkflowIDReusePolicy)
	}
}

// Duplicate delivery keeps the event ID unchanged so the store and the
// workflow dedup onto the same event row.
func TestWakeDuplicateSignalKeepsEventID(t *testing.T) {
	fake := &fakeSignalStarter{}
	for i := 0; i < 2; i++ {
		if err := wake(context.Background(), fake, "goal-1", "evt-dup"); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake.calls) != 2 || fake.calls[0].signalArg != "evt-dup" || fake.calls[1].signalArg != "evt-dup" {
		t.Fatalf("duplicate signals: %+v", fake.calls)
	}
}

func TestWakePropagatesDeliveryFailure(t *testing.T) {
	fake := &fakeSignalStarter{err: errors.New("temporal down")}
	if err := wake(context.Background(), fake, "goal-1", "evt-1"); err == nil {
		t.Fatal("delivery failure swallowed")
	}
}

// WakeGoal against an unreachable Temporal reports an error instead of
// pretending the event was delivered.
func TestWakeGoalTemporalUnreachable(t *testing.T) {
	if err := WakeGoal(context.Background(), "127.0.0.1:1", "goal-1", "evt-1"); err == nil {
		t.Fatal("unreachable Temporal accepted the event")
	}
}

// Recovery scenario: the confirmation event keeps one row and one version
// bump no matter how many times the wake is delivered or retried.
func TestConfirmWakeDedupesByEventID(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.CreateGoal(ctx, core.Goal{ID: "goal-1", Objective: "repair sensor", Criteria: criteria(), AllowedRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	proposal := core.CriteriaProposal{ID: "prop-1", GoalID: "goal-1", Status: core.ProposalPending, Criteria: criteria(), RawText: "ERC 全过"}
	if _, err = s.InsertProposal(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	conf, err := s.ConfirmGoalCriteria(ctx, "prop-1")
	if err != nil {
		t.Fatal(err)
	}
	if conf.Event.ID != "criteria-confirm-prop-1" || conf.Goal.CriteriaRevision != 1 {
		t.Fatalf("confirmation: %+v %+v", conf.Event, conf.Goal)
	}
	// Duplicate wakes of the same event (running or restarted workflow) carry
	// the same event ID, and a repeated confirm is rejected without a second
	// version bump or event row.
	fake := &fakeSignalStarter{}
	for i := 0; i < 2; i++ {
		if err = wake(ctx, fake, conf.Goal.ID, conf.Event.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.ConfirmGoalCriteria(ctx, "prop-1"); err == nil {
		t.Fatal("second confirm accepted")
	}
	events, err := s.UnprocessedEvents(ctx)
	if err != nil || len(events) != 1 || events[0].ID != conf.Event.ID {
		t.Fatalf("events after duplicate wake: %+v %v", events, err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.CriteriaRevision != 1 || snap.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("goal after duplicate confirm: %+v", snap.Goal)
	}
}
