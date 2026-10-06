package secfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateModesOnPOSIX(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b")
	if err := MkdirAllPrivate(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "a"), dir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("%s mode = %#o, want 0700", path, info.Mode().Perm())
		}
	}
	file := filepath.Join(dir, "secret")
	f, err := OpenFilePrivate(file, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file mode = %#o, want 0600", info.Mode().Perm())
	}
}

func TestCurrentUserSDDL(t *testing.T) {
	got := currentUserSDDL("S-1-5-21-1-2-3-1001")
	want := "D:P(A;;FA;;;S-1-5-21-1-2-3-1001)"
	if got != want {
		t.Fatalf("currentUserSDDL = %q, want %q", got, want)
	}
}

func TestCurrentUserPipeSDDL(t *testing.T) {
	got := currentUserPipeSDDL("S-1-5-21-1-2-3-1001")
	want := "D:P(A;;GA;;;S-1-5-21-1-2-3-1001)"
	if got != want {
		t.Fatalf("currentUserPipeSDDL = %q, want %q", got, want)
	}
}

func TestPermDeniesOthers(t *testing.T) {
	cases := []struct {
		perm os.FileMode
		ok   bool
	}{
		{0600, true},
		{0700, true},
		{0400, true},
		{0644, false},
		{0755, false},
		{0660, false},
		{0444, false},
	}
	for _, c := range cases {
		if got := permDeniesOthers(c.perm); got != c.ok {
			t.Errorf("permDeniesOthers(%#o) = %v, want %v", c.perm, got, c.ok)
		}
	}
}

func TestFixHint(t *testing.T) {
	fileHint := fixHint("file")
	dirHint := fixHint("dir")
	if fileHint == "" || dirHint == "" {
		t.Fatal("empty fix hint")
	}
	// On Linux CI we expect POSIX wording.
	if !strings.Contains(fileHint, "chmod") && !strings.Contains(fileHint, "icacls") {
		t.Fatalf("unexpected file hint: %q", fileHint)
	}
	if !strings.Contains(dirHint, "chmod") && !strings.Contains(dirHint, "icacls") {
		t.Fatalf("unexpected dir hint: %q", dirHint)
	}
}
