//go:build linux

package sandbox

import (
	"context"
	"errors"
	"io"
	"net"
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	runtime := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(runtime, "network-target.txt")
	if err := os.WriteFile(target, []byte(listener.Addr().String()), 0600); err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(runtime, "sandbox.test")
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	helper, err := os.OpenFile(helperPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(helper, source)
	closeErr := errors.Join(source.Close(), helper.Close())
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatal(err)
	}
	probe := SandboxProfile{ProjectRoot: p.ProjectRoot, CandidateRoot: p.CandidateRoot, RunRoot: p.RunRoot, WorkspaceIsolation: true, WorkspaceVolumeRoot: root, Timeout: 5 * time.Second, ReadOnlyMounts: []ReadOnlyMount{{HostPath: runtime, GuestPath: "/workspace/runtime"}}}
	networkResult, err := New().RunIsolated(context.Background(), probe, []string{"/workspace/runtime/sandbox.test", "-test.run=^TestWorkspaceNetworkProbeHelper$"}, nil)
	if err != nil || networkResult.ExitCode != 0 {
		t.Fatalf("workspace command network probe result=%+v error=%v", networkResult, err)
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

func TestWorkspaceNetworkProbeHelper(t *testing.T) {
	target, err := os.ReadFile("/workspace/runtime/network-target.txt")
	if err != nil {
		t.Skip("helper is only invoked from the real workspace sandbox")
	}
	conn, err := net.DialTimeout("tcp", strings.TrimSpace(string(target)), 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("workspace command connected to host loopback")
	}
}
