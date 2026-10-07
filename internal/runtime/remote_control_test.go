package runtime

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"stable/internal/platform/ipc"
	"stable/internal/platform/paths"
)

func TestRemoteControlRequestJSONRoundTrip(t *testing.T) {
	want := RemoteControlRequest{
		Action:      RemoteControlStart,
		ListenAddr:  "0.0.0.0:8765",
		TLSCertFile: "/private/remote.crt",
		TLSKeyFile:  "/private/remote.key",
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got RemoteControlRequest
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("request round trip = %#v, want %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestRemoteControlRequestRejectsUnknownAndIncomplete(t *testing.T) {
	for _, request := range []RemoteControlRequest{
		{},
		{Action: "restart"},
		{Action: RemoteControlStart, TLSCertFile: "remote.crt"},
		{Action: RemoteControlStop, ListenAddr: "127.0.0.1:8765"},
	} {
		if err := request.Validate(); err == nil {
			t.Errorf("Validate(%#v) unexpectedly succeeded", request)
		}
	}
}

func TestRemoteControlReturnsStatusAndSupervisorConfigurationError(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		want := RemoteControlResult{Running: true, ListenAddr: "127.0.0.1:8765"}
		got, request := callRemoteControl(t, RemoteControlRequest{Action: RemoteControlStatus}, want)
		if got != want {
			t.Fatalf("result = %#v, want %#v", got, want)
		}
		if request.Action != RemoteControlStatus {
			t.Fatalf("request action = %q", request.Action)
		}
	})

	t.Run("configuration error", func(t *testing.T) {
		want := RemoteControlResult{ErrorCode: "invalid_config", Error: "LAN listener requires TLS certificate and key"}
		got, err, _ := callRemoteControlError(t, RemoteControlRequest{Action: RemoteControlStart}, want)
		var controlErr *RemoteControlError
		if !errors.As(err, &controlErr) {
			t.Fatalf("error = %v, want RemoteControlError", err)
		}
		if got != want || controlErr.Code != want.ErrorCode || controlErr.Message != want.Error {
			t.Fatalf("result/error = %#v / %#v, want %#v and code %q", got, controlErr, want, want.ErrorCode)
		}
	})
}

func TestRemoteControlReportsRuntimeNotRunning(t *testing.T) {
	p := paths.Paths{Socket: filepath.Join(t.TempDir(), "control.sock")}
	if _, err := RemoteControl(p, RemoteControlRequest{Action: RemoteControlStatus}); err == nil || err.Error() != "runtime is not running" {
		t.Fatalf("RemoteControl error = %v, want runtime is not running", err)
	}
}

func callRemoteControl(t *testing.T, request RemoteControlRequest, response RemoteControlResult) (RemoteControlResult, RemoteControlRequest) {
	t.Helper()
	got, err, received := callRemoteControlError(t, request, response)
	if err != nil {
		t.Fatalf("RemoteControl returned error: %v", err)
	}
	return got, received
}

func callRemoteControlError(t *testing.T, request RemoteControlRequest, response RemoteControlResult) (RemoteControlResult, error, RemoteControlRequest) {
	t.Helper()
	p := paths.Paths{Socket: filepath.Join(t.TempDir(), "control.sock")}
	listener, err := ipc.ListenPrivate(p.Socket, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	requestReceived := make(chan RemoteControlRequest, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer conn.Close()
		var received RemoteControlRequest
		if decodeErr := json.NewDecoder(conn).Decode(&received); decodeErr != nil {
			serverDone <- decodeErr
			return
		}
		requestReceived <- received
		serverDone <- json.NewEncoder(conn).Encode(response)
	}()

	got, callErr := RemoteControl(p, request)
	if serverErr := <-serverDone; serverErr != nil {
		t.Fatalf("fake supervisor: %v", serverErr)
	}
	return got, callErr, <-requestReceived
}
