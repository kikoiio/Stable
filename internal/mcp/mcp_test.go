package mcp

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"stable/internal/appconfig"
)

func TestManagerStdioFixtureLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stdio fixture build command is Unix-shaped")
	}
	bin := filepath.Join(t.TempDir(), "mcp-fixture")
	if output, err := exec.Command("go", "build", "-o", bin, "./mcptest").CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	manager := NewManager([]appconfig.MCPServerConfig{{Name: "fixture", Command: bin}}, "", ManagerOptions{ConnectTimeout: 5 * time.Second})
	if err := manager.ConnectAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()
	status := manager.Status()
	if len(status) != 1 || status[0].State != "connected" || status[0].ToolCount != 1 {
		t.Fatalf("status=%+v", status)
	}
	if got := manager.Instructions(); got != "" {
		t.Fatalf("unexpected instructions without configuration: %q", got)
	}
	if _, _, err := manager.ResolveTarget("mcp__fixture__echo"); err != nil {
		t.Fatal(err)
	}
	output, isError, err := manager.CallTool(context.Background(), "fixture", "echo", map[string]any{"text": "hello"})
	if err != nil || isError || output != "hello" {
		t.Fatalf("call output=%q isError=%v err=%v", output, isError, err)
	}
	manager.Shutdown()
	if got := manager.Status()[0].State; got != "disconnected" {
		t.Fatalf("shutdown state=%q", got)
	}
}
