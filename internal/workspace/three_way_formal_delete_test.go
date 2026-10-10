package workspace

import "testing"

func TestThreeWayPreviewMergesFormalOnlyDeletionWithIndependentWorkspaceAddition(t *testing.T) {
	base := mergeManifest(t, mergeEntry("removed-by-formal.txt", "base", 0644))
	formal := mergeManifest(t)
	workspaceAddition := mergeEntry("workspace-addition.txt", "workspace", 0644)
	workspace := mergeManifest(t,
		workspaceAddition,
		mergeEntry("removed-by-formal.txt", "base", 0644),
	)

	preview, err := ThreeWayPreview(base, formal, workspace, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Conflicts) != 0 {
		t.Fatalf("formal-only deletion conflicted with an independent workspace addition: %+v", preview.Conflicts)
	}
	if len(preview.Manifest.Entries) != 1 || preview.Manifest.Entries[0] != workspaceAddition {
		t.Fatalf("merge=%+v; want only the independent workspace addition %+v", preview.Manifest.Entries, workspaceAddition)
	}
}
