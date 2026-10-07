package memory

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"stable/internal/platform/secfile"
)

const MaxInstructionFileBytes = 1 << 20

type instructionRoot struct {
	path string
	root secfile.Root
	ok   bool
}

type instructionLoader struct {
	project instructionRoot
	user    instructionRoot
	seen    map[string]bool
	issues  []LoadIssue
}

// DiscoverInstructions reads the user instruction files, then walks from the
// project root to workDir, and finally reads STABLE.local.md in workDir.
// Include lines are expanded only when their resolved path stays beneath one
// of the two trusted roots.
func DiscoverInstructions(projectRoot, workDir, userConfigDir string) ([]InstructionSource, []LoadIssue) {
	projectAbs, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, []LoadIssue{{Path: projectRoot, Reason: err.Error()}}
	}
	project, err := secfile.OpenRoot(projectAbs)
	if err != nil {
		return nil, []LoadIssue{{Path: projectAbs, Reason: err.Error()}}
	}
	loader := &instructionLoader{
		project: instructionRoot{path: filepath.Clean(projectAbs), root: project, ok: true},
		seen:    make(map[string]bool),
	}
	if userConfigDir != "" {
		userAbs, absErr := filepath.Abs(userConfigDir)
		if absErr != nil {
			loader.issues = append(loader.issues, LoadIssue{Path: userConfigDir, Reason: absErr.Error()})
		} else if userRoot, rootErr := secfile.OpenRoot(userAbs); rootErr == nil {
			loader.user = instructionRoot{path: filepath.Clean(userAbs), root: userRoot, ok: true}
		} else if !errors.Is(rootErr, os.ErrNotExist) {
			loader.issues = append(loader.issues, LoadIssue{Path: userAbs, Reason: rootErr.Error()})
		}
	}

	workAbs, err := filepath.Abs(workDir)
	if err != nil {
		loader.issues = append(loader.issues, LoadIssue{Path: workDir, Reason: err.Error()})
		return nil, loader.issues
	}
	workRel, err := filepath.Rel(loader.project.path, filepath.Clean(workAbs))
	if err != nil || workRel == ".." || strings.HasPrefix(workRel, ".."+string(filepath.Separator)) || filepath.IsAbs(workRel) {
		loader.issues = append(loader.issues, LoadIssue{Path: workAbs, Reason: "work directory is outside project root"})
		return nil, loader.issues
	}
	dirs := []string{""}
	if workRel != "." {
		current := ""
		for _, part := range strings.Split(workRel, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			dirs = append(dirs, current)
		}
	}

	type candidate struct {
		root instructionRoot
		rel  string
	}
	var candidates []candidate
	if loader.user.ok {
		for _, name := range []string{"STABLE.md", "AGENTS.md"} {
			candidates = append(candidates, candidate{loader.user, name})
		}
	}
	for _, dir := range dirs {
		for _, name := range []string{"STABLE.md", "AGENTS.md", filepath.Join(".stable", "STABLE.md")} {
			candidates = append(candidates, candidate{loader.project, filepath.Join(dir, name)})
		}
	}
	if workRel == "." {
		candidates = append(candidates, candidate{loader.project, "STABLE.local.md"})
	} else {
		candidates = append(candidates, candidate{loader.project, filepath.Join(workRel, "STABLE.local.md")})
	}

	sources := make([]InstructionSource, 0, len(candidates))
	for _, item := range candidates {
		abs := filepath.Clean(filepath.Join(item.root.path, item.rel))
		content, loaded, issue := loader.load(item.root, item.rel, abs, 0)
		if issue != nil {
			loader.issues = append(loader.issues, *issue)
		}
		if loaded {
			sources = append(sources, InstructionSource{Path: abs, Priority: len(sources), Content: content})
		}
	}
	return sources, loader.issues
}

