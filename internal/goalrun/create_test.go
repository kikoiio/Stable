package goalrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
