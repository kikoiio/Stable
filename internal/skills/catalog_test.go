package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestLoadCatalogMergesTiers(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "user")
	projectDir := filepath.Join(base, "project")
	writeFile(t, filepath.Join(userDir, "alpha", "SKILL.md"), "---\nname: alpha\ndescription: user alpha\n---\nalpha body\n")
	writeFile(t, filepath.Join(projectDir, "beta", "skill.yaml"), "name: beta\ndescription: project beta\n")
	writeFile(t, filepath.Join(projectDir, "beta", "prompt.md"), "beta body")

	cat := LoadCatalog(userDir, projectDir)
	got := cat.List()
	if len(got) != 2 || got[0].Meta.Name != "alpha" || got[1].Meta.Name != "beta" {
		t.Fatalf("List = %+v, want [alpha beta]", names(got))
	}
	if cat.Source("alpha") != "user" || cat.Source("beta") != "project" {
		t.Fatalf("sources = %q %q, want user project", cat.Source("alpha"), cat.Source("beta"))
	}
	// Phase 1: bodies are not read at load time.
	for _, s := range got {
		if s.PromptBody != "" || s.BodyLoaded {
			t.Fatalf("phase-1 read the body of %s", s.Meta.Name)
		}
	}
	beta, ok := cat.Get("beta")
	if !ok || beta.Meta.Description != "project beta" {
		t.Fatalf("Get(beta) = %+v", beta)
	}
	full, err := cat.GetFull("beta")
	if err != nil {
		t.Fatalf("GetFull(beta): %v", err)
	}
	if full.PromptBody != "beta body" {
		t.Fatalf("GetFull(beta) body = %q", full.PromptBody)
	}
	if _, ok := cat.Get("missing"); ok {
		t.Fatal("Get returned an unknown skill")
	}
}

func TestProjectOverridesUser(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "user")
	projectDir := filepath.Join(base, "project")
	writeFile(t, filepath.Join(userDir, "dupe", "SKILL.md"), "---\nname: dupe\ndescription: user version\n---\nuser body\n")
	writeFile(t, filepath.Join(projectDir, "dupe", "SKILL.md"), "---\nname: dupe\ndescription: project version\n---\nproject body\n")

	cat := LoadCatalog(userDir, projectDir)
	got := cat.List()
	if len(got) != 1 {
		t.Fatalf("List has %d skills, want 1", len(got))
	}
	if got[0].Meta.Description != "project version" {
		t.Fatalf("project did not override user: %+v", got[0].Meta)
	}
	if cat.Source("dupe") != "project" {
		t.Fatalf("source = %q, want project", cat.Source("dupe"))
	}
	full, err := cat.GetFull("dupe")
	if err != nil || full.PromptBody != "project body" {
		t.Fatalf("GetFull(dupe) = %q, %v", full.PromptBody, err)
	}
}

func TestCoexistenceReportedAndSkillMDWins(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "project")
	writeFile(t, filepath.Join(projectDir, "coexist", "SKILL.md"), "---\nname: coexist\n---\nmd body\n")
	writeFile(t, filepath.Join(projectDir, "coexist", "skill.yaml"), "name: coexist\nbody: yaml body\n")

	cat := LoadCatalog("", projectDir)
	got := cat.List()
	if len(got) != 1 || got[0].Meta.Name != "coexist" {
		t.Fatalf("List = %v", names(got))
	}
	full, err := cat.GetFull("coexist")
	if err != nil || full.PromptBody != "md body" {
		t.Fatalf("GetFull(coexist) = %q, %v", full.PromptBody, err)
	}
	rejections := cat.Rejections()
	if len(rejections) != 1 {
		t.Fatalf("rejections = %v, want exactly the coexistence note", rejections)
	}
	want := filepath.Join(projectDir, "coexist") + ": SKILL.md 与 skill.yaml 并存,已采用 SKILL.md"
	if rejections[0] != want {
		t.Fatalf("coexistence line = %q, want %q", rejections[0], want)
	}
}

func TestPhase1DoesNotReadBody(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "user")
	writeFile(t, filepath.Join(userDir, "md", "SKILL.md"), "---\nname: md\n---\nmd body\n")
	writeFile(t, filepath.Join(userDir, "yaml", "skill.yaml"), "name: yaml\n")
	writeFile(t, filepath.Join(userDir, "yaml", "prompt.md"), "prompt body\n")

	cat := LoadCatalog(userDir, "")
	if len(cat.List()) != 2 {
		t.Fatalf("List = %v", names(cat.List()))
	}
	for _, name := range []string{"md", "yaml"} {
		s, ok := cat.Get(name)
		if !ok {
			t.Fatalf("skill %s missing", name)
		}
		if s.PromptBody != "" || s.BodyLoaded {
			t.Fatalf("phase-1 read the body of %s: %q loaded=%v", name, s.PromptBody, s.BodyLoaded)
		}
	}
}

