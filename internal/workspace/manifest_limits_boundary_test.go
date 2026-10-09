package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestManifestByteLimitsAtBoundary(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "a", "1234", 0600)
	fixtureFile(t, root, "b", "5678", 0600)

	manifest, err := BuildManifest(context.Background(), root, Limits{MaxFileBytes: 4, MaxSnapshotBytes: 8})
	if err != nil {
		t.Fatalf("manifest exactly at file and snapshot limits was rejected: %v", err)
	}
	if manifest.Bytes != 8 || len(manifest.Entries) != 2 {
		t.Fatalf("unexpected manifest at byte limits: bytes=%d entries=%d", manifest.Bytes, len(manifest.Entries))
	}

	for _, tc := range []struct {
		name   string
		limits Limits
	}{
		{name: "single file one byte over", limits: Limits{MaxFileBytes: 3}},
		{name: "cumulative snapshot one byte over", limits: Limits{MaxSnapshotBytes: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildManifest(context.Background(), root, tc.limits); !errors.Is(err, ErrQuota) {
				t.Fatalf("over-limit manifest was not rejected with ErrQuota: %v", err)
			}
		})
	}
}
