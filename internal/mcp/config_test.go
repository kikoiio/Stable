package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"stable/internal/appconfig"
)

func mcpTestServer(name, command string) appconfig.MCPServerConfig {
	return appconfig.MCPServerConfig{Name: name, Command: command}
}

func mcpNamesOf(servers []appconfig.MCPServerConfig) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Name)
	}
	return out
}

func writeMCPProjectFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func joinedRejections(rejections Rejections) string {
	return strings.Join(rejections, "\n")
}

func TestValidateServerEntries(t *testing.T) {
	cases := []struct {
		desc    string
		srv     appconfig.MCPServerConfig
		wantErr string // empty means the entry is valid
	}{
		{"command server", mcpTestServer("s", "echo"), ""},
		{"url server", appconfig.MCPServerConfig{Name: "s", URL: "http://example.com"}, ""},
		{"empty transport defaults", appconfig.MCPServerConfig{Name: "s", URL: "http://example.com", Transport: ""}, ""},
		{"sse transport", appconfig.MCPServerConfig{Name: "s", URL: "http://example.com", Transport: "sse"}, ""},
		{"http transport", appconfig.MCPServerConfig{Name: "s", URL: "http://example.com", Transport: "http"}, ""},
		{"streamable transport", appconfig.MCPServerConfig{Name: "s", URL: "http://example.com", Transport: "streamable"}, ""},
		{"missing name", appconfig.MCPServerConfig{Command: "echo"}, "missing name"},
		{"blank name", appconfig.MCPServerConfig{Name: "   ", Command: "echo"}, "missing name"},
		{"command and url both empty", appconfig.MCPServerConfig{Name: "s"}, "both empty"},
		{"command and url both set", appconfig.MCPServerConfig{Name: "s", Command: "echo", URL: "http://example.com"}, "mutually exclusive"},
		{"invalid transport", appconfig.MCPServerConfig{Name: "s", URL: "http://example.com", Transport: "WebSocket"}, `invalid transport "WebSocket"`},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			err := Validate(tc.srv)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", tc.srv, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate(%+v) = %v, want error containing %q", tc.srv, err, tc.wantErr)
			}
		})
	}
}

func TestLoadMergeProjectOverridesUserKeepsPosition(t *testing.T) {
	user := []appconfig.MCPServerConfig{mcpTestServer("alpha", "a"), mcpTestServer("beta", "b")}
	path := writeMCPProjectFile(t, "- name: gamma\n  command: g\n- name: beta\n  command: b-project\n")

	got, rejections := LoadMerge(user, path)

	want := []string{"alpha", "gamma", "beta"}
	if !reflect.DeepEqual(mcpNamesOf(got), want) {
		t.Fatalf("LoadMerge names = %v, want %v", mcpNamesOf(got), want)
	}
	if got[2].Command != "b-project" {
		t.Fatalf("surviving beta entry carries command %q, want project's %q", got[2].Command, "b-project")
	}
	joined := joinedRejections(rejections)
	if !strings.Contains(joined, "user:beta") || !strings.Contains(joined, "overridden by project entry") {
		t.Fatalf("rejections %q missing override notice for user beta", joined)
	}
}

func TestLoadMergeMatchesOriginalNamesNotSanitized(t *testing.T) {
	user := []appconfig.MCPServerConfig{mcpTestServer("chrome-devtools", "user-cmd")}
	path := writeMCPProjectFile(t, "- name: chrome_devtools\n  command: project-cmd\n")

	got, rejections := LoadMerge(user, path)

	// The names only collide after sanitization; by original name they are
	// distinct, so both survive in their own sections.
	if want := []string{"chrome-devtools", "chrome_devtools"}; !reflect.DeepEqual(mcpNamesOf(got), want) {
		t.Fatalf("names = %v, want %v", mcpNamesOf(got), want)
	}
	if len(rejections) != 0 {
		t.Fatalf("rejections = %q, want none", joinedRejections(rejections))
	}
}

func TestLoadMergeRejectsInvalidUserEntries(t *testing.T) {
	user := []appconfig.MCPServerConfig{
		{Command: "echo"},                                                 // missing name
		{Name: "both-empty"},                                              // command and url both empty
		{Name: "both-set", Command: "echo", URL: "http://example.com"},    // both set
		{Name: "bad-transport", URL: "http://example.com", Transport: "ws"}, // invalid transport
		mcpTestServer("ok", "echo"),
	}

	got, rejections := LoadMerge(user, "")

	if want := []string{"ok"}; !reflect.DeepEqual(mcpNamesOf(got), want) {
		t.Fatalf("names = %v, want %v", mcpNamesOf(got), want)
	}
	joined := joinedRejections(rejections)
	for _, want := range []string{
		"user:#1: missing name",
		"user:both-empty: command and url are both empty",
		"user:both-set: command and url are mutually exclusive",
		`user:bad-transport: invalid transport "ws"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("rejections missing %q; got:\n%s", want, joined)
		}
	}
}

func TestLoadMergeRejectsInvalidProjectEntries(t *testing.T) {
	content := `- name: good
  command: echo
- command: echo
- name: both-empty
- name: both-set
  command: echo
  url: http://example.com
- name: bad-transport
  url: http://example.com
  transport: websocket
`
	path := writeMCPProjectFile(t, content)

	got, rejections := LoadMerge([]appconfig.MCPServerConfig{mcpTestServer("user-only", "u")}, path)

	if want := []string{"user-only", "good"}; !reflect.DeepEqual(mcpNamesOf(got), want) {
		t.Fatalf("names = %v, want %v", mcpNamesOf(got), want)
	}
	joined := joinedRejections(rejections)
	for _, want := range []string{
		"project:#2: missing name",
		"project:both-empty: command and url are both empty",
		"project:both-set: command and url are mutually exclusive",
		`project:bad-transport: invalid transport "websocket"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("rejections missing %q; got:\n%s", want, joined)
		}
	}
}

