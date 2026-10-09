package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceExportExistingCandidateRejectsFormalRootReplacementDuringLookup(t *testing.T) {
	ctx := context.Background()
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
		filepath.Join(formalRoot, "seed.txt"): "baseline",
		filepath.Join(baseline, "seed.txt"):   "baseline",
		filepath.Join(checkout, "writer.txt"): "workspace change",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
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
	record := workspace.Record{
		Scope: scope, Snapshot: workspace.Snapshot{ID: "1123456789abcdef0123456789abcdef", Generation: 1},
		FormalRootIdentity: formalIdentity,
	}
	exporter := workspaceCandidateExporter{service: &Service{deps: Deps{Store: db}}}
	first, err := exporter.ExportWorkspace(ctx, scope, record, paths)
	if err != nil || first.CandidateID == "" {
		t.Fatalf("initial export=%+v err=%v", first, err)
	}
	if _, err := db.GetCandidate(ctx, first.CandidateID); err != nil {
		t.Fatalf("load initial candidate: %v", err)
	}

	originalInfo, err := os.Stat(formalAbs)
	if err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(root, "displaced-formal")
	var replacementInfo os.FileInfo
	exporter.afterExistingCandidateLookup = func(candidateID string) {
		if candidateID != first.CandidateID {
			t.Errorf("existing candidate lookup ID=%q, want %q", candidateID, first.CandidateID)
			return
		}
		if err := os.Rename(formalAbs, displaced); err != nil {
			t.Errorf("displace formal root: %v", err)
			return
		}
		if err := os.Mkdir(formalAbs, 0700); err != nil {
			t.Errorf("create replacement formal root: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(formalAbs, "seed.txt"), []byte("baseline"), 0600); err != nil {
			t.Errorf("write replacement formal root: %v", err)
			return
		}
		replacementInfo, err = os.Stat(formalAbs)
		if err != nil {
			t.Errorf("stat replacement formal root: %v", err)
		}
	}

	retry, retryErr := exporter.ExportWorkspace(ctx, scope, record, paths)
	if !errors.Is(retryErr, workspace.ErrOwnership) {
		t.Fatalf("idempotent export=%+v err=%v; want formal-root ownership failure", retry, retryErr)
	}
	if retry.CandidateID != "" {
		t.Fatalf("rejected stale export returned candidate %q", retry.CandidateID)
	}
	currentInfo, err := os.Stat(formalAbs)
	if err != nil {
		t.Fatal(err)
	}
	if replacementInfo == nil || !os.SameFile(currentInfo, replacementInfo) || os.SameFile(originalInfo, currentInfo) {
		t.Fatalf("formal root replacement not preserved: original=%v replacement=%v current=%v", originalInfo, replacementInfo, currentInfo)
	}
	if got, err := os.ReadFile(filepath.Join(formalAbs, "seed.txt")); err != nil || string(got) != "baseline" {
		t.Fatalf("replacement formal seed=%q err=%v", got, err)
	}
}
