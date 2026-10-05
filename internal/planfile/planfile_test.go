package planfile

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPlanPathJoinsProjectRoot(t *testing.T) {
	root := t.TempDir()
	path, err := PlanPath(root, "session-abc")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".stable", "plans", "session-abc.md"); path != want {
		t.Fatalf("plan path %q want %q", path, want)
	}
	if DirName != ".stable/plans" {
		t.Fatalf("dir name %q", DirName)
	}
	if _, err = PlanPath("", "session-abc"); err == nil {
		t.Fatal("empty project root accepted")
	}
	if _, err = PlanPath("   ", "session-abc"); err == nil {
		t.Fatal("blank project root accepted")
	}
}

func TestUnsafeSessionIDRejected(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "a/b", "a\\b", "..", "../escape", "a/../b"} {
		if _, err := PlanPath(root, id); err == nil {
			t.Fatalf("plan path accepted session id %q", id)
		}
		if _, _, err := Ensure(root, id); err == nil {
			t.Fatalf("ensure accepted session id %q", id)
		}
		if _, err := Exists(root, id); err == nil {
			t.Fatalf("exists accepted session id %q", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".stable")); !os.IsNotExist(err) {
		t.Fatalf("unsafe session ids touched the state directory: %v", err)
	}
}

func TestEnsureCreatesOnceThenReuses(t *testing.T) {
	root := t.TempDir()
	path, existed, err := Ensure(root, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("first ensure reported an existing file")
	}
	if want := filepath.Join(root, ".stable", "plans", "session-a.md"); path != want {
		t.Fatalf("ensure path %q want %q", path, want)
	}
	for _, check := range []struct {
		name string
		path string
	}{
		{"state dir", filepath.Join(root, ".stable")},
		{"plans dir", filepath.Join(root, ".stable", "plans")},
	} {
		st, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0700 {
			t.Fatalf("%s mode %o", check.name, st.Mode().Perm())
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		t.Fatalf("plan file mode %v", st.Mode())
	}
	if st.Size() != 0 {
		t.Fatalf("fresh plan file size %d", st.Size())
	}

	if err = os.WriteFile(path, []byte("# plan draft\n"), 0600); err != nil {
		t.Fatal(err)
	}
	again, existedAgain, err := Ensure(root, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if !existedAgain {
		t.Fatal("second ensure did not report the existing file")
	}
	if again != path {
		t.Fatalf("second ensure path %q want %q", again, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# plan draft\n" {
		t.Fatalf("second ensure rewrote the plan file: %q", data)
	}
	if st, err = os.Stat(path); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("plan file mode after reuse: %v %v", st.Mode().Perm(), err)
	}
}

func TestExistsTracksPlanFile(t *testing.T) {
	root := t.TempDir()
	exists, err := Exists(root, "session-b")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("exists before ensure")
	}
	if _, _, err = Ensure(root, "session-b"); err != nil {
		t.Fatal(err)
	}
	if exists, err = Exists(root, "session-b"); err != nil || !exists {
		t.Fatalf("exists after ensure: %v %v", exists, err)
	}
	if err = os.Remove(filepath.Join(root, ".stable", "plans", "session-b.md")); err != nil {
		t.Fatal(err)
	}
	if exists, err = Exists(root, "session-b"); err != nil || exists {
		t.Fatalf("exists after remove: %v %v", exists, err)
	}
}

func TestEnsureRejectsSymlinkedPlanFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".stable", "plans")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.md")
	if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "session-c.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Ensure(root, "session-c"); err == nil {
		t.Fatal("ensure on a symlinked plan file succeeded")
	}
}

func TestConcurrentEnsureHasSingleCreator(t *testing.T) {
	root := t.TempDir()
	const goroutines = 8
	created := make([]bool, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			_, existed, err := Ensure(root, "shared-session")
			if err != nil {
				t.Error(err)
				return
			}
			created[g] = existed
		}(g)
	}
	wg.Wait()
	creators := 0
	for _, existed := range created {
		if !existed {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("creating ensures=%d want 1", creators)
	}
	st, err := os.Stat(filepath.Join(root, ".stable", "plans", "shared-session.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		t.Fatalf("plan file mode %v", st.Mode())
	}
}
