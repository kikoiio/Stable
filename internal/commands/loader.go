package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Load bounds for custom command files; files beyond them are skipped and
// reported instead of failing the whole load.
const (
	// MaxDepth is the maximum number of path segments of a command file
	// relative to its commands directory, the .md file included. It bounds
	// the command namespace, so "a/b.md" needs depth 2 and "a/b/c/d.md"
	// exceeds it.
	MaxDepth = 3
	// MaxFileBytes bounds the size of one command file.
	MaxFileBytes = 256 << 10
	// MaxCommands bounds the total number of loaded commands.
	MaxCommands = 200
)

// Meta is the frontmatter of a custom command file. It holds the subset of
// fields read for prompt commands: description, argument-hint, aliases.
type Meta struct {
	Description  string
	ArgumentHint string
	Aliases      []string
}

// fileCommand pairs a parsed command with the file it came from so
// rejections can name the file.
type fileCommand struct {
	cmd  *Command
	path string
}

// Loader loads custom prompt commands from a project directory and a user
// directory. Both directories are scanned recursively for .md files; a
// project command overrides a user command with the same name. Results are
// cached and refreshed only after a directory tree changed, so completion
// and dispatch can call Commands on every keystroke without rescanning.
type Loader struct {
	projectDir string
	userDir    string

	mu       sync.Mutex
	cacheKey map[string]time.Time
	cached   []*Command
	rejected []string
}

// NewLoader returns a loader for the given project and user command
// directories. Either directory may be empty or missing, which contributes
// no commands.
func NewLoader(projectDir, userDir string) *Loader {
	return &Loader{projectDir: projectDir, userDir: userDir}
}

// Commands returns the loaded commands and the report of rejected files.
// Every returned command is a KindPrompt command with a nil Local; the
// body it carries is the raw markdown template, expanded at dispatch time
// with ExpandPrompt. The returned slices are shared cache results and must
// be treated as read-only. Rejected files are skipped with a reason and
// never fail the load.
func (l *Loader) Commands() ([]*Command, []string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key, err := l.mtimes()
	if err != nil {
		return nil, nil, err
	}
	if sameMtimes(l.cacheKey, key) {
		return l.cached, l.rejected, nil
	}
	cmds, rejected := l.scan()
	l.cacheKey, l.cached, l.rejected = key, cmds, rejected
	return cmds, rejected, nil
}

