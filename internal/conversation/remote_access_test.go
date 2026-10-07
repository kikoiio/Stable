package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoteAccessRequestCanonicalRootAndApproval(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	registry := NewRemoteAccessRegistry()
	result := make(chan struct {
		grant RemoteGrant
		err   error
	}, 1)
	go func() {
		grant, err := registry.Request(context.Background(), "connection-1", "Laptop browser", link)
		result <- struct {
			grant RemoteGrant
			err   error
		}{grant, err}
	}()

	request := waitRemoteAccessRequest(t, registry)
	if request.ProjectRoot != root || request.ClientLabel != "Laptop browser" {
		t.Fatalf("request = %#v, want canonical root %q and client label", request, root)
	}
	if !request.ExpiresAt.After(request.CreatedAt) || request.ExpiresAt.Sub(request.CreatedAt) != remoteAccessRequestTTL {
		t.Fatalf("request deadline = %s..%s, want %s TTL", request.CreatedAt, request.ExpiresAt, remoteAccessRequestTTL)
	}
	if err := registry.Resolve(request.ID, "approve"); err != nil {
		t.Fatalf("Resolve(approve): %v", err)
	}
	got := <-result
	if got.err != nil {
		t.Fatalf("Request() error = %v", got.err)
	}
	if got.grant.ProjectRoot != root || got.grant.ConnectionID != "connection-1" {
		t.Fatalf("grant = %#v, want connection-bound canonical root", got.grant)
	}
	if got.grant.ID == "" || !got.grant.ExpiresAt.After(time.Now()) {
		t.Fatalf("grant is missing an opaque ID or future expiry: %#v", got.grant)
	}
	if _, err := registry.Grant(got.grant.ID, "connection-2"); !errors.Is(err, ErrRemoteGrantInvalid) {
		t.Fatalf("Grant from other connection error = %v", err)
	}
	if _, err := registry.Grant(got.grant.ID, "connection-1"); err != nil {
		t.Fatalf("Grant from owner: %v", err)
	}
	if err := registry.Release(got.grant.ID, "connection-2"); !errors.Is(err, ErrRemoteGrantInvalid) {
		t.Fatalf("Release from other connection error = %v", err)
	}
	if err := registry.Release(got.grant.ID, "connection-1"); err != nil {
		t.Fatalf("Release from owner: %v", err)
	}
	if _, err := registry.Grant(got.grant.ID, "connection-1"); !errors.Is(err, ErrRemoteGrantInvalid) {
		t.Fatalf("Grant after release error = %v", err)
	}
}

func TestRemoteAccessDenyAndDuplicateResolve(t *testing.T) {
	registry := NewRemoteAccessRegistry()
	result := requestRemoteAccessAsync(registry, t.TempDir())
	request := waitRemoteAccessRequest(t, registry)
	if err := registry.Resolve(request.ID, "deny"); err != nil {
		t.Fatalf("Resolve(deny): %v", err)
	}
	if got := <-result; !errors.Is(got.err, ErrRemoteAccessDenied) {
		t.Fatalf("Request error = %v, want denied", got.err)
	}
	if err := registry.Resolve(request.ID, "approve"); !errors.Is(err, ErrRemoteAccessExpired) {
		t.Fatalf("duplicate Resolve error = %v, want expired/unknown", err)
	}
}

func TestRemoteAccessTimeoutFailsClosed(t *testing.T) {
	registry := newRemoteAccessRegistry(25*time.Millisecond, time.Hour, time.Now)
	result := requestRemoteAccessAsync(registry, t.TempDir())
	if got := <-result; !errors.Is(got.err, ErrRemoteAccessExpired) {
		t.Fatalf("Request error = %v, want timeout", got.err)
	}
	if pending := registry.Pending(); len(pending) != 0 {
		t.Fatalf("pending requests after timeout = %d, want 0", len(pending))
	}
}

func TestRemoteAccessCancelWakesOnlyOwningConnection(t *testing.T) {
	registry := NewRemoteAccessRegistry()
	root := t.TempDir()
	result := make(chan struct {
		grant RemoteGrant
		err   error
	}, 1)
	go func() {
		grant, err := registry.RequestWithID(context.Background(), "request-1", "connection-1", "Browser", root)
		result <- struct {
			grant RemoteGrant
			err   error
		}{grant, err}
	}()
	request := waitRemoteAccessRequest(t, registry)
	if err := registry.CancelRequest(request.ID, "connection-2"); !errors.Is(err, ErrRemoteAccessExpired) {
		t.Fatalf("CancelRequest from other connection = %v", err)
	}
	if err := registry.CancelRequest(request.ID, "connection-1"); err != nil {
		t.Fatalf("CancelRequest from owner: %v", err)
	}
	if got := <-result; !errors.Is(got.err, ErrRemoteAccessCanceled) {
		t.Fatalf("Request result error = %v, want canceled", got.err)
	}
}

