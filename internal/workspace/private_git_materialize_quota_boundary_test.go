//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateGitMaterializeRejectsOverLimitSourceWithoutReceipt(t *testing.T) {
	layout, ownership := ownershipFixture(t)
	t.Cleanup(func() {
		if err := ownership.Close(); err != nil {
			t.Errorf("close ownership store: %v", err)
		}
		if err := layout.Close(); err != nil {
			t.Errorf("close layout: %v", err)
		}
	})
	formal := layout.FormalRoot()
	const original = "source bytes"
	if err := os.WriteFile(filepath.Join(formal, "too-large.bin"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	limits := Limits{MaxFileBytes: 4, MaxSnapshotBytes: 16, MaxFiles: 4, MaxEntries: 8}
	g, err := NewPrivateGit(layout, ownership, limits)
	if errors.Is(err, ErrUnavailable) {
		t.Skip("fixed system Git is unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	const id = "quota-boundary"
	if _, err := ownership.Create(context.Background(), scope, id, "over-limit source", "create"); err != nil {
		t.Fatal(err)
	}
	_, err = g.Materialize(context.Background(), scope, id)
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("over-limit formal source returned %v, want ErrQuota", err)
	}
	if got, readErr := os.ReadFile(filepath.Join(formal, "too-large.bin")); readErr != nil || string(got) != original {
		t.Fatalf("quota refusal changed source bytes: got=%q err=%v", got, readErr)
	}
	paths, err := layout.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"baseline", "repo.git", "checkout", "run", gitStateName} {
		if _, err := os.Lstat(filepath.Join(paths.Root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("quota refusal retained materialization output %s: %v", name, err)
		}
	}
	record, err := ownership.Load(context.Background(), scope, id)
	if err != nil || record.Snapshot.State != StateCreating {
		t.Fatalf("quota refusal lost or falsely completed the ownership intent: record=%+v err=%v", record, err)
	}
}
