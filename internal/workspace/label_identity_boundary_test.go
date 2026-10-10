//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/permission"
)

func TestWorkspaceLabelCannotSelectFilesystemOrGitIdentity(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	layout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{
		RunID: "owner-run", SessionID: scope.SessionID,
		AllowedRoot: formal, FormalRoot: formal,
		CandidateRoot: filepath.Join(parent, "candidate"),
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())

	label := "../../label-escape"
	snapshot, err := service.Create(context.Background(), scope, label)
	if err != nil {
		t.Fatalf("create workspace with path-shaped label: %v", err)
	}
	if snapshot.Label != label {
		t.Fatalf("snapshot label=%q, want original label %q", snapshot.Label, label)
	}
	paths, err := layout.Paths(snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(layout.projectRoot(), snapshot.ID)
	if paths.Root != wantRoot || filepath.Base(paths.Root) != snapshot.ID {
		t.Fatalf("workspace path was not derived from opaque ID: root=%q want=%q id=%q", paths.Root, wantRoot, snapshot.ID)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "label-escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path-shaped label created a directory outside the ID-derived root: %v", err)
	}

	refs, err := service.git.runGit(context.Background(), gitInvocation{
		Paths: paths, GitDir: paths.Repository,
		Args: []string{"for-each-ref", "--format=%(refname)"},
	})
	if err != nil {
		t.Fatalf("list private refs: %v", err)
	}
	refList := string(refs)
	if strings.TrimSpace(refList) != baselineRef+"\n"+checkoutRef && strings.TrimSpace(refList) != checkoutRef+"\n"+baselineRef {
		t.Fatalf("unexpected private refs for label-shaped workspace: %q", refList)
	}
	if strings.Contains(refList, label) || strings.Contains(refList, "label-escape") {
		t.Fatalf("workspace label influenced private Git refs: %q", refList)
	}
}
