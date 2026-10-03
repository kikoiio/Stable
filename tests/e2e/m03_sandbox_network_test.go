package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/permission"
	"stable/internal/sandbox"
	"stable/internal/sessionlog"
)

func TestM03SandboxNetwork(t *testing.T) {
	project := requiredEnv(t, "M03_PROJECT")
	candidate := requiredEnv(t, "M03_CANDIDATE")
	runDir := requiredEnv(t, "M03_RUN_DIR")
	helper := requiredEnv(t, "M03_PROXY_HELPER")
	sentinel := requiredEnv(t, "M03_SENTINEL")
	hostSecret := requiredEnv(t, "M03_HOST_SECRET")
	modelSecret := requiredEnv(t, "M03_SECRET")
	sessionRoot := requiredEnv(t, "M03_SESSION_ROOT")
	secrets := []string{modelSecret, string(readFixture(t, sentinel)), string(readFixture(t, hostSecret))}
	manager := sandbox.LinuxManager{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probeSecretBoundary(t, ctx, manager, sandbox.SandboxProfile{
		ProjectRoot: project, CandidateRoot: candidate, RunRoot: runDir,
		Timeout: 15 * time.Second, OutputLimit: 16 << 10,
	}, sentinel, hostSecret, secrets)
	probeSessionLog(t, sessionRoot, secrets)
	assertNoSecretMarkersInTree(t, secrets, filepath.Join(project, ".stable", "sessions"))

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start approved-target fixture: %v", err)
	}
	defer target.Close()
	port := target.Addr().(*net.TCPAddr).Port
	targetResponse := make(chan struct{}, 1)
	go func() {
		for {
			conn, acceptErr := target.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
				targetResponse <- struct{}{}
			}()
		}
	}()

	profile := sandbox.SandboxProfile{
		ProjectRoot: project, CandidateRoot: candidate, RunRoot: runDir,
		Timeout: 15 * time.Second, OutputLimit: 4096,
	}
	probeResult, err := manager.RunIsolated(ctx, profile, []string{"python3", "-c", `import socket,sys
s=socket.socket(); s.settimeout(1)
try:
 s.connect(("127.0.0.1", int(sys.argv[1])))
 print("DIRECT_CONNECTED")
except OSError:
 print("DIRECT_BLOCKED")
`, fmt.Sprint(port)}, nil)
	if err != nil {
		t.Fatalf("default-deny isolation run failed (unavailable is not a pass): %v", err)
	}
	assertNoSecretMarkers(t, secrets, string(probeResult.Stdout), string(probeResult.Stderr), errorString(err))
	probeOutput := string(probeResult.Stdout)
	if probeResult.ExitCode != 0 || !strings.Contains(probeOutput, "DIRECT_BLOCKED") || strings.Contains(probeOutput, "DIRECT_CONNECTED") {
		t.Fatalf("default network access was not denied: exit=%d output=%q stderr=%q", probeResult.ExitCode, probeOutput, probeResult.Stderr)
	}

	grant := permission.NetworkGrant{Protocol: "tcp", Host: "127.0.0.1", Port: uint16(port), ResolvedIPs: []string{"127.0.0.1"}}
	profile.NetworkGrants = []permission.NetworkGrant{grant}
	profile.ProxyHelperPath = helper
	payload := "m03-approved-network"
	clientScript := `import os,socket,sys,urllib.parse
proxy=urllib.parse.urlparse(os.environ["HTTP_PROXY"])
s=socket.create_connection((proxy.hostname, proxy.port), timeout=2)
s.sendall(("CONNECT 127.0.0.1:"+sys.argv[1]+" HTTP/1.1\r\nHost: 127.0.0.1:"+sys.argv[1]+"\r\n\r\n").encode())
response=b""
while b"\r\n\r\n" not in response: response+=s.recv(4096)
if b"200 Connection Established" not in response: raise SystemExit(10)
s.sendall(sys.argv[2].encode())
if s.recv(len(sys.argv[2])) != sys.argv[2].encode(): raise SystemExit(11)
print("APPROVED_CONNECTED")
`
	result, err := manager.RunIsolated(ctx, profile, []string{"python3", "-c", clientScript, fmt.Sprint(port), payload}, nil)
	assertNoSecretMarkers(t, secrets, string(result.Stdout), string(result.Stderr), errorString(err))
	assertNoSecretMarkersInTree(t, secrets, candidate, runDir, filepath.Join(filepath.Dir(runDir), ".stable-sessions", "logs"), filepath.Join(project, ".stable", "sessions"))
	if err != nil {
		t.Fatalf("approved proxy request failed: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(string(result.Stdout), "APPROVED_CONNECTED") {
		t.Fatalf("approved target was not reachable: exit=%d output=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	select {
	case <-targetResponse:
	case <-time.After(2 * time.Second):
		t.Fatal("approved listener did not receive the request")
	}

	wrongPort := port + 1
	if wrongPort == 0 {
		wrongPort = 1
	}
	deniedResult, err := manager.RunIsolated(ctx, profile, []string{"python3", "-c", clientScript, fmt.Sprint(wrongPort), payload}, nil)
	assertNoSecretMarkers(t, secrets, string(deniedResult.Stdout), string(deniedResult.Stderr), errorString(err))
	if err != nil {
		t.Fatalf("changed-port denial run failed: %v", err)
	}
	if deniedResult.ExitCode == 0 || strings.Contains(string(deniedResult.Stdout), "APPROVED_CONNECTED") {
		t.Fatalf("unapproved port was reachable: exit=%d output=%q", deniedResult.ExitCode, deniedResult.Stdout)
	}
	assertNoSecretMarkersInTree(t, secrets, candidate, runDir, filepath.Join(filepath.Dir(runDir), ".stable-sessions", "logs"), filepath.Join(project, ".stable", "sessions"))
}

func probeSessionLog(t *testing.T, root string, secrets []string) {
	t.Helper()
	session, err := sessionlog.Create(root, "m03-secret-scan")
	if err != nil {
		t.Fatalf("create private session log: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err = sessionlog.Append(root, session.ID, sessionlog.EventActivity, map[string]string{"status": "probe-completed"}); err != nil {
			t.Fatalf("append private session log: %v", err)
		}
	}
	dir, err := sessionlog.SessionDir(root)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecretMarkersInTree(t, secrets, dir)
}

func probeSecretBoundary(t *testing.T, ctx context.Context, manager sandbox.LinuxManager, profile sandbox.SandboxProfile, sentinel, hostSecret string, secrets []string) {
	t.Helper()
	probe := `import os,sys
print("\\n".join(os.environ.values()))
print(open("/proc/self/mountinfo").read())
for path in (sys.argv[1], sys.argv[2]):
 try: print(open(path).read())
 except OSError: pass
`
	result, err := manager.RunIsolated(ctx, profile, []string{"python3", "-c", probe, sentinel, hostSecret}, nil)
	assertNoSecretMarkers(t, secrets, string(result.Stdout), string(result.Stderr), errorString(err))
	if err != nil {
		t.Fatalf("secret-boundary probe failed (isolation unavailable is not a pass): %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("secret-boundary probe exited %d", result.ExitCode)
	}
	assertNoSecretMarkersInTree(t, secrets, profile.ProjectRoot, profile.CandidateRoot, profile.RunRoot, filepath.Join(filepath.Dir(profile.RunRoot), ".stable-sessions", "logs"), filepath.Join(profile.ProjectRoot, ".stable", "sessions"))
}

func assertNoSecretMarkers(t *testing.T, secrets []string, outputs ...string) {
	t.Helper()
	for _, output := range outputs {
		for _, secret := range secrets {
			if marker := strings.TrimSpace(secret); marker != "" && strings.Contains(output, marker) {
				t.Fatal("fake secret marker was present in sandbox output or error")
			}
		}
	}
}

func assertNoSecretMarkersInTree(t *testing.T, secrets []string, roots ...string) {
	t.Helper()
	for _, root := range roots {
		if _, statErr := os.Lstat(root); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			t.Fatalf("inspect persistent record root: %v", statErr)
		}
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
				return fmt.Errorf("unexpected non-regular record")
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, secret := range secrets {
				if marker := strings.TrimSpace(secret); marker != "" && strings.Contains(string(data), marker) {
					return fmt.Errorf("fake secret marker found in persistent sandbox record")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan sandbox persistent records: %v", err)
		}
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
