//go:build windows

package paths

import "os"

func defaultConfigHome() (string, error) {
	if base := os.Getenv("AppData"); base != "" {
		return base, nil
	}
	return os.UserConfigDir()
}

func defaultStateHome() (string, error) {
	if base := os.Getenv("LocalAppData"); base != "" {
		return base, nil
	}
	return os.UserCacheDir()
}
