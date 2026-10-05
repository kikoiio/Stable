package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Catalog is the in-memory registry of discovered skills. Entries are
// phase-1: only frontmatter is held, and GetFull re-reads the body from disk
// on every call (hot body reload). Additions and removals of skill
// directories are detected by NeedsReload against the mtime snapshot taken
// at load time.
//
// A Catalog is safe for concurrent use. Getter methods return copies, so
// callers never observe a body being swapped underneath them.
type Catalog struct {
	mu         sync.Mutex
	skills     map[string]*Skill
	userDir    string
	projectDir string
	// dirModTimes is the hot-reload snapshot: the tier roots (zero time
	// while missing) plus every skill directory seen at load time, accepted
	// or rejected.
	dirModTimes map[string]time.Time
	rejections  []string
}

// skillCand pairs a parsed phase-1 skill with its directory for the merge
// step, which needs the directory for report lines and the name for
// deduplication.
type skillCand struct {
	name  string
	dir   string
	skill *Skill
}

// LoadCatalog scans the user-level and project-level skills directories and
// merges them into a phase-1 catalog. Skills are direct subdirectories of
// either tier; project entries override user entries of the same name. Both
// directories may be empty or missing, which contributes nothing. Load
// problems never fail the call — they are reported through Rejections.
func LoadCatalog(userDir, projectDir string) *Catalog {
	c := &Catalog{
		skills:     make(map[string]*Skill),
		userDir:    userDir,
		projectDir: projectDir,
	}
	c.rebuild()
	return c
}

// Get returns a copy of the phase-1 skill. PromptBody may be empty because
// the body is only read by GetFull.
func (c *Catalog) Get(name string) (*Skill, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.skills[name]
	if !ok {
		return nil, false
	}
	cp := *s
	return &cp, true
}

// GetFull returns the skill with its body freshly read from disk. On a read
// or parse failure the previously cached body is preserved — both inside the
// catalog and in the returned copy — and the error (which names the skill)
// is returned; with no cached body the skill is nil.
func (c *Catalog) GetFull(name string) (*Skill, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.skills[name]
	if !ok {
		return nil, fmt.Errorf("未知技能: %s", name)
	}
	if err := loadSkillBody(s); err != nil {
		err = fmt.Errorf("读取技能 %s 正文失败: %w", name, err)
		if s.PromptBody == "" {
			return nil, err
		}
		cp := *s
		return &cp, err
	}
	cp := *s
	return &cp, nil
}

// List returns copies of all skills sorted by name.
func (c *Catalog) List() []Skill {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Skill, 0, len(c.skills))
	for _, s := range c.skills {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// Source returns the tier a skill came from ("user" or "project"), or ""
// for an unknown name.
func (c *Catalog) Source(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.skills[name]; ok {
		return s.Source
	}
	return ""
}

// NeedsReload reports whether the skills directories changed since the last
// load or reload: a tracked directory disappeared, a tracked directory's
// mtime moved (covering edits inside a skill directory and entry changes in
// a tier root), or a new subdirectory appeared under a tier root. Body-only
// edits need no reload — GetFull re-reads them anyway.
func (c *Catalog) NeedsReload() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for dir, recorded := range c.dirModTimes {
		info, err := os.Stat(dir)
		if err != nil {
			if recorded.IsZero() {
				continue // was missing at load time, still is
			}
			return true // directory disappeared
		}
		if !info.IsDir() || !info.ModTime().Equal(recorded) {
			return true
		}
	}
	for _, root := range [...]string{c.userDir, c.projectDir} {
		if root == "" {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 {
				continue
			}
			if _, tracked := c.dirModTimes[filepath.Join(root, entry.Name())]; !tracked {
				return true // new skill directory
			}
		}
	}
	return false
}

// Reload rescans both tiers and rebuilds the catalog in place, retaking the
// mtime snapshot and replacing the rejection report.
func (c *Catalog) Reload() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rebuild()
}

