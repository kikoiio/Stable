package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustParse(t *testing.T, dir string) *Skill {
	t.Helper()
	skill, note, err := parseFrontmatterOnly(dir)
	if err != nil {
		t.Fatalf("parseFrontmatterOnly(%s): %v", dir, err)
	}
	if note != "" {
		t.Fatalf("parseFrontmatterOnly(%s): unexpected note %q", dir, note)
	}
	return skill
}

func TestParseSkillMDLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review")
	writeFile(t, filepath.Join(dir, "SKILL.md"), `---
name: review
description: Review code before merging
when_to_use: whenever a diff is ready
tags:
  - go
  - review
mode: inline
fork_context: full
---

Body of the skill.
`)
	skill := mustParse(t, dir)
	meta := skill.Meta
	if meta.Name != "review" || meta.Description != "Review code before merging" ||
		meta.WhenToUse != "whenever a diff is ready" || meta.Mode != "inline" ||
		meta.ForkContext != "full" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	if len(meta.Tags) != 2 || meta.Tags[0] != "go" || meta.Tags[1] != "review" {
		t.Fatalf("unexpected tags: %v", meta.Tags)
	}
	if meta.IsFork() {
		t.Fatal("inline skill reported as fork")
	}
	if skill.PromptBody != "" || skill.BodyLoaded {
		t.Fatalf("phase-1 must not read the body: %q %v", skill.PromptBody, skill.BodyLoaded)
	}
	if skill.SourceDir != dir {
		t.Fatalf("SourceDir = %q, want %q", skill.SourceDir, dir)
	}
}

func TestParseSkillYAMLLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deploy")
	writeFile(t, filepath.Join(dir, "skill.yaml"), "name: deploy\ndescription: Deploy notes\nwhen_to_use: on release\ntags: [ops]\n")
	writeFile(t, filepath.Join(dir, "prompt.md"), "Deploy steps here.")
	skill := mustParse(t, dir)
	if skill.Meta.Name != "deploy" || skill.Meta.Description != "Deploy notes" ||
		skill.Meta.WhenToUse != "on release" || len(skill.Meta.Tags) != 1 || skill.Meta.Tags[0] != "ops" {
		t.Fatalf("unexpected meta: %+v", skill.Meta)
	}
	if skill.PromptBody != "" || skill.BodyLoaded {
		t.Fatalf("phase-1 must not read prompt.md: %q %v", skill.PromptBody, skill.BodyLoaded)
	}
	// Phase 2: the body comes from prompt.md.
	if err := loadSkillBody(skill); err != nil {
		t.Fatalf("loadSkillBody: %v", err)
	}
	if skill.PromptBody != "Deploy steps here." || !skill.BodyLoaded {
		t.Fatalf("unexpected body: %q loaded=%v", skill.PromptBody, skill.BodyLoaded)
	}
}

func TestParseSkillYAMLInlineBody(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inline")
	writeFile(t, filepath.Join(dir, "skill.yaml"), "name: inline\nbody: |\n  Line one.\n\n  Line two.\n")
	skill := mustParse(t, dir)
	if skill.Meta.Name != "inline" {
		t.Fatalf("name = %q", skill.Meta.Name)
	}
	// With no prompt.md there is nothing left to read lazily, so the inline
	// body is already loaded at phase 1.
	if skill.PromptBody != "Line one.\n\nLine two.\n" || !skill.BodyLoaded {
		t.Fatalf("unexpected inline body: %q loaded=%v", skill.PromptBody, skill.BodyLoaded)
	}
}

func TestParsePromptMDWinsOverInlineBody(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "both")
	writeFile(t, filepath.Join(dir, "skill.yaml"), "name: both\nbody: ignored inline body\n")
	writeFile(t, filepath.Join(dir, "prompt.md"), "real body")
	skill := mustParse(t, dir)
	if skill.PromptBody != "" || skill.BodyLoaded {
		t.Fatalf("phase-1 must defer to prompt.md: %q %v", skill.PromptBody, skill.BodyLoaded)
	}
	if err := loadSkillBody(skill); err != nil {
		t.Fatalf("loadSkillBody: %v", err)
	}
	if skill.PromptBody != "real body" {
		t.Fatalf("prompt.md should win over the inline body, got %q", skill.PromptBody)
	}
}

