package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHardBoundary(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	outside := filepath.Join(root, "outside")
	for _, p := range []string{project, candidate, outside} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	a := Authority{RunID: "r", SessionID: "s", AllowedRoot: project, CandidateRoot: candidate, FormalRoot: project, Mode: ModeBypass}
	tests := []struct {
		name string
		op   Operation
	}{
		{"outside read", Operation{ID: "1", Kind: OpRead, Name: "read", Target: filepath.Join(outside, "secret")}},
		{"formal write", Operation{ID: "2", Kind: OpWrite, Name: "write", Target: filepath.Join(project, "board.kicad_sch")}},
		{"candidate write", Operation{ID: "3", Kind: OpWrite, Name: "write", Target: filepath.Join(candidate, "board.kicad_sch")}},
		{"network denied", Operation{ID: "4", Kind: OpNetwork, Name: "connect", Protocol: "tcp", Host: "127.0.0.1", Port: 8080}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := (Policy{}).Decide(a, tt.op)
			if tt.name == "candidate write" {
				if d.Kind != DecisionAllow {
					t.Fatalf("candidate write should be within bypass mode: %+v", d)
				}
			} else if d.Kind != DecisionDeny {
				t.Fatalf("expected deny, got %+v", d)
			}
		})
	}
	link := filepath.Join(project, "escape")
	if err := os.Symlink(filepath.Join(outside, "secret"), link); err != nil {
		t.Fatal(err)
	}
	d := (Policy{}).Decide(a, Operation{ID: "5", Kind: OpRead, Name: "read", Target: link})
	if d.Kind != DecisionDeny {
		t.Fatalf("symlink escape was not denied: %+v", d)
	}
}

func TestModeMatrix(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	for _, p := range []string{project, candidate} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ops := []Operation{
		{ID: "r", Kind: OpRead, Name: "read", Target: filepath.Join(project, "input")},
		{ID: "w", Kind: OpWrite, Name: "write", Target: filepath.Join(candidate, "output")},
		{ID: "l", Kind: OpLegacy, Name: "legacy", Target: filepath.Join(candidate, "legacy-output")},
		{ID: "c", Kind: OpCommand, Name: "exec"},
	}
	want := map[Mode][]DecisionKind{
		ModeDefault:     {DecisionAllow, DecisionAsk, DecisionAsk, DecisionAsk},
		ModeAcceptEdits: {DecisionAllow, DecisionAllow, DecisionAllow, DecisionAsk},
		ModePlan:        {DecisionAllow, DecisionAsk, DecisionAsk, DecisionAsk},
		ModeBypass:      {DecisionAllow, DecisionAllow, DecisionAllow, DecisionAllow},
	}
	for mode, expected := range want {
		t.Run(string(mode), func(t *testing.T) {
			a := Authority{RunID: "r", SessionID: "s", AllowedRoot: project, CandidateRoot: candidate, FormalRoot: project, Mode: mode}
			for i, op := range ops {
				d := (Policy{}).Decide(a, op)
				if d.Kind != expected[i] {
					t.Errorf("%s: got %s want %s (%s)", op.Kind, d.Kind, expected[i], d.Reason)
				}
			}
		})
	}
}

func TestExactRules(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	for _, p := range []string{project, candidate} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	a := Authority{RunID: "r", SessionID: "s", AllowedRoot: project, CandidateRoot: candidate, Mode: ModeBypass}
	op := Operation{ID: "w", Kind: OpWrite, Name: "write", Target: filepath.Join(candidate, "a"), Parameters: []byte(`{"text":"one"}`)}
	scope, err := a.ScopeDigest()
	if err != nil {
		t.Fatal(err)
	}
	params, err := digest(op.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	rule := ExactRule{Effect: EffectDeny, Kind: op.Kind, Name: op.Name, Target: "candidate:a", ParametersDigest: params, ScopeDigest: scope}
	if d := (Policy{Rules: []ExactRule{rule}}).Decide(a, op); d.Kind != DecisionDeny {
		t.Fatalf("exact deny rule ignored: %+v", d)
	}
	op.Parameters = []byte(`{"text":"two"}`)
	if d := (Policy{Rules: []ExactRule{rule}}).Decide(a, op); d.Kind != DecisionAllow {
		t.Fatalf("nonmatching parameter rule affected operation: %+v", d)
	}
	otherCandidate := filepath.Join(root, "candidate-next")
	if err := os.MkdirAll(otherCandidate, 0700); err != nil {
		t.Fatal(err)
	}
	other := a
	other.RunID = "next-run"
	other.WorkItemID = "next-action"
	other.CandidateRoot = otherCandidate
	if otherScope, _ := other.ScopeDigest(); otherScope != scope {
		t.Fatal("transient candidate/run identifiers changed the saved-rule scope")
	}
	otherOp := op
	otherOp.ID = "next-operation"
	otherOp.Target = filepath.Join(otherCandidate, "a")
	otherOp.Parameters = []byte(`{"text":"one"}`)
	if d := (Policy{Rules: []ExactRule{rule}}).Decide(other, otherOp); d.Kind != DecisionDeny {
		t.Fatalf("saved rule did not follow the same relative candidate target: %+v", d)
	}
	bad := rule
	bad.Effect = "corrupted"
	if d := (Policy{Rules: []ExactRule{bad}}).Decide(a, op); d.Kind != DecisionDeny {
		t.Fatalf("corrupt rule did not fail closed: %+v", d)
	}
}

// A read-only operation must not be denied just because the lazily created
// candidate root does not exist yet (read-first normal task flows).
func TestReadAllowedBeforeCandidateExists(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(project, "a.txt")
	if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	a := Authority{RunID: "r", SessionID: "s", AllowedRoot: project, CandidateRoot: filepath.Join(root, "not-created-yet"), FormalRoot: project, Mode: ModeDefault}
	d := (Policy{}).Decide(a, Operation{ID: "1", Kind: OpRead, Name: "ReadFile", Target: target})
	if d.Kind != DecisionAllow {
		t.Fatalf("read before candidate creation should be allowed: %+v", d)
	}
}
