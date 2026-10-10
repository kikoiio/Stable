package conversation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/store"
	"stable/internal/workspace"
)

type replaceFormalRootAfterCandidateFileContext struct {
	context.Context
	candidateFile string
	formalRoot    string
	originalPath  string
	originalInfo  os.FileInfo
	replacement   os.FileInfo
	swapped       bool
	swapErr       error
}

func (c *replaceFormalRootAfterCandidateFileContext) Err() error {
	if c.swapped || c.swapErr != nil {
		return c.Context.Err()
	}
	contents, err := os.ReadFile(c.candidateFile)
	if err != nil || string(contents) != "workspace change" {
		return c.Context.Err()
	}
	if err := os.Rename(c.formalRoot, c.originalPath); err != nil {
		c.swapErr = err
		return err
	}
	if err := os.Mkdir(c.formalRoot, 0700); err != nil {
		c.swapErr = err
		return err
	}
	if err := os.WriteFile(filepath.Join(c.formalRoot, "change.txt"), []byte("formal baseline"), 0600); err != nil {
		c.swapErr = err
		return err
	}
	c.replacement, c.swapErr = os.Stat(c.formalRoot)
	c.swapped = c.swapErr == nil
	return c.Context.Err()
}

func TestWorkspaceExportRejectsByteIdenticalFormalRootReplacement(t *testing.T) {
	root := t.TempDir()
	formalRoot := filepath.Join(root, "project")
	baseline := filepath.Join(root, "baseline")
	checkout := filepath.Join(root, "checkout")
	for _, path := range []string{formalRoot, baseline, checkout} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, contents := range map[string]string{
		filepath.Join(formalRoot, "change.txt"): "formal baseline",
		filepath.Join(baseline, "change.txt"):   "formal baseline",
		filepath.Join(checkout, "change.txt"):   "workspace change",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	originalInfo, err := os.Stat(formalRoot)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const sessionID = "0123456789abcdef0123456789abcdef"
	const workspaceID = "1123456789abcdef0123456789abcdef"
	scope := workspace.Scope{
		ProjectID: "project1", SessionID: sessionID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
	}
	formalAbs, err := filepath.Abs(formalRoot)
	if err != nil {
		t.Fatal(err)
	}
	formalIdentity, err := workspace.CaptureRootIdentity(formalAbs)
	if err != nil {
		t.Fatal(err)
	}
	paths := workspace.Paths{FormalRoot: formalAbs, Baseline: baseline, Checkout: checkout}
	record := workspace.Record{Scope: scope, Snapshot: workspace.Snapshot{ID: workspaceID, Generation: 1}, FormalRootIdentity: formalIdentity}
	exporter := workspaceCandidateExporter{service: &Service{deps: Deps{Store: db}}}

	baseManifest, err := workspace.BuildManifest(context.Background(), baseline, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	formalManifest, err := workspace.BuildManifest(context.Background(), formalRoot, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	workingManifest, err := workspace.BuildManifest(context.Background(), checkout, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := workspace.ThreeWayPreview(baseManifest, formalManifest, workingManifest, workspace.DefaultLimits())
	if err != nil || len(preview.Conflicts) != 0 {
		t.Fatalf("setup merge preview=%+v err=%v; want uncontested workspace edit", preview, err)
	}
	identity := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s", workspaceID, record.Snapshot.Generation, preview.BaselineDigest, preview.FormalDigest, preview.WorkspaceDigest, preview.Manifest.Digest)
	idDigest := sha256.Sum256([]byte(identity))
	candidateID := "worktree-" + hex.EncodeToString(idDigest[:16])
	candidatePath := filepath.Join(filepath.Dir(formalAbs), ".stable-candidates", candidateID)
	inject := &replaceFormalRootAfterCandidateFileContext{
		Context: context.Background(), candidateFile: filepath.Join(candidatePath, "change.txt"),
		formalRoot: formalAbs, originalPath: filepath.Join(root, "original-formal-root"), originalInfo: originalInfo,
	}
	_, exportErr := exporter.ExportWorkspace(inject, scope, record, paths)

	if !inject.swapped || inject.swapErr != nil {
		t.Errorf("formal root replacement hook swapped=%t err=%v", inject.swapped, inject.swapErr)
	}
	if !errors.Is(exportErr, workspace.ErrOwnership) && !errors.Is(exportErr, workspace.ErrSourceChanged) {
		t.Errorf("export error=%v, want ownership or source-changed after same-byte root replacement", exportErr)
	}
	currentInfo, statErr := os.Stat(formalAbs)
	if statErr != nil {
		t.Errorf("stat replacement formal root: %v", statErr)
	} else if inject.replacement == nil || !os.SameFile(currentInfo, inject.replacement) || os.SameFile(originalInfo, currentInfo) {
		t.Errorf("formal root identity did not remain the byte-identical replacement: original=%v replacement=%v current=%v", originalInfo, inject.replacement, currentInfo)
	}
	formalContents, readErr := os.ReadFile(filepath.Join(formalAbs, "change.txt"))
	if readErr != nil || string(formalContents) != "formal baseline" {
		t.Errorf("replacement formal content=%q err=%v; want unchanged same bytes", formalContents, readErr)
	}
	if candidate, getErr := db.GetCandidate(context.Background(), candidateID); getErr == nil {
		if candidate.Candidate.Status == "ready" {
			t.Errorf("byte-identical replacement published a ready candidate: %+v", candidate.Candidate)
		}
	} else if !errors.Is(getErr, sql.ErrNoRows) {
		t.Errorf("look up candidate after rejected export: %v", getErr)
	}
}
