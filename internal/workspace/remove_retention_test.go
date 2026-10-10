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

func TestRemoveCleanPreservesIgnoredAndPrivateGitData(t *testing.T) {
	for _, scenario := range []string{"ignored checkout file", "private Git commit"} {
		t.Run(scenario, func(t *testing.T) {
			parent := t.TempDir()
			formal := filepath.Join(parent, "formal")
			if err := os.Mkdir(formal, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("baseline"), 0600); err != nil {
				t.Fatal(err)
			}
			layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
			if err != nil {
				t.Fatal(err)
			}
			scope := testScope()
			scope.Authority = permission.Authority{RunID: "run-remove-retention", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
			service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close(context.Background())

			created, err := service.Create(context.Background(), scope, "preserve private work")
			if err != nil {
				t.Fatal(err)
			}
			paths, err := layout.Paths(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			var privateCommit string
			switch scenario {
			case "ignored checkout file":
				if err := os.WriteFile(filepath.Join(paths.Checkout, ".gitignore"), []byte("ignored.txt\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(paths.Checkout, "ignored.txt"), []byte("ignored user bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			case "private Git commit":
				state, err := service.git.Validate(context.Background(), scope, created.ID)
				if err != nil {
					t.Fatal(err)
				}
				commitBytes, err := service.git.runGit(context.Background(), gitInvocation{
					Paths: paths, GitDir: paths.Repository,
					Args:  []string{"-c", "user.name=Workspace Test", "-c", "user.email=workspace@example.invalid", "commit-tree", state.BaselineCommit + "^{tree}", "-p", state.BaselineCommit},
					Stdin: strings.NewReader("private workspace commit\n"),
				})
				if err != nil {
					t.Fatal(err)
				}
				privateCommit = strings.TrimSpace(string(commitBytes))
				if _, err := service.git.runGit(context.Background(), gitInvocation{Paths: paths, GitDir: paths.Repository, Args: []string{"update-ref", checkoutRef, privateCommit, state.BaselineCommit}}); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := service.RemoveClean(context.Background(), scope, created.ID); !errors.Is(err, ErrOwnership) {
				t.Fatalf("clean remove accepted %s: %v", scenario, err)
			}
			retained, err := service.Get(context.Background(), scope, created.ID)
			if err != nil || retained.State != StateReady {
				t.Fatalf("rejected clean remove changed workspace state: %+v err=%v", retained, err)
			}
			switch scenario {
			case "ignored checkout file":
				content, err := os.ReadFile(filepath.Join(paths.Checkout, "ignored.txt"))
				if err != nil || string(content) != "ignored user bytes" {
					t.Fatalf("rejected clean remove lost ignored checkout data: content=%q err=%v", content, err)
				}
			case "private Git commit":
				ref, err := os.ReadFile(filepath.Join(paths.Repository, filepath.FromSlash(checkoutRef)))
				if err != nil || strings.TrimSpace(string(ref)) != privateCommit {
					t.Fatalf("rejected clean remove lost private commit ref: ref=%q want=%q err=%v", ref, privateCommit, err)
				}
				if _, err := service.git.runGit(context.Background(), gitInvocation{Paths: paths, GitDir: paths.Repository, Args: []string{"cat-file", "-e", privateCommit + "^{commit}"}}); err != nil {
					t.Fatalf("rejected clean remove lost private commit object: %v", err)
				}
			}
		})
	}
}
