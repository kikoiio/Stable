package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"stable/internal/core"
)

//go:embed schema.sql
var schema string

var ErrRevision = errors.New("goal revision changed")

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version < 2 {
		if version == 0 {
			var oldTable string
			err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='decisions'`).Scan(&oldTable)
			if err == nil {
				if !hasColumn(db, "decisions", "model_call_id") {
					if _, err = db.Exec(`ALTER TABLE decisions ADD COLUMN model_call_id TEXT NOT NULL DEFAULT ''`); err != nil {
						db.Close()
						return nil, err
					}
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				db.Close()
				return nil, err
			}
		}
		var goalsTable string
		err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='goals'`).Scan(&goalsTable)
		if err == nil {
			if !hasColumn(db, "goals", "criteria_revision") {
				if _, err = db.Exec(`ALTER TABLE goals ADD COLUMN criteria_revision INTEGER NOT NULL DEFAULT 0`); err != nil {
					db.Close()
					return nil, err
				}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			db.Close()
			return nil, err
		}
	}
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if version < 3 {
		if err = migrateV3(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	if version < 4 {
		if err = migrateV4(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	if version < 5 {
		if err = migrateV5(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	if version < 6 {
		if err = migrateV6(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	if version < 8 {
		if err = migrateV8(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	if version < 10 {
		if err = migrateV10(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err = db.Exec(`PRAGMA user_version = 10`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrateV10(db *sql.DB) error {
	if !hasColumn(db, "permission_decisions", "user_id") {
		if _, err := db.Exec(`ALTER TABLE permission_decisions ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`PRAGMA user_version=10`)
	return err
}

func migrateV8(db *sql.DB) error {
	if !hasColumn(db, "approval_requests", "authority_json") {
		if _, err := db.Exec(`ALTER TABLE approval_requests ADD COLUMN authority_json TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasColumn(db, "approval_requests", "reason") {
		if _, err := db.Exec(`ALTER TABLE approval_requests ADD COLUMN reason TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`PRAGMA user_version=8`)
	return err
}

func migrateV6(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`PRAGMA user_version=6`); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateV5(db *sql.DB) error {
	addGoal := !hasColumn(db, "goals", "source_session_id")
	addProposal := !hasColumn(db, "criteria_proposals", "session_id")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if addGoal {
		if _, err = tx.Exec(`ALTER TABLE goals ADD COLUMN source_session_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if addProposal {
		if _, err = tx.Exec(`ALTER TABLE criteria_proposals ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateV4 adds dependency tracking storage to existing databases.
func migrateV4(db *sql.DB) error {
	addGoalRevision := !hasColumn(db, "goals", "dependency_revision")
	addDecisionRevision := !hasColumn(db, "decisions", "dependency_revision")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if addGoalRevision {
		if _, err = tx.Exec(`ALTER TABLE goals ADD COLUMN dependency_revision INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if addDecisionRevision {
		if _, err = tx.Exec(`ALTER TABLE decisions ADD COLUMN dependency_revision INTEGER`); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS goal_dependencies (
		goal_id TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
		family TEXT NOT NULL CHECK(family IN ('kicad.erc','sensor.connection')),
		snapshot_json TEXT NOT NULL,
		PRIMARY KEY (goal_id, family)
	)`); err != nil {
		return err
	}
	if err = migrateV4Goals(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateV4Goals(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT g.id,g.status FROM goals g
		WHERE g.status IN ('verified','pending_reverification')
		AND NOT EXISTS (SELECT 1 FROM goal_dependencies d WHERE d.goal_id=g.id)
		ORDER BY g.id`)
	if err != nil {
		return err
	}
	type legacyGoal struct{ id, status string }
	var goals []legacyGoal
	for rows.Next() {
		var g legacyGoal
		if err = rows.Scan(&g.id, &g.status); err != nil {
			rows.Close()
			return err
		}
		goals = append(goals, g)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, g := range goals {
		reason := "旧版数据缺少工程依赖快照，待复核后才能确认当前结论"
		if _, err = tx.Exec(`UPDATE goals SET status='pending_reverification',reason=?,revision=revision+CASE WHEN status='verified' THEN 1 ELSE 0 END WHERE id=?`, reason, g.id); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE agents SET status=? WHERE goal_id=?`, agentStatusFor(core.GoalPendingReverification), g.id); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO events(id,goal_id,kind,payload_json,received_at,status) VALUES(?,?,?,?,?,'pending')`,
			"migrate-v4-"+g.id, g.id, core.EventKindDependencyChange, `{"source":"migration-v4"}`, now()); err != nil {
			return err
		}
	}
	return nil
}

// migrateV3 adds the V01 versioning columns to evidence and decisions inside a
// single transaction (each column add rolls the whole migration back on
// failure), then flips legacy verified goals that lack determinably current
// evidence to pending_reverification with one idempotent wake event per goal.
func migrateV3(db *sql.DB) error {
	addEvidenceRevision := !hasColumn(db, "evidence", "criteria_revision")
	addEvidenceProvenance := !hasColumn(db, "evidence", "provenance")
	addEvidenceInvalidated := !hasColumn(db, "evidence", "invalidated_reason")
	addDecisionRevision := !hasColumn(db, "decisions", "criteria_revision")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if addEvidenceRevision {
		if _, err = tx.Exec(`ALTER TABLE evidence ADD COLUMN criteria_revision INTEGER`); err != nil {
			return err
		}
	}
	if addEvidenceProvenance {
		if _, err = tx.Exec(`ALTER TABLE evidence ADD COLUMN provenance TEXT`); err != nil {
			return err
		}
	}
	if addEvidenceInvalidated {
		if _, err = tx.Exec(`ALTER TABLE evidence ADD COLUMN invalidated_reason TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if addDecisionRevision {
		if _, err = tx.Exec(`ALTER TABLE decisions ADD COLUMN criteria_revision INTEGER`); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`SELECT id,criteria_json,current_artifact_id,criteria_revision FROM goals WHERE status='verified'`)
	if err != nil {
		return err
	}
	type legacyGoal struct {
		id, criteria, artifact string
		revision               int
	}
	var verified []legacyGoal
	for rows.Next() {
		var g legacyGoal
		if err = rows.Scan(&g.id, &g.criteria, &g.artifact, &g.revision); err != nil {
			rows.Close()
			return err
		}
		verified = append(verified, g)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, g := range verified {
		current, err := goalHasCurrentEvidence(tx, g.id, g.criteria, g.artifact, g.revision)
		if err != nil {
			return err
		}
		if current {
			continue
		}
		if _, err = tx.Exec(`UPDATE goals SET status='pending_reverification',reason=?,revision=revision+1 WHERE id=? AND status='verified'`,
			"旧版达标结论缺少可核对的当前证据（标准版本/出处未知），已转入待复核", g.id); err != nil {
			return err
		}
		// The event ID doubles as the idempotency key: reopening the database
		// never produces a second wake event for the same goal.
		if _, err = tx.Exec(`INSERT OR IGNORE INTO events(id,goal_id,kind,payload_json,received_at,status) VALUES(?,?,?,?,?,'pending')`,
			"migrate-v3-"+g.id, g.id, core.EventKindCriteriaUpdate, `{"source":"migration-v3"}`, now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// goalHasCurrentEvidence reports whether every criterion of the goal is backed
// by evidence that satisfies the evidence-v1 currentness conditions. Legacy
// rows (NULL revision/provenance) never qualify.
func goalHasCurrentEvidence(tx *sql.Tx, goalID, criteriaJSON, artifactID string, revision int) (bool, error) {
	var criteria []core.Criterion
	if err := json.Unmarshal([]byte(criteriaJSON), &criteria); err != nil {
		return false, err
	}
	if len(criteria) == 0 {
		return false, nil
	}
	for _, c := range criteria {
		rows, err := tx.Query(`SELECT provenance FROM evidence
			WHERE goal_id=? AND criterion_id=? AND result='pass' AND invalidated_reason=''
			AND criteria_revision=? AND artifact_id=?`, goalID, c.ID, revision, artifactID)
		if err != nil {
			return false, err
		}
		found := false
		for rows.Next() {
			var raw sql.NullString
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				return false, err
			}
			if provenanceComplete(raw.String) {
				found = true
				break
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// provenanceComplete checks the evidence-v1 required fields. Empty or
// unparseable JSON, a newer schema version, or a non-tool_check source all
// read as unknown and never count as current evidence.
func provenanceComplete(raw string) bool {
	if raw == "" {
		return false
	}
	var p core.EvidenceProvenance
	if json.Unmarshal([]byte(raw), &p) != nil {
		return false
	}
	return p.SchemaVersion == 1 && p.Claim != "" && p.Coverage != "" &&
		p.CheckerID != "" && p.CheckerVersion != "" &&
		p.SourceLevel == "tool_check" && p.InvalidationRule != ""
}

func hasColumn(db *sql.DB, table, column string) bool {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var n, notnull, pk int
		var name, kind string
		var def sql.NullString
		if rows.Scan(&n, &name, &kind, &notnull, &def, &pk) == nil && name == column {
			return true
		}
	}
	return false
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

func encode(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
func now() string                  { return time.Now().UTC().Format(time.RFC3339Nano) }
func parseTime(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t }
func raw(v json.RawMessage) string {
	if len(v) == 0 {
		return "{}"
	}
	return string(v)
}

func (s *Store) CreateGoal(ctx context.Context, g core.Goal) (core.GoalSnapshot, error) {
	if g.ID == "" || g.AllowedRoot == "" {
		return core.GoalSnapshot{}, errors.New("goal ID and allowed root required")
	}
	if g.Status == "" {
		g.Status = core.GoalActive
	}
	if g.CheckIntervalSeconds <= 0 {
		g.CheckIntervalSeconds = 30
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.GoalSnapshot{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO goals
		(id,objective,criteria_json,allowed_root,artifact_path,check_interval_seconds,allowed_capabilities_json,status,current_artifact_id,revision,reason,created_at,source_session_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, g.ID, g.Objective, encode(g.Criteria), g.AllowedRoot, g.ArtifactPath, g.CheckIntervalSeconds,
		encode(g.AllowedCapabilities), g.Status, g.CurrentArtifactID, 1, g.Reason, g.CreatedAt.Format(time.RFC3339Nano), g.SourceSessionID)
	if err != nil {
		return core.GoalSnapshot{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO agents(id,goal_id,status) VALUES(?,?,?)`, "agent-"+g.ID, g.ID, "ready")
	if err != nil {
		return core.GoalSnapshot{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO computer_sessions(id,goal_id,status) VALUES(?,?,?)`, "computer-"+g.ID, g.ID, "absent")
	if err != nil {
		return core.GoalSnapshot{}, err
	}
	if err = tx.Commit(); err != nil {
		return core.GoalSnapshot{}, err
	}
	return s.GetGoalSnapshot(ctx, g.ID)
}

func scanGoal(row *sql.Row) (core.Goal, error) {
	var g core.Goal
	var criteria, caps, created string
	err := row.Scan(&g.ID, &g.Objective, &criteria, &g.AllowedRoot, &g.ArtifactPath, &g.CheckIntervalSeconds, &caps, &g.Status,
		&g.CurrentArtifactID, &g.CriteriaRevision, &g.DependencyRevision, &g.Revision, &g.Reason, &created, &g.SourceSessionID)
	if err != nil {
		return g, err
	}
	if err = json.Unmarshal([]byte(criteria), &g.Criteria); err != nil {
		return g, err
	}
	if err = json.Unmarshal([]byte(caps), &g.AllowedCapabilities); err != nil {
		return g, err
	}
	g.CreatedAt = parseTime(created)
	return g, nil
}

const goalSelect = `SELECT id,objective,criteria_json,allowed_root,artifact_path,check_interval_seconds,allowed_capabilities_json,status,current_artifact_id,criteria_revision,dependency_revision,revision,reason,created_at,source_session_id FROM goals WHERE id=?`

func (s *Store) UpdateStatus(ctx context.Context, id string, expected int64, status core.GoalStatus, reason string) (core.Goal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.Goal{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE goals SET status=?,reason=?,revision=revision+1 WHERE id=? AND revision=?`, status, reason, id, expected)
	if err != nil {
		return core.Goal{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return core.Goal{}, err
	}
	if n != 1 {
		return core.Goal{}, ErrRevision
	}
	agentStatus := agentStatusFor(status)
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=? WHERE goal_id=?`, agentStatus, id); err != nil {
		return core.Goal{}, err
	}
	if err = tx.Commit(); err != nil {
		return core.Goal{}, err
	}
	return scanGoal(s.db.QueryRowContext(ctx, goalSelect, id))
}

// agentStatusFor maps a goal status to the agent row's status label.
func agentStatusFor(status core.GoalStatus) string {
	switch status {
	case core.GoalActive:
		return "running"
	case core.GoalNeedsHuman:
		return "needs_human"
	case core.GoalVerified:
		return "finished"
	default:
		return "waiting"
	}
}

func (s *Store) SetCurrentArtifact(ctx context.Context, id, digest string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE goals SET current_artifact_id=?, status=CASE WHEN current_artifact_id<>? THEN 'active' ELSE status END, revision=revision+1 WHERE id=? AND current_artifact_id<>?`, digest, digest, id, digest)
	return err
}

func (s *Store) GetGoalSnapshot(ctx context.Context, id string) (core.GoalSnapshot, error) {
	var out core.GoalSnapshot
	var err error
	out.Goal, err = scanGoal(s.db.QueryRowContext(ctx, goalSelect, id))
	if err != nil {
		return out, err
	}
	var wake sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT id,goal_id,status,last_decision_id,next_wake_at FROM agents WHERE goal_id=?`, id).
		Scan(&out.Agent.ID, &out.Agent.GoalID, &out.Agent.Status, &out.Agent.LastDecisionID, &wake)
	if err != nil {
		return out, err
	}
	if wake.Valid {
		t := parseTime(wake.String)
		out.Agent.NextWakeAt = &t
	}
	err = s.db.QueryRowContext(ctx, `SELECT id,goal_id,status,generation,opened_artifact_id,last_observation_id,runtime_handle FROM computer_sessions WHERE goal_id=?`, id).
		Scan(&out.Session.ID, &out.Session.GoalID, &out.Session.Status, &out.Session.Generation, &out.Session.OpenedArtifactID, &out.Session.LastObservationID, &out.Session.RuntimeHandle)
	if err != nil {
		return out, err
	}
	if out.Events, err = s.events(ctx, id); err != nil {
		return out, err
	}
	if out.Observations, err = s.observations(ctx, id); err != nil {
		return out, err
	}
	if out.Decisions, err = s.decisions(ctx, id); err != nil {
		return out, err
	}
	if out.ModelCalls, err = s.modelCalls(ctx, id); err != nil {
		return out, err
	}
	if out.Actions, err = s.actions(ctx, id); err != nil {
		return out, err
	}
	if out.Evidence, err = s.evidence(ctx, id); err != nil {
		return out, err
	}
	if out.Conversation, err = s.GoalMessages(ctx, id); err != nil {
		return out, err
	}
	if out.Proposals, err = s.GoalProposals(ctx, id); err != nil {
		return out, err
	}
	if out.Dependencies, err = s.dependencies(ctx, id); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Store) ReconcileDependencies(ctx context.Context, goalID string, snapshots []core.DependencySnapshot) (core.DependencyRefresh, error) {
	ordered := append([]core.DependencySnapshot(nil), snapshots...)
	if len(ordered) != 2 {
		return core.DependencyRefresh{}, fmt.Errorf("expected exactly two dependency snapshots, got %d", len(ordered))
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Family < ordered[j].Family })
	if ordered[0].Family != core.CheckFamilyERC || ordered[1].Family != core.CheckFamilyConnection {
		return core.DependencyRefresh{}, fmt.Errorf("dependency snapshots must contain one ERC and one connection family")
	}
	for i := range ordered {
		if ordered[i].SchemaVersion != 1 || ordered[i].Fingerprint == "" {
			return core.DependencyRefresh{}, fmt.Errorf("dependency snapshot %s requires schema version 1 and a fingerprint", ordered[i].Family)
		}
		sort.Slice(ordered[i].Sources, func(a, b int) bool {
			if ordered[i].Sources[a].Kind == ordered[i].Sources[b].Kind {
				return ordered[i].Sources[a].Identity < ordered[i].Sources[b].Identity
			}
			return ordered[i].Sources[a].Kind < ordered[i].Sources[b].Kind
		})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.DependencyRefresh{}, err
	}
	defer tx.Rollback()
	var revision int64
	if err = tx.QueryRowContext(ctx, `SELECT dependency_revision FROM goals WHERE id=?`, goalID).Scan(&revision); err != nil {
		return core.DependencyRefresh{}, fmt.Errorf("goal %s: %w", goalID, err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT family,snapshot_json FROM goal_dependencies WHERE goal_id=?`, goalID)
	if err != nil {
		return core.DependencyRefresh{}, err
	}
	previous := map[core.CheckFamily]core.DependencySnapshot{}
	for rows.Next() {
		var family core.CheckFamily
		var rawSnapshot string
		var snapshot core.DependencySnapshot
		if err = rows.Scan(&family, &rawSnapshot); err != nil {
			rows.Close()
			return core.DependencyRefresh{}, err
		}
		if err = json.Unmarshal([]byte(rawSnapshot), &snapshot); err != nil {
			rows.Close()
			return core.DependencyRefresh{}, fmt.Errorf("decode existing dependency %s for goal %s: %w", family, goalID, err)
		}
		previous[family] = snapshot
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return core.DependencyRefresh{}, err
	}
	rows.Close()

	changed := make([]core.CheckFamily, 0, 2)
	if len(previous) > 0 {
		for _, snapshot := range ordered {
			old, ok := previous[snapshot.Family]
			if !ok || !sameDependencySnapshot(old, snapshot) {
				changed = append(changed, snapshot.Family)
			}
		}
	}
	for _, snapshot := range ordered {
		if _, err = tx.ExecContext(ctx, `INSERT INTO goal_dependencies(goal_id,family,snapshot_json) VALUES(?,?,?)
			ON CONFLICT(goal_id,family) DO UPDATE SET snapshot_json=excluded.snapshot_json`, goalID, snapshot.Family, encode(snapshot)); err != nil {
			return core.DependencyRefresh{}, err
		}
	}
	var event *core.Event
	if len(changed) > 0 {
		revision++
		reason := dependencyChangeReason(changed, ordered)
		if _, err = tx.ExecContext(ctx, `UPDATE goals SET dependency_revision=?,status=?,reason=?,revision=revision+1 WHERE id=?`, revision, core.GoalPendingReverification, reason, goalID); err != nil {
			return core.DependencyRefresh{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=? WHERE goal_id=?`, agentStatusFor(core.GoalPendingReverification), goalID); err != nil {
			return core.DependencyRefresh{}, err
		}
		for _, family := range changed {
			if err = invalidateDependencyEvidence(ctx, tx, goalID, family, reason); err != nil {
				return core.DependencyRefresh{}, err
			}
		}
		eventID := fmt.Sprintf("dependency-change-%s-%d", goalID, revision)
		payload := encode(map[string]any{"families": changed, "dependency_revision": revision})
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO events(id,goal_id,kind,payload_json,received_at,status) VALUES(?,?,?,?,?,'pending')`,
			eventID, goalID, core.EventKindDependencyChange, payload, now()); err != nil {
			return core.DependencyRefresh{}, err
		}
		event = &core.Event{ID: eventID, GoalID: goalID, Kind: core.EventKindDependencyChange, Payload: json.RawMessage(payload), ReceivedAt: time.Now().UTC(), Status: "pending"}
	}
	if err = tx.Commit(); err != nil {
		return core.DependencyRefresh{}, err
	}
	snapshot, err := s.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return core.DependencyRefresh{}, err
	}
	return core.DependencyRefresh{ChangedFamilies: changed, DependencyRevision: revision, Event: event, Snapshot: snapshot}, nil
}

func sameDependencySnapshot(a, b core.DependencySnapshot) bool {
	return a.SchemaVersion == b.SchemaVersion && a.Family == b.Family && a.CheckerID == b.CheckerID &&
		a.CheckerVersion == b.CheckerVersion && a.Fingerprint == b.Fingerprint && a.Available == b.Available && reflect.DeepEqual(a.Sources, b.Sources)
}

func dependencyChangeReason(changed []core.CheckFamily, snapshots []core.DependencySnapshot) string {
	parts := make([]string, 0, len(changed))
	for _, family := range changed {
		for _, snapshot := range snapshots {
			if snapshot.Family != family {
				continue
			}
			detail := string(family) + " 工程依赖发生变化"
			if !snapshot.Available && snapshot.Reason != "" {
				detail += "，当前不可用：" + snapshot.Reason
			}
			parts = append(parts, detail)
		}
	}
	return strings.Join(parts, "；") + "；目标待复核"
}

func invalidateDependencyEvidence(ctx context.Context, tx *sql.Tx, goalID string, family core.CheckFamily, reason string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,kind,provenance FROM evidence WHERE goal_id=? AND invalidated_reason=''`, goalID)
	if err != nil {
		return err
	}
	type evidenceRow struct {
		id, kind   string
		provenance sql.NullString
	}
	var matches []evidenceRow
	for rows.Next() {
		var row evidenceRow
		if err = rows.Scan(&row.id, &row.kind, &row.provenance); err != nil {
			rows.Close()
			return err
		}
		var provenance core.EvidenceProvenance
		if row.provenance.Valid && row.provenance.String != "" {
			_ = json.Unmarshal([]byte(row.provenance.String), &provenance)
		}
		rowFamily := provenance.Family
		if rowFamily == "" {
			switch row.kind {
			case "kicad.erc":
				rowFamily = core.CheckFamilyERC
			case "sensor.connection_present":
				rowFamily = core.CheckFamilyConnection
			}
		}
		if rowFamily == family {
			matches = append(matches, row)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, row := range matches {
		if _, err = tx.ExecContext(ctx, `UPDATE evidence SET invalidated_reason=? WHERE id=? AND invalidated_reason=''`, reason, row.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) dependencies(ctx context.Context, goalID string) ([]core.DependencySnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT snapshot_json FROM goal_dependencies WHERE goal_id=? ORDER BY family`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.DependencySnapshot
	for rows.Next() {
		var rawSnapshot string
		var snapshot core.DependencySnapshot
		if err = rows.Scan(&rawSnapshot); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(rawSnapshot), &snapshot); err != nil {
			return nil, fmt.Errorf("decode dependency snapshot for goal %s: %w", goalID, err)
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}

func (s *Store) InsertEventIfAbsent(ctx context.Context, e core.Event) (core.Event, bool, error) {
	if e.ID == "" || e.GoalID == "" {
		return e, false, errors.New("event ID and goal ID required")
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now().UTC()
	}
	if e.Status == "" {
		e.Status = "pending"
	}
	r, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO events(id,goal_id,kind,payload_json,received_at,status) VALUES(?,?,?,?,?,?)`,
		e.ID, e.GoalID, e.Kind, raw(e.Payload), e.ReceivedAt.Format(time.RFC3339Nano), e.Status)
	if err != nil {
		return e, false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return e, false, err
	}
	var payload, received string
	err = s.db.QueryRowContext(ctx, `SELECT id,goal_id,kind,payload_json,received_at,status FROM events WHERE id=?`, e.ID).
		Scan(&e.ID, &e.GoalID, &e.Kind, &payload, &received, &e.Status)
	if err != nil {
		return e, false, err
	}
	e.Payload = json.RawMessage(payload)
	e.ReceivedAt = parseTime(received)
	return e, n == 1, nil
}

func (s *Store) PendingEvents(ctx context.Context) ([]core.Event, error) {
	return s.queryEvents(ctx, `SELECT id,goal_id,kind,payload_json,received_at,status FROM events WHERE status='pending' ORDER BY received_at,id`)
}

// UnprocessedEvents returns every event that still needs delivery or whose
// processing never completed: both 'pending' and 'signaled' rows. Delivery and
// dedup key off the event ID (the primary key); 'processed' rows never return.
func (s *Store) UnprocessedEvents(ctx context.Context) ([]core.Event, error) {
	return s.queryEvents(ctx, `SELECT id,goal_id,kind,payload_json,received_at,status FROM events WHERE status IN ('pending','signaled') GROUP BY id ORDER BY received_at,id`)
}

func (s *Store) SetEventStatus(ctx context.Context, id, status string) error {
	if status != "signaled" && status != "processed" {
		return errors.New("invalid event status")
	}
	r, err := s.db.ExecContext(ctx, `UPDATE events SET status=? WHERE id=? AND (status=? OR (status='pending' AND ?='signaled') OR (status='signaled' AND ?='processed'))`, status, id, status, status, status)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return errors.New("event transition rejected")
	}
	return nil
}

func (s *Store) RecordObservation(ctx context.Context, o core.Observation) error {
	if o.ObservedAt.IsZero() {
		o.ObservedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO observations(id,goal_id,event_id,artifact_id,computer_session_id,facts_json,observed_at) VALUES(?,?,?,?,?,?,?)`,
		o.ID, o.GoalID, o.EventID, o.ArtifactID, o.ComputerSessionID, raw(o.Facts), o.ObservedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) RecordDecision(ctx context.Context, d core.Decision) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO decisions(id,agent_id,observation_id,proposal_json,model_run_id,model_call_id,criteria_revision,dependency_revision,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		d.ID, d.AgentID, d.ObservationID, encode(d.Proposal), d.ModelRunID, d.ModelCallID, revisionPtr(d.CriteriaRevision), revision64Ptr(d.DependencyRevision), d.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET last_decision_id=? WHERE id=?`, d.ID, d.AgentID); err != nil {
		return err
	}
	return tx.Commit()
}

// revisionPtr converts a nullable criteria revision to a driver value.
func revisionPtr(rev *int) any {
	if rev == nil {
		return nil
	}
	return *rev
}

func revision64Ptr(rev *int64) any {
	if rev == nil {
		return nil
	}
	return *rev
}

func (s *Store) StartModelCall(ctx context.Context, c core.ModelCall) error {
	if c.StartedAt.IsZero() {
		c.StartedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO model_calls(id,goal_id,observation_id,provider,model,host,status,started_at) VALUES(?,?,?,?,?,?,?,?)`, c.ID, c.GoalID, c.ObservationID, c.Provider, c.Model, c.Host, "started", c.StartedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) FinishModelCall(ctx context.Context, id, status, requestID, errorKind string) error {
	if status != "succeeded" && status != "failed" && status != "interrupted" {
		return errors.New("invalid model call status")
	}
	r, err := s.db.ExecContext(ctx, `UPDATE model_calls SET status=?,provider_request_id=?,error_kind=?,finished_at=? WHERE id=? AND status='started'`, status, requestID, errorKind, now(), id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("model call transition rejected")
	}
	return nil
}

func (s *Store) InterruptStartedModelCalls(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE model_calls SET status='interrupted',error_kind='process_interrupted',finished_at=? WHERE status='started'`, now())
	return err
}

func (s *Store) modelCalls(ctx context.Context, id string) ([]core.ModelCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,goal_id,observation_id,provider,model,host,provider_request_id,status,error_kind,started_at,finished_at FROM model_calls WHERE goal_id=? ORDER BY started_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.ModelCall
	for rows.Next() {
		var c core.ModelCall
		var start string
		var finish sql.NullString
		if err = rows.Scan(&c.ID, &c.GoalID, &c.ObservationID, &c.Provider, &c.Model, &c.Host, &c.ProviderRequestID, &c.Status, &c.ErrorKind, &start, &finish); err != nil {
			return nil, err
		}
		c.StartedAt = parseTime(start)
		if finish.Valid {
			t := parseTime(finish.String)
			c.FinishedAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

var ErrStaleDecision = errors.New("decision version is unknown or does not match the goal's current criteria and dependency revisions")

func (s *Store) ReserveAction(ctx context.Context, a core.ActionRecord) (core.ActionRecord, error) {
	if a.ID == "" || a.DecisionID == "" {
		return a, errors.New("action ID and decision ID required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	// Guard: the decision must carry a criteria revision matching the goal's
	// current one. Legacy (NULL) or outdated decisions cannot drive new actions.
	var decisionRevision, decisionDependencyRevision sql.NullInt64
	var goalRevision int
	var goalDependencyRevision int64
	err = tx.QueryRowContext(ctx, `SELECT d.criteria_revision, g.criteria_revision, d.dependency_revision, g.dependency_revision FROM decisions d
		JOIN agents ag ON ag.id=d.agent_id JOIN goals g ON g.id=ag.goal_id WHERE d.id=?`, a.DecisionID).
		Scan(&decisionRevision, &goalRevision, &decisionDependencyRevision, &goalDependencyRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("decision %s: not found", a.DecisionID)
	}
	if err != nil {
		return a, err
	}
	if !decisionRevision.Valid || int(decisionRevision.Int64) != goalRevision || !decisionDependencyRevision.Valid || decisionDependencyRevision.Int64 != goalDependencyRevision {
		return a, ErrStaleDecision
	}
	r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO actions(id,decision_id,expected_artifact_id,desired_postcondition_json,status,result_artifact_id,reason) VALUES(?,?,?,?,?,?,?)`,
		a.ID, a.DecisionID, a.ExpectedArtifactID, raw(a.DesiredPostcondition), "prepared", "", "")
	if err != nil {
		return a, err
	}
	n, _ := r.RowsAffected()
	if n == 1 {
		_, err = tx.ExecContext(ctx, `UPDATE goals SET status='active',revision=revision+1 WHERE id=(SELECT goal_id FROM agents WHERE id=(SELECT agent_id FROM decisions WHERE id=?))`, a.DecisionID)
		if err != nil {
			return a, err
		}
	}
	var post string
	err = tx.QueryRowContext(ctx, `SELECT id,decision_id,expected_artifact_id,desired_postcondition_json,status,result_artifact_id,reason FROM actions WHERE id=?`, a.ID).
		Scan(&a.ID, &a.DecisionID, &a.ExpectedArtifactID, &post, &a.Status, &a.ResultArtifactID, &a.Reason)
	if err != nil {
		return a, err
	}
	a.DesiredPostcondition = json.RawMessage(post)
	if err = tx.Commit(); err != nil {
		return a, err
	}
	return a, nil
}

func (s *Store) SetActionResult(ctx context.Context, id, status, digest, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions SET status=?,result_artifact_id=?,reason=? WHERE id=?`, status, digest, reason, id)
	return err
}

func (s *Store) RecordEvidence(ctx context.Context, e core.Evidence) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO evidence(id,goal_id,criterion_id,artifact_id,kind,result,report_path,criteria_revision,provenance,invalidated_reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.GoalID, e.CriterionID, e.ArtifactID, e.Kind, e.Result, e.ReportPath, revisionPtr(e.CriteriaRevision), provenanceJSON(e.Provenance), e.InvalidatedReason, e.CreatedAt.Format(time.RFC3339Nano))
	return err
}

// provenanceJSON serializes provenance; nil stays NULL so legacy-shaped rows
// keep reading as unknown and are never backfilled.
func provenanceJSON(p *core.EvidenceProvenance) any {
	if p == nil {
		return nil
	}
	return encode(p)
}

func (s *Store) InvalidateEvidence(ctx context.Context, goalID, currentDigest string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE evidence SET result='stale' WHERE goal_id=? AND artifact_id<>? AND result='pass'`, goalID, currentDigest)
	return err
}

func (s *Store) UpsertSession(ctx context.Context, c core.ComputerSession) error {
	r, err := s.db.ExecContext(ctx, `INSERT INTO computer_sessions(id,goal_id,status,generation,opened_artifact_id,last_observation_id,runtime_handle)
        VALUES(?,?,?,?,?,?,?) ON CONFLICT(goal_id) DO UPDATE SET status=excluded.status,generation=excluded.generation,
        opened_artifact_id=excluded.opened_artifact_id,last_observation_id=excluded.last_observation_id,runtime_handle=excluded.runtime_handle
        WHERE excluded.id=computer_sessions.id AND excluded.generation>=computer_sessions.generation`,
		c.ID, c.GoalID, c.Status, c.Generation, c.OpenedArtifactID, c.LastObservationID, c.RuntimeHandle)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return errors.New("stale computer session generation")
	}
	return nil
}

func (s *Store) events(ctx context.Context, id string) ([]core.Event, error) {
	return s.queryEvents(ctx, `SELECT id,goal_id,kind,payload_json,received_at,status FROM events WHERE goal_id=? ORDER BY received_at,id`, id)
}
func (s *Store) queryEvents(ctx context.Context, q string, args ...any) ([]core.Event, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Event
	for rows.Next() {
		var e core.Event
		var p, t string
		if err = rows.Scan(&e.ID, &e.GoalID, &e.Kind, &p, &t, &e.Status); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(p)
		e.ReceivedAt = parseTime(t)
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) observations(ctx context.Context, id string) ([]core.Observation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,goal_id,event_id,artifact_id,computer_session_id,facts_json,observed_at FROM observations WHERE goal_id=? ORDER BY observed_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Observation
	for rows.Next() {
		var o core.Observation
		var f, t string
		if err = rows.Scan(&o.ID, &o.GoalID, &o.EventID, &o.ArtifactID, &o.ComputerSessionID, &f, &t); err != nil {
			return nil, err
		}
		o.Facts = json.RawMessage(f)
		o.ObservedAt = parseTime(t)
		out = append(out, o)
	}
	return out, rows.Err()
}
func (s *Store) decisions(ctx context.Context, id string) ([]core.Decision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.agent_id,d.observation_id,d.proposal_json,d.model_run_id,d.model_call_id,d.criteria_revision,d.dependency_revision,d.created_at FROM decisions d JOIN agents a ON a.id=d.agent_id WHERE a.goal_id=? ORDER BY d.created_at,d.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Decision
	for rows.Next() {
		var d core.Decision
		var p, t string
		var rev, dependencyRevision sql.NullInt64
		if err = rows.Scan(&d.ID, &d.AgentID, &d.ObservationID, &p, &d.ModelRunID, &d.ModelCallID, &rev, &dependencyRevision, &t); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(p), &d.Proposal); err != nil {
			return nil, err
		}
		d.CreatedAt = parseTime(t)
		if rev.Valid {
			r := int(rev.Int64)
			d.CriteriaRevision = &r
		}
		if dependencyRevision.Valid {
			r := dependencyRevision.Int64
			d.DependencyRevision = &r
		}
		if d.ModelCallID == "" {
			d.ModelInfo = "unknown (legacy decision)"
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) actions(ctx context.Context, id string) ([]core.ActionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT x.id,x.decision_id,x.expected_artifact_id,x.desired_postcondition_json,x.status,x.result_artifact_id,x.reason FROM actions x JOIN decisions d ON d.id=x.decision_id JOIN agents a ON a.id=d.agent_id WHERE a.goal_id=? ORDER BY d.created_at,x.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.ActionRecord
	for rows.Next() {
		var a core.ActionRecord
		var p string
		if err = rows.Scan(&a.ID, &a.DecisionID, &a.ExpectedArtifactID, &p, &a.Status, &a.ResultArtifactID, &a.Reason); err != nil {
			return nil, err
		}
		a.DesiredPostcondition = json.RawMessage(p)
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) evidence(ctx context.Context, id string) ([]core.Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,goal_id,criterion_id,artifact_id,kind,result,report_path,criteria_revision,provenance,invalidated_reason,created_at FROM evidence WHERE goal_id=? ORDER BY created_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Evidence
	for rows.Next() {
		var e core.Evidence
		var t string
		var rev sql.NullInt64
		var prov sql.NullString
		if err = rows.Scan(&e.ID, &e.GoalID, &e.CriterionID, &e.ArtifactID, &e.Kind, &e.Result, &e.ReportPath, &rev, &prov, &e.InvalidatedReason, &t); err != nil {
			return nil, err
		}
		if rev.Valid {
			r := int(rev.Int64)
			e.CriteriaRevision = &r
		}
		if prov.Valid && prov.String != "" {
			var p core.EvidenceProvenance
			if err = json.Unmarshal([]byte(prov.String), &p); err != nil {
				return nil, err
			}
			e.Provenance = &p
		}
		e.CreatedAt = parseTime(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) EnsureGoal(ctx context.Context, id string) error {
	var found string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM goals WHERE id=?`, id).Scan(&found)
	if err != nil {
		return fmt.Errorf("goal %s: %w", id, err)
	}
	return nil
}

func (s *Store) ListGoals(ctx context.Context) ([]core.Goal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,objective,criteria_json,allowed_root,artifact_path,check_interval_seconds,allowed_capabilities_json,status,current_artifact_id,criteria_revision,dependency_revision,revision,reason,created_at,source_session_id FROM goals ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Goal
	for rows.Next() {
		var g core.Goal
		var criteria, caps, created string
		if err = rows.Scan(&g.ID, &g.Objective, &criteria, &g.AllowedRoot, &g.ArtifactPath, &g.CheckIntervalSeconds, &caps, &g.Status,
			&g.CurrentArtifactID, &g.CriteriaRevision, &g.DependencyRevision, &g.Revision, &g.Reason, &created, &g.SourceSessionID); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(criteria), &g.Criteria); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(caps), &g.AllowedCapabilities); err != nil {
			return nil, err
		}
		g.CreatedAt = parseTime(created)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) InsertMessage(ctx context.Context, m core.SessionMessage) (core.SessionMessage, error) {
	if m.ID == "" {
		return m, errors.New("message ID required")
	}
	switch m.Role {
	case core.MessageRoleUser, core.MessageRoleAgent, core.MessageRoleSystem:
	default:
		return m, fmt.Errorf("invalid message role %q", m.Role)
	}
	switch m.Kind {
	case core.MessageKindText, core.MessageKindQuestion, core.MessageKindReply, core.MessageKindCriteriaProposal, core.MessageKindCriteriaConfirm:
	default:
		return m, fmt.Errorf("invalid message kind %q", m.Kind)
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	var goalID any
	if m.GoalID != "" {
		goalID = m.GoalID
	}
	payload := ""
	if len(m.Payload) > 0 {
		payload = string(m.Payload)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_messages(id,goal_id,role,kind,text,payload_json,ref,delivered,created_at) VALUES(?,?,?,?,?,?,?,0,?)`,
		m.ID, goalID, m.Role, m.Kind, m.Text, payload, m.Ref, m.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return m, err
	}
	return m, nil
}

func scanMessage(row interface{ Scan(...any) error }) (core.SessionMessage, error) {
	var m core.SessionMessage
	var goalID, payload, ref sql.NullString
	var delivered int
	var created string
	if err := row.Scan(&m.ID, &goalID, &m.Role, &m.Kind, &m.Text, &payload, &ref, &delivered, &created); err != nil {
		return m, err
	}
	m.Ref = ref.String
	m.GoalID = goalID.String
	if payload.Valid && payload.String != "" {
		m.Payload = json.RawMessage(payload.String)
	}
	m.Delivered = delivered == 1
	m.CreatedAt = parseTime(created)
	return m, nil
}

const messageSelect = `SELECT id,goal_id,role,kind,text,payload_json,ref,delivered,created_at FROM session_messages`

func (s *Store) ListMessages(ctx context.Context) ([]core.SessionMessage, error) {
	return s.queryMessages(ctx, messageSelect+` ORDER BY created_at,id`)
}

func (s *Store) GoalMessages(ctx context.Context, goalID string) ([]core.SessionMessage, error) {
	return s.queryMessages(ctx, messageSelect+` WHERE goal_id=? ORDER BY created_at,id`, goalID)
}

func (s *Store) UndeliveredMessages(ctx context.Context, goalID string) ([]core.SessionMessage, error) {
	return s.queryMessages(ctx, messageSelect+` WHERE goal_id=? AND delivered=0 AND role='user' ORDER BY created_at,id`, goalID)
}

func (s *Store) MarkMessagesDelivered(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE session_messages SET delivered=1 WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// AttachMessagesToGoal moves session-level messages that reference the given
// proposal (e.g. the pre-creation criteria proposal transcript entry) under
// the goal created from its confirmation.
func (s *Store) AttachMessagesToGoal(ctx context.Context, ref, goalID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_messages SET goal_id=? WHERE ref=? AND goal_id IS NULL`, goalID, ref)
	return err
}

// UnansweredQuestion returns the latest agent question that has no later user
// reply; it drives the waiting-for-human workflow state.
func (s *Store) UnansweredQuestion(ctx context.Context, goalID string) (core.SessionMessage, bool, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, messageSelect+` WHERE goal_id=? AND role='agent' AND kind='question'
		AND NOT EXISTS (SELECT 1 FROM session_messages r WHERE r.goal_id=session_messages.goal_id AND r.role='user' AND r.created_at>session_messages.created_at)
		ORDER BY created_at DESC LIMIT 1`, goalID))
	if errors.Is(err, sql.ErrNoRows) {
		return core.SessionMessage{}, false, nil
	}
	if err != nil {
		return core.SessionMessage{}, false, err
	}
	return m, true, nil
}

func (s *Store) queryMessages(ctx context.Context, q string, args ...any) ([]core.SessionMessage, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.SessionMessage
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) InsertProposal(ctx context.Context, p core.CriteriaProposal) (core.CriteriaProposal, error) {
	if p.ID == "" {
		return p, errors.New("proposal ID required")
	}
	if len(p.Criteria) == 0 {
		return p, errors.New("proposal criteria required")
	}
	if p.Status == "" {
		p.Status = core.ProposalPending
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	var goalID any
	if p.GoalID != "" {
		goalID = p.GoalID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO criteria_proposals(id,goal_id,status,criteria_json,raw_text,created_at,session_id) VALUES(?,?,?,?,?,?,?)`,
		p.ID, goalID, p.Status, encode(p.Criteria), p.RawText, p.CreatedAt.Format(time.RFC3339Nano), p.SessionID)
	if err != nil {
		return p, err
	}
	return p, nil
}

func (s *Store) SetProposalStatus(ctx context.Context, id, status string) (core.CriteriaProposal, error) {
	if status != core.ProposalConfirmed && status != core.ProposalRejected {
		return core.CriteriaProposal{}, fmt.Errorf("invalid proposal status %q", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.CriteriaProposal{}, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE criteria_proposals SET status=? WHERE id=? AND status='proposed'`, status, id)
	if err != nil {
		return core.CriteriaProposal{}, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return core.CriteriaProposal{}, errors.New("proposal transition rejected")
	}
	if status == core.ProposalConfirmed {
		if _, err = tx.ExecContext(ctx, `UPDATE criteria_proposals SET status='superseded'
			WHERE status='confirmed' AND goal_id=(SELECT goal_id FROM criteria_proposals WHERE id=?) AND id<>?`, id, id); err != nil {
			return core.CriteriaProposal{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return core.CriteriaProposal{}, err
	}
	return s.GetProposal(ctx, id)
}

// AttachProposalGoal links a session-level proposal to the goal created from
// its confirmation.
func (s *Store) AttachProposalGoal(ctx context.Context, id, goalID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE criteria_proposals SET goal_id=? WHERE id=? AND goal_id IS NULL`, goalID, id)
	return err
}

func (s *Store) GetProposal(ctx context.Context, id string) (core.CriteriaProposal, error) {
	return scanProposal(s.db.QueryRowContext(ctx, `SELECT id,goal_id,status,criteria_json,raw_text,created_at,session_id FROM criteria_proposals WHERE id=?`, id))
}

func (s *Store) GoalProposals(ctx context.Context, goalID string) ([]core.CriteriaProposal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,goal_id,status,criteria_json,raw_text,created_at,session_id FROM criteria_proposals WHERE goal_id=? ORDER BY created_at,id`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.CriteriaProposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanProposal(row interface{ Scan(...any) error }) (core.CriteriaProposal, error) {
	var p core.CriteriaProposal
	var goalID sql.NullString
	var criteria, created string
	if err := row.Scan(&p.ID, &goalID, &p.Status, &criteria, &p.RawText, &created, &p.SessionID); err != nil {
		return p, err
	}
	p.GoalID = goalID.String
	if err := json.Unmarshal([]byte(criteria), &p.Criteria); err != nil {
		return p, err
	}
	p.CreatedAt = parseTime(created)
	return p, nil
}

// UpdateGoalCriteria replaces the goal's accepted criteria and bumps the
// revision; callers must have validated the criteria against the vocabulary.
func (s *Store) UpdateGoalCriteria(ctx context.Context, goalID string, criteria []core.Criterion) (int, error) {
	if err := core.ValidateCriteria(criteria); err != nil {
		return 0, err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE goals SET criteria_json=?,criteria_revision=criteria_revision+1,revision=revision+1 WHERE id=?`,
		encode(criteria), goalID)
	if err != nil {
		return 0, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("goal %s: not found", goalID)
	}
	var revision int
	err = s.db.QueryRowContext(ctx, `SELECT criteria_revision FROM goals WHERE id=?`, goalID).Scan(&revision)
	return revision, err
}

func (s *Store) GoalIDForDecision(ctx context.Context, decisionID string) (string, error) {
	var goalID string
	err := s.db.QueryRowContext(ctx, `SELECT a.goal_id FROM decisions d JOIN agents a ON a.id=d.agent_id WHERE d.id=?`, decisionID).Scan(&goalID)
	return goalID, err
}

// CriteriaConfirmation is the result of ConfirmGoalCriteria: the confirmed
// proposal, the goal as updated by the same transaction, and the single
// criteria_updated wake event persisted with them.
type CriteriaConfirmation struct {
	Proposal core.CriteriaProposal
	Goal     core.Goal
	Event    core.Event
}

// ConfirmGoalCriteria atomically confirms a pending proposal for an existing
// goal: in one transaction it confirms the proposal (superseding previously
// confirmed ones), bumps the goal's criteria revision, moves the goal to
// pending_reverification, records the invalidation reason on the goal's
// not-yet-invalidated evidence, and inserts the wake event. An invalid,
// missing, unattached, or already-confirmed proposal returns an error and
// leaves the goal untouched; a repeated confirmation never yields a second
// event.
func (s *Store) ConfirmGoalCriteria(ctx context.Context, proposalID string) (CriteriaConfirmation, error) {
	var out CriteriaConfirmation
	p, err := s.GetProposal(ctx, proposalID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("proposal %s: not found", proposalID)
	}
	if err != nil {
		return out, err
	}
	if p.GoalID == "" {
		return out, fmt.Errorf("proposal %s: not attached to a goal", proposalID)
	}
	if p.Status != core.ProposalPending {
		return out, fmt.Errorf("proposal %s: status %q is not confirmable", proposalID, p.Status)
	}
	if err = core.ValidateCriteria(p.Criteria); err != nil {
		return out, fmt.Errorf("proposal %s: %w", proposalID, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE criteria_proposals SET status='confirmed' WHERE id=? AND status='proposed'`, proposalID)
	if err != nil {
		return out, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return out, fmt.Errorf("proposal %s: concurrent confirmation rejected", proposalID)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE criteria_proposals SET status='superseded' WHERE status='confirmed' AND goal_id=? AND id<>?`, p.GoalID, proposalID); err != nil {
		return out, err
	}
	var oldRevision int
	err = tx.QueryRowContext(ctx, `SELECT criteria_revision FROM goals WHERE id=?`, p.GoalID).Scan(&oldRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("goal %s: not found", p.GoalID)
	}
	if err != nil {
		return out, err
	}
	newRevision := oldRevision + 1
	goalReason := fmt.Sprintf("已确认新标准（提案 %s，标准版本 v%d），待完整复核", proposalID, newRevision)
	r, err = tx.ExecContext(ctx, `UPDATE goals SET criteria_json=?,criteria_revision=criteria_revision+1,revision=revision+1,status=?,reason=? WHERE id=?`,
		encode(p.Criteria), core.GoalPendingReverification, goalReason, p.GoalID)
	if err != nil {
		return out, err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return out, fmt.Errorf("goal %s: not found", p.GoalID)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=? WHERE goal_id=?`, agentStatusFor(core.GoalPendingReverification), p.GoalID); err != nil {
		return out, err
	}
	invalidateReason := fmt.Sprintf("标准于确认提案 %s 后升级到 v%d，原结论需完整复核", proposalID, newRevision)
	if _, err = tx.ExecContext(ctx, `UPDATE evidence SET invalidated_reason=? WHERE goal_id=? AND invalidated_reason=''`, invalidateReason, p.GoalID); err != nil {
		return out, err
	}
	// The event ID is derived from the proposal ID, so crash replays and
	// duplicate delivery collapse onto one row.
	eventID := "criteria-confirm-" + proposalID
	payload := encode(map[string]any{"proposal_id": proposalID, "criteria_revision": newRevision})
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO events(id,goal_id,kind,payload_json,received_at,status) VALUES(?,?,?,?,?,'pending')`,
		eventID, p.GoalID, core.EventKindCriteriaUpdate, payload, now()); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	if out.Proposal, err = s.GetProposal(ctx, proposalID); err != nil {
		return CriteriaConfirmation{}, err
	}
	if out.Goal, err = scanGoal(s.db.QueryRowContext(ctx, goalSelect, p.GoalID)); err != nil {
		return CriteriaConfirmation{}, err
	}
	var eventPayload, received string
	out.Event = core.Event{ID: eventID}
	err = s.db.QueryRowContext(ctx, `SELECT goal_id,kind,payload_json,received_at,status FROM events WHERE id=?`, eventID).
		Scan(&out.Event.GoalID, &out.Event.Kind, &eventPayload, &received, &out.Event.Status)
	if err != nil {
		return CriteriaConfirmation{}, err
	}
	out.Event.Payload = json.RawMessage(eventPayload)
	out.Event.ReceivedAt = parseTime(received)
	return out, nil
}

// CommitVerification stores every evidence row from one reverification round
// and only lets the round change the goal's current conclusion when the token
// still matches (criteria revision AND current artifact ID). On a match the
// goal becomes verified when every criterion passed, otherwise active with the
// unmet criteria recorded as the reason. On a mismatch the round's evidence is
// kept as history with an invalidation reason, the goal is left untouched, and
// current=false is returned.
func (s *Store) CommitVerification(ctx context.Context, token core.VerificationToken, result core.VerificationResult) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var goalRevision int
	var dependencyRevision int64
	var artifactID string
	err = tx.QueryRowContext(ctx, `SELECT criteria_revision,current_artifact_id,dependency_revision FROM goals WHERE id=?`, token.GoalID).Scan(&goalRevision, &artifactID, &dependencyRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("goal %s: not found", token.GoalID)
	}
	if err != nil {
		return false, err
	}
	current := goalRevision == token.CriteriaRevision && artifactID == token.ArtifactID && dependencyRevision == token.DependencyRevision
	staleReason := ""
	if !current {
		staleReason = fmt.Sprintf("复核令牌过期：提交时目标标准版本为 v%d、产物为 %s、依赖代次为 %d（令牌为 v%d、%s、依赖代次 %d），本轮结果仅保留为历史",
			goalRevision, artifactID, dependencyRevision, token.CriteriaRevision, token.ArtifactID, token.DependencyRevision)
	}
	for _, e := range result.Evidence {
		if e.GoalID == "" {
			e.GoalID = token.GoalID
		}
		if e.CreatedAt.IsZero() {
			e.CreatedAt = time.Now().UTC()
		}
		if !current {
			e.InvalidatedReason = staleReason
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO evidence(id,goal_id,criterion_id,artifact_id,kind,result,report_path,criteria_revision,provenance,invalidated_reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, e.GoalID, e.CriterionID, e.ArtifactID, e.Kind, e.Result, e.ReportPath, revisionPtr(e.CriteriaRevision), provenanceJSON(e.Provenance), e.InvalidatedReason, e.CreatedAt.Format(time.RFC3339Nano)); err != nil {
			return false, err
		}
	}
	if current {
		status, reason, err := verificationConclusion(ctx, tx, token.GoalID)
		if err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE goals SET status=?,reason=?,revision=revision+1 WHERE id=?`, status, reason, token.GoalID); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=? WHERE goal_id=?`, agentStatusFor(status), token.GoalID); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return current, nil
}

func verificationConclusion(ctx context.Context, tx *sql.Tx, goalID string) (core.GoalStatus, string, error) {
	goal, err := scanGoal(tx.QueryRowContext(ctx, goalSelect, goalID))
	if err != nil {
		return "", "", err
	}
	dependencies, err := dependenciesFromQuery(ctx, tx, goalID)
	if err != nil {
		return "", "", err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,goal_id,criterion_id,artifact_id,kind,result,report_path,criteria_revision,provenance,invalidated_reason,created_at FROM evidence WHERE goal_id=? ORDER BY created_at,id`, goalID)
	if err != nil {
		return "", "", err
	}
	latest := map[string]core.Evidence{}
	for rows.Next() {
		var evidence core.Evidence
		var timestamp string
		var criteriaRevision sql.NullInt64
		var provenance sql.NullString
		if err = rows.Scan(&evidence.ID, &evidence.GoalID, &evidence.CriterionID, &evidence.ArtifactID, &evidence.Kind, &evidence.Result, &evidence.ReportPath, &criteriaRevision, &provenance, &evidence.InvalidatedReason, &timestamp); err != nil {
			rows.Close()
			return "", "", err
		}
		if criteriaRevision.Valid {
			revision := int(criteriaRevision.Int64)
			evidence.CriteriaRevision = &revision
		}
		if provenance.Valid && provenance.String != "" {
			var p core.EvidenceProvenance
			if err = json.Unmarshal([]byte(provenance.String), &p); err != nil {
				rows.Close()
				return "", "", err
			}
			evidence.Provenance = &p
		}
		evidence.CreatedAt = parseTime(timestamp)
		asPass := evidence
		asPass.Result = "pass"
		if !core.EvidenceCurrentWithDependencies(goal, goal.CurrentArtifactID, dependencies, asPass) {
			continue
		}
		previous, exists := latest[evidence.CriterionID]
		if !exists || evidence.CreatedAt.After(previous.CreatedAt) || (evidence.CreatedAt.Equal(previous.CreatedAt) && evidence.ID > previous.ID) {
			latest[evidence.CriterionID] = evidence
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return "", "", err
	}
	rows.Close()
	unmet := make([]string, 0)
	for _, criterion := range goal.Criteria {
		evidence, ok := latest[criterion.ID]
		if !ok || evidence.Result != "pass" {
			unmet = append(unmet, criterion.ID)
		}
	}
	if len(unmet) == 0 && len(goal.Criteria) > 0 {
		return core.GoalVerified, "", nil
	}
	for _, dependency := range dependencies {
		if !dependency.Available {
			reason := dependency.Reason
			if reason == "" {
				reason = string(dependency.Family) + " 工程依赖不可用"
			}
			return core.GoalPendingReverification, reason + "；目标待复核", nil
		}
	}
	return core.GoalActive, "复核未通过，未满足项：" + strings.Join(unmet, "、"), nil
}

func dependenciesFromQuery(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, goalID string) ([]core.DependencySnapshot, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT snapshot_json FROM goal_dependencies WHERE goal_id=? ORDER BY family`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []core.DependencySnapshot
	for rows.Next() {
		var rawSnapshot string
		var snapshot core.DependencySnapshot
		if err = rows.Scan(&rawSnapshot); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(rawSnapshot), &snapshot); err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

// UpdateStatusForToken applies a status change only while the caller's token
// still matches the goal (criteria revision AND current artifact ID); it
// returns current=false and leaves the status untouched otherwise. When the
// artifact changed while the goal is pending_reverification, the goal stays
// pending and its reason is refreshed to reflect the stale token.
func (s *Store) UpdateStatusForToken(ctx context.Context, token core.VerificationToken, status core.GoalStatus, reason string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var goalRevision int
	var dependencyRevision int64
	var artifactID string
	var goalStatus core.GoalStatus
	err = tx.QueryRowContext(ctx, `SELECT criteria_revision,current_artifact_id,dependency_revision,status FROM goals WHERE id=?`, token.GoalID).Scan(&goalRevision, &artifactID, &dependencyRevision, &goalStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("goal %s: not found", token.GoalID)
	}
	if err != nil {
		return false, err
	}
	if goalRevision != token.CriteriaRevision || artifactID != token.ArtifactID || dependencyRevision != token.DependencyRevision {
		if goalStatus == core.GoalPendingReverification {
			stale := fmt.Sprintf("复核令牌已过期（当前标准版本 v%d、产物 %s、依赖代次 %d），保持待复核", goalRevision, artifactID, dependencyRevision)
			if reason != "" {
				stale = stale + "：" + reason
			}
			if _, err = tx.ExecContext(ctx, `UPDATE goals SET reason=?,revision=revision+1 WHERE id=? AND status=?`, stale, token.GoalID, core.GoalPendingReverification); err != nil {
				return false, err
			}
		}
		if err = tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	r, err := tx.ExecContext(ctx, `UPDATE goals SET status=?,reason=?,revision=revision+1 WHERE id=?`, status, reason, token.GoalID)
	if err != nil {
		return false, err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return false, fmt.Errorf("goal %s: not found", token.GoalID)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=? WHERE goal_id=?`, agentStatusFor(status), token.GoalID); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
