package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/permission"
)

func TestRemovalContentDigestUsesWholeWorkspaceLimits(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "repo.git"), 0700); err != nil {
		t.Fatal(err)
	}
	metadata := []byte("private git metadata exceeds checkout file limit")
	if err := os.WriteFile(filepath.Join(rootPath, "repo.git", "pack.data"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	digest, err := removalContentDigest(context.Background(), root, Limits{MaxFileBytes: 1, MaxWorkspaceBytes: 1 << 20})
	if err != nil || !validDigest(digest) {
		t.Fatalf("private metadata digest=%q err=%v; checkout MaxFileBytes must not constrain workspace-wide removal scan", digest, err)
	}
}

func TestRemoveQuarantineRecoveryRetainsChangedContent(t *testing.T) {
	cases := []struct {
		policy   string
		mutation string
	}{
		{policy: "clean", mutation: "rewrite same inode"},
		{policy: "clean", mutation: "add file"},
		{policy: "user_discard", mutation: "rewrite same inode"},
	}
	for _, tc := range cases {
		t.Run(tc.policy+"/"+tc.mutation, func(t *testing.T) {
			parent := t.TempDir()
			formal := filepath.Join(parent, "formal")
			if err := os.Mkdir(formal, 0700); err != nil {
				t.Fatal(err)
			}
			stateRoot := filepath.Join(parent, "state")
			scope := testScope()
			scope.Authority = permission.Authority{RunID: "remove-content-recovery", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
			layout, err := NewLayout(stateRoot, formal, "project")
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
			if err != nil {
				t.Fatal(err)
			}
			created, err := service.Create(context.Background(), scope, "quarantine content recovery")
			if err != nil {
				t.Fatal(err)
			}
			paths, err := layout.Paths(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(paths.Root, "preserved.txt")
			if err := os.WriteFile(payload, []byte("authorized bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			preview := Snapshot{}
			if tc.policy == "user_discard" {
				preview, err = service.PreviewDiscardUser(context.Background(), scope, created.ID, "actual-user")
				if err != nil {
					t.Fatalf("preview user discard: %v", err)
				}
			}
			payloadBefore, err := os.Stat(payload)
			if err != nil {
				t.Fatal(err)
			}
			record, err := service.store.Load(context.Background(), scope, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			crash := errors.New("simulated stop after quarantine rename")
			hooks := removeHooks{
				afterQuarantine: func(string) error { return crash },
			}
			if tc.policy == "user_discard" {
				_, err = service.removeDiscardUserWithHooks(context.Background(), scope, created.ID, "actual-user", preview.DiscardID, preview.DiscardDigest, preview.Generation, hooks)
			} else {
				_, err = service.removeRecordLockedWithHooks(context.Background(), scope, record, hooks)
			}
			if !errors.Is(err, crash) {
				t.Fatalf("remove error=%v, want injected interruption", err)
			}
			intent, err := service.store.load(created.ID, false)
			if err != nil || intent.Snapshot.State != StateRemoving || !validRemovalAuthorization(intent) {
				t.Fatalf("durable removal authorization missing: record=%+v err=%v", intent, err)
			}
			quarantine := filepath.Join(filepath.Dir(paths.Root), intent.Operation.Quarantine)
			quarantinedPayload := filepath.Join(quarantine, "preserved.txt")
			payloadQuarantined, err := os.Stat(quarantinedPayload)
			if err != nil || !os.SameFile(payloadBefore, payloadQuarantined) {
				t.Fatalf("expected same payload inode in quarantine: info=%v err=%v", payloadQuarantined, err)
			}
			if tc.mutation == "rewrite same inode" {
				if err := os.WriteFile(quarantinedPayload, []byte("changed bytes survive"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(quarantine, "late-arrival.txt"), []byte("new bytes survive"), 0600); err != nil {
					t.Fatal(err)
				}
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
			blocked, err := recovered.store.Load(context.Background(), scope, created.ID)
			if err != nil || blocked.Snapshot.State != StateBlocked || blocked.Operation.Phase != "blocked" || blocked.Operation.Quarantine != intent.Operation.Quarantine {
				t.Fatalf("changed quarantine did not fail closed: record=%+v err=%v", blocked, err)
			}
			if _, err := os.Stat(quarantine); err != nil {
				t.Fatalf("quarantine was removed: %v", err)
			}
			wantPayload := "authorized bytes"
			if tc.mutation == "rewrite same inode" {
				wantPayload = "changed bytes survive"
			} else {
				late, err := os.ReadFile(filepath.Join(quarantine, "late-arrival.txt"))
				if err != nil || string(late) != "new bytes survive" {
					t.Fatalf("late file not preserved: bytes=%q err=%v", late, err)
				}
			}
			gotPayload, err := os.ReadFile(quarantinedPayload)
			if err != nil || string(gotPayload) != wantPayload {
				t.Fatalf("payload not preserved: bytes=%q err=%v", gotPayload, err)
			}
		})
	}
}

func TestRemoveQuarantineLegacyIntentFailsClosed(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "legacy-remove-quarantine", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	layout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), scope, "legacy remove journal")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Root, "legacy-bytes.txt"), []byte("retain legacy quarantine bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	record, err := service.store.Load(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	quarantineName := ".remove-" + mustID()
	legacy := record
	legacy.Snapshot.State = StateRemoving
	legacy.Snapshot.Cursor++
	legacy.Operation = Operation{ID: mustID(), Kind: "remove", Phase: "intent", Generation: record.Snapshot.Generation, UpdatedAt: time.Now().UTC(), Quarantine: quarantineName}
	if err := service.store.Save(context.Background(), scope, legacy, record.Snapshot.Generation); err != nil {
		t.Fatalf("save old v1 remove intent: %v", err)
	}
	quarantine := filepath.Join(layout.projectRoot(), quarantineName)
	if err := os.Rename(paths.Root, quarantine); err != nil {
		t.Fatalf("simulate crash after quarantine rename: %v", err)
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
	blocked, err := recovered.store.Load(context.Background(), scope, created.ID)
	if err != nil || blocked.Snapshot.State != StateBlocked || blocked.Operation.Phase != "blocked" || blocked.Operation.Quarantine != legacy.Operation.Quarantine {
		t.Fatalf("legacy remove intent did not fail closed: record=%+v err=%v", blocked, err)
	}
	got, err := os.ReadFile(filepath.Join(quarantine, "legacy-bytes.txt"))
	if err != nil || string(got) != "retain legacy quarantine bytes" {
		t.Fatalf("legacy quarantine bytes not preserved: bytes=%q err=%v", got, err)
	}
}
