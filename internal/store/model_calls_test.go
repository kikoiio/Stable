package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"stable/internal/core"
)

func TestV1DatabaseMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE goals(id TEXT PRIMARY KEY,objective TEXT NOT NULL,criteria_json TEXT NOT NULL,allowed_root TEXT NOT NULL,
		artifact_path TEXT NOT NULL DEFAULT '',check_interval_seconds INTEGER NOT NULL DEFAULT 30,allowed_capabilities_json TEXT NOT NULL,
		status TEXT NOT NULL,current_artifact_id TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL DEFAULT 0,reason TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL);
	CREATE TABLE agents(id TEXT PRIMARY KEY,goal_id TEXT NOT NULL UNIQUE REFERENCES goals(id),status TEXT NOT NULL,
		last_decision_id TEXT NOT NULL DEFAULT '',next_wake_at TEXT);
	CREATE TABLE computer_sessions(id TEXT PRIMARY KEY,goal_id TEXT NOT NULL UNIQUE REFERENCES goals(id),status TEXT NOT NULL,
		generation INTEGER NOT NULL DEFAULT 0,opened_artifact_id TEXT NOT NULL DEFAULT '',last_observation_id TEXT NOT NULL DEFAULT '',runtime_handle TEXT NOT NULL DEFAULT '');
	INSERT INTO goals(id,objective,criteria_json,allowed_root,allowed_capabilities_json,status,created_at) VALUES('legacy','old goal','[]','/tmp','[]','verified','2026-01-01T00:00:00Z');
	INSERT INTO agents(id,goal_id,status) VALUES('agent-legacy','legacy','finished');
	INSERT INTO computer_sessions(id,goal_id,status) VALUES('computer-legacy','legacy','absent');
	PRAGMA user_version = 1;`)
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
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("version %d %v", version, err)
	}
	var objective string
	if err = s.DB().QueryRow(`SELECT objective FROM goals WHERE id='legacy'`).Scan(&objective); err != nil || objective != "old goal" {
		t.Fatalf("legacy row: %q %v", objective, err)
	}
	g, err := s.GetGoalSnapshot(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if g.Goal.CriteriaRevision != 0 {
		t.Fatalf("unexpected criteria revision: %d", g.Goal.CriteriaRevision)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("reopen version %d %v", version, err)
	}
	if _, err = s.GetGoalSnapshot(context.Background(), "legacy"); err != nil {
		t.Fatal(err)
	}
}

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
	var version int
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("version %d %v", version, err)
	}
	if !hasColumn(s.DB(), "decisions", "model_call_id") {
		t.Fatal("model_call_id not migrated")
	}
	if !hasColumn(s.DB(), "goals", "criteria_revision") {
		t.Fatal("criteria_revision not migrated")
	}
	for _, table := range []string{"session_messages", "criteria_proposals"} {
		var name string
		if err = s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("%s not created: %v", table, err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
			t.Fatalf("reopen %d: version %d %v", i, version, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
