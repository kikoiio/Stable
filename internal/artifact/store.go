package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"stable/internal/platform/secfile"
	"strings"
	"time"

	"stable/internal/core"
)

type Store struct {
	Root string
}

func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0755); err != nil {
		return nil, err
	}
	return &Store{Root: abs}, nil
}

func Digest(_ context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Store) Digest(ctx context.Context, path string) (string, error) { return Digest(ctx, path) }

func (s *Store) Snapshot(ctx context.Context, goalID, path string) (core.ArtifactVersion, error) {
	var out core.ArtifactVersion
	if goalID == "" || strings.ContainsAny(goalID, `/\`) || goalID == "." || goalID == ".." {
		return out, errors.New("invalid goal ID")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return out, err
	}
	if !inside(s.Root, resolved) {
		return out, fmt.Errorf("source outside authorized run root: %s", path)
	}
	digest, err := Digest(ctx, resolved)
	if err != nil {
		return out, err
	}
	dir := filepath.Join(s.Root, "snapshots", goalID)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return out, err
	}
	dst := filepath.Join(dir, digest+filepath.Ext(path))
	if _, err = os.Stat(dst); err == nil {
		return core.ArtifactVersion{ID: digest, GoalID: goalID, Path: dst, CreatedAt: time.Now().UTC()}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	src, err := os.Open(resolved)
	if err != nil {
		return out, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return out, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = io.Copy(tmp, src); err != nil {
		tmp.Close()
		return out, err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return out, err
	}
	if err = secfile.ChmodPrivate(tmp.Name(), 0444); err != nil {
		tmp.Close()
		return out, err
	}
	if err = tmp.Close(); err != nil {
		return out, err
	}
	check, err := Digest(ctx, tmpName)
	if err != nil {
		return out, err
	}
	if check != digest {
		return out, errors.New("source changed while snapshotting")
	}
	if err = os.Rename(tmpName, dst); err != nil {
		return out, err
	}
	return core.ArtifactVersion{ID: digest, GoalID: goalID, Path: dst, CreatedAt: time.Now().UTC()}, nil
}

func inside(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
