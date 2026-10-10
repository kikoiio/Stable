//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceMountIDIdentifiesOrdinaryDirectories(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	rootID, err := workspaceMountID(root)
	if err != nil {
		t.Fatalf("read root mount ID: %v", err)
	}
	nestedID, err := workspaceMountID(nested)
	if err != nil {
		t.Fatalf("read nested directory mount ID: %v", err)
	}
	if nestedID != rootID {
		t.Fatalf("ordinary nested directory mount ID=%d, want root mount ID %d", nestedID, rootID)
	}
	if _, err := workspaceMountID(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing path returned a mount ID")
	}
}