func TestGetFullHotUpdate(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "user")
	md := filepath.Join(userDir, "hot", "SKILL.md")
	writeFile(t, md, "---\nname: hot\n---\nbody v1\n")

	cat := LoadCatalog(userDir, "")
	full, err := cat.GetFull("hot")
	if err != nil || full.PromptBody != "body v1" {
		t.Fatalf("first GetFull: %q, %v", full.PromptBody, err)
	}
	writeFile(t, md, "---\nname: hot\n---\nbody v2\n")
	full, err = cat.GetFull("hot")
	if err != nil || full.PromptBody != "body v2" {
		t.Fatalf("GetFull after edit: %q, %v", full.PromptBody, err)
	}
}

func TestGetFullInlineBodyHotUpdate(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "project")
	yamlPath := filepath.Join(projectDir, "inline", "skill.yaml")
	writeFile(t, yamlPath, "name: inline\nbody: v1\n")

	cat := LoadCatalog("", projectDir)
	full, err := cat.GetFull("inline")
	if err != nil || full.PromptBody != "v1" {
		t.Fatalf("first GetFull: %q, %v", full.PromptBody, err)
	}
	writeFile(t, yamlPath, "name: inline\nbody: v2\n")
	full, err = cat.GetFull("inline")
	if err != nil || full.PromptBody != "v2" {
		t.Fatalf("GetFull after edit: %q, %v", full.PromptBody, err)
	}
}

func TestGetFullReadFailureKeepsOldBody(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "user")
	prompt := filepath.Join(userDir, "vanish", "prompt.md")
	writeFile(t, filepath.Join(userDir, "vanish", "skill.yaml"), "name: vanish\n")
	writeFile(t, prompt, "old body")

	cat := LoadCatalog(userDir, "")
	if full, err := cat.GetFull("vanish"); err != nil || full.PromptBody != "old body" {
		t.Fatalf("first GetFull: %q, %v", full.PromptBody, err)
	}
	if err := os.Remove(prompt); err != nil {
		t.Fatal(err)
	}
	full, err := cat.GetFull("vanish")
	if err == nil {
		t.Fatal("read failure not reported")
	}
	if !strings.Contains(err.Error(), "vanish") {
		t.Fatalf("error lacks skill name: %v", err)
	}
	if full == nil || full.PromptBody != "old body" {
		t.Fatalf("cached body not returned on failure: %+v", full)
	}
	if cached, ok := cat.Get("vanish"); !ok || cached.PromptBody != "old body" {
		t.Fatalf("catalog did not preserve the cached body: %+v", cached)
	}

	// Layout 1: replacing SKILL.md with a directory makes reads fail too.
	md := filepath.Join(userDir, "blocked", "SKILL.md")
	writeFile(t, md, "---\nname: blocked\n---\nold body\n")
	cat.Reload()
	if full, err := cat.GetFull("blocked"); err != nil || full.PromptBody != "old body" {
		t.Fatalf("first GetFull(blocked): %q, %v", full.PromptBody, err)
	}
	if err := os.Remove(md); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(md, 0o755); err != nil {
		t.Fatal(err)
	}
	full, err = cat.GetFull("blocked")
	if err == nil {
		t.Fatal("read failure not reported")
	}
	if full == nil || full.PromptBody != "old body" {
		t.Fatalf("cached body not returned on failure: %+v", full)
	}

	// With no cached body the failure returns a nil skill.
	if err := os.RemoveAll(filepath.Join(userDir, "neverread")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(userDir, "neverread", "skill.yaml"), "name: neverread\n")
	writeFile(t, filepath.Join(userDir, "neverread", "prompt.md"), "unreachable body")
	cat.Reload()
	if err := os.Remove(filepath.Join(userDir, "neverread", "prompt.md")); err != nil {
		t.Fatal(err)
	}
	full, err = cat.GetFull("neverread")
	if err == nil || full != nil {
		t.Fatalf("GetFull without cached body = %+v, %v", full, err)
	}
}

