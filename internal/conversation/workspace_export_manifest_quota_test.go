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
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceExportManifestQuotaDoesNotCreateReadyCandidate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "change.txt")
	if err := os.WriteFile(formalFile, []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
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
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "workspace-state")
	svc := &Service{deps: Deps{Store: db}}
	sessionID := "0123456789abcdef0123456789abcdef"
	scope := workspace.Scope{
		ProjectID: "manifest-quota", SessionID: sessionID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Authority: permission.Authority{
			RunID: "quota-export-run", SessionID: sessionID,
			AllowedRoot: formalAbs, FormalRoot: formalAbs,
			CandidateRoot: filepath.Join(root, "candidate"),
		},
	}
	layout, err := workspace.NewLayout(stateRoot, formalAbs, scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	var checkoutFile string
	exporter := workspaceCandidateExporter{service: svc, afterCandidateEntry: func(string) {
		if err := os.Truncate(checkoutFile, workspace.DefaultLimits().MaxFileBytes+1); err != nil {
			t.Errorf("inject oversized source after candidate copy: %v", err)
		}
	}}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{Exporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close workspace manager: %v", err)
		}
	})
	created, err := manager.Create(ctx, scope, "manifest quota export")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkoutFile = filepath.Join(paths.Checkout, "change.txt")
	if err := os.WriteFile(checkoutFile, []byte("workspace edit"), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := manager.Preview(ctx, scope, created.ID)
	if err != nil || preview.ConflictCount != 0 || preview.PreviewID == "" {
		t.Fatalf("prepare conflict-free export preview: snapshot=%+v err=%v", preview, err)
	}
	mergePreview, err := workspace.BuildMergePreview(ctx, paths, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s", created.ID, preview.Generation,
		mergePreview.BaselineDigest, mergePreview.FormalDigest, mergePreview.WorkspaceDigest, mergePreview.Manifest.Digest)
	idDigest := sha256.Sum256([]byte(identity))
	candidateID := "worktree-" + hex.EncodeToString(idDigest[:16])

	if _, err := manager.Export(ctx, scope, created.ID); !errors.Is(err, workspace.ErrQuota) {
		t.Fatalf("export error=%v; want manifest ErrQuota", err)
	}
	current, err := manager.Get(ctx, scope, created.ID)
	if err != nil || current.State != workspace.StateKept || current.CandidateID != "" {
		t.Fatalf("over-quota export workspace snapshot=%+v err=%v; want kept with no candidate", current, err)
	}
	if _, err := db.GetCandidate(ctx, candidateID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("over-quota export stored candidate %q: %v", candidateID, err)
	}
	candidatePath := filepath.Join(filepath.Dir(formalAbs), ".stable-candidates", candidateID)
	if _, err := os.Lstat(candidatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("over-quota export left candidate root %q: %v", candidatePath, err)
	}
	for name, path := range map[string]string{"formal": formalFile, "baseline": filepath.Join(paths.Baseline, "change.txt")} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "baseline" {
			t.Fatalf("over-quota export changed %s bytes: %q err=%v", name, got, err)
		}
	}
}
