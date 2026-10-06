package candidate

import (
	"errors"
	"testing"
)

func TestDiffManifestsRejectsUnsafeEntryPath(t *testing.T) {
	_, err := DiffManifests(t.TempDir(), t.TempDir(), []ManifestEntry{{
		Path:   "../outside.txt",
		Mode:   0600,
		Size:   1,
		Digest: "not-a-real-digest",
	}}, nil)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("DiffManifests error = %v, want ErrUnsafePath", err)
	}
}
