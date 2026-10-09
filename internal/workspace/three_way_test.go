package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func mergeEntry(path, content string, mode uint32) ManifestEntry {
	sum := sha256.Sum256([]byte(content))
	return ManifestEntry{Path: path, Mode: mode, Size: int64(len(content)), Digest: hex.EncodeToString(sum[:])}
}

func mergeManifest(t *testing.T, entries ...ManifestEntry) Manifest {
	t.Helper()
	manifest, err := makeManifest(entries, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestThreeWayPreviewSelectsOnlyUncontestedChanges(t *testing.T) {
	base := mergeManifest(t, mergeEntry("file.txt", "base", 0644))
	formal := mergeManifest(t, mergeEntry("file.txt", "formal", 0644))
	workspace := mergeManifest(t, mergeEntry("file.txt", "base", 0644))
	preview, err := ThreeWayPreview(base, formal, workspace, Limits{})
	if err != nil || len(preview.Conflicts) != 0 || len(preview.Manifest.Entries) != 1 || preview.Manifest.Entries[0] != formal.Entries[0] {
		t.Fatalf("formal-only change preview=%+v, %v", preview, err)
	}

	formal = mergeManifest(t, mergeEntry("file.txt", "base", 0644))
	workspace = mergeManifest(t, mergeEntry("file.txt", "workspace", 0644))
	preview, err = ThreeWayPreview(base, formal, workspace, Limits{})
	if err != nil || len(preview.Conflicts) != 0 || preview.Manifest.Entries[0] != workspace.Entries[0] {
		t.Fatalf("workspace-only change preview=%+v, %v", preview, err)
	}

	formal = mergeManifest(t, mergeEntry("file.txt", "same", 0755))
	workspace = mergeManifest(t, mergeEntry("file.txt", "same", 0755))
	preview, err = ThreeWayPreview(base, formal, workspace, Limits{})
	if err != nil || len(preview.Conflicts) != 0 || preview.Manifest.Entries[0] != formal.Entries[0] {
		t.Fatalf("identical change preview=%+v, %v", preview, err)
	}
}

func TestThreeWayPreviewMergesAndConflictsOnModeOnly(t *testing.T) {
	baseEntry := mergeEntry("script.sh", "same bytes", 0644)
	formalExecutable := mergeEntry("script.sh", "same bytes", 0755)
	workspacePrivate := mergeEntry("script.sh", "same bytes", 0600)

	tests := []struct {
		name         string
		formal       ManifestEntry
		workspace    ManifestEntry
		want         ManifestEntry
		wantConflict bool
	}{
		{name: "formal chmod only", formal: formalExecutable, workspace: baseEntry, want: formalExecutable},
		{name: "workspace chmod only", formal: baseEntry, workspace: formalExecutable, want: formalExecutable},
		{name: "same chmod on both sides", formal: formalExecutable, workspace: formalExecutable, want: formalExecutable},
		{name: "different chmod on both sides", formal: formalExecutable, workspace: workspacePrivate, wantConflict: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preview, err := ThreeWayPreview(
				mergeManifest(t, baseEntry),
				mergeManifest(t, test.formal),
				mergeManifest(t, test.workspace),
				Limits{},
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantConflict {
				if len(preview.Conflicts) != 1 || preview.Conflicts[0].Path != "script.sh" {
					t.Fatalf("mode-only conflict = %+v", preview.Conflicts)
				}
				resolved, err := ResolveThreeWay(preview, map[string]string{"script.sh": UseWorkspace}, Limits{})
				if err != nil {
					t.Fatal(err)
				}
				if len(resolved.Entries) != 1 || resolved.Entries[0] != test.workspace {
					t.Fatalf("workspace mode choice = %+v, want %+v", resolved.Entries, test.workspace)
				}
				resolved, err = ResolveThreeWay(preview, map[string]string{"script.sh": UseFormal}, Limits{})
				if err != nil {
					t.Fatal(err)
				}
				if len(resolved.Entries) != 1 || resolved.Entries[0] != test.formal {
					t.Fatalf("formal mode choice = %+v, want %+v", resolved.Entries, test.formal)
				}
				return
			}
			if len(preview.Conflicts) != 0 || len(preview.Manifest.Entries) != 1 || preview.Manifest.Entries[0] != test.want {
				t.Fatalf("mode merge = manifest %+v conflicts %+v; want %+v without conflict", preview.Manifest.Entries, preview.Conflicts, test.want)
			}
		})
	}
}

func TestThreeWayPreviewHandlesAddedDeletedAndRenamedPaths(t *testing.T) {
	baseFile := mergeEntry("old.txt", "base", 0644)
	formalAdd := mergeEntry("new.txt", "formal", 0644)
	workspaceAdd := mergeEntry("new.txt", "workspace", 0644)
	renamed := mergeEntry("renamed.txt", "base", 0644)

	tests := []struct {
		name      string
		base      []ManifestEntry
		formal    []ManifestEntry
		workspace []ManifestEntry
		conflicts []string
		merged    []ManifestEntry
	}{
		{name: "formal add", formal: []ManifestEntry{formalAdd}, merged: []ManifestEntry{formalAdd}},
		{name: "workspace add", workspace: []ManifestEntry{formalAdd}, merged: []ManifestEntry{formalAdd}},
		{name: "same add", formal: []ManifestEntry{formalAdd}, workspace: []ManifestEntry{formalAdd}, merged: []ManifestEntry{formalAdd}},
		{name: "conflicting add", formal: []ManifestEntry{formalAdd}, workspace: []ManifestEntry{workspaceAdd}, conflicts: []string{"new.txt"}},
		{name: "both delete", base: []ManifestEntry{baseFile}},
		{name: "formal delete", base: []ManifestEntry{baseFile}, workspace: []ManifestEntry{baseFile}},
		{name: "delete against edit", base: []ManifestEntry{baseFile}, workspace: []ManifestEntry{mergeEntry("old.txt", "workspace", 0644)}, conflicts: []string{"old.txt"}},
		{name: "rename is delete and add", base: []ManifestEntry{baseFile}, formal: []ManifestEntry{baseFile}, workspace: []ManifestEntry{renamed}, merged: []ManifestEntry{renamed}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preview, err := ThreeWayPreview(
				mergeManifest(t, test.base...),
				mergeManifest(t, test.formal...),
				mergeManifest(t, test.workspace...),
				Limits{},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(preview.Conflicts) != len(test.conflicts) {
				t.Fatalf("conflicts=%+v, want paths %v", preview.Conflicts, test.conflicts)
			}
			for i, conflict := range preview.Conflicts {
				if conflict.Path != test.conflicts[i] {
					t.Fatalf("conflict[%d]=%q, want %q", i, conflict.Path, test.conflicts[i])
				}
			}
			if len(preview.Manifest.Entries) != len(test.merged) {
				t.Fatalf("merged entries=%+v, want %+v", preview.Manifest.Entries, test.merged)
			}
			for i, entry := range preview.Manifest.Entries {
				if entry != test.merged[i] {
					t.Fatalf("merged entry[%d]=%+v, want %+v", i, entry, test.merged[i])
				}
			}
		})
	}
}

func TestThreeWayConflictResolutionBindsExactPathsAndModes(t *testing.T) {
	base := mergeManifest(t, mergeEntry("file.txt", "base", 0644), mergeEntry("delete.txt", "base", 0644))
	formal := mergeManifest(t, mergeEntry("file.txt", "formal", 0644), mergeEntry("delete.txt", "modified", 0644))
	workspace := mergeManifest(t, mergeEntry("file.txt", "workspace", 0755)) // delete.txt removed
	preview, err := ThreeWayPreview(base, formal, workspace, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Conflicts) != 2 || preview.Conflicts[0].Path != "delete.txt" || preview.Conflicts[1].Path != "file.txt" {
		t.Fatalf("conflicts=%+v", preview.Conflicts)
	}
	resolvedWorkspace, err := ResolveThreeWay(preview, map[string]string{"delete.txt": UseWorkspace, "file.txt": UseWorkspace}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolvedWorkspace.Entries) != 1 || resolvedWorkspace.Entries[0] != workspace.Entries[0] {
		t.Fatalf("workspace resolution=%+v", resolvedWorkspace.Entries)
	}
	resolvedFormal, err := ResolveThreeWay(preview, map[string]string{"delete.txt": UseFormal, "file.txt": UseFormal}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolvedFormal.Entries) != 2 || resolvedFormal.Entries[0] != formal.Entries[0] || resolvedFormal.Entries[1] != formal.Entries[1] {
		t.Fatalf("formal resolution=%+v", resolvedFormal.Entries)
	}
	for _, choices := range []map[string]string{
		{"file.txt": UseWorkspace},
		{"delete.txt": UseWorkspace, "file.txt": UseFormal, "unrelated.txt": UseWorkspace},
		{"delete.txt": "force", "file.txt": UseFormal},
	} {
		if _, err := ResolveThreeWay(preview, choices, Limits{}); err == nil {
			t.Fatalf("invalid or incomplete resolution accepted: %#v", choices)
		}
	}
}
