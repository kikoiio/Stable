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

func TestCleanAndConfirmedDiscardRevalidateAfterDurableIntent(t *testing.T) {
	for _, mode := range []string{"clean", "discard"} {
		t.Run(mode, func(t *testing.T) {
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
			scope.Authority = permission.Authority{RunID: "remove-content-race", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
			service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close(context.Background())

			created, err := service.Create(context.Background(), scope, "content changes during remove")
			if err != nil {
				t.Fatal(err)
			}
			paths, err := layout.Paths(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "discard" {
				if err := os.WriteFile(filepath.Join(paths.Checkout, "confirmed.txt"), []byte("confirmed dirty content"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			preview := Snapshot{}
			if mode == "discard" {
				preview, err = service.PreviewDiscardUser(context.Background(), scope, created.ID, "actual-user")
				if err != nil {
					t.Fatalf("preview discard: %v", err)
				}
			}

			newData := filepath.Join(paths.Checkout, "arrived-after-confirmation.txt")
			hooks := removeHooks{afterIntent: func() error {
				return os.WriteFile(newData, []byte("new user data"), 0600)
			}}
			var result Snapshot
			if mode == "clean" {
				result, err = service.removeCleanWithHooks(context.Background(), scope, created.ID, hooks)
			} else {
				result, err = service.removeDiscardUserWithHooks(context.Background(), scope, created.ID, "actual-user", preview.DiscardID, preview.DiscardDigest, preview.Generation, hooks)
			}
			if err == nil || result.State == StateRemoved {
				t.Fatalf("%s remove result=%+v err=%v; expected changed-content refusal", mode, result, err)
			}
			if mode == "discard" && !errors.Is(err, ErrSourceChanged) {
				t.Fatalf("discard returned %v, want ErrSourceChanged", err)
			}
			if mode == "clean" && !errors.Is(err, ErrOwnership) && !errors.Is(err, ErrSourceChanged) {
				t.Fatalf("clean remove returned %v, want ownership/source-changed error", err)
			}
			stored, err := service.store.load(created.ID, false)
			if err != nil || stored.Snapshot.State != StateInterrupted || stored.Operation.Kind != "remove" || stored.Operation.Phase != "blocked" {
				t.Fatalf("changed-content removal was not retained as blocked: record=%+v err=%v", stored, err)
			}
			if _, err := os.Stat(paths.Root); err != nil {
				t.Fatalf("workspace root was removed: %v", err)
			}
			if content, err := os.ReadFile(newData); err != nil || string(content) != "new user data" {
				t.Fatalf("post-confirmation data was not retained: content=%q err=%v", content, err)
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

func TestRemoveCrashAfterQuarantineRemovalRecoversIdempotently(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "remove-finalize-crash", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	newService := func() *LifecycleService {
		t.Helper()
		layout, err := NewLayout(stateRoot, formal, "project")
		if err != nil {
			t.Fatal(err)
		}
		service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
		if err != nil {
			_ = layout.Close()
			t.Fatal(err)
		}
		return service
	}

	service := newService()
	created, err := service.Create(ctx, scope, "remove finalization crash")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := service.layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Root, "user-data.txt"), []byte("authorized removal"), 0600); err != nil {
		t.Fatal(err)
	}
	record, err := service.store.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated stop after quarantine removal")
	_, err = service.removeRecordLockedWithHooks(ctx, scope, record, removeHooks{
		afterQuarantineRemoved: func(name string) error {
			if _, err := os.Lstat(paths.Root); !errors.Is(err, os.ErrNotExist) {
				return errors.New("workspace ID path remains at finalization cut")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(paths.Root), name)); !errors.Is(err, os.ErrNotExist) {
				return errors.New("quarantine path remains at finalization cut")
			}
			return crash
		},
	})
	if !errors.Is(err, crash) {
		t.Fatalf("remove interruption error=%v, want simulated stop", err)
	}
	intent, err := service.store.load(created.ID, false)
	if err != nil || intent.Snapshot.State != StateRemoving || intent.Operation.ID == "" || intent.Operation.Quarantine == "" {
		t.Fatalf("remove intent was not retained at finalization cut: record=%+v err=%v", intent, err)
	}
	removeOperationID := intent.Operation.ID
	quarantineName := intent.Operation.Quarantine
	intentCursor := intent.Snapshot.Cursor
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	firstRecovery := newService()
	removed, err := firstRecovery.Get(ctx, scope, created.ID)
	if err != nil || removed.State != StateRemoved || removed.Cursor != intentCursor+1 {
		t.Fatalf("first recovery snapshot=%+v err=%v", removed, err)
	}
	firstRecord, err := firstRecovery.store.load(created.ID, false)
	if err != nil || firstRecord.Snapshot.State != StateRemoved || firstRecord.Snapshot.Cursor != intentCursor+1 || firstRecord.Operation.ID != removeOperationID || firstRecord.Operation.Phase != "complete" || firstRecord.Operation.Quarantine != "" {
		t.Fatalf("first recovery terminal record=%+v err=%v", firstRecord, err)
	}
	if _, err := os.Lstat(paths.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace root exists after recovery: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(paths.Root), quarantineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine exists after recovery: %v", err)
	}
	if err := firstRecovery.Close(ctx); err != nil {
		t.Fatal(err)
	}

	secondRecovery := newService()
	defer secondRecovery.Close(ctx)
	removedAgain, err := secondRecovery.Get(ctx, scope, created.ID)
	if err != nil || removedAgain.State != StateRemoved || removedAgain.Cursor != firstRecord.Snapshot.Cursor {
		t.Fatalf("second recovery snapshot=%+v err=%v", removedAgain, err)
	}
	secondRecord, err := secondRecovery.store.load(created.ID, false)
	if err != nil || secondRecord.Snapshot.State != StateRemoved || secondRecord.Snapshot.Cursor != firstRecord.Snapshot.Cursor || secondRecord.Operation.ID != removeOperationID || secondRecord.Operation.Phase != "complete" || secondRecord.Operation.Quarantine != "" {
		t.Fatalf("second recovery changed terminal record: first=%+v second=%+v err=%v", firstRecord, secondRecord, err)
	}
	if _, err := os.Lstat(paths.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace root exists after second recovery: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(paths.Root), quarantineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine exists after second recovery: %v", err)
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
