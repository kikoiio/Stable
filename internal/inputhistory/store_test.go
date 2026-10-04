package inputhistory

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAppendListAndPrivatePermissions(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Append("hello")
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != "hello" || first.At.IsZero() {
		t.Fatalf("entry: %+v", first)
	}
	if _, err = s.Append("world"); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Text != "hello" || entries[1].Text != "world" {
		t.Fatalf("list: %+v", entries)
	}
	st, err := os.Stat(filepath.Join(root, ".stable"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0700 {
		t.Fatalf("directory mode %o", st.Mode().Perm())
	}
	st, err = os.Stat(filepath.Join(root, ".stable", historyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("file mode %o", st.Mode().Perm())
	}
}

func TestReopenRestoresHistory(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three"} {
		if _, err = s.Append(text); err != nil {
			t.Fatal(err)
		}
	}
	again, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := again.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Text != "one" || entries[2].Text != "three" {
		t.Fatalf("list after reopen: %+v", entries)
	}
	if _, err = again.Append("four"); err != nil {
		t.Fatal(err)
	}
	third, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := third.List()
	if err != nil || len(listed) != 4 || listed[3].Text != "four" {
		t.Fatalf("append after reopen: %+v %v", listed, err)
	}
}

func TestBlankInputIgnored(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"", "   ", "\t\n "} {
		entry, err := s.Append(text)
		if err != nil {
			t.Fatal(err)
		}
		if entry != (Entry{}) {
			t.Fatalf("blank input stored: %+v", entry)
		}
	}
	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("blank inputs stored: %+v", entries)
	}
	if _, err = os.Stat(filepath.Join(root, ".stable", historyFileName)); !os.IsNotExist(err) {
		t.Fatalf("history file created for blank input: %v", err)
	}
}

func TestConsecutiveDuplicatesStoredOnce(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"a", "a", "b", "a"} {
		if _, err = s.Append(text); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Text != "a" || entries[1].Text != "b" || entries[2].Text != "a" {
		t.Fatalf("dedup: %+v", entries)
	}
}

func TestTrimsToMaxEntries(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= MaxEntries+5; i++ {
		if _, err = s.Append(fmt.Sprintf("input-%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != MaxEntries {
		t.Fatalf("entries=%d", len(entries))
	}
	if entries[0].Text != "input-006" || entries[MaxEntries-1].Text != fmt.Sprintf("input-%03d", MaxEntries+5) {
		t.Fatalf("trimmed bounds: %q .. %q", entries[0].Text, entries[MaxEntries-1].Text)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".stable", historyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != MaxEntries {
		t.Fatalf("file lines=%d", lines)
	}
	again, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := again.List()
	if err != nil || len(restored) != MaxEntries || restored[0].Text != "input-006" {
		t.Fatalf("trimmed history after reopen: %d %v", len(restored), err)
	}
}

func TestCursorBrowsesNewestFirst(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three"} {
		if _, err = s.Append(text); err != nil {
			t.Fatal(err)
		}
	}
	cursor, err := s.Cursor()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"three", "two", "one"} {
		entry, ok := cursor.Prev()
		if !ok || entry.Text != want {
			t.Fatalf("prev: %+v %v want %q", entry, ok, want)
		}
	}
	if _, ok := cursor.Prev(); ok {
		t.Fatal("prev past oldest entry")
	}
	if _, ok := cursor.Next(); !ok {
		t.Fatal("next from oldest entry")
	}
	entry, ok := cursor.Next()
	if !ok || entry.Text != "three" {
		t.Fatalf("next: %+v %v", entry, ok)
	}
	if _, ok = cursor.Next(); ok {
		t.Fatal("next past newest entry")
	}
}

func TestConcurrentAppendsKeepValidJSONL(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, err := s.Append(fmt.Sprintf("g%d-i%d", g, i)); err != nil {
					t.Error(err)
				}
			}
		}(g)
	}
	wg.Wait()
	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 80 {
		t.Fatalf("entries=%d", len(entries))
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Text] = true
	}
	for g := 0; g < 8; g++ {
		for i := 0; i < 10; i++ {
			if !seen[fmt.Sprintf("g%d-i%d", g, i)] {
				t.Fatalf("missing g%d-i%d", g, i)
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, ".stable", historyFileName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 80 {
		t.Fatalf("file lines=%d", len(lines))
	}
	for i, line := range lines {
		var e Entry
		if err = json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d not valid JSONL: %v", i+1, err)
		}
	}
}

func TestCorruptLineFailsClosed(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append("good"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".stable", historyFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("{broken\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err = s.List(); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("list on corrupt history: %v", err)
	}
	if _, err = s.Append("more"); err == nil {
		t.Fatal("append on corrupt history succeeded")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("corrupt history mutated by failed append")
	}
}

func TestTruncatedFinalLineFails(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".stable", historyFileName)
	if err = os.WriteFile(path, []byte("{\"text\":\"x\",\"at\":\"2026-10-04T00:00:00Z\"}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.List(); err == nil || !strings.Contains(err.Error(), "incomplete final line") {
		t.Fatalf("list on truncated history: %v", err)
	}
	if _, err = s.Append("y"); err == nil {
		t.Fatal("append on truncated history succeeded")
	}
}

func TestRedactsCredentialsBeforeAppend(t *testing.T) {
	root := t.TempDir()
	credential := "sk-test-credential-1234567890"
	s, err := Open(root, credential)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Append("please use " + credential + " here")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(entry.Text, credential) || !strings.Contains(entry.Text, redactedPlaceholder) {
		t.Fatalf("entry not redacted: %q", entry.Text)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".stable", historyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), credential) {
		t.Fatal("plaintext credential persisted")
	}
}

func TestUnsafeCredentialRefusesWrite(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, "short")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append("contains short secret"); err == nil {
		t.Fatal("append with unsafe credential succeeded")
	}
	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("plaintext stored despite unsafe credential: %+v", entries)
	}
}

func TestSymlinkedHistoryFileRejected(t *testing.T) {
	root := t.TempDir()
	if _, err := Prepare(root); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "elsewhere")
	if err := os.WriteFile(target, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, ".stable", historyFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("open on symlinked history succeeded")
	}
}

func TestPrepareAddsLocalGitExcludeIdempotently(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".stable", historyFileName)
	if err := exec.Command("git", "-C", root, "check-ignore", "-q", path).Run(); err != nil {
		t.Fatalf("history path not ignored: %v", err)
	}
	data, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		t.Fatal(err)
	}
	info := strings.TrimSpace(string(data))
	if !filepath.IsAbs(info) {
		info = filepath.Join(root, info)
	}
	b, err := os.ReadFile(info)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(b), ".stable/"+historyFileName); count != 1 {
		t.Fatalf("exclude rule count=%d contents=%q", count, b)
	}
}
