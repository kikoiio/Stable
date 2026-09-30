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
