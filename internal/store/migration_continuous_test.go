package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// schemaV3 is the storage schema as it shipped at the V01 storage-v3
// milestone (commit 7a0914d), kept verbatim so this test exercises the
// real upgrade path of databases created by that release.
const schemaV3 = `PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS goals (
    id TEXT PRIMARY KEY,
    objective TEXT NOT NULL,
    criteria_json TEXT NOT NULL,
    allowed_root TEXT NOT NULL,
    artifact_path TEXT NOT NULL DEFAULT '',
    check_interval_seconds INTEGER NOT NULL DEFAULT 30,
    allowed_capabilities_json TEXT NOT NULL,
    status TEXT NOT NULL,
    current_artifact_id TEXT NOT NULL DEFAULT '',
    criteria_revision INTEGER NOT NULL DEFAULT 0,
    revision INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS goals_status ON goals(status);
CREATE INDEX IF NOT EXISTS goals_revision ON goals(id, revision);

CREATE TABLE IF NOT EXISTS agents (
    id TEXT PRIMARY KEY,
    goal_id TEXT NOT NULL UNIQUE REFERENCES goals(id),
    status TEXT NOT NULL,
    last_decision_id TEXT NOT NULL DEFAULT '',
    next_wake_at TEXT
);

CREATE TABLE IF NOT EXISTS computer_sessions (
    id TEXT PRIMARY KEY,
    goal_id TEXT NOT NULL UNIQUE REFERENCES goals(id),
    status TEXT NOT NULL,
    generation INTEGER NOT NULL DEFAULT 0,
    opened_artifact_id TEXT NOT NULL DEFAULT '',
    last_observation_id TEXT NOT NULL DEFAULT '',
    runtime_handle TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS artifact_versions (
    id TEXT NOT NULL,
    goal_id TEXT NOT NULL REFERENCES goals(id),
    path TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(id, goal_id)
);

CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    goal_id TEXT NOT NULL REFERENCES goals(id),
    kind TEXT NOT NULL,
    payload_json TEXT NOT NULL DEFAULT '{}',
    received_at TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('pending','signaled','processed'))
);
CREATE INDEX IF NOT EXISTS events_goal_status ON events(goal_id, status, received_at);

CREATE TABLE IF NOT EXISTS observations (
    id TEXT PRIMARY KEY,
    goal_id TEXT NOT NULL REFERENCES goals(id),
    event_id TEXT NOT NULL DEFAULT '',
    artifact_id TEXT NOT NULL,
    computer_session_id TEXT NOT NULL DEFAULT '',
    facts_json TEXT NOT NULL,
    observed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS observations_goal_time ON observations(goal_id, observed_at);

CREATE TABLE IF NOT EXISTS decisions (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL REFERENCES agents(id),
    observation_id TEXT NOT NULL REFERENCES observations(id),
    proposal_json TEXT NOT NULL,
    model_run_id TEXT NOT NULL DEFAULT '',
	model_call_id TEXT NOT NULL DEFAULT '',
	criteria_revision INTEGER,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS decisions_agent_time ON decisions(agent_id, created_at);

CREATE TABLE IF NOT EXISTS model_calls (
	id TEXT PRIMARY KEY,
	goal_id TEXT NOT NULL REFERENCES goals(id),
	observation_id TEXT NOT NULL REFERENCES observations(id),
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	host TEXT NOT NULL,
	provider_request_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK(status IN ('started','succeeded','failed','interrupted')),
	error_kind TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL,
	finished_at TEXT
);
CREATE INDEX IF NOT EXISTS model_calls_goal_time ON model_calls(goal_id, started_at);

CREATE TABLE IF NOT EXISTS actions (
    id TEXT PRIMARY KEY,
    decision_id TEXT NOT NULL REFERENCES decisions(id),
    expected_artifact_id TEXT NOT NULL,
    desired_postcondition_json TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('prepared','outcome_unknown','applied','verified','blocked')),
    result_artifact_id TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS actions_status ON actions(status);

CREATE TABLE IF NOT EXISTS evidence (
    id TEXT PRIMARY KEY,
    goal_id TEXT NOT NULL REFERENCES goals(id),
    criterion_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    result TEXT NOT NULL CHECK(result IN ('pass','fail','stale')),
    report_path TEXT NOT NULL,
    criteria_revision INTEGER,
    provenance TEXT,
    invalidated_reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS evidence_goal_artifact ON evidence(goal_id, artifact_id, created_at);

CREATE TABLE IF NOT EXISTS session_messages (
    id TEXT PRIMARY KEY,
    goal_id TEXT REFERENCES goals(id),
    role TEXT NOT NULL CHECK(role IN ('user','agent','system')),
    kind TEXT NOT NULL CHECK(kind IN ('text','question','reply','criteria_proposal','criteria_confirm')),
    text TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL DEFAULT '',
    ref TEXT NOT NULL DEFAULT '',
    delivered INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS session_messages_goal_time ON session_messages(goal_id, created_at);
CREATE INDEX IF NOT EXISTS session_messages_undelivered ON session_messages(delivered, goal_id);

CREATE TABLE IF NOT EXISTS criteria_proposals (
    id TEXT PRIMARY KEY,
    goal_id TEXT REFERENCES goals(id),
    status TEXT NOT NULL CHECK(status IN ('proposed','confirmed','rejected','superseded')),
    criteria_json TEXT NOT NULL,
    raw_text TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS criteria_proposals_goal ON criteria_proposals(goal_id, created_at);
`

