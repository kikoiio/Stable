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
