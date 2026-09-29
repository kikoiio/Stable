package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotIsImmutableAndFixtureIsRejected(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fixture := filepath.Join(t.TempDir(), "original.kicad_sch")
	if err := os.WriteFile(fixture, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(root, "sensor.kicad_sch")
	if err := os.WriteFile(run, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Snapshot(ctx, "g", run)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Snapshot(ctx, "g", run)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.Path != b.Path {
		t.Fatalf("nondeterministic snapshot: %+v %+v", a, b)
	}
	if err = os.WriteFile(run, []byte("modified"), 0644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot) != "original" || string(original) != "original" {
		t.Fatal("snapshot or fixture changed")
	}
	if _, err = s.Snapshot(ctx, "g", fixture); err == nil {
		t.Fatal("outside fixture accepted")
	}
	link := filepath.Join(root, "link.kicad_sch")
	if err = os.Symlink(fixture, link); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Snapshot(ctx, "g", link); err == nil {
		t.Fatal("symlink escape accepted")
	}
}
