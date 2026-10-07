package main

import (
	"bufio"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/appconfig"
	"stable/internal/platform/ipc"
	"stable/internal/platform/paths"
	"stable/internal/runtime"
)

func TestRemoteCLIRejectsUnknownActionAndFlags(t *testing.T) {
	if err := runRemote([]string{"restart"}, appconfig.AppConfig{}, paths.Paths{}); err == nil || !strings.Contains(err.Error(), "unknown remote command") {
		t.Fatalf("unknown action error = %v", err)
	}
	if err := runRemote([]string{"pair", "--listen", "127.0.0.1:8765"}, appconfig.AppConfig{}, paths.Paths{}); err == nil || !strings.Contains(err.Error(), "does not accept") {
		t.Fatalf("pair flag error = %v", err)
	}
	if err := runRemote([]string{"status", "unexpected"}, appconfig.AppConfig{}, paths.Paths{}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("positional argument error = %v", err)
	}
}

func TestRemoteStatusIsStoppedWhenRuntimeIsDown(t *testing.T) {
	if err := runRemote([]string{"status"}, appconfig.AppConfig{}, paths.Paths{}); err != nil {
		t.Fatalf("status with no runtime = %v", err)
	}
	if err := runRemote([]string{"down"}, appconfig.AppConfig{}, paths.Paths{}); err != nil {
		t.Fatalf("down with no runtime = %v", err)
	}
}

func TestRemoteStatusReportsSupervisorRemoteState(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	listener, err := ipc.ListenPrivate(socketPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		statusConn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		line, readErr := bufio.NewReader(statusConn).ReadString('\n')
		if readErr == nil && strings.TrimSpace(line) != "status" {
			readErr = context.Canceled
		}
		if readErr == nil {
			readErr = json.NewEncoder(statusConn).Encode(runtime.Status{Running: true, PID: 42})
		}
		_ = statusConn.Close()
		if readErr != nil {
			serverErr <- readErr
			return
		}

		remoteConn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer remoteConn.Close()
		var request runtime.RemoteControlRequest
		if decodeErr := json.NewDecoder(remoteConn).Decode(&request); decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		if request.Action != runtime.RemoteControlStatus {
			serverErr <- context.Canceled
			return
		}
		serverErr <- json.NewEncoder(remoteConn).Encode(runtime.RemoteControlResult{Running: true, ListenAddr: "127.0.0.1:8765"})
	}()

	if err := runRemote([]string{"status"}, appconfig.AppConfig{}, paths.Paths{Socket: socketPath}); err != nil {
		t.Fatalf("remote status with active runtime: %v", err)
	}
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fake supervisor did not receive remote status request")
	}
}
