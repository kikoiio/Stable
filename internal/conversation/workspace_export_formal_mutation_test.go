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

type mutateFormalAfterCandidateFileContext struct {
	context.Context
	candidateFile string
	formalFile    string
	mutated       bool
}

func (c *mutateFormalAfterCandidateFileContext) Err() error {
	if !c.mutated {
		if contents, err := os.ReadFile(c.candidateFile); err == nil && string(contents) == "workspace change" {
			if err := os.WriteFile(c.formalFile, []byte("concurrent formal change"), 0600); err != nil {
				return err
			}
			c.mutated = true
		}
	}
	return c.Context.Err()
}

func TestWorkspaceExportRejectsFormalMutationDuringCandidateMaterialization(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	baseline := filepath.Join(root, "baseline")
	checkout := filepath.Join(root, "checkout")
	for _, path := range []string{formal, baseline, checkout} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	formalFile := filepath.Join(formal, "change.txt")
	for path, content := range map[string]string{
		formalFile:                            "formal baseline",
		filepath.Join(baseline, "change.txt"): "formal baseline",
		filepath.Join(checkout, "change.txt"): "workspace change",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
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
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	paths := workspace.Paths{FormalRoot: formalAbs, Baseline: baseline, Checkout: checkout}
	formalIdentity, err := workspace.CaptureRootIdentity(formalAbs)
	if err != nil {
		t.Fatal(err)
	}
	record := workspace.Record{Scope: scope, Snapshot: workspace.Snapshot{ID: workspaceID, Generation: 1}, FormalRootIdentity: formalIdentity}
	exporter := workspaceCandidateExporter{service: &Service{deps: Deps{Store: db}}}

	baseManifest, err := workspace.BuildManifest(context.Background(), baseline, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	formalManifest, err := workspace.BuildManifest(context.Background(), formal, workspace.DefaultLimits())
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
	inject := &mutateFormalAfterCandidateFileContext{
		Context: context.Background(), candidateFile: filepath.Join(candidatePath, "change.txt"), formalFile: formalFile,
	}
	if _, err := exporter.ExportWorkspace(inject, scope, record, paths); !errors.Is(err, workspace.ErrSourceChanged) {
		t.Fatalf("export error=%v, want source-changed after concurrent formal edit", err)
	}
	if !inject.mutated {
		t.Fatal("formal mutation hook did not observe the candidate file after materialization")
	}
	formalContents, err := os.ReadFile(formalFile)
	if err != nil || string(formalContents) != "concurrent formal change" {
		t.Fatalf("formal mutation fixture content=%q err=%v", formalContents, err)
	}
	if _, err := db.GetCandidate(context.Background(), candidateID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("changed formal source registered a candidate: %v", err)
	}
	if _, err := os.Lstat(candidatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed formal source left a candidate tree at %s: %v", candidatePath, err)
	}
}
