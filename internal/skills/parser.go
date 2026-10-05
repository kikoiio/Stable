package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Rejection reasons and informational notes. Each becomes one "path: text"
// line of Catalog.Rejections; parse errors carry the same vocabulary so a
// GetFull failure reads like the rejection it would have been at load time.
const (
	reasonSymlinkDir  = "技能目录为符号链接"
	reasonNoSkillFile = "缺少 SKILL.md 或 skill.yaml"
	reasonBodyMissing = "正文缺失: 无 prompt.md 且 skill.yaml 无 body"
	reasonNotDir      = "不是目录"
	// noteCoexistence is informational, not an error: it records that both
	// layouts were present and SKILL.md won.
	noteCoexistence = "SKILL.md 与 skill.yaml 并存,已采用 SKILL.md"
)

// skillYAML mirrors a skill.yaml file: the shared metadata keys plus an
// optional inline body. The body field is the fallback when prompt.md is
// absent; when prompt.md exists it is the body and Body is ignored.
type skillYAML struct {
	SkillMeta `yaml:",inline"`
	Body      string `yaml:"body"`
}

// parseFrontmatterOnly does phase-1 loading of one skill directory: read just
// enough to extract the SkillMeta, leave PromptBody empty so hundreds of
// skills stay cheap to rescan.
//
// Both layouts are accepted, SKILL.md taking precedence when both are present
// (which is reported as the coexistence note):
//   - <dir>/SKILL.md with `---` YAML frontmatter; the rest is the body
//   - <dir>/skill.yaml (+ optional prompt.md, or an inline body field)
//
// The returned note ("" when none) and error text are reason lines without
// the directory prefix; the caller turns them into "path: text" report lines.
// The description fallback scans the body, so it works for SKILL.md (whose
// body section is already in hand) and for an inline skill.yaml body, but not
// for prompt.md, which phase 1 deliberately does not read.
func parseFrontmatterOnly(dir string) (*Skill, string, error) {
	mdPath := filepath.Join(dir, "SKILL.md")
	yamlPath := filepath.Join(dir, "skill.yaml")

	if _, err := os.Stat(mdPath); err == nil {
		var note string
		if _, err := os.Stat(yamlPath); err == nil {
			note = noteCoexistence
		}
		data, err := readBounded(mdPath, "SKILL.md")
		if err != nil {
			return nil, note, err
		}
		meta, body, err := splitFrontmatter(string(data))
		if err != nil {
			return nil, note, fmt.Errorf("SKILL.md frontmatter 解析失败: %v", err)
		}
		applyMetaDefaults(&meta, dir, body)
		return &Skill{Meta: meta, SourceDir: dir}, note, nil
	}

	if _, err := os.Stat(yamlPath); err != nil {
		return nil, "", errors.New(reasonNoSkillFile)
	}
	data, err := readBounded(yamlPath, "skill.yaml")
	if err != nil {
		return nil, "", err
	}
	var wrapper skillYAML
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, "", fmt.Errorf("skill.yaml frontmatter 解析失败: %v", err)
	}
	meta := wrapper.SkillMeta

	promptPath := filepath.Join(dir, "prompt.md")
	info, err := os.Stat(promptPath)
	switch {
	case err == nil:
		if info.Size() > MaxFileBytes {
			return nil, "", fmt.Errorf("prompt.md 单文件超过 %d 字节上限", MaxFileBytes)
		}
		applyMetaDefaults(&meta, dir, "")
	case os.IsNotExist(err):
		if wrapper.Body == "" {
			return nil, "", errors.New(reasonBodyMissing)
		}
		applyMetaDefaults(&meta, dir, wrapper.Body)
		return &Skill{Meta: meta, SourceDir: dir, PromptBody: wrapper.Body, BodyLoaded: true}, "", nil
	default:
		return nil, "", fmt.Errorf("prompt.md 读取失败: %v", err)
	}
	return &Skill{Meta: meta, SourceDir: dir}, "", nil
}

