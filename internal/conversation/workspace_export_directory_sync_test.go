package conversation

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceExportDoesNotPersistReadyCandidateBeforeDirectorySync(t *testing.T) {
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
		if err := os.MkdirAll(filepath.Join(path, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "nested", "change.txt"), []byte("baseline"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(checkout, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "nested", "change.txt"), []byte("workspace"), 0600); err != nil {
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
		Scope: scope, Snapshot: workspace.Snapshot{ID: workspaceID, Generation: 1},
		FormalRootIdentity: formalIdentity,
	}
	exporter := workspaceCandidateExporter{service: &Service{deps: Deps{Store: db}}}
	injectedSyncErr := errors.New("injected final candidate directory sync failure")
	var candidateID, candidatePath, displacedPath string
	var createdIdentity, replacementIdentity string
	exporter.syncCandidateDirectories = func(path string) error {
		candidatePath = path
		candidateID = filepath.Base(path)
		createdIdentity, err = candidate.CaptureRootIdentity(path)
		if err != nil {
			return err
		}
		displacedPath = path + ".displaced-owned-candidate"
		if err := os.Rename(path, displacedPath); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "replacement-sentinel"), []byte("retain replacement"), 0600); err != nil {
			return err
		}
		replacementIdentity, err = candidate.CaptureRootIdentity(path)
		if err != nil {
			return err
		}
		return injectedSyncErr
	}

	if _, err := exporter.ExportWorkspace(ctx, scope, record, paths); !errors.Is(err, injectedSyncErr) {
		t.Fatalf("export error=%v, want injected final directory sync failure", err)
	}
	if candidateID == "" || createdIdentity == "" || replacementIdentity == "" {
		t.Fatalf("sync seam did not capture candidate identities: id=%q created=%q replacement=%q", candidateID, createdIdentity, replacementIdentity)
	}
	if _, err := db.GetCandidate(ctx, candidateID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed directory sync persisted a candidate: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(formal, "nested", "change.txt")); err != nil || string(content) != "baseline" {
		t.Fatalf("failed export changed formal file: content=%q err=%v", content, err)
	}
	if currentIdentity, err := candidate.CaptureRootIdentity(candidatePath); err != nil || currentIdentity != replacementIdentity {
		t.Fatalf("partial cleanup removed/replaced an unowned candidate root: identity=%q want=%q err=%v", currentIdentity, replacementIdentity, err)
	}
	if content, err := os.ReadFile(filepath.Join(candidatePath, "replacement-sentinel")); err != nil || string(content) != "retain replacement" {
		t.Fatalf("replacement candidate changed: content=%q err=%v", content, err)
	}

	// This test owns both roots and removes only the identities it captured.
	if identity, err := candidate.CaptureRootIdentity(candidatePath); err != nil || identity != replacementIdentity {
		t.Fatalf("replacement root changed before fixture cleanup: identity=%q err=%v", identity, err)
	}
	if err := removeOwnedPartialCandidate(candidatePath, replacementIdentity); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedPartialCandidate(displacedPath, createdIdentity); err != nil {
		t.Fatal(err)
	}

	exporter.syncCandidateDirectories = nil
	retried, err := exporter.ExportWorkspace(ctx, scope, record, paths)
	if err != nil || retried.CandidateID != candidateID || retried.State != workspace.StateExported {
		t.Fatalf("retry export=%+v err=%v, want same candidate %q", retried, err, candidateID)
	}
	stored, err := db.GetCandidate(ctx, candidateID)
	if err != nil || stored.Candidate.Status != "ready" {
		t.Fatalf("successful retry candidate=%+v err=%v, want ready candidate", stored, err)
	}
}
