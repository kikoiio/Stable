package workspace

import "testing"

func TestThreeWayPreviewConflictsWhenDeletionMeetsModeOnlyChange(t *testing.T) {
	baseEntry := mergeEntry("script.sh", "same bytes", 0644)
	executable := mergeEntry("script.sh", "same bytes", 0755)
	tests := []struct {
		name       string
		formal     []ManifestEntry
		workspace  []ManifestEntry
		deleteSide string
		keepSide   string
	}{
		{
			name:       "formal deletion versus workspace chmod",
			formal:     nil,
			workspace:  []ManifestEntry{executable},
			deleteSide: UseFormal,
			keepSide:   UseWorkspace,
		},
		{
			name:       "workspace deletion versus formal chmod",
			formal:     []ManifestEntry{executable},
			workspace:  nil,
			deleteSide: UseWorkspace,
			keepSide:   UseFormal,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preview, err := ThreeWayPreview(
				mergeManifest(t, baseEntry),
				mergeManifest(t, test.formal...),
				mergeManifest(t, test.workspace...),
				Limits{},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(preview.Conflicts) != 1 {
				t.Fatalf("delete/mode preview conflicts=%+v; want one", preview.Conflicts)
			}
			conflict := preview.Conflicts[0]
			if conflict.Path != "script.sh" || conflict.Base == nil || *conflict.Base != baseEntry {
				t.Fatalf("delete/mode conflict details=%+v; want exact baseline entry", conflict)
			}
			if conflict.Formal == nil && len(test.formal) != 0 || conflict.Formal != nil && (len(test.formal) == 0 || *conflict.Formal != test.formal[0]) {
				t.Fatalf("formal conflict entry=%+v, want %v", conflict.Formal, test.formal)
			}
			if conflict.Workspace == nil && len(test.workspace) != 0 || conflict.Workspace != nil && (len(test.workspace) == 0 || *conflict.Workspace != test.workspace[0]) {
				t.Fatalf("workspace conflict entry=%+v, want %v", conflict.Workspace, test.workspace)
			}
			for choice, want := range map[string][]ManifestEntry{
				test.deleteSide: nil,
				test.keepSide:   {executable},
			} {
				resolved, err := ResolveThreeWay(preview, map[string]string{"script.sh": choice}, Limits{})
				if err != nil {
					t.Fatalf("resolve %s: %v", choice, err)
				}
				if len(resolved.Entries) != len(want) {
					t.Fatalf("resolve %s entries=%+v; want %+v", choice, resolved.Entries, want)
				}
				if len(want) == 1 && resolved.Entries[0] != want[0] {
					t.Fatalf("resolve %s entry=%+v; want exact mode entry %+v", choice, resolved.Entries[0], want[0])
				}
			}
		})
	}
}
