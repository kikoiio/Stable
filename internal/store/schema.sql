PRAGMA foreign_keys = ON;

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
    dependency_revision INTEGER NOT NULL DEFAULT 0,
    revision INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    source_session_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS goals_status ON goals(status);
CREATE INDEX IF NOT EXISTS goals_revision ON goals(id, revision);

CREATE TABLE IF NOT EXISTS goal_dependencies (
    goal_id TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
    family TEXT NOT NULL CHECK(family IN ('kicad.erc','sensor.connection')),
    snapshot_json TEXT NOT NULL,
    PRIMARY KEY (goal_id, family)
);

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
	dependency_revision INTEGER,
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

CREATE TABLE IF NOT EXISTS permission_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    effect TEXT NOT NULL CHECK(effect IN ('allow','deny','ask')),
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    target TEXT NOT NULL,
    parameters_digest TEXT NOT NULL,
    scope_digest TEXT NOT NULL,
    protocol TEXT NOT NULL DEFAULT '',
    host TEXT NOT NULL DEFAULT '',
    port INTEGER NOT NULL DEFAULT 0 CHECK(port BETWEEN 0 AND 65535),
    created_at TEXT NOT NULL,
    UNIQUE(effect,kind,name,target,parameters_digest,scope_digest,protocol,host,port)
);

CREATE TABLE IF NOT EXISTS approval_requests (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    goal_id TEXT NOT NULL DEFAULT '',
    work_item_id TEXT NOT NULL DEFAULT '',
    operation_json TEXT NOT NULL,
    authority_json TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    operation_digest TEXT NOT NULL,
    scope_digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('pending','allowed_once','saved','denied','cancelled','expired')),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    consumed_at TEXT
);
CREATE INDEX IF NOT EXISTS approval_requests_session_status ON approval_requests(session_id,status,created_at);

CREATE TABLE IF NOT EXISTS permission_decisions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id TEXT NOT NULL DEFAULT '',
    run_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    goal_id TEXT NOT NULL DEFAULT '',
    work_item_id TEXT NOT NULL DEFAULT '',
    operation_id TEXT NOT NULL,
    operation_digest TEXT NOT NULL,
    scope_digest TEXT NOT NULL,
    decision TEXT NOT NULL CHECK(decision IN ('allow','deny','ask')),
    reason TEXT NOT NULL,
    approval_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS permission_decisions_run_operation ON permission_decisions(run_id,operation_id,created_at);

CREATE TABLE IF NOT EXISTS criteria_proposals (
    id TEXT PRIMARY KEY,
    goal_id TEXT REFERENCES goals(id),
    status TEXT NOT NULL CHECK(status IN ('proposed','confirmed','rejected','superseded')),
    criteria_json TEXT NOT NULL,
    raw_text TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS criteria_proposals_goal ON criteria_proposals(goal_id, created_at);
