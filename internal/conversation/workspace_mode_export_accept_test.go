package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceModeChangeSurvivesExportReviewAndAcceptance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "script.sh")
	if err := os.WriteFile(formalFile, []byte("#!/bin/sh\necho stable\n"), 0644); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace mode acceptance")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{deps: Deps{
		Store: db, ProjectRoot: root,
		CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
	}}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, "modeproject")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{
		Exporter: workspaceCandidateExporter{service: service},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close workspace service: %v", err)
		}
	})
	scope := workspace.Scope{
		ProjectID: "modeproject", SessionID: session.ID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Authority: permission.Authority{
			RunID: "setup-run", SessionID: session.ID, AllowedRoot: formalAbs,
			FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "unused-candidate"),
		},
	}
	created, err := manager.Create(ctx, scope, "chmod acceptance")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkoutFile := filepath.Join(paths.Checkout, "script.sh")
	if err := os.Chmod(checkoutFile, 0755); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(formalFile); err != nil {
		t.Fatal(err)
	} else if mode := info.Mode().Perm(); mode != 0644 {
		t.Fatalf("formal mode changed before acceptance: mode=%v", mode)
	}

	exported, err := manager.Export(ctx, scope, created.ID)
	if err != nil || exported.CandidateID == "" || exported.State != workspace.StateExported {
		t.Fatalf("workspace export=%+v err=%v", exported, err)
	}
	stored, err := db.GetCandidate(ctx, exported.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(stored.Candidate.CandidateRoot, "script.sh")); err != nil {
		t.Fatal(err)
	} else if mode := info.Mode().Perm(); mode != 0755 {
		t.Fatalf("exported candidate mode=%v; want executable mode 0755", mode)
	}
	if info, err := os.Stat(formalFile); err != nil {
		t.Fatal(err)
	} else if mode := info.Mode().Perm(); mode != 0644 {
		t.Fatalf("export changed formal mode before acceptance: mode=%v", mode)
	}

	review, err := service.reviewCandidate(ctx, exported.CandidateID, session.ID)
	if err != nil || review.CandidateDigest == "" || review.Digest == "" || len(review.Findings) != 1 || review.Findings[0].Result != candidate.FindingPass {
		t.Fatalf("candidate review=%+v err=%v", review, err)
	}
	decisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := service.acceptReviewedCandidate(ctx, ClientMsg{
		CandidateID: exported.CandidateID, SessionID: session.ID, DecisionID: decisionID,
		PreviewDigest: review.Digest, CandidateDigest: review.CandidateDigest,
		FormalDigest: review.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal),
	})
	if err != nil || receipt.ID == "" {
		t.Fatalf("candidate acceptance receipt=%+v err=%v", receipt, err)
	}
	if info, err := os.Stat(formalFile); err != nil {
		t.Fatal(err)
	} else if mode := info.Mode().Perm(); mode != 0755 {
		t.Fatalf("accepted formal mode=%v; want executable mode 0755", mode)
	}
}
