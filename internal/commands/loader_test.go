package commands

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func findCommand(cmds []*Command, name string) *Command {
	for _, cmd := range cmds {
		if cmd.Name == name {
			return cmd
		}
	}
	return nil
}

func reportsPath(rejected []string, path string) bool {
	for _, line := range rejected {
		if strings.HasPrefix(line, path+": ") {
			return true
		}
	}
	return false
}

func TestLoaderNamesAndFrontmatter(t *testing.T) {
	user := t.TempDir()
	writeFile(t, filepath.Join(user, "git", "log.md"), "---\ndescription: Show git log\n---\nBody\n")
	writeFile(t, filepath.Join(user, "My Cmd.md"), "body of my cmd\n")
	writeFile(t, filepath.Join(user, "deploy.md"),
		"---\ndescription: \"Deploy it\"\nargument-hint: '<env>'\naliases: [dep, d]\n---\nDeploy $ARGUMENTS\n")
	writeFile(t, filepath.Join(user, "ship.md"),
		"---\ndescription: Ship\naliases:\n  - release\n  - go\n---\nShip\n")
	writeFile(t, filepath.Join(user, "plain.md"), "# Heading\n\nFirst plain line.\n")
	writeFile(t, filepath.Join(user, "nested", "deep", "leaf.md"), "deep body\n")

	cmds, rejected, err := NewLoader("", user).Commands()
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(cmds) != 6 {
		t.Fatalf("loaded %d commands: %v", len(cmds), cmds)
	}

	cmd := findCommand(cmds, "git:log")
	if cmd == nil || cmd.Description != "Show git log" || cmd.Body != "Body" {
		t.Fatalf("namespaced command: %+v", cmd)
	}
	if cmd.Kind != KindPrompt || cmd.Local != nil {
		t.Fatalf("loaded command must be a prompt command: %+v", cmd)
	}

	if cmd = findCommand(cmds, "my-cmd"); cmd == nil || cmd.Body != "body of my cmd" {
		t.Fatalf("space and case normalization: %+v", cmd)
	}

	cmd = findCommand(cmds, "deploy")
	if cmd == nil || cmd.Description != "Deploy it" || cmd.ArgPrompt != "<env>" || cmd.Body != "Deploy $ARGUMENTS" {
		t.Fatalf("inline frontmatter: %+v", cmd)
	}
	if len(cmd.Aliases) != 2 || cmd.Aliases[0] != "dep" || cmd.Aliases[1] != "d" {
		t.Fatalf("inline aliases: %v", cmd.Aliases)
	}

	cmd = findCommand(cmds, "ship")
	if cmd == nil || len(cmd.Aliases) != 2 || cmd.Aliases[0] != "release" || cmd.Aliases[1] != "go" {
		t.Fatalf("multi-line aliases: %+v", cmd)
	}

	if cmd = findCommand(cmds, "plain"); cmd == nil || cmd.Description != "First plain line." {
		t.Fatalf("description fallback: %+v", cmd)
	}
	if findCommand(cmds, "nested:deep:leaf") == nil {
		t.Fatalf("depth-3 command missing: %v", cmds)
	}
}

func TestLoaderProjectOverridesUser(t *testing.T) {
	project := t.TempDir()
	user := t.TempDir()
	writeFile(t, filepath.Join(user, "deploy.md"), "user deploy body\n")
	writeFile(t, filepath.Join(project, "deploy.md"), "project deploy body\n")

	cmds, rejected, err := NewLoader(project, user).Commands()
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		t.Fatalf("loaded %d commands: %v", len(cmds), cmds)
	}
	if cmds[0].Name != "deploy" || cmds[0].Body != "project deploy body" {
		t.Fatalf("project command must win: %+v", cmds[0])
	}
	if len(rejected) != 0 {
		t.Fatalf("override must be silent, got: %v", rejected)
	}
}

func TestLoaderRejectsBoundsAndBadFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "b", "c", "d.md"), "too deep\n")
	writeFile(t, filepath.Join(dir, "big.md"), strings.Repeat("x", MaxFileBytes+1))
	writeFile(t, filepath.Join(dir, "unclosed.md"), "---\ndescription: x\nbody\n")
	writeFile(t, filepath.Join(dir, "badline.md"), "---\ndescription: x\nnot a key\n---\nbody\n")
	writeFile(t, filepath.Join(dir, "good.md"), "fine\n")

	cmds, rejected, err := NewLoader(dir, "").Commands()
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Name != "good" {
		t.Fatalf("only good.md should load: %v", cmds)
	}
	for _, name := range []string{
		filepath.Join(dir, "a", "b", "c", "d.md"),
		filepath.Join(dir, "big.md"),
		filepath.Join(dir, "unclosed.md"),
		filepath.Join(dir, "badline.md"),
	} {
		if !reportsPath(rejected, name) {
			t.Fatalf("report misses %q: %v", name, rejected)
		}
	}
	if len(rejected) != 4 {
		t.Fatalf("unexpected extra rejections: %v", rejected)
	}
}

