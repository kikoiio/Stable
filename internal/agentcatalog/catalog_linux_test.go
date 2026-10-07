//go:build linux

package agentcatalog

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCatalogRejectsSpecialFileWithoutOpening(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "blocked.md"), 0600); err != nil {
		t.Fatal(err)
	}
	got := New(dir, "").Snapshot()
	if len(got.Definitions) != 3 || len(got.Rejections) != 1 {
		t.Fatalf("special file was not rejected: %#v", got)
	}
}
