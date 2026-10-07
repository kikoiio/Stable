//go:build linux

package candidate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetadataMoveNeverReplacesConcurrentDestination(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "git_pointer", true: "git_directory"}[directory], func(t *testing.T) {
			source, destination := filepath.Join(t.TempDir(), ".git"), filepath.Join(t.TempDir(), ".git")
			for _, path := range []string{source, destination} {
				if directory {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(path, []byte(path), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			sourceInfo, err := os.Lstat(source)
			if err != nil {
				t.Fatal(err)
			}
			destinationInfo, err := os.Lstat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if err := moveMetadataNoReplace(source, destination); err == nil {
				t.Fatal("existing destination was replaced")
			}
			for path, info := range map[string]os.FileInfo{source: sourceInfo, destination: destinationInfo} {
				got, err := os.Lstat(path)
				if err != nil || !os.SameFile(info, got) {
					t.Fatalf("identity at %s changed: %v", path, err)
				}
			}
		})
	}
}
