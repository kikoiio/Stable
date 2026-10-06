package appconfig

import (
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