// TestContinuousMigrationFromV3 walks a v3 database through every
// migration step to v10 in one Open call and checks that pre-existing
// rows survive the actions table rebuilds and that the permission and
// candidate tables added by M03 are present afterwards.
func TestContinuousMigrationFromV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schemaV3 + `; PRAGMA user_version=3`); err != nil {
		t.Fatal(err)
	}
	rows := []string{
		"INSERT INTO goals(id,objective,criteria_json,allowed_root,allowed_capabilities_json,status,created_at) VALUES('g1','objective','[]','/tmp/g1','[]','active','2026-01-01T00:00:00Z')",
		"INSERT INTO agents(id,goal_id,status) VALUES('a-g1','g1','active')",
		"INSERT INTO observations(id,goal_id,artifact_id,facts_json,observed_at) VALUES('o1','g1','art0','{}','2026-01-01T00:00:01Z')",
		"INSERT INTO decisions(id,agent_id,observation_id,proposal_json,created_at) VALUES('d1','a-g1','o1','{}','2026-01-01T00:00:02Z')",
		"INSERT INTO actions(id,decision_id,expected_artifact_id,desired_postcondition_json,status,result_artifact_id,reason) VALUES('act1','d1','art0','{}','applied','art1','repair applied')",
	}
	for _, q := range rows {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open v3 database: %v", err)
	}
	defer s.Close()
	var version int
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var id, status, reason string
	if err = s.DB().QueryRow(`SELECT id,status,reason FROM actions WHERE id='act1'`).Scan(&id, &status, &reason); err != nil || status != "applied" || reason != "repair applied" {
		t.Fatalf("action row lost or altered: id=%s status=%s reason=%s err=%v", id, status, reason, err)
	}
	if _, err = s.DB().Exec(`INSERT INTO actions(id,decision_id,expected_artifact_id,desired_postcondition_json,status) VALUES('act2','d1','art1','{}','candidate_ready')`); err != nil {
		t.Fatalf("migrated actions CHECK rejects M03 statuses: %v", err)
	}
	for _, column := range []struct{ table, column string }{
		{"goals", "criteria_revision"}, {"goals", "dependency_revision"}, {"goals", "source_session_id"},
		{"decisions", "model_call_id"}, {"decisions", "dependency_revision"},
		{"criteria_proposals", "session_id"},
		{"approval_requests", "authority_json"}, {"permission_decisions", "user_id"},
	} {
		if !hasColumn(s.DB(), column.table, column.column) {
			t.Fatalf("migration did not restore %s.%s", column.table, column.column)
		}
	}
	for _, table := range []string{"permission_rules", "approval_requests", "permission_decisions", "candidates", "candidate_reviews", "acceptance_decisions", "acceptance_receipts", "acceptance_apply_journal"} {
		var name string
		if err = s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("migrated database lacks %s: %v", table, err)
		}
	}
}