// Rejections returns one "path: reason" line per skipped or rejected skill,
// deduplicated and sorted by path. The coexistence note (SKILL.md preferred
// over skill.yaml) is informational and appears here too. The returned slice
// is a copy.
func (c *Catalog) Rejections() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.rejections))
	copy(out, c.rejections)
	sort.Strings(out)
	compacted := make([]string, 0, len(out))
	for i, line := range out {
		if i == 0 || line != out[i-1] {
			compacted = append(compacted, line)
		}
	}
	return compacted
}

// rebuild rescans both tiers, merges the candidates and retakes the mtime
// snapshot. The caller holds the mutex.
func (c *Catalog) rebuild() {
	userCands, userRejected, userWatched := scanTier(c.userDir, "user")
	projCands, projRejected, projWatched := scanTier(c.projectDir, "project")

	rejected := make([]string, 0, len(userRejected)+len(projRejected))
	rejected = append(rejected, userRejected...)
	rejected = append(rejected, projRejected...)

	// Merge with project priority: project candidates are accepted first so
	// they win both the name and a slot under MaxSkills; user candidates
	// follow for the remaining slots. Within a tier the first candidate of
	// a name wins, and a name already taken by the other tier is skipped
	// silently — that is the documented precedence rule, not a rejection.
	skills := make(map[string]*Skill, len(userCands)+len(projCands))
	accepted := 0
	accept := func(cands []skillCand) {
		for _, cand := range cands {
			if _, dup := skills[cand.name]; dup {
				continue
			}
			if accepted >= MaxSkills {
				rejected = append(rejected, fmt.Sprintf("%s: 超过技能数量上限 %d", cand.dir, MaxSkills))
				continue
			}
			skills[cand.name] = cand.skill
			accepted++
		}
	}
	accept(projCands)
	accept(userCands)

	// Snapshot the tier roots (zero while missing) and every watched skill
	// directory for NeedsReload.
	modTimes := make(map[string]time.Time, len(userWatched)+len(projWatched)+2)
	for _, root := range [...]string{c.userDir, c.projectDir} {
		if root == "" {
			continue
		}
		if info, err := os.Stat(root); err != nil {
			modTimes[root] = time.Time{}
		} else {
			modTimes[root] = info.ModTime()
		}
	}
	for _, dir := range userWatched {
		modTimes[dir] = dirModTime(dir)
	}
	for _, dir := range projWatched {
		modTimes[dir] = dirModTime(dir)
	}

	c.skills = skills
	c.rejections = rejected
	c.dirModTimes = modTimes
}

// scanTier scans one tier root. Every direct subdirectory is a skill
// candidate; non-directory entries are ignored, and symlink entries are
// rejected without being followed, so nothing outside the skills tree enters
// the catalog. Parse failures are reported, never fatal. The returned watched
// paths are the directories (accepted or not) whose mtimes NeedsReload
// tracks.
func scanTier(dir, source string) (cands []skillCand, rejected []string, watched []string) {
	if dir == "" {
		return nil, nil, nil
	}
	if info, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			rejected = append(rejected, fmt.Sprintf("%s: %v", dir, err))
		}
		return nil, rejected, nil
	} else if !info.IsDir() {
		return nil, []string{dir + ": " + reasonNotDir}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", dir, err)}, nil
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.Type()&fs.ModeSymlink != 0 {
			rejected = append(rejected, path+": "+reasonSymlinkDir)
			watched = append(watched, path)
			continue
		}
		if !entry.IsDir() {
			continue // loose files are not skills
		}
		watched = append(watched, path)
		skill, note, err := parseFrontmatterOnly(path)
		if note != "" {
			rejected = append(rejected, path+": "+note)
		}
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		skill.Source = source
		cands = append(cands, skillCand{name: skill.Meta.Name, dir: path, skill: skill})
	}
	return cands, rejected, watched
}

// dirModTime returns the mtime of dir, or the zero time when it cannot be
// stat'ed.
func dirModTime(dir string) time.Time {
	info, err := os.Stat(dir)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
