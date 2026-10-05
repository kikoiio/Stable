package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/platform/sandbox"
)

func profilePayload(t *testing.T, project, candidateRoot, run string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"project_root": project, "candidate_root": candidateRoot, "run_root": run})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The worker entry assembles the legacy bridges on top of the sandbox profile
// validator: untrusted or out-of-root requests are refused before any isolated
// process is constructed, and when isolation is unavailable the bridge fails
// closed instead of falling back to host execution.
func TestWorkerAssembly(t *testing.T) {
	root := t.TempDir()
	runRoot := filepath.Join(root, "run")
	project := filepath.Join(runRoot, "formal")
	candidateRoot := filepath.Join(runRoot, "candidate")
	for _, p := range []string{runRoot, project, candidateRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	profileFor := sandboxProfileFor(runRoot, "/nonexistent/helper")

	t.Run("rejects incomplete roots", func(t *testing.T) {
		req := core.CapabilityRequest{Payload: profilePayload(t, "", candidateRoot, runRoot)}
		if _, err := profileFor(req); err == nil {
			t.Fatal("missing project root accepted")
		}
	})
	t.Run("rejects run directory outside the worker run root", func(t *testing.T) {
		req := core.CapabilityRequest{Payload: profilePayload(t, project, candidateRoot, filepath.Join(root, "elsewhere"))}
		if _, err := profileFor(req); err == nil {
			t.Fatal("outside run directory accepted")
		}
	})
	t.Run("rejects hostile project and candidate roots", func(t *testing.T) {
		outsideProject := filepath.Join(root, "outside-project")
		outsideCandidate := filepath.Join(root, "outside-candidate")
		for _, p := range []string{outsideProject, outsideCandidate} {
			if err := os.MkdirAll(p, 0700); err != nil {
				t.Fatal(err)
			}
		}
		for name, payload := range map[string]json.RawMessage{
			"host project":      profilePayload(t, "/etc", candidateRoot, runRoot),
			"host candidate":    profilePayload(t, project, "/tmp", runRoot),
			"sibling project":   profilePayload(t, outsideProject, candidateRoot, runRoot),
			"sibling candidate": profilePayload(t, project, outsideCandidate, runRoot),
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := profileFor(core.CapabilityRequest{Payload: payload}); err == nil {
					t.Fatal("hostile root accepted")
				}
			})
		}
	})
	t.Run("rejects run paths that cross a symlink", func(t *testing.T) {
		link := filepath.Join(runRoot, "link")
		if err := os.Symlink(root, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		payload := profilePayload(t, project, candidateRoot, filepath.Join(link, "escaped"))
		if _, err := profileFor(core.CapabilityRequest{Payload: payload}); err == nil {
			t.Fatal("symlinked run path accepted")
		}
		if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
			t.Fatalf("symlink escape created host directory: %v", err)
		}
	})
	t.Run("rejects malformed payload", func(t *testing.T) {
		if _, err := profileFor(core.CapabilityRequest{Payload: json.RawMessage(`{"project_root":`)}); err == nil {
			t.Fatal("malformed payload accepted")
		}
	})
	t.Run("accepts a scoped request and prepares the private run directory", func(t *testing.T) {
		privateRun := filepath.Join(runRoot, "goal-1", "candidate-1")
		req := core.CapabilityRequest{Payload: profilePayload(t, project, candidateRoot, privateRun)}
		profile, err := profileFor(req)
		if err != nil {
			t.Fatal(err)
		}
		if profile.ProjectRoot != project || profile.CandidateRoot != candidateRoot || profile.RunRoot != privateRun {
			t.Fatalf("profile=%+v", profile)
		}
		info, err := os.Stat(privateRun)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("private run directory: %v %v", info.Mode(), err)
		}
	})

	t.Run("bridge fails closed when isolation is unavailable", func(t *testing.T) {
		// If the bridge ever fell back to host execution this script would run
		// and drop a marker next to the candidate.
		marker := filepath.Join(root, "host-executed")
		script := filepath.Join(root, "bridge.py")
		body := "import pathlib\npathlib.Path(" + strconv.Quote(marker) + ").write_text('ran')\nprint('{}')\n"
		if err := os.WriteFile(script, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(candidateRoot, "sensor.kicad_sch")
		if err := os.WriteFile(target, []byte("design"), 0600); err != nil {
			t.Fatal(err)
		}
		bridge := &execution.PythonBridge{
			Script:     script,
			Sandbox:    sandbox.LinuxManager{Bwrap: "/nonexistent/bwrap"},
			ProfileFor: profileFor,
		}
		payload := map[string]string{
			"project_root":   project,
			"candidate_root": candidateRoot,
			"run_root":       filepath.Join(runRoot, "g", "c"),
			"path":           target,
			"allowed_root":   candidateRoot,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req := core.CapabilityRequest{
			ProtocolVersion: 1,
			OperationID:     "op-1",
			Kind:            "kicad.repair_connection",
			Payload:         raw,
		}
		_, err = bridge.Call(context.Background(), req)
		if err == nil {
			t.Fatal("bridge call succeeded without isolation")
		}
		if !errors.Is(err, execution.ErrOutcomeUnknown) {
			t.Fatalf("expected outcome-unknown failure, got: %v", err)
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatal("bridge script executed on the host")
		}
	})
}
