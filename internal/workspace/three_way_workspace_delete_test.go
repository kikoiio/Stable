package workspace

import "testing"

func TestThreeWayPreviewMergesWorkspaceOnlyDeletionWithIndependentFormalAddition(t *testing.T) {
	base := mergeManifest(t, mergeEntry("removed-by-workspace.txt", "base", 0644))
	formal := mergeManifest(t,
		mergeEntry("formal-addition.txt", "formal", 0644),
		mergeEntry("removed-by-workspace.txt", "base", 0644),
	)
	workspace := mergeManifest(t)

	preview, err := ThreeWayPreview(base, formal, workspace, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Conflicts) != 0 {
		t.Fatalf("workspace-only deletion conflicted with an independent formal addition: %+v", preview.Conflicts)
	}
	if len(preview.Manifest.Entries) != 1 || preview.Manifest.Entries[0] != formal.Entries[0] {
		t.Fatalf("merge=%+v; want only the independent formal addition %+v", preview.Manifest.Entries, formal.Entries[0])
	}
}
