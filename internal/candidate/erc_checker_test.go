package candidate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/platform/kicad"
	"stable/internal/platform/sandbox"
)

type checkerSandbox struct {
	violations int
	mutate     string
	calls      [][]string
}

func (f *checkerSandbox) Probe(context.Context, sandbox.SandboxProfile) error { return nil }
func (f *checkerSandbox) RunIsolated(_ context.Context, profile sandbox.SandboxProfile, argv []string, _ io.Reader) (sandbox.SandboxResult, error) {
	f.calls = append(f.calls, append([]string(nil), argv...))
	if len(argv) == 2 && argv[1] == "version" {
		return sandbox.SandboxResult{Stdout: []byte("KiCad 9.0.1\n")}, nil
	}
	outputIndex := -1
	for i, arg := range argv {
		if arg == "--output" && i+1 < len(argv) {
			outputIndex = i + 1
			break
		}
	}
	if outputIndex < 0 || !strings.HasPrefix(argv[outputIndex], "/workspace/run/") {
		return sandbox.SandboxResult{}, os.ErrInvalid
	}
	rel, err := filepath.Rel("/workspace/run", argv[outputIndex])
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return sandbox.SandboxResult{}, os.ErrInvalid
	}
	var violationRows []json.RawMessage
	for range f.violations {
		violationRows = append(violationRows, json.RawMessage(`{"type":"mock_violation"}`))
	}
	report, err := json.Marshal(map[string]any{"sheets": []any{map[string]any{"violations": violationRows}}})
	if err != nil {
		return sandbox.SandboxResult{}, err
	}
	if err = os.WriteFile(filepath.Join(profile.RunRoot, rel), report, 0600); err != nil {
		return sandbox.SandboxResult{}, err
	}
	if f.mutate != "" {
		if err = os.WriteFile(filepath.Join(profile.CandidateRoot, f.mutate), []byte("late write"), 0600); err != nil {
			return sandbox.SandboxResult{}, err
		}
	}
	code := 0
	if f.violations > 0 {
		code = 5
	}
	return sandbox.SandboxResult{ExitCode: code}, nil
}
func (f *checkerSandbox) StartIsolatedSession(context.Context, sandbox.SandboxProfile) (sandbox.SandboxSession, error) {
	return sandbox.SandboxSession{}, os.ErrInvalid
}
func (f *checkerSandbox) CallIsolatedSession(context.Context, sandbox.SandboxSession, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, os.ErrInvalid
}
func (f *checkerSandbox) StopIsolatedSession(context.Context, string) error { return nil }

func checkerFixture(t *testing.T) (Candidate, string) {
	t.Helper()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.kicad_sch"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := CreateCandidate("checker-candidate", formal, filepath.Join(root, "candidates"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(candidate.CandidateRoot, "board.kicad_sch"), []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	candidate.Status = "frozen"
	return candidate, filepath.Join(root, "private-run")
}

func checkerCapabilities() *kicad.Capabilities {
	return &kicad.Capabilities{
		Python:       kicad.ToolStatus{Name: "python3", Available: true},
		CLI:          kicad.ToolStatus{Name: "kicad-cli", Available: true, Version: "KiCad 9.0.1"},
		Template:     kicad.ToolStatus{Name: "template", Available: true},
		TemplateRoot: "/usr/share/kicad/template",
		Sandbox:      kicad.SandboxCapability{Name: "bubblewrap", Available: true},
	}
}

func TestKicadERCCheckerRecordsPassAndFail(t *testing.T) {
	for _, test := range []struct {
		name       string
		violations int
		want       FindingResult
	}{
		{name: "pass", violations: 0, want: FindingPass},
		{name: "fail", violations: 2, want: FindingFail},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, runRoot := checkerFixture(t)
			fake := &checkerSandbox{violations: test.violations}
			finding, err := (KicadERCChecker{Sandbox: fake, RunRoot: runRoot, Capabilities: checkerCapabilities()}).Check(context.Background(), candidate)
			if err != nil {
				t.Fatal(err)
			}
			if finding.Checker != "kicad-cli-erc" || finding.Version != "KiCad 9.0.1" || finding.Result != test.want || len(finding.Files) != 1 || finding.Files[0] != "board.kicad_sch" {
				t.Fatalf("unexpected ERC finding: %+v", finding)
			}
			if len(fake.calls) != 1 || fake.calls[0][1] != "sch" {
				t.Fatalf("unexpected isolated checker calls: %v", fake.calls)
			}
		})
	}
}

func TestCandidateReviewRejectsWritesDuringChecker(t *testing.T) {
	candidate, runRoot := checkerFixture(t)
	fake := &checkerSandbox{mutate: "late.txt"}
	if _, err := BuildReview(context.Background(), candidate, []Checker{KicadERCChecker{Sandbox: fake, RunRoot: runRoot, Capabilities: checkerCapabilities()}}); err == nil {
		t.Fatal("candidate write during checks did not invalidate the review")
	}
}

func TestKicadERCCheckerUsesCapabilityResolver(t *testing.T) {
	candidate, runRoot := checkerFixture(t)
	fake := &checkerSandbox{}
	original := discoverKicadCapabilities
	defer func() { discoverKicadCapabilities = original }()
	called := false
	discoverKicadCapabilities = func(_ context.Context, manager sandbox.SandboxManager, profile sandbox.SandboxProfile) (kicad.Capabilities, error) {
		called = true
		if manager != fake || profile.ProjectRoot != candidate.FormalRoot || profile.CandidateRoot != candidate.CandidateRoot || profile.RunRoot == "" {
			t.Fatalf("resolver received wrong profile: %+v", profile)
		}
		return *checkerCapabilities(), nil
	}
	if _, err := (KicadERCChecker{Sandbox: fake, RunRoot: runRoot}).Check(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("KiCad capability resolver was not called")
	}
}
