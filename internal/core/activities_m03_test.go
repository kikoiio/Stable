package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBridgePayloadKeepsReportsOutsideProject(t *testing.T) {
	runRoot := t.TempDir()
	project := filepath.Join(runRoot, "goal-1")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(project, "board.kicad_sch")
	if err := os.WriteFile(artifact, []byte("design"), 0600); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{RunRoot: runRoot}
	encoded, err := activities.requestPayload(Goal{ID: "goal-1", AllowedRoot: project, ArtifactPath: artifact}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ProjectRoot   string `json:"project_root"`
		CandidateRoot string `json:"candidate_root"`
		RunRoot       string `json:"run_root"`
	}
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ProjectRoot != project || payload.CandidateRoot == "" || payload.RunRoot == "" {
		t.Fatalf("sandbox roots are missing: %+v", payload)
	}
	if rel, relErr := filepath.Rel(project, payload.RunRoot); relErr != nil || rel == "." || rel != ".." && len(rel) > 2 && rel[:3] != ".."+string(os.PathSeparator) {
		t.Fatalf("private report root is inside the project: %s", payload.RunRoot)
	}
	for _, path := range []string{payload.CandidateRoot, payload.RunRoot} {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("sandbox root is not a private directory: %s (%v)", path, statErr)
		}
	}
}

func TestComputerBridgeTargetsAnIsolatedCandidateCopy(t *testing.T) {
	runRoot := t.TempDir()
	project := filepath.Join(runRoot, "goal-1")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(project, "board.kicad_sch")
	if err := os.WriteFile(artifact, []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{RunRoot: runRoot}
	encoded, err := activities.computerRequestPayload(Goal{ID: "goal-1", AllowedRoot: project, ArtifactPath: artifact}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Path          string `json:"path"`
		AllowedRoot   string `json:"allowed_root"`
		ProjectRoot   string `json:"project_root"`
		CandidateRoot string `json:"candidate_root"`
		RunRoot       string `json:"run_root"`
	}
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ProjectRoot != project || payload.AllowedRoot != payload.CandidateRoot || payload.Path == artifact {
		t.Fatalf("computer bridge was not bound to the candidate: %+v", payload)
	}
	data, err := os.ReadFile(payload.Path)
	if err != nil || string(data) != "formal" {
		t.Fatalf("candidate copy does not match the formal baseline: %q %v", data, err)
	}
	if filepath.Dir(payload.Path) != payload.CandidateRoot || payload.RunRoot == payload.CandidateRoot {
		t.Fatalf("candidate and private run roots are not separated: %+v", payload)
	}
}
