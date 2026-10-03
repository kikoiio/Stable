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
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
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
	// The legacy verified goal has no determinably current evidence, so the
	// migration flips it to pending_reverification and queues one wake event.
	if g.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("legacy verified goal not flipped: %q", g.Goal.Status)
	}
	if g.Goal.Reason == "" {
		t.Fatal("migration left the reason empty")
	}
	if len(g.Events) != 2 || g.Events[0].Kind != core.EventKindCriteriaUpdate || g.Events[1].Kind != core.EventKindDependencyChange || g.Events[1].ID != "migrate-v4-legacy" || g.Events[0].Status != "pending" || g.Events[1].Status != "pending" {
		t.Fatalf("migration events: %+v", g.Events)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("reopen version %d %v", version, err)
	}
	g, err = s.GetGoalSnapshot(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Events) != 2 {
		t.Fatalf("reopen generated duplicate events: %+v", g.Events)
	}
}

func TestV2DatabaseMigratesLegacyVerifiedGoals(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	// v2 shape: evidence and decisions without the V01 versioning columns.
	_, err = db.Exec(`CREATE TABLE goals(id TEXT PRIMARY KEY,objective TEXT NOT NULL,criteria_json TEXT NOT NULL,allowed_root TEXT NOT NULL,
		artifact_path TEXT NOT NULL DEFAULT '',check_interval_seconds INTEGER NOT NULL DEFAULT 30,allowed_capabilities_json TEXT NOT NULL,
		status TEXT NOT NULL,current_artifact_id TEXT NOT NULL DEFAULT '',criteria_revision INTEGER NOT NULL DEFAULT 0,
		revision INTEGER NOT NULL DEFAULT 0,reason TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL);
	CREATE TABLE agents(id TEXT PRIMARY KEY,goal_id TEXT NOT NULL UNIQUE REFERENCES goals(id),status TEXT NOT NULL,
		last_decision_id TEXT NOT NULL DEFAULT '',next_wake_at TEXT);
	CREATE TABLE computer_sessions(id TEXT PRIMARY KEY,goal_id TEXT NOT NULL UNIQUE REFERENCES goals(id),status TEXT NOT NULL,
		generation INTEGER NOT NULL DEFAULT 0,opened_artifact_id TEXT NOT NULL DEFAULT '',last_observation_id TEXT NOT NULL DEFAULT '',runtime_handle TEXT NOT NULL DEFAULT '');
	CREATE TABLE events(id TEXT PRIMARY KEY,goal_id TEXT NOT NULL REFERENCES goals(id),kind TEXT NOT NULL,payload_json TEXT NOT NULL DEFAULT '{}',
		received_at TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('pending','signaled','processed')));
	CREATE TABLE evidence(id TEXT PRIMARY KEY,goal_id TEXT NOT NULL REFERENCES goals(id),criterion_id TEXT NOT NULL,artifact_id TEXT NOT NULL,
		kind TEXT NOT NULL,result TEXT NOT NULL CHECK(result IN ('pass','fail','stale')),report_path TEXT NOT NULL,created_at TEXT NOT NULL);
	INSERT INTO goals(id,objective,criteria_json,allowed_root,allowed_capabilities_json,status,current_artifact_id,created_at)
		VALUES('g-old','old verified','[{"id":"erc","kind":"kicad.erc_clean","payload":{"max_violations":0}}]','/tmp','[]','verified','sha-old','2026-01-01T00:00:00Z'),
		      ('g-active','still active','[]','/tmp','[]','active','','2026-01-01T00:00:00Z');
	INSERT INTO agents(id,goal_id,status) VALUES('agent-g-old','g-old','finished'),('agent-g-active','g-active','running');
	INSERT INTO computer_sessions(id,goal_id,status) VALUES('computer-g-old','g-old','absent'),('computer-g-active','g-active','absent');
	INSERT INTO evidence(id,goal_id,criterion_id,artifact_id,kind,result,report_path,created_at)
		VALUES('ev-old','g-old','erc','sha-old','kicad.erc','pass','/tmp/old.json','2026-01-02T00:00:00Z');
	PRAGMA user_version = 2;`)
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
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("version %d %v", version, err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "g-old")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.Status != core.GoalPendingReverification || snap.Goal.Reason == "" {
		t.Fatalf("goal not pending reverification: %+v", snap.Goal)
	}
	// The legacy evidence row is preserved untouched: original result kept,
	// new fields read as unknown and are never backfilled.
	if len(snap.Evidence) != 1 {
		t.Fatalf("evidence rows changed: %+v", snap.Evidence)
	}
	ev := snap.Evidence[0]
	if ev.Result != "pass" || ev.CriteriaRevision != nil || ev.Provenance != nil || ev.InvalidatedReason != "" {
		t.Fatalf("legacy evidence mutated: %+v", ev)
	}
	if len(snap.Events) != 2 || snap.Events[0].ID != "migrate-v3-g-old" || snap.Events[0].Status != "pending" || snap.Events[1].ID != "migrate-v4-g-old" || snap.Events[1].Kind != core.EventKindDependencyChange {
		t.Fatalf("wake event: %+v", snap.Events)
	}
	active, err := s.GetGoalSnapshot(ctx, "g-active")
	if err != nil || active.Goal.Status != core.GoalActive {
		t.Fatalf("non-verified goal touched: %+v %v", active.Goal, err)
	}

	// Reopening must not repeat the migration or queue a second event.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err = s.GetGoalSnapshot(ctx, "g-old")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Events) != 2 || len(snap.Evidence) != 1 || snap.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("reopen not idempotent: events=%d evidence=%d status=%q", len(snap.Events), len(snap.Evidence), snap.Goal.Status)
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
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("version %d %v", version, err)
	}
	if !hasColumn(s.DB(), "decisions", "model_call_id") {
		t.Fatal("model_call_id not migrated")
	}
	if !hasColumn(s.DB(), "decisions", "criteria_revision") {
		t.Fatal("decisions.criteria_revision not migrated")
	}
	if !hasColumn(s.DB(), "goals", "criteria_revision") {
		t.Fatal("criteria_revision not migrated")
	}
	for _, col := range []string{"criteria_revision", "provenance", "invalidated_reason"} {
		if !hasColumn(s.DB(), "evidence", col) {
			t.Fatalf("evidence.%s not migrated", col)
		}
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
		if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
			t.Fatalf("reopen %d: version %d %v", i, version, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
