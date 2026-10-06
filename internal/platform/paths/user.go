package paths

import (
	"os"
	"path/filepath"
)

// UserConfigHome returns the platform's user configuration base directory.
// XDG_CONFIG_HOME remains an explicit override on every platform.
func UserConfigHome() (string, error) {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return base, nil
	}
	return defaultConfigHome()
}

// UserStateHome returns the platform's user state base directory.
// XDG_STATE_HOME remains an explicit override on every platform.
func UserStateHome() (string, error) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return base, nil
	}
	return defaultStateHome()
}

func userHomeJoin(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}
