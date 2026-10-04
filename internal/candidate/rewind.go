package candidate

import (
	"errors"
	"os"
	"path/filepath"
)

// StageRewind materializes a validated snapshot into a staging sibling of
// the candidate root, so the later exchange stays on one filesystem. The
// formal project tree is never a valid staging or rewind target.
func StageRewind(store *SnapshotStore, snap FileSnapshot, candidateRoot string) (string, error) {
	if store == nil {
		return "", errors.New("snapshot store is required")
	}
	candidateRoot, err := filepath.Abs(candidateRoot)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(candidateRoot)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrUnsafePath
	}
	staging := filepath.Join(filepath.Dir(candidateRoot), ".rewind-staging-"+snap.SnapshotID)
	if _, err = os.Lstat(staging); err == nil {
		return "", errors.New("rewind staging directory already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err = os.Mkdir(staging, 0700); err != nil {
		return "", err
	}
	if err = store.Materialize(snap, staging); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	return staging, nil
}

// GuardRewindTarget refuses to ever stage or swap over the formal project.
func GuardRewindTarget(formalRoot, candidateRoot string) error {
	formalRoot, err := filepath.Abs(formalRoot)
	if err != nil {
		return err
	}
	candidateRoot, err = filepath.Abs(candidateRoot)
	if err != nil {
		return err
	}
	if filepath.Clean(formalRoot) == filepath.Clean(candidateRoot) {
		return errors.New("formal project is never a rewind target")
	}
	return nil
}

// SwapWithStaging exchanges the candidate root with the prepared staging
// directory in one atomic rename; afterwards the candidate holds the
// snapshot and the staging directory holds the previous candidate content
// for cleanup.
func SwapWithStaging(candidateRoot, staging string) error {
	return ExchangeProjectDir(candidateRoot, staging)
}
