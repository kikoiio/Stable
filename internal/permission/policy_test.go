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

// In plan mode the session's plan file is the only ask-free write target;
// every other plan-mode decision matches default mode. Non-plan modes keep
// the formal project (including the plan file) read-only.
func TestPlanModeMatrix(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	plans := filepath.Join(project, ".stable", "plans")
	for _, p := range []string{project, candidate, plans} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	planFile := filepath.Join(plans, "sess-1.md")
	if err := os.WriteFile(planFile, []byte("# plan"), 0600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(plans, "other.md")
	if err := os.WriteFile(sibling, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	base := Authority{RunID: "r", SessionID: "s", AllowedRoot: project, CandidateRoot: candidate, FormalRoot: project}
	ops := []Operation{
		{ID: "pw", Kind: OpWrite, Name: "WriteFile", Target: planFile},
		{ID: "cw", Kind: OpWrite, Name: "WriteFile", Target: filepath.Join(candidate, "board.kicad_sch")},
		{ID: "fw", Kind: OpWrite, Name: "WriteFile", Target: filepath.Join(project, "board.kicad_sch")},
		{ID: "c", Kind: OpCommand, Name: "exec"},
		{ID: "rd", Kind: OpRead, Name: "ReadFile", Target: filepath.Join(project, "input")},
	}
	want := []DecisionKind{DecisionAllow, DecisionAsk, DecisionDeny, DecisionAsk, DecisionAllow}

	t.Run("plan", func(t *testing.T) {
		a := base
		a.Mode = ModePlan
		a.PlanFilePath = planFile
		for i, op := range ops {
			if d := (Policy{}).Decide(a, op); d.Kind != want[i] {
				t.Errorf("%s %s: got %s want %s (%s)", op.Kind, op.Target, d.Kind, want[i], d.Reason)
			}
		}
		// Only the exact plan file path is exempt; a sibling file under
		// .stable/plans/ is still a read-only formal project write.
		if d := (Policy{}).Decide(a, Operation{ID: "sw", Kind: OpWrite, Name: "WriteFile", Target: sibling}); d.Kind != DecisionDeny {
			t.Errorf("sibling plan write: got %s want deny (%s)", d.Kind, d.Reason)
		}
		// An empty PlanFilePath never hits the plan-file exemption.
		a.PlanFilePath = ""
		if d := (Policy{}).Decide(a, ops[0]); d.Kind != DecisionDeny {
			t.Errorf("plan write without PlanFilePath: got %s want deny (%s)", d.Kind, d.Reason)
		}
	})
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits} {
		t.Run(string(mode), func(t *testing.T) {
			a := base
			a.Mode = mode
			a.PlanFilePath = planFile
			if d := (Policy{}).Decide(a, ops[0]); d.Kind != DecisionDeny {
				t.Errorf("plan file write in %s mode: got %s want deny (%s)", mode, d.Kind, d.Reason)
			}
		})
	}
}