func TestRemoteAccessCloseWakesWaiterAndRevokesGrants(t *testing.T) {
	registry := NewRemoteAccessRegistry()
	result := requestRemoteAccessAsync(registry, t.TempDir())
	request := waitRemoteAccessRequest(t, registry)
	registry.Close()
	if got := <-result; !errors.Is(got.err, ErrRemoteAccessClosed) {
		t.Fatalf("Request error = %v, want closed", got.err)
	}
	if err := registry.Resolve(request.ID, "approve"); !errors.Is(err, ErrRemoteAccessClosed) {
		t.Fatalf("Resolve after close error = %v, want closed", err)
	}
	if got := registry.Pending(); len(got) != 0 {
		t.Fatalf("Pending after close = %d, want 0", len(got))
	}
}

func TestRemoteAccessGrantExpires(t *testing.T) {
	now := time.Now()
	registry := newRemoteAccessRegistry(time.Minute, time.Minute, func() time.Time { return now })
	result := requestRemoteAccessAsync(registry, t.TempDir())
	request := waitRemoteAccessRequest(t, registry)
	if err := registry.Resolve(request.ID, "approve"); err != nil {
		t.Fatalf("Resolve(approve): %v", err)
	}
	grant := (<-result).grant
	now = now.Add(time.Minute)
	if _, err := registry.Grant(grant.ID, "connection-1"); !errors.Is(err, ErrRemoteGrantInvalid) {
		t.Fatalf("Grant after expiry error = %v, want invalid", err)
	}
}

func TestRemoteAccessProtocolValidation(t *testing.T) {
	tests := []struct {
		name string
		msg  ClientMsg
		want bool
	}{
		{name: "request missing root", msg: ClientMsg{Op: "remote_access_request"}},
		{name: "request missing identity", msg: ClientMsg{Op: "remote_access_request", ProjectRoot: "/tmp/project"}},
		{name: "request root", msg: ClientMsg{Op: "remote_access_request", ProjectRoot: "/tmp/project", RemoteConnectionID: "connection", RemoteAccessRequestID: "request"}, want: true},
		{name: "list", msg: ClientMsg{Op: "remote_access_list"}, want: true},
		{name: "resolve missing decision", msg: ClientMsg{Op: "remote_access_resolve", RemoteAccessRequestID: "request"}},
		{name: "resolve approve", msg: ClientMsg{Op: "remote_access_resolve", RemoteAccessRequestID: "request", RemoteAccessDecision: "approve"}, want: true},
		{name: "resolve deny", msg: ClientMsg{Op: "remote_access_resolve", RemoteAccessRequestID: "request", RemoteAccessDecision: "deny"}, want: true},
		{name: "resolve invalid decision", msg: ClientMsg{Op: "remote_access_resolve", RemoteAccessRequestID: "request", RemoteAccessDecision: "allow"}},
		{name: "release missing grant", msg: ClientMsg{Op: "remote_access_release"}},
		{name: "release missing connection", msg: ClientMsg{Op: "remote_access_release", RemoteGrantID: "grant"}},
		{name: "release grant", msg: ClientMsg{Op: "remote_access_release", RemoteGrantID: "grant", RemoteConnectionID: "connection"}, want: true},
		{name: "cancel request", msg: ClientMsg{Op: "remote_access_cancel", RemoteAccessRequestID: "request", RemoteConnectionID: "connection"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateClient(tt.msg)
			if (err == nil) != tt.want {
				t.Fatalf("validateClient() error = %v, want success %t", err, tt.want)
			}
		})
	}
}

func requestRemoteAccessAsync(registry *RemoteAccessRegistry, root string) <-chan struct {
	grant RemoteGrant
	err   error
} {
	result := make(chan struct {
		grant RemoteGrant
		err   error
	}, 1)
	go func() {
		grant, err := registry.Request(context.Background(), "connection-1", "Browser", root)
		result <- struct {
			grant RemoteGrant
			err   error
		}{grant, err}
	}()
	return result
}

func waitRemoteAccessRequest(t *testing.T, registry *RemoteAccessRegistry) RemoteAccessRequest {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if requests := registry.Pending(); len(requests) > 0 {
			return requests[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("remote access request did not become pending")
	return RemoteAccessRequest{}
}
