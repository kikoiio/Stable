package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
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
	if _, err = db.Exec(`PRAGMA user_version = 2`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
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
		(id,objective,criteria_json,allowed_root,artifact_path,check_interval_seconds,allowed_capabilities_json,status,current_artifact_id,revision,reason,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, g.ID, g.Objective, encode(g.Criteria), g.AllowedRoot, g.ArtifactPath, g.CheckIntervalSeconds,
		encode(g.AllowedCapabilities), g.Status, g.CurrentArtifactID, 1, g.Reason, g.CreatedAt.Format(time.RFC3339Nano))
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
		&g.CurrentArtifactID, &g.CriteriaRevision, &g.Revision, &g.Reason, &created)
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

const goalSelect = `SELECT id,objective,criteria_json,allowed_root,artifact_path,check_interval_seconds,allowed_capabilities_json,status,current_artifact_id,criteria_revision,revision,reason,created_at FROM goals WHERE id=?`

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
	agentStatus := "waiting"
	if status == core.GoalActive {
		agentStatus = "running"
	}
	if status == core.GoalNeedsHuman {
		agentStatus = "needs_human"
	}
	if status == core.GoalVerified {
		agentStatus = "finished"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=? WHERE goal_id=?`, agentStatus, id); err != nil {
		return core.Goal{}, err
	}
	if err = tx.Commit(); err != nil {
		return core.Goal{}, err
	}
	return scanGoal(s.db.QueryRowContext(ctx, goalSelect, id))
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
	return out, nil
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
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO decisions(id,agent_id,observation_id,proposal_json,model_run_id,model_call_id,created_at) VALUES(?,?,?,?,?,?,?)`,
		d.ID, d.AgentID, d.ObservationID, encode(d.Proposal), d.ModelRunID, d.ModelCallID, d.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET last_decision_id=? WHERE id=?`, d.ID, d.AgentID); err != nil {
		return err
	}
	return tx.Commit()
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

func (s *Store) ReserveAction(ctx context.Context, a core.ActionRecord) (core.ActionRecord, error) {
	if a.ID == "" || a.DecisionID == "" {
		return a, errors.New("action ID and decision ID required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
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
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO evidence(id,goal_id,criterion_id,artifact_id,kind,result,report_path,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		e.ID, e.GoalID, e.CriterionID, e.ArtifactID, e.Kind, e.Result, e.ReportPath, e.CreatedAt.Format(time.RFC3339Nano))
	return err
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
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.agent_id,d.observation_id,d.proposal_json,d.model_run_id,d.model_call_id,d.created_at FROM decisions d JOIN agents a ON a.id=d.agent_id WHERE a.goal_id=? ORDER BY d.created_at,d.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Decision
	for rows.Next() {
		var d core.Decision
		var p, t string
		if err = rows.Scan(&d.ID, &d.AgentID, &d.ObservationID, &p, &d.ModelRunID, &d.ModelCallID, &t); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(p), &d.Proposal); err != nil {
			return nil, err
		}
		d.CreatedAt = parseTime(t)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,goal_id,criterion_id,artifact_id,kind,result,report_path,created_at FROM evidence WHERE goal_id=? ORDER BY created_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Evidence
	for rows.Next() {
		var e core.Evidence
		var t string
		if err = rows.Scan(&e.ID, &e.GoalID, &e.CriterionID, &e.ArtifactID, &e.Kind, &e.Result, &e.ReportPath, &t); err != nil {
			return nil, err
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_messages(id,goal_id,role,kind,text,payload_json,delivered,created_at) VALUES(?,?,?,?,?,?,0,?)`,
		m.ID, goalID, m.Role, m.Kind, m.Text, payload, m.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return m, err
	}
	return m, nil
}

func scanMessage(row interface{ Scan(...any) error }) (core.SessionMessage, error) {
	var m core.SessionMessage
	var goalID, payload sql.NullString
	var delivered int
	var created string
	if err := row.Scan(&m.ID, &goalID, &m.Role, &m.Kind, &m.Text, &payload, &delivered, &created); err != nil {
		return m, err
	}
	m.GoalID = goalID.String
	if payload.Valid && payload.String != "" {
		m.Payload = json.RawMessage(payload.String)
	}
	m.Delivered = delivered == 1
	m.CreatedAt = parseTime(created)
	return m, nil
}

const messageSelect = `SELECT id,goal_id,role,kind,text,payload_json,delivered,created_at FROM session_messages`

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
	_, err := s.db.ExecContext(ctx, `INSERT INTO criteria_proposals(id,goal_id,status,criteria_json,raw_text,created_at) VALUES(?,?,?,?,?,?)`,
		p.ID, goalID, p.Status, encode(p.Criteria), p.RawText, p.CreatedAt.Format(time.RFC3339Nano))
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
	return scanProposal(s.db.QueryRowContext(ctx, `SELECT id,goal_id,status,criteria_json,raw_text,created_at FROM criteria_proposals WHERE id=?`, id))
}

func (s *Store) GoalProposals(ctx context.Context, goalID string) ([]core.CriteriaProposal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,goal_id,status,criteria_json,raw_text,created_at FROM criteria_proposals WHERE goal_id=? ORDER BY created_at,id`, goalID)
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
	if err := row.Scan(&p.ID, &goalID, &p.Status, &criteria, &p.RawText, &created); err != nil {
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
