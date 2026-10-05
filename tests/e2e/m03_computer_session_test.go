package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/platform/sandbox"
)

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestM03ComputerIsolatedSessionLifecycle(t *testing.T) {
	formal := requiredEnv(t, "M03_PROJECT")
	candidateRoot := requiredEnv(t, "M03_SESSION_CANDIDATE")
	runRoot := requiredEnv(t, "M03_RUN_DIR")
	bridgeBinary := requiredEnv(t, "M03_BRIDGE_BINARY")
	bridgeScript := requiredEnv(t, "M03_BRIDGE_SCRIPT")
	created, err := candidate.CreateCandidate("computer-fixture", formal, filepath.Dir(candidateRoot))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(created.CandidateRoot, candidateRoot); err != nil {
		t.Fatal(err)
	}
	before, formalDigest, err := candidate.BuildManifest(formal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Fatalf("Xvfb unavailable: %v", err)
	}
	manager := sandbox.New()
	bridge := &execution.PythonBridge{Script: bridgeScript, Sandbox: manager, ProfileFor: func(core.CapabilityRequest) (sandbox.SandboxProfile, error) {
		return sandbox.SandboxProfile{ProjectRoot: formal, CandidateRoot: candidateRoot, RunRoot: runRoot, Timeout: 90 * time.Second, OutputLimit: 1 << 20, ProxyHelperPath: bridgeBinary}, nil
	}}
	payload, err := json.Marshal(map[string]any{"path": filepath.Join(candidateRoot, "sensor.kicad_sch"), "allowed_root": candidateRoot, "project_root": formal, "candidate_root": candidateRoot, "run_root": runRoot, "session": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	request := func(operation string, kind string, session map[string]any) core.CapabilityRequest {
		p := map[string]any{}
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatal(err)
		}
		p["session"] = session
		encoded, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return core.CapabilityRequest{ProtocolVersion: 1, OperationID: operation, GoalID: "m03-computer-goal", Kind: kind, Payload: encoded}
	}
	result, err := bridge.Call(context.Background(), request("computer-open", "computer.ensure_open", map[string]any{"id": "m03-computer", "generation": 0}))
	if err != nil {
		t.Fatalf("start isolated Xvfb/KiCad session: %v", err)
	}
	var observation core.ComputerObservation
	if err = json.Unmarshal(result.Postcondition, &observation); err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" || observation.Status != "open" || observation.Generation != 1 || observation.WindowIdentity == "" || observation.OpenedArtifactID != fileSHA256(t, filepath.Join(candidateRoot, "sensor.kicad_sch")) {
		t.Fatalf("isolated desktop did not open the candidate: status=%s observation=%+v", result.Status, observation)
	}
	defer func() {
		_ = bridge.Sandbox.StopIsolatedSession(context.Background(), "computer-m03-computer-goal")
	}()
	if len(result.EvidencePaths) != 1 {
		t.Fatalf("desktop did not produce screenshot evidence: %+v", result.EvidencePaths)
	}
	for _, path := range result.EvidencePaths {
		if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(runRoot)+string(filepath.Separator)) {
			t.Fatalf("screenshot escaped private run root: %s", path)
		}
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Fatalf("screenshot evidence unavailable: %s %v", path, err)
		}
	}
	if err = manager.StopIsolatedSession(context.Background(), "computer-m03-computer-goal"); err != nil {
		t.Fatal(err)
	}
	stale, err := bridge.Call(context.Background(), request("computer-observe-stopped", "computer.observe", map[string]any{"id": observation.SessionID, "generation": observation.Generation, "runtime_handle": observation.RuntimeHandle}))
	if err == nil || !strings.Contains(err.Error(), "handle is stale") {
		t.Fatalf("stopped GUI generation was not rejected: result=%+v err=%v", stale, err)
	}
	recovered, err := bridge.Call(context.Background(), request("computer-recover", "computer.recover", map[string]any{"id": observation.SessionID, "generation": observation.Generation, "runtime_handle": observation.RuntimeHandle}))
	if err != nil {
		t.Fatalf("recover isolated GUI session: %v", err)
	}
	var recoveredObservation core.ComputerObservation
	if err = json.Unmarshal(recovered.Postcondition, &recoveredObservation); err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "applied" || recoveredObservation.Status != "open" || recoveredObservation.Generation != observation.Generation+1 {
		t.Fatalf("recovery did not create a fresh generation: %+v %+v", recovered, recoveredObservation)
	}
	if err = manager.StopIsolatedSession(context.Background(), "computer-m03-computer-goal"); err != nil {
		t.Fatal(err)
	}
	if _, afterDigest, err := candidate.BuildManifest(formal); err != nil || afterDigest != formalDigest || len(before) == 0 {
		t.Fatalf("computer session changed formal project: digest=%s err=%v", afterDigest, err)
	}
	for _, name := range []string{"Xvfb", "eeschema"} {
		if out, err := exec.Command("pgrep", "-af", name+".*m03-computer").CombinedOutput(); err == nil {
			t.Fatalf("managed GUI process remains after stop: %s: %s", name, out)
		}
	}
}
