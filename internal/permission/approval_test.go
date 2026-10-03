package permission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memoryApprovalRepo struct {
	requests  map[string]ApprovalRequest
	consumed  map[string]bool
	rules     []ExactRule
	decisions []PermissionDecision
}

func newMemoryApprovalRepo() *memoryApprovalRepo {
	return &memoryApprovalRepo{requests: map[string]ApprovalRequest{}, consumed: map[string]bool{}}
}
func (m *memoryApprovalRepo) CreateApproval(_ context.Context, r ApprovalRequest) error {
	if _, ok := m.requests[r.ID]; ok {
		return errors.New("duplicate")
	}
	m.requests[r.ID] = r
	return nil
}
func (m *memoryApprovalRepo) GetApproval(_ context.Context, id string) (ApprovalRequest, error) {
	r, ok := m.requests[id]
	if !ok {
		return r, errors.New("missing")
	}
	return r, nil
}
func (m *memoryApprovalRepo) GetApprovalForOperation(_ context.Context, runID, operation, scope string) (ApprovalRequest, bool, error) {
	for _, r := range m.requests {
		if r.RunID == runID && r.OperationDigest == operation && r.ScopeDigest == scope {
			return r, true, nil
		}
	}
	return ApprovalRequest{}, false, nil
}
func (m *memoryApprovalRepo) ResolveApproval(_ context.Context, id string, s ApprovalStatus, scope, op string, rule *ExactRule) error {
	r := m.requests[id]
	if r.Status != ApprovalPending || r.ScopeDigest != scope || r.OperationDigest != op {
		return errors.New("stale")
	}
	r.Status = s
	m.requests[id] = r
	if rule != nil {
		m.rules = append(m.rules, *rule)
	}
	return nil
}
func (m *memoryApprovalRepo) CancelApproval(_ context.Context, id, session string) error {
	r := m.requests[id]
	if r.Status != ApprovalPending || r.SessionID != session {
		return errors.New("stale")
	}
	r.Status = ApprovalCancelled
	m.requests[id] = r
	return nil
}
func (m *memoryApprovalRepo) ConsumeApproval(_ context.Context, id, scope, op string) error {
	r := m.requests[id]
	if r.Status != ApprovalAllowedOnce || r.ScopeDigest != scope || r.OperationDigest != op || m.consumed[id] {
		return errors.New("not consumable")
	}
	m.consumed[id] = true
	return nil
}
func (m *memoryApprovalRepo) RecordPermissionDecision(_ context.Context, d PermissionDecision, _ Authority, _ Operation) error {
	m.decisions = append(m.decisions, d)
	return nil
}

func TestApprovalService(t *testing.T) {
	repo := newMemoryApprovalRepo()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc := PermissionService{Repository: repo, NewID: func() string { return "approval-1" }, Now: func() time.Time { return now }}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	a := Authority{RunID: "run", SessionID: "session", AllowedRoot: project, CandidateRoot: candidate, Mode: ModeDefault}
	o := Operation{ID: "op", Kind: OpWrite, Name: "write", Target: filepath.Join(candidate, "file"), Parameters: []byte(`{"text":"x"}`)}
	d, err := svc.Authorize(context.Background(), a, o)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != DecisionAsk || d.ApprovalID != "approval-1" {
		t.Fatalf("decision=%+v", d)
	}
	principal := UserPrincipal{SessionID: "session", UserID: "local-user", Authenticated: true}
	if _, err = svc.ResolveApproval(context.Background(), d.ApprovalID, ChoiceAllowOnce, principal, a, o); err != nil {
		t.Fatal(err)
	}
	if err = svc.ConsumeApproval(context.Background(), d.ApprovalID, a, o); err != nil {
		t.Fatal(err)
	}
	if err = svc.ConsumeApproval(context.Background(), d.ApprovalID, a, o); err == nil {
		t.Fatal("one-shot approval consumed twice")
	}
}

func TestApprovalScopeAndPrincipal(t *testing.T) {
	repo := newMemoryApprovalRepo()
	now := time.Now()
	svc := PermissionService{Repository: repo, NewID: func() string { return "a" }, Now: func() time.Time { return now }}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	a := Authority{RunID: "run", SessionID: "session", AllowedRoot: project, CandidateRoot: candidate, Mode: ModeDefault}
	o := Operation{ID: "op", Kind: OpCommand, Name: "exec", Parameters: []byte(`{"argv":["true"]}`)}
	d, err := svc.Authorize(context.Background(), a, o)
	if err != nil {
		t.Fatal(err)
	}
	bad := UserPrincipal{SessionID: "other", UserID: "u", Authenticated: true}
	if _, err = svc.ResolveApproval(context.Background(), d.ApprovalID, ChoiceAllowOnce, bad, a, o); err == nil {
		t.Fatal("cross-session approval accepted")
	}
	changed := o
	changed.Parameters = []byte(`{"argv":["false"]}`)
	good := UserPrincipal{SessionID: "session", UserID: "u", Authenticated: true}
	if _, err = svc.ResolveApproval(context.Background(), d.ApprovalID, ChoiceAllowOnce, good, a, changed); err == nil {
		t.Fatal("changed operation reused approval")
	}
}
