package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"stable/internal/permission"
)

func TestPermissionSchema(t *testing.T) {
	s, _ := newGoalStore(t)
	for _, table := range []string{"permission_rules", "approval_requests", "permission_decisions"} {
		var name string
		if err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("missing table %s: %v", table, err)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO approval_requests(id,run_id,session_id,operation_json,operation_digest,scope_digest,status,created_at,expires_at) VALUES('bad','r','s','{}','o','s','nonsense','now','later')`); err == nil {
		t.Fatal("approval status constraint missing")
	}
}

func TestPermissionMigration(t *testing.T) {
	s, path := newGoalStore(t)
	if _, err := s.DB().Exec(`PRAGMA user_version=5`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 11 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var n int
	if err = s.DB().QueryRow(`SELECT count(*) FROM goals WHERE id='goal-1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("old goal count=%d err=%v", n, err)
	}
}

func TestPermissionStore(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	op := permission.Operation{ID: "op", Kind: permission.OpWrite, Name: "write", Target: "/candidate/file", Parameters: []byte(`{"content":"x"}`)}
	r := permission.ApprovalRequest{ID: "approval", RunID: "run", SessionID: "session", GoalID: "goal", WorkItemID: "item", Operation: op, OperationDigest: "op-digest", ScopeDigest: "scope", Status: permission.ApprovalPending, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := s.CreateApproval(ctx, r); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.GetApproval(ctx, r.ID)
	if err != nil || loaded.Operation.Name != op.Name || loaded.Status != permission.ApprovalPending {
		t.Fatalf("approval roundtrip: %+v %v", loaded, err)
	}
	rule := &permission.ExactRule{Effect: permission.EffectAllow, Kind: op.Kind, Name: op.Name, Target: op.Target, ParametersDigest: "parameters", ScopeDigest: "scope"}
	if err = s.ResolveApproval(ctx, r.ID, permission.ApprovalSaved, "scope", "op-digest", rule); err != nil {
		t.Fatal(err)
	}
	rules, err := s.ListExactRules(ctx, "scope")
	if err != nil || len(rules) != 1 || rules[0].Name != op.Name {
		t.Fatalf("rules=%+v err=%v", rules, err)
	}
	if err = s.ResolveApproval(ctx, r.ID, permission.ApprovalAllowedOnce, "scope", "op-digest", nil); err == nil {
		t.Fatal("resolved approval accepted a second transition")
	}
	if err = s.RecordPermissionDecision(ctx, permission.PermissionDecision{Kind: permission.DecisionAllow, Reason: "test", UserID: "local-user", ScopeDigest: "scope", OperationDigest: "op-digest"}, permission.Authority{RunID: "run", SessionID: "session", GoalID: "goal", WorkItemID: "item"}, op); err != nil {
		t.Fatal(err)
	}
	var count int
	var userID string
	if err = s.DB().QueryRow(`SELECT count(*),max(user_id) FROM permission_decisions WHERE run_id='run' AND operation_id='op'`).Scan(&count, &userID); err != nil || count != 1 || userID != "local-user" {
		t.Fatalf("audit count=%d user=%q err=%v", count, userID, err)
	}
}

func TestOneTimeApprovalConcurrentConsume(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r := permission.ApprovalRequest{ID: "once", RunID: "r", SessionID: "s", Operation: permission.Operation{ID: "op", Kind: permission.OpCommand, Name: "exec"}, OperationDigest: "op", ScopeDigest: "scope", Status: permission.ApprovalPending, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := s.CreateApproval(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveApproval(ctx, r.ID, permission.ApprovalAllowedOnce, "scope", "op", nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.ConsumeApproval(ctx, r.ID, "scope", "op") == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("one-time token consumed %d times", success)
	}
}
