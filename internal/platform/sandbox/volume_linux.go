//go:build linux

package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// BoundedWorkspaceVolume verifies an already configured disk volume. It never
// configures quota or mounts. A small physical filesystem is a hard capacity
// boundary even while a hostile command writes; periodic usage scans are not.
// A shared volume is deliberately limited to one workspace's maximum, so its
// aggregate capacity also remains below the larger project budget.
func BoundedWorkspaceVolume(root string, paths ...string) error {
	const maxBytes = uint64(512 << 20)
	var volumeFS unix.Statfs_t
	if !filepath.IsAbs(root) || unix.Statfs(root, &volumeFS) != nil || volumeFS.Bsize <= 0 || volumeFS.Blocks == 0 || uint64(volumeFS.Bsize) > maxBytes/volumeFS.Blocks {
		return fmt.Errorf("%w: workspace requires a disk volume of at most 512 MiB", ErrUnavailable)
	}
	// ext4 and XFS support real bounded block devices. Memory filesystems,
	// overlays and remote storage cannot establish this capacity contract.
	if volumeFS.Type != unix.EXT4_SUPER_MAGIC && volumeFS.Type != unix.XFS_SUPER_MAGIC {
		return fmt.Errorf("%w: workspace volume must be ext4 or XFS", ErrUnavailable)
	}
	var rootStat unix.Stat_t
	if unix.Lstat(root, &rootStat) != nil {
		return ErrUnavailable
	}
	rootMountID, err := workspaceMountID(root)
	if err != nil {
		return fmt.Errorf("%w: workspace mount identity is unavailable", ErrUnavailable)
	}
	for _, path := range append([]string{root}, paths...) {
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil || canonical != filepath.Clean(path) || !pathContains(root, path) {
			return ErrUnavailable
		}
		info, err := os.Lstat(path)
		var stat unix.Stat_t
		mountID, mountErr := workspaceMountID(path)
		if err != nil || !info.IsDir() || unix.Lstat(path, &stat) != nil || stat.Dev != rootStat.Dev || mountErr != nil || mountID != rootMountID {
			return ErrUnavailable
		}
	}
	// Existing nested mounts, links, devices and private-Git hardlinks must
	// not open a second filesystem or a sibling path inside the writable view.
	entries := 0
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return ErrUnavailable
		}
		entries++
		if entries > 65536 {
			return ErrUnavailable
		}
		var stat unix.Stat_t
		mountID, mountErr := workspaceMountID(path)
		if unix.Lstat(path, &stat) != nil || stat.Dev != rootStat.Dev || mountErr != nil || mountID != rootMountID {
			return ErrUnavailable
		}
		mode := stat.Mode & unix.S_IFMT
		if mode != unix.S_IFDIR && (mode != unix.S_IFREG || stat.Nlink != 1) {
			return ErrUnavailable
		}
		return nil
	})
}

// workspaceMountID uses statx's mount ID because st_dev alone cannot detect
// bind mounts made from another directory on the same filesystem. If the
// kernel cannot provide a mount ID, workspace isolation fails closed.
func workspaceMountID(path string) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_NO_AUTOMOUNT|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		return 0, fmt.Errorf("statx did not return mount ID for %s", path)
	}
	return stat.Mnt_id, nil
}
