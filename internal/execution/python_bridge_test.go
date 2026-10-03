package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/core"
	"stable/internal/sandbox"
)

type fakeSandbox struct {
	result        sandbox.SandboxResult
	err           error
	args          []string
	profile       sandbox.SandboxProfile
	lastInput     []byte
	sessionStarts int
	sessionCalls  int
	sessionResult sandbox.SandboxResult
}

func (f *fakeSandbox) Probe(context.Context, sandbox.SandboxProfile) error { return nil }
func (f *fakeSandbox) RunIsolated(_ context.Context, p sandbox.SandboxProfile, args []string, in io.Reader) (sandbox.SandboxResult, error) {
	f.args = append([]string(nil), args...)
	f.profile = p
	data, _ := io.ReadAll(in)
	f.lastInput = data
	if len(data) == 0 {
		return sandbox.SandboxResult{}, errors.New("empty stdin")
	}
	return f.result, f.err
}
func (f *fakeSandbox) StartIsolatedSession(_ context.Context, p sandbox.SandboxProfile) (sandbox.SandboxSession, error) {
	f.sessionStarts++
	f.profile = p
	return sandbox.SandboxSession{ID: p.SessionID, Generation: 1, CandidateID: p.CandidateID}, nil
}
func (f *fakeSandbox) CallIsolatedSession(_ context.Context, _ sandbox.SandboxSession, in io.Reader) (sandbox.SandboxResult, error) {
	f.sessionCalls++
	if _, err := io.ReadAll(in); err != nil {
		return sandbox.SandboxResult{}, err
	}
	return f.sessionResult, nil
}
func (f *fakeSandbox) StopIsolatedSession(context.Context, string) error { return nil }

func TestBridgeProtocolAndUnknownOutcome(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	runRoot := filepath.Join(root, "run")
	for _, p := range []string{candidate, runRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "project.txt")
	if err := os.WriteFile(target, []byte("project"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := sandbox.SandboxProfile{ProjectRoot: root, CandidateRoot: candidate, RunRoot: runRoot}
	result := func(version int, operation, status string, evidence []string) sandbox.SandboxResult {
		var quoted []string
		for _, p := range evidence {
			quoted = append(quoted, "\""+p+"\"")
		}
		return sandbox.SandboxResult{ExitCode: 0, Stdout: []byte("{\"protocol_version\":" + string(rune('0'+version)) + ",\"operation_id\":\"" + operation + "\",\"status\":\"" + status + "\",\"actual_artifact_id\":\"sha\",\"evidence_paths\":[" + strings.Join(quoted, ",") + "],\"postcondition\":{}}\n")}
	}
	fake := &fakeSandbox{result: result(1, "op-1", "observed", nil)}
	bridge := PythonBridge{Script: "/workspace/project/bridge.py", AllowedRoot: root, Sandbox: fake, Profile: profile, Timeout: time.Second}
	payload := []byte(`{"path":"` + target + `","allowed_root":"` + root + `"}`)
	req := core.CapabilityRequest{ProtocolVersion: 1, OperationID: "op-1", Kind: "inspect_design", GoalID: "g", Payload: payload}
	got, err := bridge.Call(context.Background(), req)
	if err != nil || got.Status != "observed" {
		t.Fatalf("valid isolated call %+v %v", got, err)
	}
	if len(fake.args) != 2 || fake.args[0] != "python3" || fake.profile.ProjectRoot != root {
		t.Fatalf("isolated invocation args=%v profile=%+v", fake.args, fake.profile)
	}
	fake.result = result(2, "op-1", "observed", nil)
	if _, err = bridge.Call(context.Background(), req); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("version: %v", err)
	}
	fake.result = result(1, "other", "observed", nil)
	if _, err = bridge.Call(context.Background(), req); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("operation ID: %v", err)
	}
	fake.result = result(1, "op-1", "observed", []string{filepath.Join(t.TempDir(), "outside-report")})
	if _, err = bridge.Call(context.Background(), req); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("evidence path: %v", err)
	}
	fake.result = sandbox.SandboxResult{}
	fake.err = context.DeadlineExceeded
	if _, err = bridge.Call(context.Background(), req); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("timeout: %v", err)
	}
}

