package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/appconfig"
	"stable/internal/platform/paths"
	"stable/internal/runtime"
)

func TestProviderListAndShowDoNotExposeCredentials(t *testing.T) {
	var out, stderr bytes.Buffer
	if err := runProvider([]string{"list"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"openai", "anthropic", "openai-compatible", "gemini"} {
		if !strings.Contains(out.String(), provider) {
			t.Fatalf("list omitted %s: %s", provider, out.String())
		}
	}
	home := t.TempDir()
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	secret := "sk-show-test-secret"
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"model":{"provider":"openai","model":"test-model","api_key":"`+secret+`"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STABLE_CONFIG", path)
	t.Setenv("STABLE_PROVIDER", "")
	t.Setenv("STABLE_MODEL", "")
	t.Setenv("STABLE_BASE_URL", "")
	out.Reset()
	if err := runProvider([]string{"show"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatal("provider show exposed an API key")
	}
	if !strings.Contains(out.String(), "test-model (file)") {
		t.Fatalf("show output = %q", out.String())
	}
}

func TestPrintReadToolAllowlist(t *testing.T) {
	for _, name := range []string{"read_file", "glob", "grep"} {
		if !printReadTool(name) {
			t.Errorf("%s should be allowed", name)
		}
	}
	for _, name := range []string{"write_file", "command", "ask_user", "exit_plan_mode", "mcp_call"} {
		if printReadTool(name) {
			t.Errorf("%s should be denied", name)
		}
	}
}

func TestProviderUseRequiresStoppedRuntimeAndPreservesConfig(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	state := filepath.Join(home, "state")
	original := `{"state_dir":"` + filepath.ToSlash(state) + `","model":{"provider":"openai","model":"old","api_key":"old-secret"},"mcp_servers":[{"name":"keep"}]}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STABLE_CONFIG", path)
	for _, name := range []string{"STABLE_PROVIDER", "STABLE_MODEL", "STABLE_BASE_URL", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "STABLE_API_KEY"} {
		t.Setenv(name, "")
	}
	oldControl := providerRuntimeControl
	t.Cleanup(func() { providerRuntimeControl = oldControl })
	providerRuntimeControl = func(_ paths.Paths, _ string) (runtime.Status, error) {
		return runtime.Status{Running: true}, nil
	}
	var out, stderr bytes.Buffer
	err := providerUse([]string{"anthropic", "--model", "claude-next"}, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "stable down") {
		t.Fatalf("running runtime error = %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != original {
		t.Fatalf("running runtime changed config: %s", data)
	}

	providerRuntimeControl = func(_ paths.Paths, _ string) (runtime.Status, error) {
		return runtime.Status{}, errors.New("runtime is not running")
	}
	t.Setenv("STABLE_MODEL", "env-model-wins")
	if err = providerUse([]string{"anthropic", "--model", "claude-next"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "STABLE_MODEL") {
		t.Fatalf("missing active environment override notice: %q", stderr.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err = json.Unmarshal(updated, &config); err != nil {
		t.Fatal(err)
	}
	model := config["model"].(map[string]any)
	if model["provider"] != "anthropic" || model["model"] != "claude-next" || model["api_key"] != nil {
		t.Fatalf("saved selection = %#v", model)
	}
	if len(config["mcp_servers"].([]any)) != 1 {
		t.Fatalf("unrelated config lost: %#v", config)
	}
	loaded, err := appconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Model.Model != "env-model-wins" {
		t.Fatalf("model override did not win: %#v", loaded.Model)
	}
	if strings.Contains(out.String()+stderr.String(), "old-secret") {
		t.Fatal("provider use leaked credentials")
	}
}