func TestGetFullUnknownSkill(t *testing.T) {
	cat := LoadCatalog(filepath.Join(t.TempDir(), "none"), "")
	if _, err := cat.GetFull("nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("GetFull(unknown) error = %v", err)
	}
	if cat.Source("nope") != "" {
		t.Fatal("unknown skill has a source")
	}
}

func TestNeedsReloadDetectsChanges(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "user")
	projectDir := filepath.Join(base, "project")
	writeFile(t, filepath.Join(userDir, "alpha", "SKILL.md"), "---\nname: alpha\n---\nbody\n")
	// The project tier starts as an existing but empty root, so its removal
	// below is a disappearance of a tracked directory.
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cat := LoadCatalog(userDir, projectDir)
	if cat.NeedsReload() {
		t.Fatal("fresh catalog needs reload")
	}

	// New skill directory in a tracked root.
	writeFile(t, filepath.Join(userDir, "bravo", "SKILL.md"), "---\nname: bravo\n---\nbody\n")
	if !cat.NeedsReload() {
		t.Fatal("added skill directory not detected")
	}
	cat.Reload()
	if cat.NeedsReload() {
		t.Fatal("reload did not retake the snapshot")
	}
	if len(cat.List()) != 2 {
		t.Fatalf("List after reload = %v", names(cat.List()))
	}

	// mtime change inside a skill directory (frontmatter edit).
	skillDir := filepath.Join(userDir, "alpha")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(skillDir, future, future); err != nil {
		t.Fatal(err)
	}
	if !cat.NeedsReload() {
		t.Fatal("skill directory mtime change not detected")
	}
	cat.Reload()
	if cat.NeedsReload() {
		t.Fatal("reload did not retake the snapshot")
	}

	// Removing a skill directory.
	if err := os.RemoveAll(filepath.Join(userDir, "bravo")); err != nil {
		t.Fatal(err)
	}
	if !cat.NeedsReload() {
		t.Fatal("removed skill directory not detected")
	}
	cat.Reload()
	if cat.NeedsReload() {
		t.Fatal("reload did not retake the snapshot")
	}
	if len(cat.List()) != 1 {
		t.Fatalf("List after removal = %v", names(cat.List()))
	}

	// Removing a whole tier root.
	if err := os.RemoveAll(projectDir); err != nil {
		t.Fatal(err)
	}
	if !cat.NeedsReload() {
		t.Fatal("removed tier root not detected")
	}
	cat.Reload()
	if cat.NeedsReload() {
		t.Fatal("reload did not retake the snapshot")
	}

	// A tier root that appears after the load, with a skill inside.
	writeFile(t, filepath.Join(projectDir, "late", "SKILL.md"), "---\nname: late\n---\nbody\n")
	if !cat.NeedsReload() {
		t.Fatal("newly created tier root not detected")
	}
	cat.Reload()
	if _, ok := cat.Get("late"); !ok {
		t.Fatal("skill in the new tier root missing after reload")
	}
}

