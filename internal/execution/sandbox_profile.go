package execution

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"stable/internal/platform/secfile"
	"strings"
	"time"

	"stable/internal/core"
	"stable/internal/platform/sandbox"
)

// SandboxProfileFor builds the trusted sandbox profile validator shared by the
// worker and the conversation service: every legacy bridge request must name a
// project, candidate and run root, and the private run directory must stay
// inside the process's own run root. Anything else is refused before a
// sandbox is even constructed.
func SandboxProfileFor(runRoot, helperPath string) func(core.CapabilityRequest) (sandbox.SandboxProfile, error) {
	return func(req core.CapabilityRequest) (sandbox.SandboxProfile, error) {
		var payload struct {
			ProjectRoot   string `json:"project_root"`
			CandidateRoot string `json:"candidate_root"`
			RunRoot       string `json:"run_root"`
		}
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			return sandbox.SandboxProfile{}, err
		}
		if payload.ProjectRoot == "" || payload.CandidateRoot == "" || payload.RunRoot == "" {
			return sandbox.SandboxProfile{}, fmt.Errorf("bridge request lacks trusted project, candidate, or run root")
		}
		absProject, err := filepath.Abs(payload.ProjectRoot)
		if err != nil {
			return sandbox.SandboxProfile{}, err
		}
		absCandidate, err := filepath.Abs(payload.CandidateRoot)
		if err != nil {
			return sandbox.SandboxProfile{}, err
		}
		absRun, err := filepath.Abs(payload.RunRoot)
		if err != nil {
			return sandbox.SandboxProfile{}, err
		}
		workerRoot, err := realDirectory(runRoot)
		if err != nil {
			return sandbox.SandboxProfile{}, fmt.Errorf("worker run root: %w", err)
		}
		absProject, err = realDirectory(absProject)
		if err != nil {
			return sandbox.SandboxProfile{}, fmt.Errorf("project root: %w", err)
		}
		absCandidate, err = realDirectory(absCandidate)
		if err != nil {
			return sandbox.SandboxProfile{}, fmt.Errorf("candidate root: %w", err)
		}
		canonicalRun, err := pathWithoutSymlinkAncestors(absRun)
		if err != nil || canonicalRun != absRun || !pathWithinOrEqual(workerRoot, canonicalRun) {
			return sandbox.SandboxProfile{}, fmt.Errorf("bridge run directory is outside the worker run root or crosses a symbolic link")
		}
		if !pathWithinOrEqual(workerRoot, absProject) || !pathWithinOrEqual(workerRoot, absCandidate) {
			return sandbox.SandboxProfile{}, fmt.Errorf("project or candidate root is outside the worker run root")
		}
		if absProject == absCandidate || pathWithin(absProject, absCandidate) || pathWithin(absCandidate, absProject) {
			return sandbox.SandboxProfile{}, fmt.Errorf("project and candidate roots overlap")
		}
		if err := secfile.MkdirAllPrivate(absRun, 0700); err != nil {
			return sandbox.SandboxProfile{}, err
		}
		canonicalRun, err = realDirectory(absRun)
		if err != nil || canonicalRun != absRun || !pathWithinOrEqual(workerRoot, canonicalRun) {
			return sandbox.SandboxProfile{}, fmt.Errorf("private run root changed or crosses a symbolic link")
		}
		if err := secfile.ChmodPrivate(canonicalRun, 0700); err != nil {
			return sandbox.SandboxProfile{}, err
		}
		return sandbox.SandboxProfile{ProjectRoot: absProject, CandidateRoot: absCandidate, RunRoot: canonicalRun, Timeout: 90 * time.Second, OutputLimit: 1 << 20, ProxyHelperPath: helperPath}, nil
	}
}

func realDirectory(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is not a real directory", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(path) {
		return "", fmt.Errorf("%s crosses a symbolic link", path)
	}
	return filepath.Clean(path), nil
}

func pathWithoutSymlinkAncestors(path string) (string, error) {
	clean := filepath.Clean(path)
	missing := []string{}
	for current := clean; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("%s crosses a symbolic link", current)
			}
			resolved, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil || filepath.Clean(resolved) != current {
				return "", fmt.Errorf("%s crosses a symbolic link", current)
			}
			result := current
			for i := len(missing) - 1; i >= 0; i-- {
				result = filepath.Join(result, missing[i])
			}
			return result, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
	}
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func pathWithinOrEqual(root, target string) bool {
	return filepath.Clean(root) == filepath.Clean(target) || pathWithin(root, target)
}
