//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceRealVolumeQuotaAndMetadataAttacks(t *testing.T) {
	volume := os.Getenv("STABLE_M09_VOLUME")
	if volume == "" {
		t.Skip("requires disposable cloud disk volume")
	}
	if err := BoundedWorkspaceVolume(volume); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(volume, "quota-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	p := SandboxProfile{ProjectRoot: filepath.Join(root, "baseline"), CandidateRoot: filepath.Join(root, "checkout"), RunRoot: filepath.Join(root, "run"), WorkspaceIsolation: true, WorkspaceVolumeRoot: root, Timeout: 15 * time.Second}
	for _, path := range []string{p.ProjectRoot, p.CandidateRoot, p.RunRoot} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(p.CandidateRoot, ".git"), []byte("gitdir: private repo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.ProjectRoot, "baseline.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	attack := `set -eu
 test ! -s .git
 if printf attack > .git 2>/dev/null; then exit 31; fi
 if mkdir .stable/attack 2>/dev/null; then exit 32; fi
 if mkdir .mewcode/attack 2>/dev/null; then exit 33; fi
 if touch /outside-volume 2>/dev/null; then exit 34; fi
 if touch /home/outside-volume 2>/dev/null; then exit 35; fi
 if touch /dev/outside-volume 2>/dev/null; then exit 36; fi
 if ln /workspace/project/baseline.txt hardlink-attack 2>/dev/null; then rm hardlink-attack; exit 39; fi
 ln -s /workspace/repo.git escaped
 if printf attack > escaped/objects 2>/dev/null; then exit 37; fi
 rm escaped
 if dd if=/dev/zero of=/tmp/quota.bin bs=1048576 count=128 2>/dev/null; then rm -f /tmp/quota.bin; exit 38; fi
 size=$(stat -c %s /tmp/quota.bin)
 rm -f /tmp/quota.bin
 test "$size" -lt 134217728
 printf 'quota-enforced metadata-hidden\n'
 `
	result, err := New().RunIsolated(context.Background(), p, []string{"/bin/sh", "-c", attack}, nil)
	if err != nil || result.ExitCode != 0 || !strings.Contains(string(result.Stdout), "quota-enforced metadata-hidden") {
		t.Fatalf("attack result=%+v error=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(p.CandidateRoot, ".git"))
	if err != nil || string(data) != "gitdir: private repo" {
		t.Fatalf("private git mask changed host: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(p.RunRoot, "quota.bin")); !os.IsNotExist(err) {
		t.Fatalf("quota fixture leaked file: %v", err)
	}
}
