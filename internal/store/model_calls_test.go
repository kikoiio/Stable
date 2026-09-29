package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"proactive-agent/internal/core"
)

func TestModelCallLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g := core.Goal{ID: "g", AllowedRoot: t.TempDir(), ArtifactPath: "/tmp/none"}
	if _, err = s.CreateGoal(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordObservation(ctx, core.Observation{ID: "o", GoalID: "g", Facts: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	c := core.ModelCall{ID: "m", GoalID: "g", ObservationID: "o", Provider: "openai", Model: "test", Host: "api.openai.com"}
	if err = s.StartModelCall(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishModelCall(ctx, "m", "failed", "req1", "authentication"); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishModelCall(ctx, "m", "succeeded", "", ""); err == nil {
		t.Fatal("duplicate transition accepted")
	}
	snap, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ModelCalls) != 1 || snap.ModelCalls[0].ErrorKind != "authentication" {
		t.Fatal(snap.ModelCalls)
	}
	if err = s.StartModelCall(ctx, core.ModelCall{ID: "m2", GoalID: "g", ObservationID: "o", Provider: "gemini", Model: "test", Host: "h"}); err != nil {
		t.Fatal(err)
	}
	if err = s.InterruptStartedModelCalls(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err = s.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if snap.ModelCalls[1].Status != "interrupted" {
		t.Fatal(snap.ModelCalls)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err = s.GetGoalSnapshot(ctx, "g")
	if err != nil || len(snap.ModelCalls) != 2 {
		t.Fatalf("reopen: %v %v", err, snap.ModelCalls)
	}
}

func TestOldSchemaMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE decisions(id TEXT PRIMARY KEY,agent_id TEXT NOT NULL,observation_id TEXT NOT NULL,proposal_json TEXT NOT NULL,model_run_id TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version %d %v", version, err)
	}
	rows, err := s.DB().Query(`PRAGMA table_info(decisions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var n, notnull, pk int
		var name, kind string
		var def sql.NullString
		if err = rows.Scan(&n, &name, &kind, &notnull, &def, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "model_call_id" {
			found = true
		}
	}
	if !found {
		t.Fatal("model_call_id not migrated")
	}
}