func TestLoadMergeRefusesSymlinkedProjectFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(target, []byte("- name: sneaky\n  command: echo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "mcp.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got, rejections := LoadMerge([]appconfig.MCPServerConfig{mcpTestServer("u", "echo")}, link)

	if want := []string{"u"}; !reflect.DeepEqual(mcpNamesOf(got), want) {
		t.Fatalf("names = %v, want %v", mcpNamesOf(got), want)
	}
	if joined := joinedRejections(rejections); !strings.Contains(joined, "symbolic link refused") {
		t.Fatalf("rejections = %q, want symbolic link refusal", joined)
	}
}

func TestLoadMergeRejectsOversizedProjectFile(t *testing.T) {
	path := writeMCPProjectFile(t, strings.Repeat("#", MaxMCPFileBytes+1))

	got, rejections := LoadMerge(nil, path)

	if len(got) != 0 {
		t.Fatalf("got %d servers from oversized file, want 0", len(got))
	}
	if joined := joinedRejections(rejections); !strings.Contains(joined, fmt.Sprintf("file exceeds %d bytes", MaxMCPFileBytes)) {
		t.Fatalf("rejections = %q, want size-cap notice", joined)
	}
}

func TestLoadMergeRejectsEntriesOverPerFileLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= MaxMCPServersPerFile; i++ {
		fmt.Fprintf(&b, "- name: srv%03d\n  command: echo\n", i)
	}
	path := writeMCPProjectFile(t, b.String())

	got, rejections := LoadMerge(nil, path)

	if len(got) != MaxMCPServersPerFile {
		t.Fatalf("got %d servers, want %d", len(got), MaxMCPServersPerFile)
	}
	joined := joinedRejections(rejections)
	if !strings.Contains(joined, "exceeds per-file limit 100") {
		t.Fatalf("rejections = %q, want per-file limit notice", joined)
	}
	if got := strings.Count(joined, "exceeds per-file limit"); got != 1 {
		t.Fatalf("got %d limit rejections for one extra entry, want 1", got)
	}
}

func TestLoadMergeMissingProjectFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")

	got, rejections := LoadMerge([]appconfig.MCPServerConfig{mcpTestServer("u", "echo")}, path)

	if want := []string{"u"}; !reflect.DeepEqual(mcpNamesOf(got), want) {
		t.Fatalf("names = %v, want %v", mcpNamesOf(got), want)
	}
	if len(rejections) != 0 {
		t.Fatalf("rejections = %q, want none for missing file", joinedRejections(rejections))
	}
}

func TestLoadMergeEmptyProjectFileIsEmpty(t *testing.T) {
	path := writeMCPProjectFile(t, "")

	got, rejections := LoadMerge(nil, path)

	if len(got) != 0 || len(rejections) != 0 {
		t.Fatalf("got %d servers, %d rejections; want 0 and 0", len(got), len(rejections))
	}
}

func TestLoadMergeInvalidProjectYAMLRejectsWholeFile(t *testing.T) {
	// A mapping rather than the expected top-level array.
	path := writeMCPProjectFile(t, "servers:\n  - name: x\n    command: echo\n")

	got, rejections := LoadMerge(nil, path)

	if len(got) != 0 {
		t.Fatalf("got %d servers from malformed file, want 0", len(got))
	}
	if joined := joinedRejections(rejections); !strings.Contains(joined, "invalid YAML") {
		t.Fatalf("rejections = %q, want invalid YAML notice", joined)
	}
}

func TestLoadMergeKeepsUserThenProjectOrder(t *testing.T) {
	user := []appconfig.MCPServerConfig{mcpTestServer("u1", "echo"), mcpTestServer("u2", "echo")}
	path := writeMCPProjectFile(t, "- name: p1\n  command: echo\n- name: p2\n  command: echo\n")

	got, rejections := LoadMerge(user, path)

	if want := []string{"u1", "u2", "p1", "p2"}; !reflect.DeepEqual(mcpNamesOf(got), want) {
		t.Fatalf("names = %v, want %v", mcpNamesOf(got), want)
	}
	if len(rejections) != 0 {
		t.Fatalf("rejections = %q, want none", joinedRejections(rejections))
	}
}

func TestLoadMergeProjectEntryFieldsRoundTrip(t *testing.T) {
	content := `- name: remote
  url: http://example.com/sse
  transport: sse
  headers:
    Authorization: Bearer token
  env:
    FOO: bar
`
	path := writeMCPProjectFile(t, content)

	got, rejections := LoadMerge(nil, path)

	if len(rejections) != 0 {
		t.Fatalf("rejections = %q, want none", joinedRejections(rejections))
	}
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1", len(got))
	}
	s := got[0]
	if s.URL != "http://example.com/sse" || s.Transport != "sse" {
		t.Fatalf("url/transport = %q/%q, want parsed values", s.URL, s.Transport)
	}
	if s.Headers["Authorization"] != "Bearer token" || s.Env["FOO"] != "bar" {
		t.Fatalf("headers/env = %v/%v, want parsed maps", s.Headers, s.Env)
	}
}
