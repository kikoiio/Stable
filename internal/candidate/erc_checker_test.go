package candidate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
			finding, err := (KicadERCChecker{Sandbox: fake, RunRoot: runRoot}).Check(context.Background(), candidate)
			if err != nil {
				t.Fatal(err)
			}
			if finding.Checker != "kicad-cli-erc" || finding.Version != "KiCad 9.0.1" || finding.Result != test.want || len(finding.Files) != 1 || finding.Files[0] != "board.kicad_sch" {
				t.Fatalf("unexpected ERC finding: %+v", finding)
			}
			if len(fake.calls) != 2 || fake.calls[0][1] != "version" || fake.calls[1][1] != "sch" {
				t.Fatalf("unexpected isolated checker calls: %v", fake.calls)
			}
		})
	}
}

func TestCandidateReviewRejectsWritesDuringChecker(t *testing.T) {
	candidate, runRoot := checkerFixture(t)
	fake := &checkerSandbox{mutate: "late.txt"}
	if _, err := BuildReview(context.Background(), candidate, []Checker{KicadERCChecker{Sandbox: fake, RunRoot: runRoot}}); err == nil {
		t.Fatal("candidate write during checks did not invalidate the review")
	}
}
