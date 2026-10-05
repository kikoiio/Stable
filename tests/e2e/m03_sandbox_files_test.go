package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"stable/internal/platform/sandbox"
)

func TestM03SandboxFiles(t *testing.T) {
	project := requiredEnv(t, "M03_PROJECT")
	candidate := requiredEnv(t, "M03_CANDIDATE")
	runDir := requiredEnv(t, "M03_RUN_DIR")
	sentinel := requiredEnv(t, "M03_SENTINEL")
	marker := "m03-candidate-write-" + filepath.Base(t.Name())
	sentinelContents, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read outside sentinel: %v", err)
	}
	formalBefore, err := treeDigest(project)
	if err != nil {
		t.Fatalf("digest formal project before sandbox run: %v", err)
	}
	linkPath := filepath.Join(candidate, "m03-outside-link")
	if err = os.Symlink(sentinel, linkPath); err != nil {
		t.Fatalf("create candidate symlink fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(linkPath) })

	manager := sandbox.New()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	profile := sandbox.SandboxProfile{
		ProjectRoot:   project,
		CandidateRoot: candidate,
		RunRoot:       runDir,
		Timeout:       10 * time.Second,
		OutputLimit:   4096,
	}

	command := `for path in "$1" "$2" "$3"; do
		if cat "$path" 2>/dev/null; then exit 41; fi
	done
	printf '%s' "$4" > /workspace/candidate/m03-write-check
	/bin/sh -c 'if cat "$1" 2>/dev/null; then exit 42; fi; if printf child-write > "$2" 2>/dev/null; then exit 43; fi' sandbox-child "$1" "$5"`
	formalTarget := filepath.Join(project, "sensor.kicad_sch")
	result, err := manager.RunIsolated(ctx, profile, []string{"/bin/sh", "-c", command, "sandbox-check", sentinel, "/workspace/project/../outside/sentinel.txt", "/workspace/candidate/m03-outside-link", marker, "/workspace/project/sensor.kicad_sch"}, nil)
	if err != nil {
		t.Fatalf("isolated run failed (isolation unavailable is not a pass): %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("isolated command exited %d: %s", result.ExitCode, result.Stderr)
	}
	if strings.Contains(string(result.Stdout), string(sentinelContents)) {
		t.Fatal("outside sentinel contents were readable inside sandbox")
	}
	if _, err = os.Stat(formalTarget); err != nil {
		t.Fatalf("formal project schematic disappeared: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(candidate, "m03-write-check"))
	if err != nil {
		t.Fatalf("candidate write was not visible on host: %v", err)
	}
	if string(written) != marker {
		t.Fatalf("unexpected candidate marker %q", written)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("outside sentinel disappeared or was modified: %v", err)
	}
	formalAfter, err := treeDigest(project)
	if err != nil {
		t.Fatalf("digest formal project after sandbox run: %v", err)
	}
	if formalAfter != formalBefore {
		t.Fatalf("formal project changed across sandbox run: before=%s after=%s", formalBefore, formalAfter)
	}
}

func treeDigest(root string) (string, error) {
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected symlink in formal project: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected non-regular formal project entry: %s", path)
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		if _, err = io.WriteString(h, rel+"\x00"); err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(h, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Skipf("%s is supplied by the dedicated E2E fixture script", name)
	}
	return value
}
