package appconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndValidate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	dir := filepath.Join(home, "config", "stable")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.json")
	secret := "sk-test-DO-NOT-LEAK"
	if err := os.WriteFile(p, []byte(`{"model":{"provider":"openai-compatible","model":"mock","base_url":"http://127.0.0.1:8877/v1","api_key":"`+secret+`"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Validate(true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.Summary(), secret) {
		t.Fatal("secret in summary")
	}
	if c.StateDir != filepath.Join(home, "state", "stable") {
		t.Fatal(c.StateDir)
	}
	t.Setenv("STABLE_MODEL", "override")
	c, err = Load()
	if err != nil || c.Model.Model != "override" {
		t.Fatalf("env override: %v %+v", err, c)
	}
	if err = os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(); err == nil {
		t.Fatal("public config accepted")
	}
}

func TestUserSkillsDirUsesXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("STABLE_CONFIG", filepath.Join(home, "elsewhere", "config.json"))
	dir, err := UserSkillsDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(xdg, "stable", "skills") {
		t.Fatalf("UserSkillsDir with XDG_CONFIG_HOME = %q", dir)
	}
}

func TestUserSkillsDirFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err := UserSkillsDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(home, ".config", "stable", "skills") {
		t.Fatalf("UserSkillsDir without XDG_CONFIG_HOME = %q", dir)
	}
}

func TestUserMemoryDirUsesXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("STABLE_CONFIG", filepath.Join(home, "elsewhere", "config.json"))
	dir, err := UserMemoryDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdg, "stable", "memory"); dir != want {
		t.Fatalf("UserMemoryDir with XDG_CONFIG_HOME = %q, want %q", dir, want)
	}
}

func TestUserMemoryDirFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err := UserMemoryDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "stable", "memory"); dir != want {
		t.Fatalf("UserMemoryDir without XDG_CONFIG_HOME = %q, want %q", dir, want)
	}
}

func TestUserHooksPathUsesXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("STABLE_CONFIG", filepath.Join(home, "elsewhere", "config.json"))
	path, err := UserHooksPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(xdg, "stable", "hooks.yaml") {
		t.Fatalf("UserHooksPath with XDG_CONFIG_HOME = %q", path)
	}
}

func TestUserHooksPathFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	path, err := UserHooksPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, ".config", "stable", "hooks.yaml") {
		t.Fatalf("UserHooksPath without XDG_CONFIG_HOME = %q", path)
	}
}

func TestCompatibleURL(t *testing.T) {
	for _, s := range []string{"http://127.0.0.1:1000/v1", "http://[::1]:1000/v1", "https://example.com/v1"} {
		if err := ValidateBaseURL(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"http://example.com/v1", "https://u:p@example.com/v1", "https://example.com/v1?token=x", "https://example.com/v1#x", "file:///tmp/x"} {
		if err := ValidateBaseURL(s); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func TestProviderSelectionUpdatePreservesConfigAndHidesKey(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stable.json")
	t.Setenv("STABLE_CONFIG", path)
	t.Setenv("STABLE_PROVIDER", "")
	t.Setenv("STABLE_MODEL", "")
	t.Setenv("STABLE_BASE_URL", "")
	secret := "sk-private-test"
	if err := os.WriteFile(path, []byte(`{"model":{"provider":"openai","model":"old","api_key":"`+secret+`"},"mcp_servers":[{"name":"keep"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateModelSelection("anthropic", "claude-test", ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatal("old provider key remained after provider switch")
	}
	var decoded map[string]any
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	model := decoded["model"].(map[string]any)
	if model["provider"] != "anthropic" || model["model"] != "claude-test" || model["api_key"] != nil || model["base_url"] != nil {
		t.Fatalf("model config = %#v", model)
	}
	if len(decoded["mcp_servers"].([]any)) != 1 {
		t.Fatalf("other config was lost: %#v", decoded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
	c, selection, err := LoadWithModelSources()
	if err != nil {
		t.Fatal(err)
	}
	if selection.ProviderSource != SourceFile || selection.ModelSource != SourceFile {
		t.Fatalf("sources = %+v", selection)
	}
	encoded, _ := json.Marshal(selection)
	if strings.Contains(string(encoded), secret) || strings.Contains(c.Summary(), secret) {
		t.Fatal("credential escaped into provider selection")
	}
}

func TestCompatibleProviderRetainsBaseURLOnModelChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("STABLE_CONFIG", path)
	if err := os.WriteFile(path, []byte(`{"model":{"provider":"openai-compatible","model":"old","base_url":"http://127.0.0.1:8123/v1"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateModelSelection("openai-compatible", "new", ""); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.Model != "new" || c.Model.BaseURL != "http://127.0.0.1:8123/v1" {
		t.Fatalf("model selection = %+v", c.Model)
	}
}

func TestUpdateModelSelectionCreatesMissingConfigPrivately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new", "config.json")
	t.Setenv("STABLE_CONFIG", path)
	if err := UpdateModelSelection("openai", "fresh-model", ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions = %o", info.Mode().Perm())
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.Provider != "openai" || c.Model.Model != "fresh-model" {
		t.Fatalf("created selection = %+v", c.Model)
	}
}

func TestUpdateModelSelectionFailureLeavesOriginalConfigUntouched(t *testing.T) {
	tests := []struct {
		name, content string
	}{
		{name: "invalid JSON", content: `{"model":`},
		{name: "invalid model object", content: `{"model":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("STABLE_CONFIG", path)
			if err := UpdateModelSelection("anthropic", "model", ""); err == nil {
				t.Fatal("expected configuration error")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.content {
				t.Fatalf("failed update changed original bytes: %q", data)
			}
		})
	}
}

func TestUpdateModelSelectionWriteFailureLeavesTargetUntouched(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config.json")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "preserve")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STABLE_CONFIG", target)
	if err := UpdateModelSelection("openai", "model", ""); err == nil {
		t.Fatal("expected write target failure")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "keep" {
		t.Fatalf("target changed after failed update: %q, %v", data, err)
	}
}

func TestLoadMCPServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	dir := filepath.Join(home, "config", "stable")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.json")
	cfg := `{"mcp_servers":[
		{"name":"fs","command":"npx","args":["-y","server-fs","/tmp"],"env":{"NODE_ENV":"production"}},
		{"name":"search","url":"http://127.0.0.1:9733/mcp","transport":"http","headers":{"Authorization":"Bearer dummy"}}
	]}`
	if err := os.WriteFile(p, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MCPServers) != 2 {
		t.Fatalf("want 2 MCP servers, got %d", len(c.MCPServers))
	}
	fs := c.MCPServers[0]
	if fs.Name != "fs" || fs.Command != "npx" || len(fs.Args) != 3 || fs.Args[1] != "server-fs" || fs.Env["NODE_ENV"] != "production" {
		t.Fatalf("stdio server parse: %+v", fs)
	}
	if fs.URL != "" || fs.Transport != "" || len(fs.Headers) != 0 {
		t.Fatalf("unexpected stdio server fields: %+v", fs)
	}
	remote := c.MCPServers[1]
	if remote.Name != "search" || remote.URL != "http://127.0.0.1:9733/mcp" || remote.Transport != "http" || remote.Headers["Authorization"] != "Bearer dummy" {
		t.Fatalf("remote server parse: %+v", remote)
	}
	if remote.Command != "" || len(remote.Args) != 0 || len(remote.Env) != 0 {
		t.Fatalf("unexpected remote server fields: %+v", remote)
	}

	// Missing mcp_servers key leaves the slice nil.
	if err = os.WriteFile(p, []byte(`{"model":{"provider":"openai","model":"gpt"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.MCPServers != nil {
		t.Fatalf("missing mcp_servers: want nil, got %+v", c.MCPServers)
	}

	// A wrong field type (command as a number) must fail JSON parsing the same
	// way as any other invalid config, not panic.
	if err = os.WriteFile(p, []byte(`{"mcp_servers":[{"name":"bad","command":42}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(); err == nil {
		t.Fatal("invalid mcp_servers JSON accepted")
	}
}
