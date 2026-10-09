package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestDefaultFileCountLimitAtBoundary(t *testing.T) {
	root := t.TempDir()
	limit := DefaultLimits().MaxFiles
	for i := 0; i < limit; i++ {
		path := filepath.Join(root, fmt.Sprintf("file-%05d", i))
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatalf("create empty file %d of %d: %v", i+1, limit, err)
		}
	}

	manifest, err := BuildManifest(context.Background(), root, Limits{})
	if err != nil {
		t.Fatalf("manifest with exactly the default file limit was rejected: %v", err)
	}
	if len(manifest.Entries) != limit || manifest.Bytes != 0 {
		t.Fatalf("unexpected manifest at default file limit: entries=%d bytes=%d want entries=%d bytes=0", len(manifest.Entries), manifest.Bytes, limit)
	}

	extra := filepath.Join(root, fmt.Sprintf("file-%05d", limit))
	if err := os.WriteFile(extra, nil, 0600); err != nil {
		t.Fatalf("create file one over default limit: %v", err)
	}
	if _, err := BuildManifest(context.Background(), root, Limits{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("manifest with one file over default limit did not return ErrQuota: %v", err)
	}
}
