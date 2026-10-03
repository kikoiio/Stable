package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/sandbox"
)

func TestM03KicadRepairRunsOnlyAgainstCandidateInLinuxSandbox(t *testing.T) {
	formal := requiredEnv(t, "M03_PROJECT")
	candidateRoot := requiredEnv(t, "M03_BRIDGE_CANDIDATE")
	runRoot := requiredEnv(t, "M03_RUN_DIR")
	bridgeBinary := requiredEnv(t, "M03_BRIDGE_BINARY")
	bridgeScript := requiredEnv(t, "M03_BRIDGE_SCRIPT")
	beforeEntries, formalBefore, err := candidate.BuildManifest(formal)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeEntries) == 0 {
		t.Fatal("fixture project is empty")
	}
	_, candidateBefore, err := candidate.BuildManifest(candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	manager := sandbox.LinuxManager{}
	bridge := &execution.PythonBridge{Script: bridgeScript, Sandbox: manager, ProfileFor: func(core.CapabilityRequest) (sandbox.SandboxProfile, error) {
		return sandbox.SandboxProfile{ProjectRoot: formal, CandidateRoot: candidateRoot, RunRoot: runRoot, Timeout: 90 * time.Second, OutputLimit: 1 << 20, ProxyHelperPath: bridgeBinary}, nil
	}}
	schPath := filepath.Join(candidateRoot, "sensor.kicad_sch")
	// The repair operation's optimistic token is the target file digest, not the
	// tree manifest digest.
	data, err := os.ReadFile(schPath)
	if err != nil {
		t.Fatal(err)
	}
	fileDigest := sha256Hex(data)
	call := func(path, allowedRoot, operation, kind string, expected string, extra map[string]any) core.CapabilityResult {
		t.Helper()
		payload := map[string]any{"path": path, "allowed_root": allowedRoot, "project_root": formal, "candidate_root": candidateRoot, "run_root": runRoot}
		for k, v := range extra {
			payload[k] = v
		}
		raw, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		result, callErr := bridge.Call(context.Background(), core.CapabilityRequest{ProtocolVersion: 1, OperationID: operation, GoalID: "m03-kicad", Kind: kind, ExpectedArtifactID: expected, Payload: raw})
		if callErr != nil {
			t.Fatalf("%s isolated bridge call failed: %v", kind, callErr)
		}
		return result
	}
	formalPath := filepath.Join(formal, "sensor.kicad_sch")
	formalRepairPayload, _ := json.Marshal(map[string]any{"path": formalPath, "allowed_root": formal, "project_root": formal, "candidate_root": candidateRoot, "run_root": runRoot})
	if _, err = bridge.Call(context.Background(), core.CapabilityRequest{ProtocolVersion: 1, OperationID: "m03-formal-repair-rejected", GoalID: "m03-kicad", Kind: "kicad.repair_connection", ExpectedArtifactID: fileDigest, Payload: formalRepairPayload}); err == nil {
		t.Fatal("repair targeting the formal project was accepted")
	}
	if _, digest, digestErr := candidate.BuildManifest(formal); digestErr != nil || digest != formalBefore {
		t.Fatalf("formal project changed during rejected repair: %s %v", digest, digestErr)
	}
	repaired := call(schPath, candidateRoot, "m03-candidate-repair", "kicad.repair_connection", fileDigest, nil)
	if repaired.Status != "applied" || repaired.Postcondition == nil {
		t.Fatalf("candidate repair did not apply: %+v", repaired)
	}
	_, formalAfterRepair, err := candidate.BuildManifest(formal)
	if err != nil || formalAfterRepair != formalBefore {
		t.Fatalf("formal project changed during candidate repair: %s %v", formalAfterRepair, err)
	}
	_, candidateAfterRepair, err := candidate.BuildManifest(candidateRoot)
	if err != nil || candidateAfterRepair == candidateBefore {
		t.Fatalf("repair did not change candidate: before=%s after=%s err=%v", candidateBefore, candidateAfterRepair, err)
	}
	checker := candidate.KicadERCChecker{Sandbox: manager, RunRoot: runRoot, Timeout: 90 * time.Second, OutputLimit: 1 << 20}
	finding, err := checker.Check(context.Background(), candidate.Candidate{ID: "m03-kicad-candidate", FormalRoot: formal, CandidateRoot: candidateRoot, BaselineDigest: formalBefore, CandidateDigest: candidateAfterRepair, Status: "frozen"})
	if err != nil {
		t.Fatalf("independent isolated ERC check failed: %v", err)
	}
	if finding.Result == candidate.FindingUnavailable || finding.Version == "not-run" || len(finding.Files) == 0 {
		t.Fatalf("ERC did not produce a concrete finding: %+v", finding)
	}
	if _, digest, err := candidate.BuildManifest(formal); err != nil || digest != formalBefore {
		t.Fatalf("formal project changed during ERC: %s %v", digest, err)
	}
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