// mtimes builds the cache key: the modification times of every file and
// directory under both command trees. Creating, modifying, or deleting a
// file changes its own or its parent directory time, so any such change
// invalidates the cache. Symlinked subdirectories are not followed.
func (l *Loader) mtimes() (map[string]time.Time, error) {
	key := make(map[string]time.Time)
	for _, dir := range [...]string{l.projectDir, l.userDir} {
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if !info.IsDir() {
			// Keep the entry in the key so replacing it with a real
			// directory invalidates the cache.
			key[dir] = info.ModTime()
			continue
		}
		err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			key[path] = info.ModTime()
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return key, nil
}

// sameMtimes reports whether two cache keys describe unchanged trees. A nil
// old key means nothing was loaded yet.
func sameMtimes(old, new map[string]time.Time) bool {
	if old == nil {
		return false
	}
	if len(old) != len(new) {
		return false
	}
	for path, modTime := range old {
		if other, ok := new[path]; !ok || !modTime.Equal(other) {
			return false
		}
	}
	return true
}

// scan rescans both directories and merges the results. Project commands
// are registered first so they take precedence over user commands of the
// same name; the shadowed user file is skipped silently because that is the
// documented precedence rule, not a rejection. Remaining collisions, which
// include frontmatter aliases clashing with other command names, are
// reported per file.
func (l *Loader) scan() ([]*Command, []string) {
	var rejected []string
	userFiles, userRejected := l.scanDir(l.userDir)
	rejected = append(rejected, userRejected...)
	projectFiles, projectRejected := l.scanDir(l.projectDir)
	rejected = append(rejected, projectRejected...)

	reg := NewRegistry()
	projectNames := make(map[string]bool, len(projectFiles))
	accepted := make([]*Command, 0, len(projectFiles)+len(userFiles))
	reject := func(fc *fileCommand, reason string) {
		rejected = append(rejected, fc.path+": "+reason)
	}
	for _, fc := range projectFiles {
		if len(accepted) >= MaxCommands {
			reject(fc, fmt.Sprintf("limit of %d commands reached", MaxCommands))
			continue
		}
		if reg.RegisterOptional(fc.cmd) {
			accepted = append(accepted, fc.cmd)
			projectNames[fc.cmd.Name] = true
			continue
		}
		reject(fc, "conflicts with an earlier command name or alias")
	}
	for _, fc := range userFiles {
		if projectNames[fc.cmd.Name] {
			continue
		}
		if len(accepted) >= MaxCommands {
			reject(fc, fmt.Sprintf("limit of %d commands reached", MaxCommands))
			continue
		}
		if reg.RegisterOptional(fc.cmd) {
			accepted = append(accepted, fc.cmd)
			continue
		}
		reject(fc, "conflicts with an earlier command name or alias")
	}
	sort.Slice(accepted, func(i, j int) bool { return accepted[i].Name < accepted[j].Name })
	return accepted, rejected
}

// scanDir walks one commands directory and parses every .md file. Files
// beyond the load bounds or with broken frontmatter are skipped and
// reported with their reason. A missing directory contributes nothing.
func (l *Loader) scanDir(dir string) ([]*fileCommand, []string) {
	if dir == "" {
		return nil, nil
	}
	if info, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("%s: %v", dir, err)}
	} else if !info.IsDir() {
		return nil, []string{fmt.Sprintf("%s: not a directory", dir)}
	}

	var cmds []*fileCommand
	var rejected []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			rejected = append(rejected, fmt.Sprintf("%s: %v", path, walkErr))
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		fc, reason := commandFromFile(dir, path)
		if reason != "" {
			rejected = append(rejected, reason)
			return nil
		}
		cmds = append(cmds, fc)
		return nil
	})
	if err != nil {
		rejected = append(rejected, fmt.Sprintf("%s: %v", dir, err))
	}
	return cmds, rejected
}

// commandFromFile parses one .md file into a prompt command. A non-empty
// returned reason describes why the file was rejected.
func commandFromFile(root, path string) (*fileCommand, string) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", path, err)
	}
	segments := strings.Split(filepath.ToSlash(rel), "/")
	if len(segments) > MaxDepth {
		return nil, fmt.Sprintf("%s: nesting deeper than %d levels", path, MaxDepth)
	}
	name, ok := commandName(segments)
	if !ok {
		return nil, fmt.Sprintf("%s: empty command name", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", path, err)
	}
	if info.Size() > MaxFileBytes {
		return nil, fmt.Sprintf("%s: larger than %d bytes", path, MaxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", path, err)
	}
	meta, body, err := splitFrontmatter(string(data))
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", path, err)
	}
	body = strings.TrimSpace(body)
	if meta.Description == "" {
		meta.Description = firstNonHeaderLine(body)
	}
	return &fileCommand{
		cmd: &Command{
			Name:        name,
			Description: meta.Description,
			ArgPrompt:   meta.ArgumentHint,
			Aliases:     meta.Aliases,
			Kind:        KindPrompt,
			Body:        body,
		},
		path: path,
	}, ""
}

// commandName maps a relative path to a command name: segments are
// lowercased, spaces become "-", and directory separators become ":" so
// "git/log.md" is named "git:log". It fails when a segment normalizes to
// nothing.
func commandName(segments []string) (string, bool) {
	parts := make([]string, len(segments))
	for i, segment := range segments {
		segment = strings.ToLower(strings.ReplaceAll(segment, " ", "-"))
		segment = strings.TrimSuffix(segment, ".md")
		if segment == "" {
			return "", false
		}
		parts[i] = segment
	}
	return strings.Join(parts, ":"), true
}

