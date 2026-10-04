package candidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrUnsafePath = errors.New("unsafe candidate path")

type Candidate struct {
	ID              string
	FormalRoot      string
	CandidateRoot   string
	BaselineDigest  string
	CandidateDigest string
	RootMode        uint32
	Status          string
}

type ManifestEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

func CleanRelative(path string) (string, error) {
	if path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return "", ErrUnsafePath
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrUnsafePath
	}
	return clean, nil
}

func BuildManifest(root string) ([]ManifestEntry, string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, "", err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, "", ErrUnsafePath
	}
	entries := make([]ManifestEntry, 0)
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// The service keeps session logs under <root>/.stable. That subtree is
		// runtime state, not project content: it grows while a run is in
		// flight, so counting it would make every session-scoped candidate look
		// stale at review time, and exchanging it would revert the transcript.
		if rel == ".stable" && d.IsDir() {
			return filepath.SkipDir
		}
		rel, err = CleanRelative(rel)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("%w: symlink or special file %s", ErrUnsafePath, rel)
		}
		if info.IsDir() {
			return nil
		}
		f, err := secureOpen(root, rel)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		entries = append(entries, ManifestEntry{Path: rel, Mode: uint32(info.Mode().Perm()), Size: info.Size(), Digest: hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00%s\n", e.Path, e.Mode, e.Size, e.Digest)
	}
	return entries, hex.EncodeToString(h.Sum(nil)), nil
}

func CreateCandidate(id, formalRoot, candidatesParent string) (Candidate, error) {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, "/\\") {
		return Candidate{}, errors.New("candidate ID is required")
	}
	formalRoot, err := filepath.Abs(formalRoot)
	if err != nil {
		return Candidate{}, err
	}
	formalRoot, err = filepath.EvalSymlinks(formalRoot)
	if err != nil {
		return Candidate{}, err
	}
	candidatesParent, err = filepath.Abs(candidatesParent)
	if err != nil {
		return Candidate{}, err
	}
	if err := os.MkdirAll(candidatesParent, 0700); err != nil {
		return Candidate{}, err
	}
	resolvedParent, err := filepath.EvalSymlinks(candidatesParent)
	if err != nil {
		return Candidate{}, err
	}
	if filepath.Clean(resolvedParent) != filepath.Clean(candidatesParent) {
		return Candidate{}, fmt.Errorf("candidate parent must not traverse a symlink")
	}
	var formalStat, candidateStat unix.Stat_t
	if err = unix.Stat(formalRoot, &formalStat); err != nil {
		return Candidate{}, err
	}
	if err = unix.Stat(candidatesParent, &candidateStat); err != nil {
		return Candidate{}, err
	}
	if formalStat.Dev != candidateStat.Dev {
		return Candidate{}, errors.New("candidate must be created on the formal project's filesystem")
	}
	base, digest, err := BuildManifest(formalRoot)
	if err != nil {
		return Candidate{}, err
	}
	target := filepath.Join(candidatesParent, id)
	if _, err = os.Lstat(target); err == nil {
		return Candidate{}, os.ErrExist
	} else if !os.IsNotExist(err) {
		return Candidate{}, err
	}
	if err = os.Mkdir(target, 0700); err != nil {
		return Candidate{}, err
	}
	rootInfo, err := os.Stat(formalRoot)
	if err != nil {
		return Candidate{}, err
	}
	rootMode := uint32(rootInfo.Mode().Perm())
	if err = os.Chmod(target, os.FileMode(rootMode)); err != nil {
		return Candidate{}, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = os.RemoveAll(target)
		}
	}()
	for _, e := range base {
		dst := filepath.Join(target, e.Path)
		if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return Candidate{}, err
		}
		if err = copyFile(formalRoot, e.Path, dst, os.FileMode(e.Mode)); err != nil {
			return Candidate{}, err
		}
	}
	rollback = false
	return Candidate{ID: id, FormalRoot: formalRoot, CandidateRoot: target, BaselineDigest: digest, CandidateDigest: digest, RootMode: rootMode, Status: "prepared"}, nil
}

func FreezeCandidate(c Candidate, stopWriter func(context.Context) error, ctx context.Context) (Candidate, error) {
	if c.Status != "prepared" && c.Status != "running" {
		return c, fmt.Errorf("candidate cannot be frozen from state %q", c.Status)
	}
	if stopWriter != nil {
		if err := stopWriter(ctx); err != nil {
			return c, err
		}
	}
	_, digest, err := BuildManifest(c.CandidateRoot)
	if err != nil {
		return c, err
	}
	c.CandidateDigest = digest
	c.Status = "frozen"
	return c, nil
}

func secureOpen(root, rel string) (*os.File, error) {
	clean, err := CleanRelative(rel)
	if err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat2(rootFD, clean, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), clean)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, ErrUnsafePath
	}
	return file, nil
}

func copyFile(root, rel, dst string, mode os.FileMode) error {
	in, err := secureOpen(root, rel)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	return nil
}
