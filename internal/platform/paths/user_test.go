package paths

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestUserHomesXDGOverrides(t *testing.T) {
	configWant := filepath.Join(t.TempDir(), "config")
	stateWant := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_CONFIG_HOME", configWant)
	t.Setenv("XDG_STATE_HOME", stateWant)
	config, err := UserConfigHome()
	if err != nil || config != configWant {
		t.Fatalf("config home: %q, %v", config, err)
	}
	state, err := UserStateHome()
	if err != nil || state != stateWant {
		t.Fatalf("state home: %q, %v", state, err)
	}
}

func TestUserHomesLinuxDefaults(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux default regression")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	config, err := UserConfigHome()
	if err != nil || config != filepath.Join(home, ".config") {
		t.Fatalf("config home = %q, %v", config, err)
	}
	state, err := UserStateHome()
	if err != nil || state != filepath.Join(home, ".local", "state") {
		t.Fatalf("state home = %q, %v", state, err)
	}
}
