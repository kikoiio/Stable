//go:build linux || darwin

package secfile

import (
	"os"
	"path/filepath"
	"syscall"
)

func mkdirAllPrivate(path string, perm os.FileMode) error {
	var missing []string
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		_, err := os.Stat(current)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	// MkdirAll applies the mode only to newly created directories and umask can
	// further reduce it. Re-apply it to every newly created layer and the final
	// path so an existing permissive directory is tightened as well.
	for _, current := range missing {
		if err := os.Chmod(current, perm); err != nil {
			return err
		}
	}
	return os.Chmod(path, perm)
}

func openFilePrivate(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, perm); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func chmodPrivate(path string, perm os.FileMode) error { return os.Chmod(path, perm) }

func ownedByCurrentUser(info os.FileInfo) (bool, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return uint64(st.Uid) == uint64(os.Getuid()), nil
}

func isPrivate(info os.FileInfo) (bool, error) {
	if info == nil {
		return false, nil
	}
	owned, err := ownedByCurrentUser(info)
	if err != nil {
		return false, err
	}
	return permDeniesOthers(info.Mode().Perm()) && owned, nil
}

func isPrivatePath(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return isPrivate(info)
}
