package workspace

import (
	"errors"
	"sort"
	"strings"
)

const (
	UseWorkspace = "use_workspace"
	UseFormal    = "use_formal"
)

type MergeConflict struct {
	Path      string         `json:"path"`
	Base      *ManifestEntry `json:"base,omitempty"`
	Formal    *ManifestEntry `json:"formal,omitempty"`
	Workspace *ManifestEntry `json:"workspace,omitempty"`
}

type MergePreview struct {
	BaselineDigest  string          `json:"baseline_digest"`
	FormalDigest    string          `json:"formal_digest"`
	WorkspaceDigest string          `json:"workspace_digest"`
	Manifest        Manifest        `json:"manifest"`
	Conflicts       []MergeConflict `json:"conflicts,omitempty"`
}

// ThreeWayPreview performs a path-level B/F/W merge. It never reads file
// contents or writes a candidate; callers must revalidate all three manifests
// before materializing the returned project data.
func ThreeWayPreview(base, formal, workspace Manifest, limits Limits) (MergePreview, error) {
	var preview MergePreview
	limits = limits.Normalized()
	baseMap, baseDigest, err := validateMergeManifest(base, limits)
	if err != nil {
		return preview, err
	}
	formalMap, formalDigest, err := validateMergeManifest(formal, limits)
	if err != nil {
		return preview, err
	}
	workspaceMap, workspaceDigest, err := validateMergeManifest(workspace, limits)
	if err != nil {
		return preview, err
	}
	preview.BaselineDigest, preview.FormalDigest, preview.WorkspaceDigest = baseDigest, formalDigest, workspaceDigest
	paths := make(map[string]bool, len(baseMap)+len(formalMap)+len(workspaceMap))
	for path := range baseMap {
		paths[path] = true
	}
	for path := range formalMap {
		paths[path] = true
	}
	for path := range workspaceMap {
		paths[path] = true
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	merged := make([]ManifestEntry, 0, len(ordered))
	for _, path := range ordered {
		b, f, w := baseMap[path], formalMap[path], workspaceMap[path]
		switch {
		case sameEntry(f, b):
			if w != nil {
				merged = append(merged, *w)
			}
		case sameEntry(w, b):
			if f != nil {
				merged = append(merged, *f)
			}
		case sameEntry(f, w):
			if f != nil {
				merged = append(merged, *f)
			}
		default:
			preview.Conflicts = append(preview.Conflicts, MergeConflict{Path: path, Base: cloneEntry(b), Formal: cloneEntry(f), Workspace: cloneEntry(w)})
		}
	}
	preview.Manifest, err = makeManifest(merged, limits)
	if err != nil {
		return MergePreview{}, err
	}
	return preview, nil
}

// ResolveThreeWay requires exactly one explicit choice for every conflict in
// the preview. Choices are bound to the preview's B/F/W digests by the caller's
// durable resolution record and must be checked again before export.
func ResolveThreeWay(preview MergePreview, choices map[string]string, limits Limits) (Manifest, error) {
	if preview.BaselineDigest == "" || preview.FormalDigest == "" || preview.WorkspaceDigest == "" {
		return Manifest{}, errors.New("merge preview has no bound input digests")
	}
	conflictByPath := make(map[string]MergeConflict, len(preview.Conflicts))
	for _, conflict := range preview.Conflicts {
		if _, exists := conflictByPath[conflict.Path]; exists {
			return Manifest{}, ErrUnsafePath
		}
		conflictByPath[conflict.Path] = conflict
	}
	if len(choices) != len(conflictByPath) {
		return Manifest{}, errors.New("every merge conflict needs one explicit choice")
	}
	entries := append([]ManifestEntry(nil), preview.Manifest.Entries...)
	for path, choice := range choices {
		conflict, ok := conflictByPath[path]
		if !ok {
			return Manifest{}, errors.New("resolution contains an unrelated path")
		}
		var selected *ManifestEntry
		switch choice {
		case UseWorkspace:
			selected = conflict.Workspace
		case UseFormal:
			selected = conflict.Formal
		default:
			return Manifest{}, errors.New("invalid merge conflict choice")
		}
		if selected != nil {
			entries = append(entries, *selected)
		}
	}
	return makeManifest(entries, limits)
}

func validateMergeManifest(manifest Manifest, limits Limits) (map[string]*ManifestEntry, string, error) {
	if manifest.PolicyVersion != ManifestPolicy || len(manifest.Entries) > limits.MaxFiles || manifest.Bytes < 0 || manifest.Bytes > limits.MaxSnapshotBytes {
		return nil, "", ErrQuota
	}
	entries := make(map[string]*ManifestEntry, len(manifest.Entries))
	var size int64
	for i := range manifest.Entries {
		entry := manifest.Entries[i]
		if entry.Size > limits.MaxFileBytes {
			return nil, "", ErrQuota
		}
		if _, exists := entries[entry.Path]; exists {
			return nil, "", ErrUnsafePath
		}
		copy := entry
		entries[entry.Path] = &copy
		size += entry.Size
		if size > limits.MaxSnapshotBytes {
			return nil, "", ErrQuota
		}
	}
	digest, err := ManifestDigest(manifest.Entries)
	if err != nil {
		return nil, "", err
	}
	if manifest.Digest != "" && manifest.Digest != digest {
		return nil, "", ErrSourceChanged
	}
	if manifest.Bytes != size {
		return nil, "", ErrSourceChanged
	}
	return entries, digest, nil
}

func makeManifest(entries []ManifestEntry, limits Limits) (Manifest, error) {
	limits = limits.Normalized()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if len(entries) > limits.MaxFiles {
		return Manifest{}, ErrQuota
	}
	var size int64
	seen := make(map[string]bool, len(entries))
	for i := range entries {
		path := entries[i].Path
		if seen[path] {
			return Manifest{}, ErrUnsafePath
		}
		for parent := path; ; {
			index := strings.LastIndexByte(parent, '/')
			if index < 0 {
				break
			}
			parent = parent[:index]
			if seen[parent] {
				return Manifest{}, ErrUnsafePath
			}
		}
		seen[path] = true
		size += entries[i].Size
		if entries[i].Size > limits.MaxFileBytes || size > limits.MaxSnapshotBytes {
			return Manifest{}, ErrQuota
		}
	}
	digest, err := ManifestDigest(entries)
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{PolicyVersion: ManifestPolicy, Entries: entries, Digest: digest, Bytes: size}, nil
}

func sameEntry(a, b *ManifestEntry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func cloneEntry(entry *ManifestEntry) *ManifestEntry {
	if entry == nil {
		return nil
	}
	copy := *entry
	return &copy
}
