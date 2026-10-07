//go:build linux

package candidate

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Pin both parent directories and let the kernel refuse an existing target.
// The same operation handles directory metadata and a linked-worktree pointer.
func moveMetadataNoReplace(source, destination string) error {
	sourceFD, err := unix.Open(filepath.Dir(source), unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(sourceFD)
	destinationFD, err := unix.Open(filepath.Dir(destination), unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(destinationFD)
	return unix.Renameat2(sourceFD, filepath.Base(source), destinationFD, filepath.Base(destination), unix.RENAME_NOREPLACE)
}
