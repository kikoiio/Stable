package workspace

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"stable/internal/platform/secfile"
)

type ManifestEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type Manifest struct {
	PolicyVersion string          `json:"policy_version"`
	Entries       []ManifestEntry `json:"entries"`
	Digest        string          `json:"digest"`
	Bytes         int64           `json:"bytes"`
}

func ProtectedRoot(path string) bool {
	first := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")[0]
	return strings.EqualFold(first, ".git") || strings.EqualFold(first, ".stable") || strings.EqualFold(first, ".mewcode")
}

func CleanRelative(path string) (string, error) {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\\\x00") {
		return "", ErrUnsafePath
	}
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".." {
			return "", ErrUnsafePath
		}
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrUnsafePath
	}
	return clean, nil
}

// ManifestDigest retains candidate's stable project data digest encoding.
// PolicyVersion must still be persisted separately: identical project data
// must not make an old Git-inclusive policy interchangeable with project-v2.
func ManifestDigest(entries []ManifestEntry) (string, error) {
	entries = append([]ManifestEntry(nil), entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	h := sha256.New()
	previous := ""
	for _, entry := range entries {
		path, err := CleanRelative(entry.Path)
		if err != nil || path != entry.Path || ProtectedRoot(path) || containsGit(path) || path == previous || entry.Size < 0 || entry.Mode > 0777 {
			return "", ErrUnsafePath
		}
		digest, err := hex.DecodeString(entry.Digest)
		if err != nil || len(digest) != sha256.Size {
			return "", errors.New("workspace manifest contains an invalid digest")
		}
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00%s\n", path, entry.Mode, entry.Size, entry.Digest)
		previous = path
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openVerifiedRoot(path string) (*os.Root, os.FileInfo, error) {
	if err := validateAncestors(path, false); err != nil {
		return nil, nil, err
	}
	expected, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, actual) {
		root.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, ErrSourceChanged
	}
	return root, expected, nil
}

func revalidateRoot(path string, expected os.FileInfo) error {
	if err := validateAncestors(path, false); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(current, expected) {
		return ErrSourceChanged
	}
	return nil
}

// CaptureRootIdentity records a directory's physical identity for an
// operation that must stay bound to the same project root across filesystem
// path replacement.
func CaptureRootIdentity(path string) (RootIdentity, error) {
	root, info, err := openVerifiedRoot(path)
	if err != nil {
		return RootIdentity{}, err
	}
	defer root.Close()
	identity, err := rootIdentity(info)
	if err != nil {
		return RootIdentity{}, err
	}
	if err := revalidateRoot(path, info); err != nil {
		return RootIdentity{}, ErrOwnership
	}
	return identity, nil
}

// ValidateRootIdentity fails closed if path no longer names the captured
// directory.
func ValidateRootIdentity(path string, expected RootIdentity) error {
	actual, err := CaptureRootIdentity(path)
	if err != nil || actual != expected {
		return ErrOwnership
	}
	return nil
}

// RootIdentityFromToken converts the transaction journal's Unix device/inode
// identity into the workspace receipt representation.
func RootIdentityFromToken(token string) (RootIdentity, error) {
	parts := strings.Split(token, ":")
	if len(parts) != 3 || parts[0] != "unix" {
		return RootIdentity{}, ErrOwnership
	}
	device, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return RootIdentity{}, ErrOwnership
	}
	inode, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || inode == 0 {
		return RootIdentity{}, ErrOwnership
	}
	return RootIdentity{Device: device, Inode: inode}, nil
}

func BuildManifest(ctx context.Context, path string, limits Limits) (Manifest, error) {
	limits = limits.Normalized()
	root, identity, err := openVerifiedRoot(path)
	if err != nil {
		return Manifest{}, err
	}
	defer root.Close()
	manifest := Manifest{PolicyVersion: ManifestPolicy, Entries: []ManifestEntry{}}
	entries := 0
	err = walkBounded(ctx, root, limits.MaxEntries, func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		entries++
		if entries > limits.MaxEntries {
			return fmt.Errorf("%w: too many filesystem entries", ErrQuota)
		}
		rel, err := CleanRelative(name)
		if err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s", ErrUnsafePath, rel)
		}
		if !strings.Contains(rel, "/") && ProtectedRoot(rel) {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		for _, component := range strings.Split(rel, "/") {
			if strings.EqualFold(component, ".git") {
				return fmt.Errorf("%w: nested Git metadata", ErrUnsafePath)
			}
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return ErrUnsafePath
		}
		if len(manifest.Entries) >= limits.MaxFiles || info.Size() > limits.MaxFileBytes || info.Size() > limits.MaxSnapshotBytes-manifest.Bytes {
			return ErrQuota
		}
		file, err := openRegular(root, name, info)
		if err != nil {
			return err
		}
		defer file.Close()
		if rel == ".gitmodules" {
			if err := validateGitmodules(ctx, file); err != nil {
				return err
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return err
			}
		}
		h := sha256.New()
		written, err := io.Copy(h, io.LimitReader(contextReader{ctx, file}, info.Size()+1))
		if err != nil {
			return err
		}
		after, err := file.Stat()
		if err != nil {
			return err
		}
		current, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if written != info.Size() || after.Size() != info.Size() || after.Mode() != info.Mode() || !after.ModTime().Equal(info.ModTime()) || !os.SameFile(info, current) || current.Mode() != info.Mode() || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
			return ErrSourceChanged
		}
		if err := validateHardlinks(after); err != nil {
			return err
		}
		manifest.Bytes += written
		manifest.Entries = append(manifest.Entries, ManifestEntry{Path: rel, Mode: uint32(info.Mode().Perm()), Size: written, Digest: hex.EncodeToString(h.Sum(nil))})
		return file.Close()
	})
	if err != nil {
		return Manifest{}, err
	}
	if err := revalidateRoot(path, identity); err != nil {
		return Manifest{}, err
	}
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	manifest.Digest, err = ManifestDigest(manifest.Entries)
	return manifest, err
}