// loadSkillBody reads the body for an already-parsed skill, resolving the
// layout fresh from disk each time (SKILL.md first, then prompt.md, then an
// inline skill.yaml body). Called by Catalog.GetFull on every invocation so
// body edits take effect immediately. On any error the skill's existing
// PromptBody is left untouched, letting callers fall back to the cached body.
func loadSkillBody(skill *Skill) error {
	mdPath := filepath.Join(skill.SourceDir, "SKILL.md")
	if _, err := os.Stat(mdPath); err == nil {
		data, err := readBounded(mdPath, "SKILL.md")
		if err != nil {
			return err
		}
		_, body, err := splitFrontmatter(string(data))
		if err != nil {
			return fmt.Errorf("SKILL.md frontmatter 解析失败: %v", err)
		}
		skill.PromptBody = body
		skill.BodyLoaded = true
		return nil
	}

	promptPath := filepath.Join(skill.SourceDir, "prompt.md")
	if _, err := os.Stat(promptPath); err == nil {
		data, err := readBounded(promptPath, "prompt.md")
		if err != nil {
			return err
		}
		skill.PromptBody = string(data)
		skill.BodyLoaded = true
		return nil
	}

	yamlPath := filepath.Join(skill.SourceDir, "skill.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		return errors.New(reasonNoSkillFile)
	}
	data, err := readBounded(yamlPath, "skill.yaml")
	if err != nil {
		return err
	}
	var wrapper skillYAML
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return fmt.Errorf("skill.yaml frontmatter 解析失败: %v", err)
	}
	if wrapper.Body == "" {
		return errors.New(reasonBodyMissing)
	}
	skill.PromptBody = wrapper.Body
	skill.BodyLoaded = true
	return nil
}

// readBounded reads one skill file, refusing files beyond MaxFileBytes.
// Errors are prefixed with the file label so they work directly as rejection
// reasons.
func readBounded(path, label string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s 读取失败: %v", label, err)
	}
	if info.Size() > MaxFileBytes {
		return nil, fmt.Errorf("%s 单文件超过 %d 字节上限", label, MaxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s 读取失败: %v", label, err)
	}
	return data, nil
}

// splitFrontmatter separates a "---" delimited YAML frontmatter block from
// the markdown body. Files without a frontmatter block return the whole
// (trimmed) content as body with a zero-value meta. A block that is never
// closed or cannot be parsed is an error, so the file can be reported as
// invalid instead of silently misloaded.
func splitFrontmatter(content string) (SkillMeta, string, error) {
	var meta SkillMeta
	lines := strings.Split(content, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start >= len(lines) || trimLine(lines[start]) != "---" {
		return meta, strings.TrimSpace(content), nil
	}
	end := start + 1
	for end < len(lines) && trimLine(lines[end]) != "---" {
		end++
	}
	if end >= len(lines) {
		return meta, "", errors.New("frontmatter 未以 --- 闭合")
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[start+1:end], "\n")), &meta); err != nil {
		return meta, "", err
	}
	return meta, strings.TrimSpace(strings.Join(lines[end+1:], "\n")), nil
}

// trimLine strips trailing whitespace and carriage returns so files with CRLF
// line endings parse the same as LF ones.
func trimLine(line string) string {
	return strings.TrimRight(line, " \t\r")
}

// applyMetaDefaults fills in frontmatter fallbacks:
//   - missing name → directory basename, lowercased, spaces as "-"
//   - missing description → first non-blank, non-heading body line
//   - missing Mode → "fork" when the legacy Context says so, else "inline"
//   - fork skills without fork_context → "none"
func applyMetaDefaults(meta *SkillMeta, dir, body string) {
	if meta.Name == "" {
		base := filepath.Base(dir)
		meta.Name = strings.ToLower(strings.ReplaceAll(base, " ", "-"))
	}
	if meta.Description == "" {
		meta.Description = firstNonHeadingLine(body)
	}
	if meta.Mode == "" {
		if meta.Context == "fork" {
			meta.Mode = "fork"
		} else {
			meta.Mode = "inline"
		}
	}
	if meta.IsFork() && meta.ForkContext == "" {
		meta.ForkContext = "none"
	}
}

// firstNonHeadingLine returns the first non-empty line of body that is not a
// markdown heading or a "---" rule; it is the description fallback.
func firstNonHeadingLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "---") {
			continue
		}
		return line
	}
	return ""
}
