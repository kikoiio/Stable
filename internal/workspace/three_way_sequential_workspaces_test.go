package workspace

import "testing"

func TestThreeWayPreviewKeepsEarlierAcceptedChangeWhenExportingOlderWorkspace(t *testing.T) {
	base := mergeManifest(t,
		mergeEntry("first.txt", "original first", 0644),
		mergeEntry("second.txt", "original second", 0644),
	)

	// Workspace one was exported and accepted. Workspace two was created from
	// the same original baseline, so its checkout does not contain first.txt's
	// accepted value.
	formalAfterFirstAcceptance := mergeManifest(t,
		mergeEntry("first.txt", "accepted from workspace one", 0644),
		mergeEntry("second.txt", "original second", 0644),
	)
	olderWorkspaceWithIndependentEdit := mergeManifest(t,
		mergeEntry("first.txt", "original first", 0644),
		mergeEntry("second.txt", "edited by workspace two", 0644),
	)

	preview, err := ThreeWayPreview(base, formalAfterFirstAcceptance, olderWorkspaceWithIndependentEdit, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Conflicts) != 0 {
		t.Fatalf("independent edits from sequential workspaces conflicted: %+v", preview.Conflicts)
	}
	want := mergeManifest(t,
		mergeEntry("first.txt", "accepted from workspace one", 0644),
		mergeEntry("second.txt", "edited by workspace two", 0644),
	)
	if len(preview.Manifest.Entries) != len(want.Entries) {
		t.Fatalf("merged entries=%+v, want %+v", preview.Manifest.Entries, want.Entries)
	}
	for i := range want.Entries {
		if preview.Manifest.Entries[i] != want.Entries[i] {
			t.Fatalf("merged entry[%d]=%+v, want %+v", i, preview.Manifest.Entries[i], want.Entries[i])
		}
	}
}
