package candidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"stable/internal/platform/secfile"
)

var ErrUnsafePath = errors.New("unsafe candidate path")

const (
	ManifestPolicyLegacy  = "legacy-v1"
	ManifestPolicyProject = "project-v2"
)

type Candidate struct {
	ID              string
	ManifestPolicy  string
	FormalRoot      string
	CandidateRoot   string
	BaselineDigest  string
	CandidateDigest string
	RootMode        uint32
	Status          string
}

func normalizeManifestPolicy(policy string) (string, error) {
	if policy == "" {
		return ManifestPolicyLegacy, nil
	}
	switch policy {
	case ManifestPolicyLegacy, ManifestPolicyProject:
		return policy, nil
	default:
		return "", fmt.Errorf("unsupported manifest policy %q", policy)
	}
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
	return BuildManifestForPolicy(root, ManifestPolicyLegacy)
}

func BuildManifestForPolicy(root, policy string) ([]ManifestEntry, string, error) {
	policy, err := normalizeManifestPolicy(policy)
	if err != nil {
		return nil, "", err
	}
	secureRoot, err := secfile.OpenRoot(root)
	if err != nil {
		return nil, "", err
	}
	root = secureRoot.Path()
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
		rel, err = CleanRelative(rel)
		if err != nil {
			return err
		}
		if policy == ManifestPolicyLegacy && rel == ".stable" && d.IsDir() {
			return filepath.SkipDir
		}
		if policy == ManifestPolicyProject {
			if rel == ".git" || rel == ".stable" || rel == ".mewcode" {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Base(rel) == ".git" {
				return fmt.Errorf("%w: nested git metadata %s", ErrUnsafePath, rel)
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if policy == ManifestPolicyProject && rel == ".gitmodules" && info.Size() > 0 {
			return fmt.Errorf("%w: submodule declarations are unsupported", ErrUnsafePath)
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("%w: symlink or special file %s", ErrUnsafePath, rel)
		}
		if info.IsDir() {
			return nil
		}
		f, err := secureRoot.Open(rel)
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
	if err := secureRoot.Revalidate(); err != nil {
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
	return CreateCandidateForPolicy(id, formalRoot, candidatesParent, ManifestPolicyLegacy)
}

func CreateCandidateForPolicy(id, formalRoot, candidatesParent, policy string) (result Candidate, returnErr error) {
	policy, err := normalizeManifestPolicy(policy)
	if err != nil {
		return Candidate{}, err
	}
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, "/\\") {
		return Candidate{}, errors.New("candidate ID is required")
	}
	formal, err := secfile.OpenRoot(formalRoot)
	if err != nil {
		return Candidate{}, err
	}
	formalRoot = formal.Path()
	candidatesParent, err = filepath.Abs(candidatesParent)
	if err != nil {
		return Candidate{}, err
	}
	if err := secfile.MkdirAllPrivate(candidatesParent, 0700); err != nil {
		return Candidate{}, err
	}
	if err := SyncDirectory(filepath.Dir(candidatesParent)); err != nil {
		return Candidate{}, err
	}
	if _, err = secfile.OpenRoot(candidatesParent); err != nil {
		return Candidate{}, fmt.Errorf("candidate parent is unsafe: %w", err)
	}
	if err = secfile.SameVolume(formalRoot, candidatesParent); err != nil {
		if errors.Is(err, secfile.ErrDifferentDevice) {
			return Candidate{}, errors.New("candidate must be created on the formal project's filesystem")
		}
		return Candidate{}, err
	}
	base, digest, err := BuildManifestForPolicy(formal.Path(), policy)
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
	createdIdentity, err := CaptureRootIdentity(target)
	if err != nil {
		// Without an identity there is no safe compensation for this path.
		return Candidate{}, err
	}
	rollback := true
	defer func() {
		if !rollback {
			return
		}
		currentIdentity, identityErr := CaptureRootIdentity(target)
		if identityErr != nil || currentIdentity != createdIdentity {
			returnErr = errors.Join(returnErr, errors.New("candidate cleanup identity mismatch; root retained"))
			return
		}
		if err := os.RemoveAll(target); err != nil {
			returnErr = errors.Join(returnErr, err)
			return
		}
		if err := SyncDirectory(candidatesParent); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	// Make the new root name durable before any later candidate record can
	// refer to it. The merged exporter also syncs the completed tree before it
	// publishes a ready row.
	if err := SyncDirectory(candidatesParent); err != nil {
		return Candidate{}, err
	}
	rootInfo, err := os.Stat(formalRoot)
	if err != nil {
		return Candidate{}, err
	}
	rootMode := uint32(rootInfo.Mode().Perm())
	if err = secfile.ChmodPrivate(target, os.FileMode(rootMode)); err != nil {
		return Candidate{}, err
	}
	for _, e := range base {
		dst := filepath.Join(target, e.Path)
		if err = secfile.MkdirAllPrivate(filepath.Dir(dst), 0700); err != nil {
			return Candidate{}, err
		}
		if err = copyFile(formal, e.Path, dst, os.FileMode(e.Mode)); err != nil {
			return Candidate{}, err
		}
	}
	if err := SyncDirectoryTree(target); err != nil {
		return Candidate{}, err
	}
	rollback = false
	return Candidate{ID: id, ManifestPolicy: policy, FormalRoot: formalRoot, CandidateRoot: target, BaselineDigest: digest, CandidateDigest: digest, RootMode: rootMode, Status: "prepared"}, nil
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
	_, digest, err := BuildManifestForPolicy(c.CandidateRoot, c.ManifestPolicy)
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
	file, err := secfile.SecureOpen(root, clean)
	if err != nil {
		if errors.Is(err, secfile.ErrUnsafePath) {
			return nil, ErrUnsafePath
		}
		return nil, err
	}
	return file, nil
}

func copyFile(root secfile.Root, rel, dst string, mode os.FileMode) error {
	in, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
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

// SyncDirectoryTree persists directory entries below root, syncing deepest
// directories first and root last. File contents must be synced by their
// writers before this function is called.
func SyncDirectoryTree(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	identity, err := root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(identity, current) {
		return errors.New("candidate root changed before directory sync")
	}
	if err := syncDirectoryTree(root, "."); err != nil {
		return err
	}
	current, err = os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(identity, current) {
		return errors.New("candidate root changed during directory sync")
	}
	return nil
}

func syncDirectoryTree(root *os.Root, relative string) error {
	directory, err := root.Open(relative)
	if err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		child := filepath.Join(relative, entry.Name())
		info, err := root.Lstat(child)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := syncDirectoryTree(root, child); err != nil {
			return err
		}
	}
	directory, err = root.Open(relative)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// SyncDirectory persists entries in a single directory and checks that its
// pathname still identifies the opened directory after syncing.
func SyncDirectory(path string) error {
	directory, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	info, err := directory.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
		return errors.New("candidate parent changed before directory sync")
	}
	file, err := directory.Open(".")
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	current, err = os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
		return errors.New("candidate parent changed during directory sync")
	}
	return nil
}
