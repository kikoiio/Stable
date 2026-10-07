package memory

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"stable/internal/platform/secfile"
)

const (
	MaxMemoryFileBytes = 1 << 20
	MaxMemoryFiles     = 200
	MaxFrontmatterLine = 30
	MaxMemoryIndexLine = 200
	MaxMemoryIndexByte = 25 * 1024
)

var safeFilename = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,119}\.md$`)

type Store struct {
	projectPath string
	userPath    string
	projectRoot secfile.Root
	userRoot    secfile.Root
}

type memoryFrontmatter struct {
	Scope       MemoryScope `yaml:"scope"`
	Type        MemoryType  `yaml:"type"`
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	UpdatedAt   time.Time   `yaml:"updated_at"`
}

func NewStore(projectPath, userConfigPath string) (*Store, error) {
	projectPath, err := filepath.Abs(projectPath)
	if err != nil {
		return nil, err
	}
	userConfigPath, err = filepath.Abs(userConfigPath)
	if err != nil {
		return nil, err
	}
	if err := secfile.MkdirAllPrivate(userConfigPath, 0700); err != nil {
		return nil, fmt.Errorf("prepare user config root: %w", err)
	}
	projectRoot, err := secfile.OpenRoot(projectPath)
	if err != nil {
		return nil, fmt.Errorf("open project root: %w", err)
	}
	userRoot, err := secfile.OpenRoot(userConfigPath)
	if err != nil {
		return nil, fmt.Errorf("open user config root: %w", err)
	}
	return &Store{
		projectPath: projectPath,
		userPath:    userConfigPath,
		projectRoot: projectRoot,
		userRoot:    userRoot,
	}, nil
}

func (s *Store) rootAndDir(scope MemoryScope) (secfile.Root, string, error) {
	switch scope {
	case ScopeUser:
		return s.userRoot, filepath.ToSlash(filepath.Join("stable", "memory")), nil
	case ScopeProject:
		return s.projectRoot, filepath.ToSlash(filepath.Join(".stable", "memory")), nil
	default:
		return secfile.Root{}, "", fmt.Errorf("invalid memory scope %q", scope)
	}
}

func (s *Store) ensureScope(scope MemoryScope) (secfile.Root, string, error) {
	root, dir, err := s.rootAndDir(scope)
	if err != nil {
		return secfile.Root{}, "", err
	}
	perm := os.FileMode(0755)
	if scope == ScopeUser {
		perm = 0700
	}
	if err := root.MkdirAll(dir, perm); err != nil {
		return secfile.Root{}, "", err
	}
	if scope == ScopeUser {
		path := filepath.Join(s.userPath, filepath.FromSlash(dir))
		private, err := secfile.IsPrivatePath(path)
		if err != nil {
			return secfile.Root{}, "", err
		}
		if !private {
			return secfile.Root{}, "", fmt.Errorf("user memory directory must be private (%s)", secfile.FixHint("dir"))
		}
	}
	return root, dir, nil
}

func (s *Store) Headers(scope MemoryScope) ([]MemoryHeader, []LoadIssue, error) {
	root, dir, err := s.rootAndDir(scope)
	if err != nil {
		return nil, nil, err
	}
	entries, err := root.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	headers := make([]MemoryHeader, 0)
	issues := make([]LoadIssue, 0)
	seen := 0
	for _, entry := range entries {
		name := entry.Name()
		if name == "MEMORY.md" || !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		seen++
		if seen > MaxMemoryFiles {
			issues = append(issues, LoadIssue{Path: filepath.Join(dir, name), Reason: "memory file limit reached; remaining entries skipped"})
			break
		}
		if entry.Type()&os.ModeSymlink != 0 || !safeFilename.MatchString(name) {
			issues = append(issues, LoadIssue{Path: filepath.Join(dir, name), Reason: "unsafe memory filename or file type"})
			continue
		}
		item, issue := readMemoryFile(root, dir, scope, name)
		if issue != nil {
			issues = append(issues, *issue)
			continue
		}
		headers = append(headers, item.MemoryHeader)
	}
	return headers, issues, nil
}

func (s *Store) List() ([]MemoryHeader, []LoadIssue, error) {
	var all []MemoryHeader
	var issues []LoadIssue
	for _, scope := range []MemoryScope{ScopeUser, ScopeProject} {
		headers, found, err := s.Headers(scope)
		if err != nil {
			issues = append(issues, LoadIssue{Path: string(scope), Reason: err.Error()})
			continue
		}
		all = append(all, headers...)
		issues = append(issues, found...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Scope != all[j].Scope {
			return all[i].Scope < all[j].Scope
		}
		return all[i].Filename < all[j].Filename
	})
	return all, issues, nil
}

func (s *Store) Read(scope MemoryScope, filename string) (MemoryEntry, error) {
	root, dir, err := s.rootAndDir(scope)
	if err != nil {
		return MemoryEntry{}, err
	}
	if !safeFilename.MatchString(filename) {
		return MemoryEntry{}, secfile.ErrUnsafePath
	}
	entry, issue := readMemoryFile(root, dir, scope, filename)
	if issue != nil {
		return MemoryEntry{}, fmt.Errorf("%s: %s", issue.Path, issue.Reason)
	}
	return entry, nil
}

func (s *Store) Save(change MemoryChange) (MemoryHeader, error) {
	if change.Action != ActionUpsert {
		return MemoryHeader{}, errors.New("memory save requires the upsert action")
	}
	wantScope, ok := ScopeForType(change.Type)
	if !ok || wantScope != change.Scope {
		return MemoryHeader{}, errors.New("memory type does not belong to requested scope")
	}
	change.Name = strings.TrimSpace(change.Name)
	change.Description = strings.TrimSpace(change.Description)
	change.Body = strings.TrimSpace(change.Body)
	if change.Name == "" || len(change.Name) > 160 || change.Body == "" || len(change.Description) > 1000 {
		return MemoryHeader{}, errors.New("invalid memory name, description, or body")
	}
	root, dir, err := s.ensureScope(change.Scope)
	if err != nil {
		return MemoryHeader{}, err
	}
	headers, _, err := s.Headers(change.Scope)
	if err != nil {
		return MemoryHeader{}, err
	}
	filename := ""
	used := make(map[string]bool, len(headers))
	for _, header := range headers {
		used[header.Filename] = true
		if header.Type == change.Type && header.Name == change.Name {
			filename = header.Filename
			break
		}
	}
	if filename == "" {
		filename = makeMemoryFilename(change.Name)
		if used[filename] {
			hash := sha256.Sum256([]byte(string(change.Type) + "\x00" + change.Name))
			filename = strings.TrimSuffix(filename, ".md") + "-" + fmt.Sprintf("%x", hash[:5]) + ".md"
		}
		if used[filename] {
			return MemoryHeader{}, errors.New("memory filename collision")
		}
	}
	header := MemoryHeader{
		Scope:       change.Scope,
		Type:        change.Type,
		Filename:    filename,
		Name:        change.Name,
		Description: change.Description,
		UpdatedAt:   time.Now().UTC().Truncate(time.Second),
	}
	data, err := marshalMemoryFile(header, change.Body)
	if err != nil {
		return MemoryHeader{}, err
	}
	if len(data) > MaxMemoryFileBytes {
		return MemoryHeader{}, errors.New("memory file exceeds 1 MiB limit")
	}
	perm := os.FileMode(0644)
	if change.Scope == ScopeUser {
		perm = 0600
	}
	if err := root.WriteFileAtomic(filepath.ToSlash(filepath.Join(dir, filename)), data, perm); err != nil {
		return MemoryHeader{}, err
	}
	if err := s.rebuildIndex(change.Scope); err != nil {
		return MemoryHeader{}, err
	}
	return header, nil
}

func (s *Store) Delete(scope MemoryScope, filename string) error {
	if !safeFilename.MatchString(filename) {
		return secfile.ErrUnsafePath
	}
	root, dir, err := s.rootAndDir(scope)
	if err != nil {
		return err
	}
	if err := root.RemoveFile(filepath.ToSlash(filepath.Join(dir, filename))); err != nil {
		return err
	}
	return s.rebuildIndex(scope)
}

func (s *Store) Clear(scope MemoryScope) (int, error) {
	root, dir, err := s.rootAndDir(scope)
	if err != nil {
		return 0, err
	}
	headers, issues, err := s.Headers(scope)
	if err != nil {
		return 0, err
	}
	for _, header := range headers {
		if err := root.RemoveFile(filepath.ToSlash(filepath.Join(dir, header.Filename))); err != nil {
			return 0, err
		}
	}
	if len(issues) > 0 {
		if err := s.rebuildIndex(scope); err != nil {
			return len(headers), err
		}
		return len(headers), fmt.Errorf("cleared %d entries; %d unsafe or invalid entries remain", len(headers), len(issues))
	}
	if err := root.RemoveFile(filepath.ToSlash(filepath.Join(dir, "MEMORY.md"))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	return len(headers), nil
}

func (s *Store) Index(scope MemoryScope) (string, bool, []LoadIssue, error) {
	root, dir, err := s.rootAndDir(scope)
	if err != nil {
		return "", false, nil, err
	}
	filename := filepath.ToSlash(filepath.Join(dir, "MEMORY.md"))
	f, err := root.Open(filename)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil, nil
	}
	if err != nil {
		return "", false, []LoadIssue{{Path: filename, Reason: err.Error()}}, nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxMemoryIndexByte+1))
	if err != nil {
		return "", false, []LoadIssue{{Path: filename, Reason: err.Error()}}, nil
	}
	truncated := len(data) > MaxMemoryIndexByte
	if truncated {
		data = data[:MaxMemoryIndexByte]
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > MaxMemoryIndexLine {
		lines = lines[:MaxMemoryIndexLine]
		truncated = true
	}
	return strings.Join(lines, "\n"), truncated, nil, nil
}

func (s *Store) rebuildIndex(scope MemoryScope) error {
	root, dir, err := s.ensureScope(scope)
	if err != nil {
		return err
	}
	headers, _, err := s.Headers(scope)
	if err != nil {
		return err
	}
	data := renderMemoryIndex(headers)
	perm := os.FileMode(0644)
	if scope == ScopeUser {
		perm = 0600
	}
	return root.WriteFileAtomic(filepath.ToSlash(filepath.Join(dir, "MEMORY.md")), data, perm)
}

func readMemoryFile(root secfile.Root, dir string, scope MemoryScope, filename string) (MemoryEntry, *LoadIssue) {
	path := filepath.ToSlash(filepath.Join(dir, filename))
	f, err := root.Open(path)
	if err != nil {
		return MemoryEntry{}, &LoadIssue{Path: path, Reason: err.Error()}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxMemoryFileBytes+1))
	if err != nil {
		return MemoryEntry{}, &LoadIssue{Path: path, Reason: err.Error()}
	}
	if len(data) > MaxMemoryFileBytes {
		return MemoryEntry{}, &LoadIssue{Path: path, Reason: "memory file exceeds 1 MiB limit"}
	}
	entry, err := parseMemoryFile(data)
	if err != nil {
		return MemoryEntry{}, &LoadIssue{Path: path, Reason: err.Error()}
	}
	if entry.Scope != scope {
		return MemoryEntry{}, &LoadIssue{Path: path, Reason: "frontmatter scope does not match directory"}
	}
	wantScope, ok := ScopeForType(entry.Type)
	if !ok || wantScope != scope || strings.TrimSpace(entry.Name) == "" || len(entry.Name) > 160 || entry.UpdatedAt.IsZero() {
		return MemoryEntry{}, &LoadIssue{Path: path, Reason: "invalid memory frontmatter"}
	}
	entry.Filename = filename
	return entry, nil
}

func parseMemoryFile(data []byte) (MemoryEntry, error) {
	if len(data) == 0 || len(data) > MaxMemoryFileBytes {
		return MemoryEntry{}, errors.New("invalid memory file size")
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if lines[0] != "---" {
		return MemoryEntry{}, errors.New("missing frontmatter opener")
	}
	end := -1
	for i := 1; i < len(lines) && i < MaxFrontmatterLine; i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return MemoryEntry{}, errors.New("frontmatter exceeds 30 lines or is not closed")
	}
	var meta memoryFrontmatter
	decoder := yaml.NewDecoder(strings.NewReader(strings.Join(lines[1:end], "\n")))
	decoder.KnownFields(true)
	if err := decoder.Decode(&meta); err != nil {
		return MemoryEntry{}, fmt.Errorf("invalid frontmatter: %w", err)
	}
	body := strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	return MemoryEntry{MemoryHeader: MemoryHeader{
		Scope: meta.Scope, Type: meta.Type, Name: strings.TrimSpace(meta.Name),
		Description: strings.TrimSpace(meta.Description), UpdatedAt: meta.UpdatedAt,
	}, Body: body}, nil
}

func marshalMemoryFile(header MemoryHeader, body string) ([]byte, error) {
	meta := memoryFrontmatter{
		Scope: header.Scope, Type: header.Type, Name: header.Name,
		Description: header.Description, UpdatedAt: header.UpdatedAt,
	}
	frontmatter, err := yaml.Marshal(meta)
	if err != nil {
		return nil, err
	}
	return append(append(append([]byte("---\n"), frontmatter...), []byte("---\n\n")...), []byte(body+"\n")...), nil
}

func renderMemoryIndex(headers []MemoryHeader) []byte {
	sort.Slice(headers, func(i, j int) bool {
		if headers[i].Type != headers[j].Type {
			return headers[i].Type < headers[j].Type
		}
		return headers[i].Name < headers[j].Name
	})
	var buffer bytes.Buffer
	buffer.WriteString("# Memory Index\n\n")
	truncated := false
	for i, header := range headers {
		line := fmt.Sprintf("- [%s](%s) — %s — %s (%s)\n", oneLine(header.Name), header.Filename,
			oneLine(header.Description), header.Type, header.UpdatedAt.UTC().Format("2006-01-02"))
		if i+1 > MaxMemoryIndexLine-1 || buffer.Len()+len(line) > MaxMemoryIndexByte-64 {
			truncated = true
			break
		}
		buffer.WriteString(line)
	}
	if truncated {
		buffer.WriteString("\n_Index truncated; inspect memory entries for the complete list._\n")
	}
	return buffer.Bytes()
}

func makeMemoryFilename(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastDash = false
		} else if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
		if b.Len() >= 110 {
			break
		}
	}
	stem := strings.Trim(b.String(), "-_")
	if stem == "" {
		stem = "memory"
	}
	return stem + ".md"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