// splitFrontmatter separates a "---" delimited frontmatter block from the
// markdown body. Files without a frontmatter block return the whole content
// as body; a block that is never closed or cannot be parsed is an error, so
// the file can be reported as invalid instead of silently misloaded.
func splitFrontmatter(content string) (Meta, string, error) {
	lines := strings.Split(content, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start >= len(lines) || trimLine(lines[start]) != "---" {
		return Meta{}, content, nil
	}
	end := start + 1
	for end < len(lines) && trimLine(lines[end]) != "---" {
		end++
	}
	if end >= len(lines) {
		return Meta{}, "", errors.New("frontmatter is not closed with ---")
	}
	meta, err := parseMeta(lines[start+1 : end])
	if err != nil {
		return Meta{}, "", err
	}
	return meta, strings.Join(lines[end+1:], "\n"), nil
}

// trimLine strips trailing whitespace and carriage returns so files with
// CRLF line endings parse the same as LF ones.
func trimLine(line string) string {
	return strings.TrimRight(line, " \t\r")
}

// parseMeta reads the supported frontmatter keys from the lines between the
// "---" markers: "key: value" scalars, "aliases: [a, b]" inline lists, and
// "aliases:" followed by indented "- a" lines. Unknown keys are ignored;
// lines that are neither a key nor a list item are a parse error.
func parseMeta(lines []string) (Meta, error) {
	var meta Meta
	for i := 0; i < len(lines); {
		line := strings.TrimSpace(trimLine(lines[i]))
		if line == "" {
			i++
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Meta{}, fmt.Errorf("invalid frontmatter line %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "description":
			meta.Description = scalar(value)
		case "argument-hint":
			meta.ArgumentHint = scalar(value)
		case "aliases":
			if value == "" {
				var err error
				meta.Aliases, i, err = parseListItems(lines, i+1)
				if err != nil {
					return Meta{}, err
				}
				continue
			}
			aliases, err := parseInlineList(value)
			if err != nil {
				return Meta{}, err
			}
			meta.Aliases = aliases
		}
		i++
	}
	return meta, nil
}

// parseListItems consumes the "- a" lines of a multi-line list starting at
// index i and returns the items and the index of the first line after the
// list.
func parseListItems(lines []string, i int) ([]string, int, error) {
	var items []string
	for i < len(lines) {
		line := strings.TrimSpace(trimLine(lines[i]))
		if line == "" {
			i++
			continue
		}
		if !strings.HasPrefix(line, "-") {
			if len(items) == 0 {
				return nil, i, fmt.Errorf("aliases list must use \"- item\" lines")
			}
			break
		}
		if item := scalar(strings.TrimSpace(strings.TrimPrefix(line, "-"))); item != "" {
			items = append(items, item)
		}
		i++
	}
	return items, i, nil
}

// parseInlineList parses an "aliases: [a, b]" style list.
func parseInlineList(value string) ([]string, error) {
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, fmt.Errorf("aliases must be a [a, b] list or \"- item\" lines")
	}
	inner := strings.TrimSpace(value[1 : len(value)-1])
	if inner == "" {
		return nil, nil
	}
	var aliases []string
	for _, part := range strings.Split(inner, ",") {
		if alias := scalar(strings.TrimSpace(part)); alias != "" {
			aliases = append(aliases, alias)
		}
	}
	return aliases, nil
}

// scalar strips one pair of matching surrounding quotes from a value, the
// subset of YAML scalar quoting that hand-written frontmatter needs.
func scalar(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

// firstNonHeaderLine returns the first non-empty, non-heading line of a
// body; it is the description fallback for files without frontmatter.
func firstNonHeaderLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// ExpandPrompt renders a command body with the user arguments. Every
// "$ARGUMENTS" placeholder is replaced; a body without a placeholder gets
// the arguments appended in a "## User Request" section instead, so they
// are never silently dropped.
func ExpandPrompt(body, args string) string {
	if strings.Contains(body, "$ARGUMENTS") {
		return strings.ReplaceAll(body, "$ARGUMENTS", args)
	}
	if strings.TrimSpace(args) == "" {
		return body
	}
	return body + "\n\n## User Request\n\n" + args
}