func TestComputerBridgeUsesPersistentCandidateBoundSession(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	runRoot := filepath.Join(root, "run")
	bridgeDir := filepath.Join(root, "bridge")
	for _, path := range []string{project, candidate, runRoot, bridgeDir} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(candidate, "design.kicad_sch")
	if err := os.WriteFile(target, []byte("design"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(bridgeDir, "bridge.py")
	if err := os.WriteFile(script, []byte("# bridge"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSandbox{sessionResult: sandbox.SandboxResult{Stdout: []byte(`{"protocol_version":1,"operation_id":"op","status":"observed","actual_artifact_id":"sha","evidence_paths":[],"postcondition":{}}`)}}
	bridge := PythonBridge{Script: script, AllowedRoot: root, Sandbox: fake, Profile: sandbox.SandboxProfile{ProjectRoot: project, CandidateRoot: candidate, RunRoot: runRoot}}
	payload := []byte(`{"path":"` + target + `","allowed_root":"` + candidate + `","project_root":"` + project + `","candidate_root":"` + candidate + `","run_root":"` + runRoot + `"}`)
	req := core.CapabilityRequest{ProtocolVersion: 1, OperationID: "op", Kind: "computer.observe", GoalID: "goal", Payload: payload}
	for range 2 {
		if _, err := bridge.Call(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if fake.sessionStarts != 1 || fake.sessionCalls != 2 || fake.profile.CandidateID != "candidate" || fake.profile.SessionID != "computer-goal" || len(fake.profile.SessionArgv) != 2 {
		t.Fatalf("computer bridge did not reuse its isolated candidate session: starts=%d calls=%d profile=%+v", fake.sessionStarts, fake.sessionCalls, fake.profile)
	}
	if len(fake.args) != 0 {
		t.Fatal("computer bridge fell back to one-shot host-managed execution")
	}
}

func TestBridgeFailsClosedWithoutSandbox(t *testing.T) {
	bridge := PythonBridge{Script: "bridge.py"}
	_, err := bridge.Call(context.Background(), core.CapabilityRequest{ProtocolVersion: 1, OperationID: "op", Kind: "inspect_design"})
	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("expected unavailable sandbox error, got %v", err)
	}
}

func TestBridgeMapsNonexistentReportInsidePrivateRunRoot(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	runRoot := filepath.Join(root, "run")
	for _, p := range []string{project, candidate, runRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(project, "board.kicad_sch")
	if err := os.WriteFile(target, []byte("design"), 0600); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(runRoot, "checks", "goal", "erc.json")
	if err := os.MkdirAll(filepath.Dir(report), 0700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSandbox{result: sandbox.SandboxResult{Stdout: []byte(`{"protocol_version":1,"operation_id":"erc-1","status":"pass","actual_artifact_id":"sha","evidence_paths":[],"postcondition":{}}`)}}
	bridge := PythonBridge{Script: "/workspace/bridge/bridge.py", Sandbox: fake,
		Profile: sandbox.SandboxProfile{ProjectRoot: project, CandidateRoot: candidate, RunRoot: runRoot}}
	payload := []byte(`{"path":"` + target + `","allowed_root":"` + project + `","report_path":"` + report + `","run_root":"` + runRoot + `"}`)
	request := core.CapabilityRequest{ProtocolVersion: 1, OperationID: "erc-1", Kind: "kicad.run_erc", GoalID: "goal", Payload: payload}
	if _, err := bridge.Call(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Payload map[string]string `json:"payload"`
	}
	if err := json.Unmarshal(fake.lastInput, &wire); err != nil {
		t.Fatal(err)
	}
	if got, want := wire.Payload["report_path"], "/workspace/run/checks/goal/erc.json"; got != want {
		t.Fatalf("report path = %q, want %q", got, want)
	}
	if got, want := wire.Payload["run_root"], "/workspace/run"; got != want {
		t.Fatalf("run root = %q, want %q", got, want)
	}
	if _, err := hostOutputPathToGuest(filepath.Join(root, "outside.json"), runRoot); err == nil {
		t.Fatal("report outside private run root was accepted")
	}
}

func TestRepairBridgeRequiresCandidateRoot(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	runRoot := filepath.Join(root, "run")
	for _, p := range []string{project, candidate, runRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	formal := filepath.Join(project, "board")
	if err := os.WriteFile(formal, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	err := validateBridgeTarget(core.CapabilityRequest{Kind: "kicad.repair_connection", Payload: []byte(`{"path":"` + formal + `","allowed_root":"` + project + `"}`)}, sandbox.SandboxProfile{ProjectRoot: project, CandidateRoot: candidate, RunRoot: runRoot})
	if err == nil {
		t.Fatal("repair targeting formal project was accepted")
	}
}
