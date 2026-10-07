package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverInstructionsLayeringAndPriority(t *testing.T) {
	project := t.TempDir()
	user := t.TempDir()
	work := filepath.Join(project, "nested", "work")
	for _, dir := range []string{user, work, filepath.Join(project, ".stable"), filepath.Join(project, "nested"), filepath.Join(project, "nested", ".stable")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(user, "STABLE.md"), "user stable")
	write(filepath.Join(user, "AGENTS.md"), "user agents")
	write(filepath.Join(project, "STABLE.md"), "project stable")
	write(filepath.Join(project, "AGENTS.md"), "project agents")
	write(filepath.Join(project, ".stable", "STABLE.md"), "project metadata")
	write(filepath.Join(project, "nested", "STABLE.md"), "nested stable")
	write(filepath.Join(project, "nested", "AGENTS.md"), "nested agents")
	write(filepath.Join(project, "nested", ".stable", "STABLE.md"), "nested metadata")
	write(filepath.Join(work, "STABLE.local.md"), "local override")

	sources, issues := DiscoverInstructions(project, work, user)
	if len(issues) != 0 {
		t.Fatalf("DiscoverInstructions issues = %v", issues)
	}
	want := []string{"user stable", "user agents", "project stable", "project agents", "project metadata", "nested stable", "nested agents", "nested metadata", "local override"}
	if len(sources) != len(want) {
		t.Fatalf("got %d sources, want %d: %+v", len(sources), len(want), sources)
	}
	for i, source := range sources {
		if source.Priority != i || !strings.Contains(source.Content, want[i]) {
			t.Errorf("source %d = %+v, want priority %d with %q", i, source, i, want[i])
		}
	}
}

func TestDiscoverInstructionsExpandsAllowedIncludeForms(t *testing.T) {
	project := t.TempDir()
	user := t.TempDir()
	for _, dir := range []string{project, user} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	absolute := filepath.Join(project, "absolute.md")
	write(filepath.Join(project, "STABLE.md"), "root\n@./relative.md\n@./nested/child.md\n@~/user.md\n@"+filepath.ToSlash(absolute)+"\n")
	write(filepath.Join(project, "relative.md"), "relative include")
	write(filepath.Join(project, "absolute.md"), "absolute include")
	if err := os.Mkdir(filepath.Join(project, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(project, "parent.md"), "parent include")
	write(filepath.Join(project, "nested", "child.md"), "child\n@../parent.md\n")
	write(filepath.Join(user, "user.md"), "user include")
	sources, issues := DiscoverInstructions(project, project, user)
	if len(issues) != 0 {
		t.Fatalf("DiscoverInstructions issues = %v", issues)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want one root source", len(sources))
	}
	for _, want := range []string{"root", "relative include", "child", "parent include", "user include", "absolute include"} {
		if !strings.Contains(sources[0].Content, want) {
			t.Errorf("expanded content does not contain %q: %q", want, sources[0].Content)
		}
	}
}

func TestDiscoverInstructionsBoundsIncludesAndPreservesFailures(t *testing.T) {
	project := t.TempDir()
	user := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(project, "STABLE.md"), ""+
		"@./a.md\n@./a.md\n```text\n@/outside/fenced.md\n```\n@../../outside.md\n")
	write(filepath.Join(project, "a.md"), "A\n@./STABLE.md\n")
	sources, issues := DiscoverInstructions(project, project, user)
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want one", len(sources))
	}
	for _, preserved := range []string{"@./a.md", "@../../outside.md", "@/outside/fenced.md"} {
		if !strings.Contains(sources[0].Content, preserved) {
			t.Errorf("failed/fenced include line %q was not preserved: %q", preserved, sources[0].Content)
		}
	}
	if strings.Contains(sources[0].Content, "fenced.md") && !strings.Contains(sources[0].Content, "@/outside/fenced.md") {
		t.Fatal("code-fenced include was expanded")
	}
	if len(issues) < 3 {
		t.Fatalf("issues = %v, want duplicate, cycle, and boundary issues", issues)
	}

	chainRoot := t.TempDir()
	write(filepath.Join(chainRoot, "STABLE.md"), "@./level0.md\n")
	for i := 0; i < 7; i++ {
		body := "level" + string(rune('0'+i)) + "\n"
		if i < 6 {
			body += "@./level" + string(rune('0'+i+1)) + ".md\n"
		}
		write(filepath.Join(chainRoot, "level"+string(rune('0'+i))+".md"), body)
	}
	chainSources, chainIssues := DiscoverInstructions(chainRoot, chainRoot, "")
	if len(chainSources) != 1 || !strings.Contains(chainSources[0].Content, "@./level5.md") || len(chainIssues) == 0 {
		t.Fatalf("deep include result = %+v, issues=%v", chainSources, chainIssues)
	}
}

func TestDiscoverInstructionsRejectsSymlinkEscape(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "STABLE.md"), []byte("@./linked/secret.md\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sources, issues := DiscoverInstructions(project, project, "")
	if len(sources) != 1 || !strings.Contains(sources[0].Content, "@./linked/secret.md") || len(issues) == 0 {
		t.Fatalf("symlink include result = %+v, issues=%v", sources, issues)
	}
	if strings.Contains(sources[0].Content, "secret\n") {
		t.Fatal("symlink target content was loaded")
	}
}

func TestDiscoverInstructionsReportsOversizedFile(t *testing.T) {
	project := t.TempDir()
	content := strings.Repeat("x", MaxInstructionFileBytes+1)
	if err := os.WriteFile(filepath.Join(project, "STABLE.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	sources, issues := DiscoverInstructions(project, project, "")
	if len(sources) != 0 || len(issues) == 0 || !strings.Contains(issues[0].Reason, "exceeds 1 MiB") {
		t.Fatalf("oversized instruction result = sources:%d issues:%v", len(sources), issues)
	}
}