func containsGit(path string) bool {
	for _, component := range strings.Split(path, "/") {
		if strings.EqualFold(component, ".git") {
			return true
		}
	}
	return false
}

// walkBounded reads at most 64 directory entries at once. WalkDir's sorted
// directory reads can allocate unbounded memory before the callback sees a
// hostile directory's entry count.
func walkBounded(ctx context.Context, root *os.Root, limit int, visit fs.WalkDirFunc) error {
	count := 0
	var walk func(string) error
	walk = func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		expected, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		dir, err := openBeneath(root, name, true)
		if err != nil {
			return err
		}
		defer dir.Close()
		actual, err := dir.Stat()
		if err != nil || !os.SameFile(expected, actual) {
			return ErrSourceChanged
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, readErr := dir.ReadDir(64)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			for _, entry := range entries {
				count++
				if count > limit {
					return ErrQuota
				}
				child := entry.Name()
				if name != "." {
					child = name + "/" + child
				}
				if err := visit(child, entry, nil); err != nil {
					if errors.Is(err, fs.SkipDir) {
						continue
					}
					return err
				}
				if entry.IsDir() {
					if err := walk(child); err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				current, err := root.Lstat(name)
				if err != nil {
					return err
				}
				if !os.SameFile(expected, current) || !expected.ModTime().Equal(current.ModTime()) {
					return ErrSourceChanged
				}
				return nil
			}
		}
	}
	return walk(".")
}

func openRegular(root *os.Root, path string, expected os.FileInfo) (*os.File, error) {
	if err := validateHardlinks(expected); err != nil {
		return nil, err
	}
	file, err := openBeneath(root, path, false)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err == nil && (!actual.Mode().IsRegular() || !os.SameFile(actual, expected)) {
		err = ErrSourceChanged
	}
	if err == nil {
		err = validateHardlinks(actual)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func validateGitmodules(ctx context.Context, reader io.Reader) error {
	scanner := bufio.NewScanner(contextReader{ctx, reader})
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, ";") {
			return ErrUnsupportedSubmodule
		}
	}
	return scanner.Err()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// CopySnapshot creates a new target; failures leave partial data in that
// explicitly allocated target for the caller's ownership journal to reconcile.
// It never recursively removes a path supplied by a caller.
func CopySnapshot(ctx context.Context, source, target string, limits Limits) (Manifest, error) {
	return copySnapshot(ctx, source, target, limits, nil)
}

func copySnapshot(ctx context.Context, source, target string, limits Limits, allocated func(os.FileInfo) error) (Manifest, error) {
	return copySnapshotWithSync(ctx, source, target, limits, allocated, nil)
}

// snapshotDirectorySync is a narrow fault-injection seam for materialization
// durability tests. The callback receives an already identity-checked open
// directory; production callers leave it nil and use File.Sync directly.
type snapshotDirectorySync func(rootPath, relative string, directory *os.File) error

func copySnapshotWithSync(ctx context.Context, source, target string, limits Limits, allocated func(os.FileInfo) error, syncDirectory snapshotDirectorySync) (Manifest, error) {
	limits = limits.Normalized()
	source, err := filepath.Abs(source)
	if err != nil {
		return Manifest{}, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return Manifest{}, err
	}
	if contains(source, target) || contains(target, source) {
		return Manifest{}, ErrUnsafePath
	}
	manifest, err := BuildManifest(ctx, source, limits)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateAncestors(filepath.Dir(target), true); err != nil {
		return Manifest{}, err
	}
	parent, parentIdentity, err := openVerifiedRoot(filepath.Dir(target))
	if err != nil {
		return Manifest{}, err
	}
	defer parent.Close()
	if err := parent.Mkdir(filepath.Base(target), 0700); err != nil {
		return Manifest{}, err
	}
	if err := revalidateRoot(filepath.Dir(target), parentIdentity); err != nil {
		return Manifest{}, err
	}
	destination, targetIdentity, err := openVerifiedRoot(target)
	if err != nil {
		return Manifest{}, err
	}
	defer destination.Close()
	if allocated != nil {
		if err := allocated(targetIdentity); err != nil {
			return Manifest{}, err
		}
	}
	if err := syncSnapshotDirectory(parent, filepath.Dir(target), parentIdentity); err != nil {
		return Manifest{}, err
	}
	input, sourceIdentity, err := openVerifiedRoot(source)
	if err != nil {
		return Manifest{}, err
	}
	defer input.Close()
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		info, err := input.Lstat(entry.Path)
		if err != nil {
			return Manifest{}, err
		}
		in, err := openRegular(input, entry.Path, info)
		if err != nil {
			return Manifest{}, err
		}
		if err := destination.MkdirAll(filepath.Dir(entry.Path), 0700); err != nil {
			in.Close()
			return Manifest{}, err
		}
		out, err := destination.OpenFile(entry.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(entry.Mode))
		if err != nil {
			in.Close()
			return Manifest{}, err
		}
		h := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(contextReader{ctx, in}, entry.Size+1))
		if copyErr == nil {
			copyErr = out.Sync()
		}
		if closeErr := out.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if closeErr := in.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return Manifest{}, copyErr
		}
		if written != entry.Size || hex.EncodeToString(h.Sum(nil)) != entry.Digest {
			return Manifest{}, ErrSourceChanged
		}
		if err := secfile.ChmodRoot(destination, entry.Path, os.FileMode(entry.Mode)); err != nil {
			return Manifest{}, err
		}
		if err := syncSnapshotFile(destination, entry.Path); err != nil {
			return Manifest{}, err
		}
	}
	if err := revalidateRoot(source, sourceIdentity); err != nil {
		return Manifest{}, err
	}
	if err := revalidateRoot(target, targetIdentity); err != nil {
		return Manifest{}, err
	}
	after, err := BuildManifest(ctx, source, limits)
	if err != nil {
		return Manifest{}, err
	}
	copied, err := BuildManifest(ctx, target, limits)
	if err != nil {
		return Manifest{}, err
	}
	if after.Digest != manifest.Digest || copied.Digest != manifest.Digest {
		return Manifest{}, ErrSourceChanged
	}
	if err := syncSnapshotDirectoryTree(destination, target, targetIdentity, syncDirectory); err != nil {
		return Manifest{}, err
	}
	if err := syncSnapshotDirectory(parent, filepath.Dir(target), parentIdentity); err != nil {
		return Manifest{}, err
	}
	return copied, nil
}