func TestRejectionsCompleteSortedAndSkillSkipped(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "skills")
	target := filepath.Join(base, "outside-target")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}

	// Symlinked skill directory, rejected without being followed.
	if err := os.Symlink(target, filepath.Join(root, "z_link")); err != nil {
		t.Fatal(err)
	}
	// Oversize SKILL.md.
	writeFile(t, filepath.Join(root, "a_big", "SKILL.md"), strings.Repeat("x", MaxFileBytes+1))
	// Broken frontmatter.
	writeFile(t, filepath.Join(root, "m_bad", "SKILL.md"), "---\ntags: 5\n---\nBody.\n")
	// Neither layout file present.
	if err := os.MkdirAll(filepath.Join(root, "q_empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Layout 2 without a body.
	writeFile(t, filepath.Join(root, "b_nobody", "skill.yaml"), "name: nobody\n")

	cat := LoadCatalog(root, "")
	if got := cat.List(); len(got) != 0 {
		t.Fatalf("no fixture should load, got %v", names(got))
	}
	rejections := cat.Rejections()
	expect := 5
	if os.Getuid() != 0 {
		// An unreadable SKILL.md, only reliable as non-root.
		writeFile(t, filepath.Join(root, "u_locked", "SKILL.md"), "secret body")
		if err := os.Chmod(filepath.Join(root, "u_locked", "SKILL.md"), 0o000); err != nil {
			t.Fatal(err)
		}
		cat.Reload()
		expect = 6
		rejections = cat.Rejections()
	}
	if len(rejections) != expect {
		t.Fatalf("rejections = %d lines, want %d:\n%s", len(rejections), expect, strings.Join(rejections, "\n"))
	}
	if !sort.StringsAreSorted(rejections) {
		t.Fatalf("rejections not sorted: %v", rejections)
	}
	for path, reason := range map[string]string{
		filepath.Join(root, "z_link"):   "技能目录为符号链接",
		filepath.Join(root, "a_big"):    "单文件超过",
		filepath.Join(root, "m_bad"):    "frontmatter 解析失败",
		filepath.Join(root, "q_empty"):  "缺少 SKILL.md 或 skill.yaml",
		filepath.Join(root, "b_nobody"): "正文缺失",
	} {
		if !containsLine(rejections, path, reason) {
			t.Fatalf("rejections lack %s: %s\n%v", path, reason, rejections)
		}
	}
	if os.Getuid() != 0 && !containsLine(rejections, filepath.Join(root, "u_locked"), "读取失败") {
		t.Fatalf("rejections lack the unreadable file: %v", rejections)
	}
	// Stable across calls: deduplicated copies of the same report.
	again := cat.Rejections()
	if len(again) != len(rejections) || strings.Join(again, "|") != strings.Join(rejections, "|") {
		t.Fatalf("Rejections not stable:\n%v\n%v", rejections, again)
	}
}

func TestMaxSkillsCap(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "user")
	for i := 0; i < MaxSkills+1; i++ {
		writeFile(t, filepath.Join(userDir, skillDirName(i), "SKILL.md"), "body "+skillDirName(i)+"\n")
	}
	cat := LoadCatalog(userDir, "")
	if got := cat.List(); len(got) != MaxSkills {
		t.Fatalf("List has %d skills, want %d", len(got), MaxSkills)
	}
	rejections := cat.Rejections()
	if len(rejections) != 1 || !strings.Contains(rejections[0], "超过技能数量上限 200") {
		t.Fatalf("cap not reported: %v", rejections)
	}
	if !strings.Contains(rejections[0], filepath.Join(userDir, "skill200")) {
		t.Fatalf("cap line names the wrong skill: %v", rejections)
	}

	// Project skills claim slots first: a project skill overriding a user
	// name keeps the total at 200 without a cap rejection.
	projectDir := filepath.Join(base, "project")
	writeFile(t, filepath.Join(projectDir, "skill000", "SKILL.md"), "---\nname: skill000\ndescription: project\n---\nbody\n")
	// A fresh user tier with exactly 200 skills; the earlier userDir holds
	// 201 distinct names, which are over the cap on their own.
	userDir200 := filepath.Join(base, "user200")
	for i := 0; i < MaxSkills; i++ {
		writeFile(t, filepath.Join(userDir200, skillDirName(i), "SKILL.md"), "body "+skillDirName(i)+"\n")
	}
	cat = LoadCatalog(userDir200, projectDir)
	if got := cat.List(); len(got) != MaxSkills {
		t.Fatalf("List has %d skills with project override, want %d", len(got), MaxSkills)
	}
	if cat.Source("skill000") != "project" {
		t.Fatalf("source = %q, want project", cat.Source("skill000"))
	}
	if rejections := cat.Rejections(); len(rejections) != 0 {
		t.Fatalf("unexpected rejections: %v", rejections)
	}
}

func TestLoadCatalogMissingAndInvalidRoots(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing")
	cat := LoadCatalog(missing, "")
	if got := cat.List(); len(got) != 0 {
		t.Fatalf("List = %v, want empty", names(got))
	}
	if rejections := cat.Rejections(); len(rejections) != 0 {
		t.Fatalf("missing roots must not be reported: %v", rejections)
	}
	if cat.NeedsReload() {
		t.Fatal("missing roots must not trigger reload")
	}

	// A tier root that is a file is reported.
	notDir := filepath.Join(base, "file-root")
	writeFile(t, notDir, "not a directory")
	cat = LoadCatalog(notDir, "")
	if len(cat.Rejections()) != 1 || !strings.Contains(cat.Rejections()[0], reasonNotDir) {
		t.Fatalf("file root not reported: %v", cat.Rejections())
	}
}

func skillDirName(i int) string {
	return fmt.Sprintf("skill%03d", i)
}

func names(skills []Skill) []string {
	out := make([]string, 0, len(skills))
	for _, s := range skills {
		out = append(out, s.Meta.Name)
	}
	return out
}

func containsLine(lines []string, path, reason string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, path+": ") && strings.Contains(line, reason) {
			return true
		}
	}
	return false
}
