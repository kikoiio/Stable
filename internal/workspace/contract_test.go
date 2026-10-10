package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/permission"
)

func testScope() Scope {
	return Scope{ProjectID: "project", SessionID: "session", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session"}}
}

func TestWorkspaceAuthorityCannotWidenProjectRoot(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	defer layout.stateIdentity.Close()
	defer layout.projectIdentity.Close()
	service := &LifecycleService{layout: layout}
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	if err := service.validateProjectAuthority(scope); err != nil {
		t.Fatalf("valid project authority rejected: %v", err)
	}
	scope.Authority.AllowedRoot = filepath.Join(formal, "subdir")
	if err := service.validateProjectAuthority(scope); !errors.Is(err, ErrOwnership) {
		t.Fatalf("narrow execution root widened to full project snapshot: %v", err)
	}
	scope.Authority.AllowedRoot = formal
	scope.Authority.FormalRoot = filepath.Join(parent, "other")
	if err := service.validateProjectAuthority(scope); !errors.Is(err, ErrOwnership) {
		t.Fatalf("different formal project authority accepted: %v", err)
	}
}

func TestScopeAndPublicSerialization(t *testing.T) {
	scope := testScope()
	if err := scope.Validate(); err != nil {
		t.Fatal(err)
	}
	other := scope
	other.SessionID = "other"
	if other.Validate() == nil || scope.SameOwner(other) {
		t.Fatal("cross-session scope accepted")
	}
	goal := scope
	goal.Work.Kind = agent.WorkGoal
	if goal.Validate() == nil {
		t.Fatal("goal without goal ID accepted")
	}
	goal.Work.GoalID = "goal"
	goal.Authority = permission.Authority{RunID: "run", SessionID: "session", GoalID: "goal", AllowedRoot: "/baseline", CandidateRoot: "/checkout", FormalRoot: "/formal"}
	if err := goal.ValidateAuthority(); err != nil {
		t.Fatal(err)
	}
	goal.Authority.SessionID = "other"
	if goal.ValidateAuthority() == nil {
		t.Fatal("authority for another session accepted")
	}
	raw, err := json.Marshal(goal)
	if err != nil || strings.Contains(string(raw), "checkout") || strings.Contains(string(raw), "authority") {
		t.Fatalf("private authority exposed: %s, %v", raw, err)
	}
	lease := WriterLease{WorkspaceID: "work", RunID: "run", Generation: 1, Paths: Paths{Root: "/private-root"}, Authority: goal.Authority}
	raw, err = json.Marshal(lease)
	if err != nil || strings.Contains(string(raw), "private-root") || strings.Contains(string(raw), "checkout") {
		t.Fatalf("private lease paths exposed: %s, %v", raw, err)
	}
}

func TestLabelsIDsAndLimits(t *testing.T) {
	for _, bad := range []string{"", " ", "x\n", strings.Repeat("界", 22), string([]byte{0xff})} {
		if ValidateLabel(bad) == nil {
			t.Errorf("label accepted: %q", bad)
		}
	}
	if err := ValidateLabel("修复 / 表格"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../id", ".", "a/b", "a.b", "a b"} {
		if ValidID(bad) {
			t.Errorf("ID accepted: %q", bad)
		}
	}
	d := DefaultLimits()
	l := (Limits{MaxFiles: 4, QueueCapacity: 1000, MaxDuration: time.Second, MaxProjectBytes: 9 << 30}).Normalized()
	if l.MaxFiles != 4 || l.QueueCapacity != d.QueueCapacity || l.MaxDuration != time.Second || l.MaxProjectBytes != d.MaxProjectBytes {
		t.Fatalf("limit widening: %+v", l)
	}
}