func syncSnapshotDirectoryTree(root *os.Root, rootPath string, identity os.FileInfo, syncDirectory snapshotDirectorySync) error {
	if err := revalidateRoot(rootPath, identity); err != nil {
		return err
	}
	var syncTree func(string, os.FileInfo) error
	syncTree = func(relative string, expected os.FileInfo) error {
		directory, err := root.Open(relative)
		if err != nil {
			return err
		}
		opened, err := directory.Stat()
		if err != nil || !opened.IsDir() || !os.SameFile(opened, expected) {
			directory.Close()
			if err != nil {
				return err
			}
			return ErrSourceChanged
		}
		current, err := root.Lstat(relative)
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
			directory.Close()
			if err != nil {
				return err
			}
			return ErrSourceChanged
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
			childInfo, err := root.Lstat(child)
			if err != nil {
				return err
			}
			if !childInfo.IsDir() || childInfo.Mode()&os.ModeSymlink != 0 {
				return ErrSourceChanged
			}
			if err := syncTree(child, childInfo); err != nil {
				return err
			}
		}
		directory, err = root.Open(relative)
		if err != nil {
			return err
		}
		opened, err = directory.Stat()
		current, statErr := root.Lstat(relative)
		if err != nil || statErr != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) || !os.SameFile(opened, expected) {
			directory.Close()
			if err != nil {
				return err
			}
			if statErr != nil {
				return statErr
			}
			return ErrSourceChanged
		}
		if syncDirectory == nil {
			err = directory.Sync()
		} else {
			err = syncDirectory(rootPath, relative, directory)
		}
		closeErr = directory.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	}
	if err := syncTree(".", identity); err != nil {
		return err
	}
	return revalidateRoot(rootPath, identity)
}

func syncSnapshotFile(root *os.Root, relative string) error {
	expected, err := root.Lstat(relative)
	if err != nil || !expected.Mode().IsRegular() || expected.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return err
		}
		return ErrSourceChanged
	}
	file, err := root.Open(relative)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, expected) {
		file.Close()
		if err != nil {
			return err
		}
		return ErrSourceChanged
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	current, err := root.Lstat(relative)
	if err != nil {
		return err
	}
	if !os.SameFile(expected, current) {
		return ErrSourceChanged
	}
	return nil
}

func syncSnapshotDirectory(root *os.Root, path string, identity os.FileInfo) error {
	if err := revalidateRoot(path, identity); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	opened, err := directory.Stat()
	current, statErr := root.Lstat(".")
	if err != nil || statErr != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		directory.Close()
		if err != nil {
			return err
		}
		if statErr != nil {
			return statErr
		}
		return ErrSourceChanged
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return err
	}
	if err := directory.Close(); err != nil {
		return err
	}
	return revalidateRoot(path, identity)
}
