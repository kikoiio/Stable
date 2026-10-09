package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestManifestEntryCountLimitsAtBoundary(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "a", "a", 0600)
	fixtureFile(t, root, "b", "b", 0600)

	manifest, err := BuildManifest(context.Background(), root, Limits{MaxFiles: 2, MaxEntries: 2})
	if err != nil {
		t.Fatalf("manifest exactly at file and entry count limits was rejected: %v", err)
	}
	if len(manifest.Entries) != 2 || manifest.Bytes != 2 {
		t.Fatalf("unexpected manifest at count limits: entries=%d bytes=%d", len(manifest.Entries), manifest.Bytes)
	}

	for _, tc := range []struct {
		name   string
		limits Limits
	}{
		{name: "file count one over", limits: Limits{MaxFiles: 1}},
		{name: "filesystem entry count one over", limits: Limits{MaxEntries: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildManifest(context.Background(), root, tc.limits); !errors.Is(err, ErrQuota) {
				t.Fatalf("over-limit manifest was not rejected with ErrQuota: %v", err)
			}
		})
	}
}

func TestManifestDefaultSingleFileLimitAtBoundary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sparse.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	limit := DefaultLimits().MaxFileBytes
	if err := file.Truncate(limit); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	manifest, err := BuildManifest(context.Background(), root, Limits{})
	if err != nil {
		t.Fatalf("default-size file at the exact limit was rejected: %v", err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].Size != limit || manifest.Bytes != limit {
		t.Fatalf("unexpected manifest at default file limit: entries=%+v bytes=%d want=%d", manifest.Entries, manifest.Bytes, limit)
	}

	if err := os.Truncate(path, limit+1); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildManifest(context.Background(), root, Limits{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("default-size file one byte over limit was not rejected with ErrQuota: %v", err)
	}
}
