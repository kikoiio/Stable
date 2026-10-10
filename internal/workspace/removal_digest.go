package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
)

type removalDigestEntry struct {
	path   string
	mode   uint32
	size   int64
	digest string
	dir    bool
}

// removalContentDigest hashes every entry beneath an already pinned workspace
// root. Unlike a project manifest, it includes private Git and workspace
// metadata. walkBounded limits directory reads and traverses through root.
func removalContentDigest(ctx context.Context, root *os.Root, limits Limits) (string, error) {
	limits = limits.Normalized()
	rootInfo, err := root.Stat(".")
	if err != nil || !rootInfo.IsDir() {
		return "", ErrUnsafePath
	}
	used, err := allocatedBytes(rootInfo)
	if err != nil || used > limits.MaxWorkspaceBytes {
		if err != nil {
			return "", err
		}
		return "", ErrQuota
	}
	entries := make([]removalDigestEntry, 0, 256)
	logicalBytes := int64(0)
	err = walkBounded(ctx, root, limits.MaxEntries, func(name string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		path, err := CleanRelative(name)
		if err != nil || path != name || strings.ContainsRune(path, '\x00') {
			return ErrUnsafePath
		}
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return ErrUnsafePath
		}
		allocated, err := allocatedBytes(info)
		if err != nil {
			return err
		}
		if allocated > limits.MaxWorkspaceBytes-used {
			return ErrQuota
		}
		used += allocated
		entry := removalDigestEntry{path: path, mode: uint32(info.Mode().Perm()), dir: info.IsDir()}
		if info.IsDir() {
			entries = append(entries, entry)
			return nil
		}
		// MaxFileBytes/MaxFiles constrain project manifests, not private Git or
		// metadata files. The entire pinned workspace scan is bounded by the
		// workspace byte and entry limits instead.
		if info.Size() < 0 || info.Size() > limits.MaxWorkspaceBytes-logicalBytes {
			return ErrQuota
		}
		logicalBytes += info.Size()
		if err := validateHardlinks(info); err != nil {
			return err
		}
		file, err := openRegular(root, path, info)
		if err != nil {
			return err
		}
		h := sha256.New()
		written, copyErr := io.Copy(h, io.LimitReader(contextReader{ctx, file}, info.Size()+1))
		fileInfo, statErr := file.Stat()
		current, pathErr := root.Lstat(path)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if statErr != nil {
			return statErr
		}
		if pathErr != nil {
			return pathErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written != info.Size() || fileInfo.Size() != info.Size() || fileInfo.Mode() != info.Mode() || !fileInfo.ModTime().Equal(info.ModTime()) || !os.SameFile(info, current) || current.Mode() != info.Mode() || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
			return ErrSourceChanged
		}
		if err := validateHardlinks(fileInfo); err != nil {
			return err
		}
		entry.size = written
		entry.digest = hex.EncodeToString(h.Sum(nil))
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return "", err
	}
	currentRoot, err := root.Stat(".")
	if err != nil || !os.SameFile(rootInfo, currentRoot) || !rootInfo.ModTime().Equal(currentRoot.ModTime()) {
		return "", ErrSourceChanged
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, entry := range entries {
		kind := "f"
		if entry.dir {
			kind = "d"
		}
		if _, err := fmt.Fprintf(h, "%s\x00%s\x00%o\x00%d\x00%s\n", entry.path, kind, entry.mode, entry.size, entry.digest); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