func TestLoaderAliasConflictReported(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x.md"), "---\naliases: [y]\n---\nx body\n")
	writeFile(t, filepath.Join(dir, "y.md"), "y body\n")

	cmds, rejected, err := NewLoader(dir, "").Commands()
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Name != "x" {
		t.Fatalf("only x.md should load: %v", cmds)
	}
	if !reportsPath(rejected, filepath.Join(dir, "y.md")) {
		t.Fatalf("alias conflict not reported: %v", rejected)
	}
}

func TestLoaderHotReload(t *testing.T) {
	dir := t.TempDir()
	loader := NewLoader(dir, "")
	path := filepath.Join(dir, "a.md")
	writeFile(t, path, "version one\n")

	cmds, _, err := loader.Commands()
	if err != nil || len(cmds) != 1 || cmds[0].Name != "a" {
		t.Fatalf("first load: %v %v", cmds, err)
	}
	first := cmds

	// An unchanged tree returns the cached result.
	again, _, err := loader.Commands()
	if err != nil || len(again) != 1 || &again[0] != &first[0] {
		t.Fatalf("unchanged tree was rescanned: %v %v", again, err)
	}

	// Modifying a file in place must be picked up.
	writeFile(t, path, "version two $ARGUMENTS\n")
	cmds, _, err = loader.Commands()
	if err != nil || len(cmds) != 1 {
		t.Fatalf("load after modify: %v %v", cmds, err)
	}
	if &cmds[0] == &first[0] {
		t.Fatal("modified tree returned the cached command")
	}
	if expanded := ExpandPrompt(cmds[0].Body, "beta"); expanded != "version two beta" {
		t.Fatalf("updated body not used: %q", expanded)
	}

	// Adding and deleting files must be picked up too.
	writeFile(t, filepath.Join(dir, "b.md"), "other\n")
	if cmds, _, err = loader.Commands(); err != nil || len(cmds) != 2 {
		t.Fatalf("load after add: %v %v", cmds, err)
	}
	if err = os.Remove(filepath.Join(dir, "a.md")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	if cmds, _, err = loader.Commands(); err != nil || len(cmds) != 0 {
		t.Fatalf("load after delete: %v %v", cmds, err)
	}
}

func TestLoaderCommandLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= MaxCommands; i++ {
		name := "cmd" + strings.Repeat("0", 3-len(strconv.Itoa(i))) + strconv.Itoa(i) + ".md"
		writeFile(t, filepath.Join(dir, name), "body\n")
	}

	cmds, rejected, err := NewLoader(dir, "").Commands()
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != MaxCommands {
		t.Fatalf("loaded %d commands, want %d", len(cmds), MaxCommands)
	}
	found := false
	for _, line := range rejected {
		if strings.Contains(line, "limit of 200 commands reached") {
			found = true
		}
	}
	if !found {
		t.Fatalf("limit not reported: %v", rejected)
	}
}

func TestLoaderMissingAndEmptyDirs(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	cmds, rejected, err := NewLoader(missing, "").Commands()
	if err != nil || len(cmds) != 0 || len(rejected) != 0 {
		t.Fatalf("missing dirs: %v %v %v", cmds, rejected, err)
	}
	if cmds, rejected, err = NewLoader("", "").Commands(); err != nil || len(cmds) != 0 || len(rejected) != 0 {
		t.Fatalf("empty dirs: %v %v %v", cmds, rejected, err)
	}
}

func TestExpandPrompt(t *testing.T) {
	if got := ExpandPrompt("deploy to $ARGUMENTS and $ARGUMENTS", "prod"); got != "deploy to prod and prod" {
		t.Fatalf("placeholder expansion: %q", got)
	}
	if got := ExpandPrompt("no placeholder", "prod"); got != "no placeholder\n\n## User Request\n\nprod" {
		t.Fatalf("appended arguments: %q", got)
	}
	if got := ExpandPrompt("no placeholder", "   "); got != "no placeholder" {
		t.Fatalf("blank arguments must be dropped: %q", got)
	}
	if got := ExpandPrompt("no placeholder", ""); got != "no placeholder" {
		t.Fatalf("empty arguments must be dropped: %q", got)
	}
	if got := ExpandPrompt("$ARGUMENTS only", ""); got != " only" {
		t.Fatalf("empty arguments with placeholder: %q", got)
	}
}

func TestLoadBounds(t *testing.T) {
	if MaxFileBytes != 256<<10 || MaxDepth != 3 || MaxCommands != 200 {
		t.Fatalf("load bounds changed: %d %d %d", MaxFileBytes, MaxDepth, MaxCommands)
	}
}
