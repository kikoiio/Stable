package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestDeleteEphemeralSessionRecordsKeepsReusableRules(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, sessionID := range []string{"ephemeral", "other"} {
		_, err = s.db.Exec(`INSERT INTO approval_requests(id,run_id,session_id,operation_json,operation_digest,scope_digest,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?)`, "a-"+sessionID, "run", sessionID, `{}`, "op", "scope", "denied", "now", "later")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.db.Exec(`INSERT INTO permission_decisions(run_id,session_id,operation_id,operation_digest,scope_digest,decision,reason,created_at) VALUES(?,?,?,?,?,?,?,?)`, "run", sessionID, "operation", "op", "scope", "deny", "test", "now")
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.db.Exec(`INSERT INTO permission_rules(effect,kind,name,target,parameters_digest,scope_digest,created_at) VALUES('allow','read','read_file','project:x','{}','scope','now')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteEphemeralSessionRecords(ctx, ""); err == nil {
		t.Fatal("empty session ID accepted")
	}
	if err = s.DeleteEphemeralSessionRecords(ctx, "ephemeral"); err != nil {
		t.Fatal(err)
	}
	var approvals, decisions, rules int
	if err = s.db.QueryRow(`SELECT count(*) FROM approval_requests WHERE session_id='ephemeral'`).Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM permission_decisions WHERE session_id='ephemeral'`).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM approval_requests WHERE session_id='other'`).Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if approvals != 1 {
		t.Fatalf("other session approval count = %d", approvals)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM permission_decisions WHERE session_id='other'`).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 {
		t.Fatalf("other session decision count = %d", decisions)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM permission_rules`).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if rules != 1 {
		t.Fatalf("permission rule count = %d", rules)
	}
}

func TestDeleteEphemeralSessionRecordsAllowsNoMatchingRows(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.DeleteEphemeralSessionRecords(context.Background(), "missing-session"); err != nil {
		t.Fatalf("delete with no records: %v", err)
	}
}

func TestDeleteEphemeralSessionRecordsRollsBackOnStatementFailure(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO approval_requests(id,run_id,session_id,operation_json,operation_digest,scope_digest,status,created_at,expires_at) VALUES('approval','run','ephemeral','{}','op','scope','denied','now','later')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO permission_decisions(run_id,session_id,operation_id,operation_digest,scope_digest,decision,reason,created_at) VALUES('run','ephemeral','operation','op','scope','deny','test','now')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`CREATE TRIGGER fail_ephemeral_decision_delete BEFORE DELETE ON permission_decisions BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	err = s.DeleteEphemeralSessionRecords(context.Background(), "ephemeral")
	if err == nil {
		t.Fatal("expected injected transaction failure")
	}
	for _, table := range []string{"approval_requests", "permission_decisions"} {
		var count int
		if err = s.db.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE session_id='ephemeral'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s rows after rollback = %d", table, count)
		}
	}
}
