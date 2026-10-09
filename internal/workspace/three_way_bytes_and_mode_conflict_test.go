package workspace

import "testing"

func TestThreeWayPreviewConflictsOnDivergentBytesAndModeAndResolvesFullEntry(t *testing.T) {
	baseEntry := mergeEntry("shared.sh", "base bytes", 0644)
	formalEntry := mergeEntry("shared.sh", "formal bytes", 0755)
	workspaceEntry := mergeEntry("shared.sh", "workspace bytes", 0600)

	preview, err := ThreeWayPreview(
		mergeManifest(t, baseEntry),
		mergeManifest(t, formalEntry),
		mergeManifest(t, workspaceEntry),
		Limits{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Conflicts) != 1 {
		t.Fatalf("conflicts=%+v; want one conflict", preview.Conflicts)
	}
	conflict := preview.Conflicts[0]
	if conflict.Path != "shared.sh" || conflict.Base == nil || *conflict.Base != baseEntry || conflict.Formal == nil || *conflict.Formal != formalEntry || conflict.Workspace == nil || *conflict.Workspace != workspaceEntry {
		t.Fatalf("conflict=%+v; want exact B/F/W entries", conflict)
	}
	if len(preview.Manifest.Entries) != 0 {
		t.Fatalf("unresolved conflicting path leaked into preview manifest: %+v", preview.Manifest.Entries)
	}

	for choice, want := range map[string]ManifestEntry{
		UseFormal:    formalEntry,
		UseWorkspace: workspaceEntry,
	} {
		resolved, err := ResolveThreeWay(preview, map[string]string{"shared.sh": choice}, Limits{})
		if err != nil {
			t.Fatalf("resolve %s: %v", choice, err)
		}
		if len(resolved.Entries) != 1 || resolved.Entries[0] != want {
			t.Fatalf("resolve %s = %+v; want complete entry %+v", choice, resolved.Entries, want)
		}
	}
}
