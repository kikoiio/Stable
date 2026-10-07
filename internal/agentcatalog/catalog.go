package agentcatalog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"stable/internal/platform/secfile"
)

type Snapshot struct {
	Definitions []Metadata `json:"definitions"`
	Rejections  []string   `json:"rejections,omitempty"`
}

type Catalog interface {
	Snapshot() Snapshot
	Reload() Snapshot
	Resolve(string) (Definition, bool)
}

// Registry replaces complete snapshots on reload. Resolved definitions and
// public inventories own their slices and remain unchanged by later reloads.
type Registry struct {
	mu                  sync.RWMutex
	reloadMu            sync.Mutex
	userDir, projectDir string
	definitions         map[string]Definition
	rejections          []string
}

// New only reads the explicitly supplied directories; runtime owns their trust.
// Empty or absent directories leave the three built-in roles available.
func New(userDir, projectDir string) *Registry {
	r := &Registry{userDir: userDir, projectDir: projectDir}
	r.Reload()
	return r
}

func (r *Registry) Resolve(name string) (Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.definitions[strings.ToLower(strings.TrimSpace(name))]
	return cloneDefinition(d), ok
}

func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return snapshot(r.definitions, r.rejections)
}

func snapshot(definitions map[string]Definition, rejections []string) Snapshot {
	result := Snapshot{Definitions: make([]Metadata, 0, len(definitions)), Rejections: cloneStrings(rejections)}
	for _, d := range definitions {
		result.Definitions = append(result.Definitions, d.Metadata())
	}
	sort.Slice(result.Definitions, func(i, j int) bool { return result.Definitions[i].Name < result.Definitions[j].Name })
	return result
}

func (r *Registry) Reload() Snapshot {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	definitions := builtins()
	var rejections []string
	r.loadDirectory(r.userDir, "user", definitions, &rejections)
	r.loadDirectory(r.projectDir, "project", definitions, &rejections)
	r.mu.Lock()
	r.definitions, r.rejections = definitions, rejections
	r.mu.Unlock()
	return snapshot(definitions, rejections)
}

func builtins() map[string]Definition {
	return map[string]Definition{
		"explore":         {Name: "explore", Description: "Read-only exploration of project files and architecture.", Model: "inherit", Instruction: "Explore the assigned question using read-only project inspection. Report concrete findings and relevant paths.", Source: "builtin", MaxTurns: MaxTurns},
		"plan":            {Name: "plan", Description: "Read-only analysis and implementation planning.", Model: "inherit", Instruction: "Inspect the project and develop a concrete plan for the assigned task. Identify affected files, dependencies, risks and validation. Do not modify files.", Source: "builtin", MaxTurns: MaxTurns},
		"general-purpose": {Name: "general-purpose", Description: "General project investigation, limited to read-only tools in M09-D.", Model: "inherit", Instruction: "Investigate the assigned task using read-only project inspection. Return a concise factual summary. This role has no write, command, network or delegation capability.", Source: "builtin", MaxTurns: MaxTurns},
	}
}

func (r *Registry) loadDirectory(dir, source string, definitions map[string]Definition, rejections *[]string) {
	if dir == "" {
		return
	}
	reject := func(path, reason string) { *rejections = append(*rejections, safeLabel(path)+": "+reason) }
	if err := validateDirectory(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			reject(dir, "definition directory is unsafe or unreadable")
		}
		return
	}
	root, err := secfile.OpenRoot(dir)
	if err != nil {
		reject(dir, "definition directory is unsafe or unreadable")
		return
	}
	defer root.Close()
	entries, err := os.ReadDir(root.Path())
	if err != nil {
		reject(dir, "cannot list definition directory")
		return
	}
	tier := make(map[string]Definition)
	for _, entry := range entries {
		// Direct Markdown files are the supported layout. Links and special
		// entries are rejected before opening, including links to directories.
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			reject(path, "cannot inspect definition entry")
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			reject(path, "symlinks and special files are not supported")
			continue
		}
		if info.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		if info.Size() > MaxFileBytes {
			reject(path, "definition exceeds 64 KiB")
			continue
		}
		data, err := readDefinition(root, entry.Name())
		if err != nil {
			reject(path, "definition file is unsafe, unreadable or exceeds 64 KiB")
			continue
		}
		d, err := ParseDefinition(data)
		if err != nil {
			reject(path, err.Error())
			continue
		}
		d.Source = source
		tier[d.Name] = d
	}
	if root.Revalidate() != nil || validateDirectory(dir) != nil {
		// Never publish a partially scanned tier whose trusted root changed.
		reject(dir, "definition directory changed while loading")
		return
	}
	for name, d := range tier {
		definitions[name] = d
	}
}

func readDefinition(root secfile.Root, name string) ([]byte, error) {
	if err := root.Revalidate(); err != nil {
		return nil, err
	}
	// Anchor the secure component walk at the filesystem root so no ancestor
	// of the agents directory can redirect the open through a symlink.
	anchor := filepath.VolumeName(root.Path()) + string(filepath.Separator)
	rel, err := filepath.Rel(anchor, filepath.Join(root.Path(), name))
	if err != nil {
		return nil, err
	}
	f, err := secfile.SecureOpen(anchor, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("definition exceeds %d bytes", MaxFileBytes)
	}
	if err := root.Revalidate(); err != nil {
		return nil, err
	}
	return data, nil
}

// Check every component, including .stable, so a symlinked ancestor cannot
// redirect even a trusted project-derived agents directory.
func validateDirectory(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := abs
	for {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return secfile.ErrUnsafePath
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func safeLabel(path string) string {
	path = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, path)
	if len(path) > 512 {
		path = path[:512]
		for !utf8.ValidString(path) {
			path = path[:len(path)-1]
		}
	}
	return path
}

var _ Catalog = (*Registry)(nil)