func TestParseDefaults(t *testing.T) {
	// No frontmatter at all: whole content is the body; the description
	// fallback skips blank lines, headings and "---" rules.
	dir := filepath.Join(t.TempDir(), "My Skill")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "# Heading\n\n---\n\nReal line.\n")
	skill := mustParse(t, dir)
	if skill.Meta.Name != "my-skill" {
		t.Fatalf("name fallback = %q, want my-skill", skill.Meta.Name)
	}
	if skill.Meta.Description != "Real line." {
		t.Fatalf("description fallback = %q, want Real line.", skill.Meta.Description)
	}
	if skill.Meta.Mode != "inline" {
		t.Fatalf("mode default = %q, want inline", skill.Meta.Mode)
	}
	if got := loadSkillBody(skill); got != nil {
		t.Fatalf("loadSkillBody: %v", got)
	}
	if skill.PromptBody != "# Heading\n\n---\n\nReal line." {
		t.Fatalf("body without frontmatter = %q", skill.PromptBody)
	}

	// Frontmatter present but name and description missing.
	dir2 := filepath.Join(t.TempDir(), "Fallback Skill")
	writeFile(t, filepath.Join(dir2, "SKILL.md"), "---\nwhen_to_use: anytime\n---\n\nSecond-case line.\n")
	skill2 := mustParse(t, dir2)
	if skill2.Meta.Name != "fallback-skill" {
		t.Fatalf("name fallback = %q, want fallback-skill", skill2.Meta.Name)
	}
	if skill2.Meta.Description != "Second-case line." {
		t.Fatalf("description fallback = %q", skill2.Meta.Description)
	}
}

func TestParseForkFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "forked")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: forked\nmode: fork\n---\nBody.\n")
	skill := mustParse(t, dir)
	if !skill.Meta.IsFork() {
		t.Fatal("mode: fork not detected")
	}
	if skill.Meta.ForkContext != "none" {
		t.Fatalf("fork_context default = %q, want none", skill.Meta.ForkContext)
	}

	// Legacy path: context: fork without a mode.
	dir2 := filepath.Join(t.TempDir(), "legacy")
	writeFile(t, filepath.Join(dir2, "skill.yaml"), "name: legacy\ncontext: fork\nbody: legacy body\n")
	skill2 := mustParse(t, dir2)
	if !skill2.Meta.IsFork() {
		t.Fatal("context: fork not detected")
	}
	if skill2.Meta.Mode != "fork" {
		t.Fatalf("legacy context should normalize Mode to fork, got %q", skill2.Meta.Mode)
	}
	if skill2.Meta.ForkContext != "none" {
		t.Fatalf("fork_context default = %q, want none", skill2.Meta.ForkContext)
	}

	// An explicit fork_context is preserved.
	dir3 := filepath.Join(t.TempDir(), "recent")
	writeFile(t, filepath.Join(dir3, "SKILL.md"), "---\nmode: fork\nfork_context: recent\n---\nBody.\n")
	skill3 := mustParse(t, dir3)
	if skill3.Meta.ForkContext != "recent" {
		t.Fatalf("fork_context = %q, want recent", skill3.Meta.ForkContext)
	}
}

func TestParseOversizeRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "big")
	writeFile(t, filepath.Join(dir, "SKILL.md"), strings.Repeat("x", MaxFileBytes+1))
	skill, note, err := parseFrontmatterOnly(dir)
	if err == nil {
		t.Fatal("oversize SKILL.md accepted")
	}
	if skill != nil {
		t.Fatal("oversize skill must be nil")
	}
	if note != "" {
		t.Fatalf("unexpected note %q", note)
	}
	if !strings.Contains(err.Error(), "单文件超过") || !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("error lacks bounds reason: %v", err)
	}

	// Same bound for prompt.md at phase 1.
	dir2 := filepath.Join(t.TempDir(), "bigprompt")
	writeFile(t, filepath.Join(dir2, "skill.yaml"), "name: bigprompt\n")
	writeFile(t, filepath.Join(dir2, "prompt.md"), strings.Repeat("x", MaxFileBytes+1))
	if _, _, err := parseFrontmatterOnly(dir2); err == nil || !strings.Contains(err.Error(), "单文件超过") {
		t.Fatalf("oversize prompt.md accepted: %v", err)
	}
}

