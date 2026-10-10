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
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
	formal := filepath.Join(root, "formal")
	privateRepo := filepath.Join(root, "private-repo.git")
	p := SandboxProfile{ProjectRoot: formal, CandidateRoot: filepath.Join(root, "checkout"), RunRoot: filepath.Join(root, "run"), WorkspaceIsolation: true, WorkspaceVolumeRoot: root, Timeout: 15 * time.Second}
	for _, path := range []string{p.ProjectRoot, p.CandidateRoot, p.RunRoot} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(formal, ".git"), filepath.Join(formal, ".stable"), filepath.Join(privateRepo, "objects"), filepath.Join(p.CandidateRoot, ".stable")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixtureFiles := map[string]string{
		filepath.Join(formal, ".git", "config"):             "formal git config",
		filepath.Join(formal, ".stable", "marker"):          "formal stable marker",
		filepath.Join(formal, "baseline.txt"):               "baseline",
		filepath.Join(privateRepo, "objects", "marker"):     "private repo object",
		filepath.Join(p.CandidateRoot, ".stable", "marker"): "candidate stable marker",
		filepath.Join(p.CandidateRoot, ".git"):              "gitdir: private repo",
	}
	for path, contents := range fixtureFiles {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// This sentinel lives on the runner's root filesystem, outside the bounded
	// ext4 volume. The real helper sees the path through the read-only root mount.
	outsideDir, err := os.MkdirTemp("/var/tmp", "m09-outside-volume-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outsideDir)
	outsideSentinel := filepath.Join(outsideDir, "sentinel")
	if err := os.WriteFile(outsideSentinel, []byte("outside bounded volume"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := BoundedWorkspaceVolume(volume, outsideDir); err == nil {
		t.Fatalf("outside sentinel unexpectedly belongs to the bounded volume: %s", outsideDir)
	}
	type protectedEntry struct {
		path string
		info os.FileInfo
		data []byte
	}
	protected := make([]protectedEntry, 0, len(fixtureFiles)+5)
	for _, path := range []string{filepath.Join(formal, ".git"), filepath.Join(formal, ".stable"), privateRepo, filepath.Join(p.CandidateRoot, ".stable")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		protected = append(protected, protectedEntry{path: path, info: info})
	}
	for path := range fixtureFiles {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		protected = append(protected, protectedEntry{path: path, info: info, data: data})
	}
	outsideInfo, err := os.Lstat(outsideSentinel)
	if err != nil {
		t.Fatal(err)
	}
	protected = append(protected, protectedEntry{path: outsideSentinel, info: outsideInfo, data: []byte("outside bounded volume")})
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
(
 if printf child-attack > /workspace/project/baseline.txt 2>/dev/null; then exit 41; fi
 if printf child-attack > /workspace/project/.git/config 2>/dev/null; then exit 42; fi
 if printf child-attack > /workspace/project/.stable/marker 2>/dev/null; then exit 43; fi
 if printf child-attack > .git 2>/dev/null; then exit 44; fi
 if mkdir .stable/child-attack 2>/dev/null; then exit 45; fi
 if printf child-attack > /workspace/private-repo.git/objects/marker 2>/dev/null; then exit 46; fi
 if printf child-attack > "$1" 2>/dev/null; then exit 47; fi
 printf 'child-process-completed\n' > child-ran
) &
child_pid=$!
wait "$child_pid"
test "$(cat child-ran)" = child-process-completed
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
	result, err := New().RunIsolated(context.Background(), p, []string{"/bin/sh", "-c", attack, "m09-volume-attack", outsideSentinel}, nil)
	if err != nil || result.ExitCode != 0 || !strings.Contains(string(result.Stdout), "quota-enforced metadata-hidden") {
		t.Fatalf("attack result=%+v error=%v", result, err)
	}
	for _, entry := range protected {
		info, err := os.Lstat(entry.path)
		if err != nil || !os.SameFile(entry.info, info) {
			t.Fatalf("protected entry identity changed at %s: before=%v after=%v err=%v", entry.path, entry.info, info, err)
		}
		if entry.data != nil {
			data, err := os.ReadFile(entry.path)
			if err != nil || string(data) != string(entry.data) {
				t.Fatalf("protected bytes changed at %s: got=%q want=%q err=%v", entry.path, data, entry.data, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(p.RunRoot, "quota.bin")); !os.IsNotExist(err) {
		t.Fatalf("quota fixture leaked file: %v", err)
	}
}

func TestWorkspaceVolumeRejectsUnsafeEntries(t *testing.T) {
	volume := os.Getenv("STABLE_M09_VOLUME")
	if volume == "" {
		t.Skip("requires disposable cloud disk volume")
	}
	if err := BoundedWorkspaceVolume(volume); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(volume, "unsafe-volume-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	var mountedTarget string
	t.Cleanup(func() {
		if mountedTarget != "" {
			if err := unix.Unmount(mountedTarget, 0); err != nil {
				t.Errorf("unmount disposable bind-mount fixture %s: %v", mountedTarget, err)
				return
			}
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove disposable volume fixture %s: %v", root, err)
		}
	})

	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	assertAccepted := func(stage string) {
		t.Helper()
		if err := BoundedWorkspaceVolume(root); err != nil {
			t.Fatalf("clean bounded volume rejected after %s: %v", stage, err)
		}
	}
	assertRejected := func(stage string) {
		t.Helper()
		if err := BoundedWorkspaceVolume(root); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("unsafe volume entry %s was accepted: %v", stage, err)
		}
	}

	link := filepath.Join(root, "symlink")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	assertRejected("symlink")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	assertAccepted("symlink removal")

	hardlink := filepath.Join(root, "hardlink")
	if err := os.Link(regular, hardlink); err != nil {
		t.Fatal(err)
	}
	assertRejected("hardlink")
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	assertAccepted("hardlink removal")

	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	assertRejected("FIFO")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	assertAccepted("FIFO removal")

	outside := filepath.Dir(root)
	if err := BoundedWorkspaceVolume(root, outside); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("path outside volume was accepted: %v", err)
	}
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	checkoutLink := filepath.Join(root, "checkout-link")
	if err := os.Symlink(checkout, checkoutLink); err != nil {
		t.Fatal(err)
	}
	if err := BoundedWorkspaceVolume(root, checkoutLink); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("symlink workspace root was accepted: %v", err)
	}
	if err := os.Remove(checkoutLink); err != nil {
		t.Fatal(err)
	}

	// A same-filesystem bind mount retains st_dev. Keep it inside this
	// disposable volume and require mount-ID validation to reject it.
	bindCheckout := filepath.Join(root, "bind-checkout")
	if err := os.MkdirAll(filepath.Join(bindCheckout, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "bind-sibling")
	if err := os.Mkdir(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "sentinel"), []byte("fixture sibling"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := BoundedWorkspaceVolume(root, bindCheckout); err != nil {
		t.Fatalf("clean checkout on disposable volume rejected: %v", err)
	}
	target := filepath.Join(bindCheckout, "nested")
	if err := unix.Mount(sibling, target, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind-mount sibling inside disposable checkout fixture: %v", err)
	}
	mountedTarget = target
	if err := BoundedWorkspaceVolume(root, bindCheckout); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("same-filesystem sibling bind mount inside checkout was accepted: %v", err)
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
