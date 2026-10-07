package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"stable/internal/conversation"
	"stable/internal/platform/ipc"
)

const remoteErrorCredentialMarker = "provider-secret-marker-93f2"

func TestSafeRemoteErrorDoesNotExposeProviderCredential(t *testing.T) {
	if got := safeRemoteError(errors.New("provider rejected request with key " + remoteErrorCredentialMarker)); got != "remote request failed" || strings.Contains(got, remoteErrorCredentialMarker) {
		t.Fatalf("safeRemoteError() = %q", got)
	}
}

func TestWebSocketRejectsDuplicateAndSeventeenthActiveRequest(t *testing.T) {
	chatPath := filepath.Join(t.TempDir(), "chat.sock")
	listener, err := ipc.ListenPrivate(chatPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	projectRoot := t.TempDir()
	acceptedRequests := make(chan conversation.ClientMsg, 32)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				var request conversation.ClientMsg
				if json.NewDecoder(bufio.NewReader(conn)).Decode(&request) != nil {
					return
				}
				switch request.Op {
				case "remote_access_request":
					_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "remote_grant", RemoteGrant: &conversation.RemoteGrant{ID: "grant", ProjectRoot: projectRoot}})
					_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "done"})
				case "session_list":
					acceptedRequests <- request
					_, _ = io.Copy(io.Discard, conn) // Keep the request active until the bridge cancels it.
				case "session_search":
					_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "error", Error: "provider diagnostic: " + remoteErrorCredentialMarker})
					_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "done"})
				case "remote_access_release":
					_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "done"})
				}
			}()
		}
	}()

	manager := NewManager(chatPath)
	started, err := manager.Start(context.Background(), Config{ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Stop(ctx)
		_ = listener.Close()
		select {
		case <-serverDone:
		case <-ctx.Done():
			t.Error("fake IPC server did not stop")
		}
	}()

	token, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.store.Consume(token)
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "http://" + started.ListenAddr
	header := http.Header{"Origin": []string{baseURL}, "Cookie": []string{pairingCookieName + "=" + session.Credential}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+started.ListenAddr+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "test complete")

	send := func(id string, message conversation.ClientMsg) {
		t.Helper()
		data, marshalErr := json.Marshal(wsEnvelope{ID: id, Message: message})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := ws.Write(ctx, websocket.MessageText, data); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	read := func() wsResponse {
		t.Helper()
		_, data, readErr := ws.Read(ctx)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var response wsResponse
		if unmarshalErr := json.Unmarshal(data, &response); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return response
	}

	send("access", conversation.ClientMsg{Op: "remote_access_request", ProjectRoot: projectRoot})
	if grant, done := read(), read(); grant.Message.Type != "remote_grant" || done.Message.Type != "done" {
		t.Fatalf("directory access responses = %+v, %+v", grant, done)
	}
	send("secret", conversation.ClientMsg{Op: "session_search", Text: "query"})
	if failure, done := read(), read(); failure.ID != "secret" || failure.Message.Type != "error" || failure.Message.Error != "remote request failed" || strings.Contains(failure.Message.Error, remoteErrorCredentialMarker) || done.Message.Type != "done" {
		t.Fatalf("provider error response leaked details: %+v, %+v", failure, done)
	}
	for i := 0; i < 16; i++ {
		send("active-"+string(rune('a'+i)), conversation.ClientMsg{Op: "session_list"})
	}
	for i := 0; i < 16; i++ {
		select {
		case <-acceptedRequests:
		case <-ctx.Done():
			t.Fatalf("only %d requests reached the fake conversation service", i)
		}
	}
	send("active-a", conversation.ClientMsg{Op: "session_list"}) // Duplicate must not replace the original stream.
	send("seventeenth", conversation.ClientMsg{Op: "session_list"})
	var duplicateRejected, overflowRejected, overflowDone bool
	for i := 0; i < 3; i++ {
		response := read()
		switch {
		case response.ID == "" && response.Message.Type == "error":
			duplicateRejected = response.Message.Error == "remote request failed"
		case response.ID == "seventeenth" && response.Message.Type == "error":
			overflowRejected = response.Message.Error == "remote request failed"
		case response.ID == "seventeenth" && response.Message.Type == "done":
			overflowDone = true
		default:
			t.Fatalf("unexpected request-limit response: %+v", response)
		}
	}
	if !duplicateRejected || !overflowRejected || !overflowDone {
		t.Fatalf("duplicate=%v overflow=%v done=%v", duplicateRejected, overflowRejected, overflowDone)
	}
}

func TestWebSocketLimitsConcurrentConnectionsAndFrameSize(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "missing-chat.sock"))
	started, err := manager.Start(context.Background(), Config{ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		if err := manager.Stop(stopCtx); err != nil {
			t.Errorf("Stop(): %v", err)
		}
	}()
	token, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.store.Consume(token)
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "http://" + started.ListenAddr
	header := http.Header{"Origin": []string{baseURL}, "Cookie": []string{pairingCookieName + "=" + session.Credential}}
	var connections []*websocket.Conn
	for i := 0; i < 8; i++ {
		conn, _, dialErr := websocket.Dial(ctx, "ws://"+started.ListenAddr+"/ws", &websocket.DialOptions{HTTPHeader: header})
		if dialErr != nil {
			t.Fatalf("dial connection %d: %v", i+1, dialErr)
		}
		connections = append(connections, conn)
	}
	if _, response, dialErr := websocket.Dial(ctx, "ws://"+started.ListenAddr+"/ws", &websocket.DialOptions{HTTPHeader: header}); dialErr == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ninth WebSocket = response %v, error %v", response, dialErr)
	}
	for _, conn := range connections {
		_ = conn.Close(websocket.StatusNormalClosure, "release test slot")
	}

	frameConn, _, err := websocket.Dial(ctx, "ws://"+started.ListenAddr+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	if err := frameConn.Write(ctx, websocket.MessageText, make([]byte, maxWebSocketMessage+1)); err != nil {
		t.Fatalf("send oversized frame: %v", err)
	}
	_, _, err = frameConn.Read(ctx)
	if err == nil || websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("oversized WebSocket frame error = %v, close status %v", err, websocket.CloseStatus(err))
	}
}
