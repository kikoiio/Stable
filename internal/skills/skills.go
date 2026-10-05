// Package skills discovers and parses skill directories (reusable prompt
// packages) from a user-level and a project-level skills directory. It is a
// two-phase catalog: phase 1 reads only frontmatter so a rescan is cheap, and
// Catalog.GetFull re-reads the body from disk on every call, which makes body
// edits hot without any reload. Directory additions and removals are detected
// by comparing directory mtimes against a snapshot taken at load time.
//
// The package has zero internal dependencies: it needs only the standard
// library and gopkg.in/yaml.v3.
package skills

// Load bounds; entries beyond them are skipped and reported instead of
// failing the whole load. The magnitudes align with the M06 commands loader.
const (
	// MaxFileBytes bounds the size of one skill file (SKILL.md, skill.yaml
	// or prompt.md).
	MaxFileBytes = 256 << 10
	// MaxSkills bounds the total number of skills in a catalog. Further
	// candidates are rejected and reported.
	MaxSkills = 200
)

// SkillMeta is the frontmatter of a skill. It is shared by both layouts: the
// YAML frontmatter block of SKILL.md and the body of skill.yaml.
type SkillMeta struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	WhenToUse   string   `yaml:"when_to_use"`
	Tags        []string `yaml:"tags"`
	// Mode selects the execution mode. "inline" (default) injects the skill
	// body into the current conversation; "fork" would run it in a sub-agent
	// with isolated context, which is reserved for M09: fork skills parse,
	// list and register, but every activation path rejects them.
	Mode string `yaml:"mode"`
	// Context is kept for backward compatibility with old skills that used
	// `context: fork` to mean Mode=fork. Treated as fork mode if "fork".
	Context string `yaml:"context"`
	// ForkContext controls how much of the parent conversation a forked
	// sub-agent would carry. Parsed and preserved; no behavior before M09.
	ForkContext string `yaml:"fork_context"`
}

// IsFork reports whether the skill is marked for fork mode. Both the Mode
// field and the legacy Context field count.
func (m SkillMeta) IsFork() bool {
	return m.Mode == "fork" || m.Context == "fork"
}

// Skill is one discovered skill. Phase-1 entries carry only the parsed
// frontmatter: PromptBody is empty and BodyLoaded is false until the body is
// read, either by GetFull or — for the skill.yaml layout with an inline body
// field — at parse time, because there is nothing left to read lazily.
type Skill struct {
	Meta       SkillMeta
	PromptBody string
	// SourceDir is the skill directory on disk; the body is re-read from it
	// on every GetFull call.
	SourceDir string
	// Source is the tier the skill came from: "user" or "project".
	Source string
	// BodyLoaded reports whether PromptBody has been read from disk.
	BodyLoaded bool
}