func TestParseBadFrontmatterRejected(t *testing.T) {
	// Unclosed frontmatter block.
	dir := filepath.Join(t.TempDir(), "unclosed")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: unclosed\n")
	if _, _, err := parseFrontmatterOnly(dir); err == nil {
		t.Fatal("unclosed frontmatter accepted")
	}

	// YAML type error in the frontmatter.
	dir2 := filepath.Join(t.TempDir(), "typerror")
	writeFile(t, filepath.Join(dir2, "SKILL.md"), "---\ntags: 5\n---\nBody.\n")
	_, _, err := parseFrontmatterOnly(dir2)
	if err == nil || !strings.Contains(err.Error(), "frontmatter 解析失败") {
		t.Fatalf("bad SKILL.md frontmatter not reported: %v", err)
	}

	// Invalid YAML in skill.yaml.
	dir3 := filepath.Join(t.TempDir(), "badyaml")
	writeFile(t, filepath.Join(dir3, "skill.yaml"), "name: [unclosed\n")
	_, _, err = parseFrontmatterOnly(dir3)
	if err == nil || !strings.Contains(err.Error(), "skill.yaml") {
		t.Fatalf("bad skill.yaml not reported: %v", err)
	}
}

func TestParseBodyMissingRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lonely")
	writeFile(t, filepath.Join(dir, "skill.yaml"), "name: lonely\n")
	skill, _, err := parseFrontmatterOnly(dir)
	if err == nil {
		t.Fatal("skill.yaml without prompt.md and body accepted")
	}
	if skill != nil {
		t.Fatal("body-missing skill must be nil")
	}
	if !strings.Contains(err.Error(), "正文缺失") {
		t.Fatalf("error lacks body-missing reason: %v", err)
	}
}

func TestParseCoexistencePrefersSkillMD(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "coexist")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: from-md\n---\nmd body\n")
	writeFile(t, filepath.Join(dir, "skill.yaml"), "name: from-yaml\nbody: yaml body\n")
	skill, note, err := parseFrontmatterOnly(dir)
	if err != nil {
		t.Fatalf("parseFrontmatterOnly: %v", err)
	}
	if skill.Meta.Name != "from-md" {
		t.Fatalf("SKILL.md should win, got name %q", skill.Meta.Name)
	}
	if !strings.Contains(note, "并存") || !strings.Contains(note, "SKILL.md") {
		t.Fatalf("coexistence note missing: %q", note)
	}
	// The body follows the same precedence: SKILL.md, not the inline body.
	if err := loadSkillBody(skill); err != nil {
		t.Fatalf("loadSkillBody: %v", err)
	}
	if skill.PromptBody != "md body" {
		t.Fatalf("body = %q, want md body", skill.PromptBody)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	meta, body, err := splitFrontmatter("just a body")
	if err != nil || meta.Name != "" || body != "just a body" {
		t.Fatalf("no-frontmatter split: %+v %q %v", meta, body, err)
	}

	meta, body, err = splitFrontmatter("---\r\nname: crlf\r\n---\r\nbody here\r\n")
	if err != nil {
		t.Fatalf("CRLF split: %v", err)
	}
	if meta.Name != "crlf" || body != "body here" {
		t.Fatalf("CRLF split: %+v %q", meta, body)
	}

	if _, _, err := splitFrontmatter("---\nname: x\n"); err == nil {
		t.Fatal("unclosed frontmatter accepted")
	}
}

func TestLoadSkillBodyReadFailureKeepsOldBody(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	writeFile(t, filepath.Join(dir, "skill.yaml"), "name: gone\n")
	writeFile(t, filepath.Join(dir, "prompt.md"), "old body")
	skill := mustParse(t, dir)
	if err := loadSkillBody(skill); err != nil {
		t.Fatalf("loadSkillBody: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "prompt.md")); err != nil {
		t.Fatal(err)
	}
	if err := loadSkillBody(skill); err == nil {
		t.Fatal("read failure not reported")
	}
	if skill.PromptBody != "old body" || !skill.BodyLoaded {
		t.Fatalf("old body not preserved: %q", skill.PromptBody)
	}
}
