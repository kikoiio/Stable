//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/permission"
)

func TestPlanModeWriterEntryDenialLeavesOwnedWorkspaceUntouched(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "base.txt")
	if err := os.WriteFile(formalFile, []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateRoot := filepath.Join(parent, "candidate")
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{
		RunID: "lead-run", SessionID: scope.SessionID,
		AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidateRoot,
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	created, err := service.Create(ctx, scope, "plan mode writer boundary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enter(ctx, scope, created.ID); err != nil {
		t.Fatalf("bind workspace: %v", err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkoutFile := filepath.Join(paths.Checkout, "writer-boundary.txt")
	if err := os.WriteFile(checkoutFile, []byte("existing checkout bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	formalBefore, err := os.ReadFile(formalFile)
	if err != nil {
		t.Fatal(err)
	}
	checkoutBefore, err := os.ReadFile(checkoutFile)
	if err != nil {
		t.Fatal(err)
	}
	ownerBefore, err := service.Get(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	boundBefore, err := service.Binding(scope)
	if err != nil || boundBefore != created.ID {
		t.Fatalf("initial binding=%q err=%v want=%q", boundBefore, err, created.ID)
	}
	repositoryInfo, err := os.Stat(paths.Repository)
	if err != nil {
		t.Fatal(err)
	}
	checkoutInfo, err := os.Stat(paths.Checkout)
	if err != nil {
		t.Fatal(err)
	}

	planScope := scope
	planScope.Authority.Mode = permission.ModePlan
	if _, err := service.AcquireWriter(ctx, planScope, created.ID, "plan-child-run"); !errors.Is(err, ErrOwnership) {
		t.Fatalf("plan-mode authority acquired a writer: %v", err)
	}

	ownerAfter, err := service.Get(ctx, scope, created.ID)
	if err != nil || !reflect.DeepEqual(ownerAfter, ownerBefore) {
		t.Fatalf("plan-mode denial changed owner snapshot: got=%+v err=%v want=%+v", ownerAfter, err, ownerBefore)
	}
	boundAfter, err := service.Binding(scope)
	if err != nil || boundAfter != boundBefore {
		t.Fatalf("plan-mode denial changed binding: got=%q err=%v want=%q", boundAfter, err, boundBefore)
	}
	formalAfter, err := os.ReadFile(formalFile)
	if err != nil || !reflect.DeepEqual(formalAfter, formalBefore) {
		t.Fatalf("plan-mode denial changed formal bytes: got=%q err=%v want=%q", formalAfter, err, formalBefore)
	}
	checkoutAfter, err := os.ReadFile(checkoutFile)
	if err != nil || !reflect.DeepEqual(checkoutAfter, checkoutBefore) {
		t.Fatalf("plan-mode denial changed checkout bytes: got=%q err=%v want=%q", checkoutAfter, err, checkoutBefore)
	}
	repositoryAfter, err := os.Stat(paths.Repository)
	if err != nil || !os.SameFile(repositoryInfo, repositoryAfter) {
		t.Fatalf("plan-mode denial changed private Git identity: before=%v after=%v err=%v", repositoryInfo, repositoryAfter, err)
	}
	checkoutAfterInfo, err := os.Stat(paths.Checkout)
	if err != nil || !os.SameFile(checkoutInfo, checkoutAfterInfo) {
		t.Fatalf("plan-mode denial changed checkout identity: before=%v after=%v err=%v", checkoutInfo, checkoutAfterInfo, err)
	}
	if _, err := os.Lstat(candidateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan-mode denial created candidate root: %v", err)
	}
}