func (l *instructionLoader) load(root instructionRoot, rel, abs string, depth int) (string, bool, *LoadIssue) {
	abs = filepath.Clean(abs)
	if l.seen[abs] {
		return "", false, &LoadIssue{Path: abs, Reason: "duplicate instruction reference skipped"}
	}
	l.seen[abs] = true
	f, err := root.root.Open(filepath.ToSlash(rel))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, &LoadIssue{Path: abs, Reason: err.Error()}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxInstructionFileBytes+1))
	if err != nil {
		return "", false, &LoadIssue{Path: abs, Reason: err.Error()}
	}
	if len(data) > MaxInstructionFileBytes {
		return "", false, &LoadIssue{Path: abs, Reason: "instruction file exceeds 1 MiB limit"}
	}
	content := l.expand(string(data), root, abs, depth)
	return content, true, nil
}

func (l *instructionLoader) expand(content string, containingRoot instructionRoot, containingPath string, depth int) string {
	var out strings.Builder
	inFence := false
	fence := ""
	for _, line := range strings.SplitAfter(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker := trimmed[:3]
			if !inFence {
				inFence, fence = true, marker
			} else if marker == fence {
				inFence, fence = false, ""
			}
			out.WriteString(line)
			continue
		}
		if inFence || !strings.HasPrefix(trimmed, "@") {
			out.WriteString(line)
			continue
		}
		include := strings.TrimSpace(strings.TrimPrefix(trimmed, "@"))
		if !hasAllowedIncludePrefix(include) {
			out.WriteString(line)
			l.issues = append(l.issues, LoadIssue{Path: containingPath, Reason: "unsupported include syntax"})
			continue
		}
		if depth >= 5 {
			out.WriteString(line)
			l.issues = append(l.issues, LoadIssue{Path: containingPath, Reason: "include depth exceeds 5"})
			continue
		}
		target, targetRoot, rel, resolveErr := l.resolveInclude(include, containingRoot, containingPath)
		if resolveErr != nil {
			out.WriteString(line)
			l.issues = append(l.issues, LoadIssue{Path: containingPath, Reason: resolveErr.Error()})
			continue
		}
		expanded, ok, issue := l.load(targetRoot, rel, target, depth+1)
		if issue != nil {
			l.issues = append(l.issues, *issue)
		}
		if !ok && issue == nil {
			l.issues = append(l.issues, LoadIssue{Path: containingPath, Reason: "include target was not found"})
		}
		if !ok || issue != nil {
			out.WriteString(line)
			continue
		}
		out.WriteString(expanded)
		if strings.HasSuffix(line, "\n") && !strings.HasSuffix(expanded, "\n") {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func (l *instructionLoader) resolveInclude(include string, containingRoot instructionRoot, containingPath string) (string, instructionRoot, string, error) {
	var target string
	switch {
	case strings.HasPrefix(include, "~/"):
		if !l.user.ok {
			return "", instructionRoot{}, "", errors.New("user config include root is unavailable")
		}
		target = filepath.Join(l.user.path, filepath.FromSlash(strings.TrimPrefix(include, "~/")))
	case strings.HasPrefix(include, "/"):
		target = filepath.Clean(include)
	default:
		target = filepath.Join(filepath.Dir(containingPath), filepath.FromSlash(include))
	}
	target, err := filepath.Abs(target)
	if err != nil {
		return "", instructionRoot{}, "", err
	}
	target = filepath.Clean(target)
	for _, root := range []instructionRoot{l.project, l.user} {
		if !root.ok {
			continue
		}
		rel, relErr := filepath.Rel(root.path, target)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) && rel != "." {
			return target, root, rel, nil
		}
	}
	return "", instructionRoot{}, "", fmt.Errorf("include path is outside allowed roots")
}

func hasAllowedIncludePrefix(include string) bool {
	return strings.HasPrefix(include, "./") || strings.HasPrefix(include, "../") ||
		strings.HasPrefix(include, "~/") || strings.HasPrefix(include, "/")
}
