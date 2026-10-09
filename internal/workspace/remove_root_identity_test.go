package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/permission"
)

func TestRemoveRootReplacementAfterIntentIsRetained(t *testing.T) {
	for _, stage := range []string{"after intent", "after quarantine rename"} {
		t.Run(stage, func(t *testing.T) {
			parent := t.TempDir()
			formal := filepath.Join(parent, "formal")
			if err := os.Mkdir(formal, 0700); err != nil {
				t.Fatal(err)
			}
			layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
			if err != nil {
				t.Fatal(err)
			}
			scope := testScope()
			scope.Authority = permission.Authority{RunID: "remove-root-race", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
			service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close(context.Background())

			created, err := service.Create(context.Background(), scope, "replacement during remove")
			if err != nil {
				t.Fatal(err)
			}
			paths, err := layout.Paths(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			originalData := filepath.Join(paths.Root, "preserved.txt")
			if err := os.WriteFile(originalData, []byte("original owned root"), 0600); err != nil {
				t.Fatal(err)
			}
			record, err := service.store.Load(context.Background(), scope, created.ID)
			if err != nil {
				t.Fatal(err)
			}

			installReplacement := func(path string) error {
				if err := os.Mkdir(path, 0700); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(path, "replacement-sentinel.txt"), []byte("never delete replacement"), 0600)
			}
			hooks := removeHooks{}
			if stage == "after intent" {
				hooks.afterIntent = func() error {
					if err := os.Rename(paths.Root, paths.Root+".original"); err != nil {
						return err
					}
					return installReplacement(paths.Root)
				}
			} else {
				hooks.afterQuarantine = func(name string) error {
					quarantine := filepath.Join(filepath.Dir(paths.Root), name)
					if err := os.Rename(quarantine, paths.Root+".original"); err != nil {
						return err
					}
					return installReplacement(quarantine)
				}
			}

			if got, err := service.removeRecordLockedWithHooks(context.Background(), scope, record, hooks); !errors.Is(err, ErrOwnership) || got.State == StateRemoved {
				t.Fatalf("replacement remove result=%+v err=%v, want ownership failure without Removed", got, err)
			}
			stored, err := service.store.load(created.ID, false)
			if err != nil || stored.Snapshot.State != StateRemoving || stored.Operation.Quarantine == "" {
				t.Fatalf("replacement intent was not retained: record=%+v err=%v", stored, err)
			}
			quarantine := filepath.Join(filepath.Dir(paths.Root), stored.Operation.Quarantine)
			for _, retainedPath := range []string{quarantine, paths.Root + ".original"} {
				content, err := os.ReadFile(filepath.Join(retainedPath, "replacement-sentinel.txt"))
				if retainedPath == paths.Root+".original" {
					content, err = os.ReadFile(filepath.Join(retainedPath, "preserved.txt"))
					if err != nil || string(content) != "original owned root" {
						t.Fatalf("original workspace data not retained at %q: content=%q err=%v", retainedPath, content, err)
					}
					continue
				}
				if err != nil || string(content) != "never delete replacement" {
					t.Fatalf("replacement sentinel not retained at %q: content=%q err=%v", retainedPath, content, err)
				}
			}
		})
	}
}

func TestRemoveQuarantineRecoveryCompletesPinnedOwnedRoot(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "remove-quarantine-recovery", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	layout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), scope, "recover remove quarantine")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Root, "user-data.txt"), []byte("authorized removal"), 0600); err != nil {
		t.Fatal(err)
	}
	record, err := service.store.Load(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated service stop after quarantine rename")
	_, err = service.removeRecordLockedWithHooks(context.Background(), scope, record, removeHooks{
		afterQuarantine: func(string) error { return crash },
	})
	if !errors.Is(err, crash) {
		t.Fatalf("remove interruption error=%v, want simulated stop", err)
	}
	stored, err := service.store.load(created.ID, false)
	if err != nil || stored.Snapshot.State != StateRemoving || stored.Operation.Quarantine == "" {
		t.Fatalf("remove intent not persisted for recovery: record=%+v err=%v", stored, err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	recoveryLayout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewService(recoveryLayout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close(context.Background())
	removed, err := recovered.Get(context.Background(), scope, created.ID)
	if err != nil || removed.State != StateRemoved {
		t.Fatalf("valid quarantined root recovery=%+v err=%v", removed, err)
	}
	if _, err := os.Lstat(paths.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace ID root remains after recovery: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(paths.Root), stored.Operation.Quarantine)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine remains after recovery: %v", err)
	}
}

func TestRemoveQuarantineReplacementFailsClosedOnRestart(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "remove-quarantine-replacement", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	layout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), scope, "replacement quarantine recovery")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Root, "original.txt"), []byte("original workspace root"), 0600); err != nil {
		t.Fatal(err)
	}
	record, err := service.store.Load(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.removeRecordLockedWithHooks(context.Background(), scope, record, removeHooks{
		afterQuarantine: func(name string) error {
			quarantine := filepath.Join(filepath.Dir(paths.Root), name)
			if err := os.Rename(quarantine, paths.Root+".original"); err != nil {
				return err
			}
			if err := os.Mkdir(quarantine, 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(quarantine, "replacement-sentinel.txt"), []byte("keep replacement across restart"), 0600)
		},
	})
	if !errors.Is(err, ErrOwnership) {
		t.Fatalf("replacement quarantine remove error=%v, want ownership failure", err)
	}
	intent, err := service.store.load(created.ID, false)
	if err != nil || intent.Snapshot.State != StateRemoving || intent.Operation.Phase != "intent" || intent.Operation.Quarantine == "" {
		t.Fatalf("pre-restart remove intent=%+v err=%v", intent, err)
	}
	quarantinePath := filepath.Join(filepath.Dir(paths.Root), intent.Operation.Quarantine)
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	recoveryLayout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(recoveryLayout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}}); !errors.Is(err, ErrOwnership) {
		_ = recoveryLayout.Close()
		t.Fatalf("restart accepted replacement quarantine: %v", err)
	}

	sentinel, err := os.ReadFile(filepath.Join(quarantinePath, "replacement-sentinel.txt"))
	if err != nil || string(sentinel) != "keep replacement across restart" {
		t.Fatalf("restart changed replacement sentinel: bytes=%q err=%v", sentinel, err)
	}
	original, err := os.ReadFile(filepath.Join(paths.Root+".original", "original.txt"))
	if err != nil || string(original) != "original workspace root" {
		t.Fatalf("restart changed original workspace data: bytes=%q err=%v", original, err)
	}

	checkLayout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	checkStore, err := NewOwnershipStore(checkLayout)
	if err != nil {
		_ = checkLayout.Close()
		t.Fatal(err)
	}
	defer checkStore.Close()
	defer checkLayout.Close()
	after, err := checkStore.load(created.ID, false)
	if err != nil || after.Snapshot.State != StateRemoving || after.Operation.ID != intent.Operation.ID || after.Operation.Phase != "intent" || after.Operation.Quarantine != intent.Operation.Quarantine {
		t.Fatalf("restart altered durable remove intent: before=%+v after=%+v err=%v", intent, after, err)
	}
}
