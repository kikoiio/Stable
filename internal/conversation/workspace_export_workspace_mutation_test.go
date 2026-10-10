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

type mutateWorkspaceAfterCandidateEntryContext struct {
	context.Context
	candidateFile string
	workspaceFile string
	sawCopiedFile bool
	mutated       bool
	mutationErr   error
}

func (c *mutateWorkspaceAfterCandidateEntryContext) Err() error {
	if !c.mutated {
		if contents, err := os.ReadFile(c.candidateFile); err == nil && string(contents) == "first workspace change" {
			c.sawCopiedFile = true
			if err := os.WriteFile(c.workspaceFile, []byte("concurrent workspace change"), 0600); err != nil {
				c.mutationErr = err
				return err
			}
			c.mutated = true
		}
	}
	return c.Context.Err()
}

func TestWorkspaceExportRejectsWorkspaceMutationDuringCandidateMaterialization(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	baseline := filepath.Join(root, "baseline")
	checkout := filepath.Join(root, "checkout")
	for _, path := range []string{formal, baseline, checkout} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{formal, baseline} {
		for name, content := range map[string]string{"a-first.txt": "first baseline", "b-second.txt": "second baseline"} {
			if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, content := range map[string]string{"a-first.txt": "first workspace change", "b-second.txt": "second workspace change"} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
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
	formalIdentity, err := workspace.CaptureRootIdentity(formalAbs)
	if err != nil {
		t.Fatal(err)
	}
	paths := workspace.Paths{FormalRoot: formalAbs, Baseline: baseline, Checkout: checkout}
	record := workspace.Record{
		Scope:              scope,
		Snapshot:           workspace.Snapshot{ID: workspaceID, Generation: 1},
		FormalRootIdentity: formalIdentity,
	}
	exporter := workspaceCandidateExporter{service: &Service{deps: Deps{Store: db}}}

	baseManifest, err := workspace.BuildManifest(ctx, baseline, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	formalManifest, err := workspace.BuildManifest(ctx, formal, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	workingManifest, err := workspace.BuildManifest(ctx, checkout, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := workspace.ThreeWayPreview(baseManifest, formalManifest, workingManifest, workspace.DefaultLimits())
	if err != nil || len(preview.Conflicts) != 0 {
		t.Fatalf("setup merge preview=%+v err=%v; want uncontested workspace edits", preview, err)
	}
	identity := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s", workspaceID, record.Snapshot.Generation, preview.BaselineDigest, preview.FormalDigest, preview.WorkspaceDigest, preview.Manifest.Digest)
	idDigest := sha256.Sum256([]byte(identity))
	candidateID := "worktree-" + hex.EncodeToString(idDigest[:16])
	candidatePath := filepath.Join(filepath.Dir(formalAbs), ".stable-candidates", candidateID)
	mutate := &mutateWorkspaceAfterCandidateEntryContext{
		Context: ctx, candidateFile: filepath.Join(candidatePath, "a-first.txt"),
		workspaceFile: filepath.Join(checkout, "b-second.txt"),
	}

	if _, err := exporter.ExportWorkspace(mutate, scope, record, paths); !errors.Is(err, workspace.ErrSourceChanged) {
		t.Fatalf("export error=%v, want source-changed after concurrent workspace edit", err)
	}
	if mutate.mutationErr != nil {
		t.Fatalf("mutate workspace source during copy: %v", mutate.mutationErr)
	}
	if !mutate.sawCopiedFile || !mutate.mutated {
		t.Fatalf("workspace mutation hook did not run after first candidate entry: saw=%v mutated=%v", mutate.sawCopiedFile, mutate.mutated)
	}
	for name, want := range map[string]string{"a-first.txt": "first baseline", "b-second.txt": "second baseline"} {
		contents, readErr := os.ReadFile(filepath.Join(formal, name))
		if readErr != nil || string(contents) != want {
			t.Fatalf("formal %s=%q err=%v; want unchanged %q", name, contents, readErr, want)
		}
	}
	if _, err := db.GetCandidate(ctx, candidateID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("changed workspace source registered a candidate: %v", err)
	}
	if _, err := os.Lstat(candidatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed workspace source left a partial candidate tree at %s: %v", candidatePath, err)
	}
}
