package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"stable/internal/conversation"
	"stable/internal/platform/ipc"
)

const maxWebSocketMessage = 1 << 20

type wsEnvelope struct {
	ID      string                 `json:"id"`
	Message conversation.ClientMsg `json:"message"`
}

type wsResponse struct {
	ID      string                 `json:"id"`
	Message conversation.ServerMsg `json:"message"`
}

func (m *Manager) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "request rejected", http.StatusForbidden)
		return
	}
	cookie, err := r.Cookie(pairingCookieName)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if _, ok := m.store.Authenticate(cookie.Value); !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if m.chatSocket == "" {
		http.Error(w, "remote conversation service unavailable", http.StatusServiceUnavailable)
		return
	}
	select {
	case m.connections <- struct{}{}:
		defer func() { <-m.connections }()
	default:
		http.Error(w, "too many remote connections", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: false})
	if err != nil {
		return
	}
	conn.SetReadLimit(maxWebSocketMessage)
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		_ = conn.CloseNow()
		return
	}
	m.activeWS[conn] = struct{}{}
	m.activeWG.Add(1)
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.activeWS, conn)
		m.mu.Unlock()
		m.activeWG.Done()
		_ = conn.Close(websocket.StatusNormalClosure, "connection closed")
	}()
	m.serveWebSocket(r.Context(), conn)
}

func (m *Manager) serveWebSocket(parent context.Context, ws *websocket.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer ws.CloseNow()
	connectionID, err := randomCredential()
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "connection setup failed")
		return
	}
	var writeMu sync.Mutex
	write := func(id string, msg conversation.ServerMsg) error {
		if msg.Type == "error" {
			msg.Error = safeRemoteError(errors.New(msg.Error))
		}
		payload, err := json.Marshal(wsResponse{ID: id, Message: msg})
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.Write(ctx, websocket.MessageText, payload)
	}

	type wsReadResult struct {
		typ  websocket.MessageType
		data []byte
		err  error
	}
	readFrames := make(chan wsReadResult, 1)
	go func() {
		for {
			typ, data, err := ws.Read(ctx)
			select {
			case readFrames <- wsReadResult{typ: typ, data: data, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	firstFrame := <-readFrames
	if firstFrame.err != nil || firstFrame.typ != websocket.MessageText {
		return
	}
	var first wsEnvelope
	if err := decodeEnvelope(firstFrame.data, &first); err != nil || first.ID == "" || len(first.ID) > 128 || first.Message.Op != "remote_access_request" {
		_ = write(first.ID, conversation.ServerMsg{Type: "error", Error: "first message must request an approved project directory"})
		return
	}
	clientLabel := strings.TrimSpace(first.Message.RemoteClientLabel)
	if clientLabel == "" {
		clientLabel = "Browser client"
	}
	requestID, err := randomCredential()
	if err != nil {
		return
	}
	type grantResult struct {
		grant conversation.RemoteGrant
		err   error
	}
	grantCtx, grantCancel := context.WithCancel(ctx)
	defer grantCancel()
	grantReady := make(chan grantResult, 1)
	go func() {
		grant, err := m.requestGrant(grantCtx, connectionID, requestID, clientLabel, first.Message.ProjectRoot)
		grantReady <- grantResult{grant: grant, err: err}
	}()
	var grant conversation.RemoteGrant
	select {
	case result := <-grantReady:
		grant, err = result.grant, result.err
	case frame := <-readFrames:
		grantCancel()
		if frame.err == nil {
			_ = write(first.ID, conversation.ServerMsg{Type: "error", Error: "wait for local directory approval before sending more messages"})
		}
		return
	}
	if err != nil {
		_ = write(first.ID, conversation.ServerMsg{Type: "error", Error: safeRemoteError(err)})
		return
	}
	if err := write(first.ID, conversation.ServerMsg{Type: "remote_grant", RemoteGrant: &grant}); err != nil {
		m.releaseGrant(connectionID, grant.ID)
		return
	}
	_ = write(first.ID, conversation.ServerMsg{Type: "done"})
	defer m.releaseGrant(connectionID, grant.ID)

	type activeRequest struct{ cancel context.CancelFunc }
	active := map[string]activeRequest{}
	var activeMu sync.Mutex
	var workers sync.WaitGroup
	defer func() {
		cancel()
		activeMu.Lock()
		for _, request := range active {
			request.cancel()
		}
		activeMu.Unlock()
		workers.Wait()
	}()

	for {
		frame := <-readFrames
		if frame.err != nil {
			return
		}
		if frame.typ != websocket.MessageText {
			_ = write("", conversation.ServerMsg{Type: "error", Error: "binary messages are not supported"})
			return
		}
		var envelope wsEnvelope
		if err := decodeEnvelope(frame.data, &envelope); err != nil || envelope.ID == "" || len(envelope.ID) > 128 {
			_ = write("", conversation.ServerMsg{Type: "error", Error: "invalid request envelope"})
			return
		}
		if envelope.Message.Op == "remote_access_request" || envelope.Message.RemoteGrantID != "" || envelope.Message.RemoteConnectionID != "" || envelope.Message.RemoteClientLabel != "" || envelope.Message.RemoteAccessRequestID != "" || envelope.Message.RemoteAccessDecision != "" {
			_ = write(envelope.ID, conversation.ServerMsg{Type: "error", Error: "remote authorization fields are server managed"})
			continue
		}
		if !remoteOperationAllowed(envelope.Message.Op) {
			_ = write(envelope.ID, conversation.ServerMsg{Type: "error", Error: "operation is unavailable to remote clients"})
			_ = write(envelope.ID, conversation.ServerMsg{Type: "done"})
			continue
		}
		activeMu.Lock()
		if _, exists := active[envelope.ID]; exists {
			activeMu.Unlock()
			// Reusing an active ID would make the rejection ambiguous with the
			// original stream. Use an uncorrelated response so clients cannot
			// mistake it for data from the existing request.
			_ = write("", conversation.ServerMsg{Type: "error", Error: "request ID is already active"})
			continue
		}
		if len(active) >= 16 {
			activeMu.Unlock()
			_ = write(envelope.ID, conversation.ServerMsg{Type: "error", Error: "too many active requests"})
			_ = write(envelope.ID, conversation.ServerMsg{Type: "done"})
			continue
		}
		reqCtx, reqCancel := context.WithCancel(ctx)
		active[envelope.ID] = activeRequest{cancel: reqCancel}
		activeMu.Unlock()
		workers.Add(1)
		go func(id string, message conversation.ClientMsg) {
			defer workers.Done()
			defer reqCancel()
			defer func() {
				activeMu.Lock()
				delete(active, id)
				activeMu.Unlock()
			}()
			message.RemoteGrantID = grant.ID
			message.RemoteConnectionID = connectionID
			message.ProjectRoot = grant.ProjectRoot
			err := m.forwardConversation(reqCtx, id, message.Op, message, write)
			if err != nil && reqCtx.Err() == nil {
				_ = write(id, conversation.ServerMsg{Type: "error", Error: safeRemoteError(err)})
				_ = write(id, conversation.ServerMsg{Type: "done"})
			}
		}(envelope.ID, envelope.Message)
	}
}

func decodeEnvelope(data []byte, envelope *wsEnvelope) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(envelope); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func remoteOperationAllowed(op string) bool {
	switch op {
	case "session_list", "session_create", "session_load", "session_search", "chat", "run_start", "run_subscribe", "run_cancel", "approval_list", "approval_resolve", "approval_cancel", "question_list", "reply", "plan_resolve":
		return true
	default:
		return false
	}
}

func (m *Manager) requestGrant(ctx context.Context, connectionID, requestID, clientLabel, root string) (conversation.RemoteGrant, error) {
	conn, err := ipc.DialPrivate(m.chatSocket, 2*time.Second)
	if err != nil {
		return conversation.RemoteGrant{}, errors.New("session service is unavailable")
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(conversation.ClientMsg{Op: "remote_access_request", ProjectRoot: root, RemoteClientLabel: clientLabel, RemoteConnectionID: connectionID, RemoteAccessRequestID: requestID}); err != nil {
		return conversation.RemoteGrant{}, err
	}
	stopWatcher := make(chan struct{})
	defer close(stopWatcher)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopWatcher:
		}
	}()
	decoder := json.NewDecoder(bufio.NewReader(conn))
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		var response conversation.ServerMsg
		if err := decoder.Decode(&response); err != nil {
			if ctx.Err() != nil {
				m.cancelGrantRequest(connectionID, requestID)
				return conversation.RemoteGrant{}, ctx.Err()
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				return conversation.RemoteGrant{}, errors.New("directory approval timed out")
			}
			return conversation.RemoteGrant{}, err
		}
		if response.Type == "remote_grant" && response.RemoteGrant != nil {
			return *response.RemoteGrant, nil
		}
		if response.Type == "error" {
			return conversation.RemoteGrant{}, errors.New(response.Error)
		}
		if response.Type == "done" {
			return conversation.RemoteGrant{}, errors.New("directory approval did not return a grant")
		}
		select {
		case <-ctx.Done():
			m.cancelGrantRequest(connectionID, requestID)
			return conversation.RemoteGrant{}, ctx.Err()
		default:
		}
	}
}

func (m *Manager) cancelGrantRequest(connectionID, requestID string) {
	conn, err := ipc.DialPrivate(m.chatSocket, 500*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = json.NewEncoder(conn).Encode(conversation.ClientMsg{Op: "remote_access_cancel", RemoteConnectionID: connectionID, RemoteAccessRequestID: requestID})
}

func (m *Manager) releaseGrant(connectionID, grantID string) {
	conn, err := ipc.DialPrivate(m.chatSocket, 500*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = json.NewEncoder(conn).Encode(conversation.ClientMsg{Op: "remote_access_release", RemoteConnectionID: connectionID, RemoteGrantID: grantID})
}

func (m *Manager) forwardConversation(ctx context.Context, requestID, op string, message conversation.ClientMsg, write func(string, conversation.ServerMsg) error) error {
	conn, err := ipc.DialPrivate(m.chatSocket, 2*time.Second)
	if err != nil {
		return errors.New("session service is unavailable")
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	if err := json.NewEncoder(conn).Encode(message); err != nil {
		return err
	}
	decoder := json.NewDecoder(bufio.NewReader(conn))
	streaming := op == "run_start" || op == "run_subscribe"
	streamEstablished := false
	for {
		var response conversation.ServerMsg
		if err := decoder.Decode(&response); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if err := write(requestID, response); err != nil {
			return err
		}
		if response.Type == "run_started" {
			streamEstablished = true
		}
		if response.Type == "run_event" {
			if !streamEstablished && response.RunEvent != nil && response.RunEvent.Kind == "terminal" {
				_ = write(requestID, conversation.ServerMsg{Type: "done"})
				return nil
			}
			streamEstablished = true
		}
		if response.Type == "error" && streaming && !streamEstablished {
			_ = write(requestID, conversation.ServerMsg{Type: "done"})
			return nil
		}
		if response.Type == "done" || response.Type == "run_outcome" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func safeRemoteError(err error) string {
	if err == nil {
		return "remote request failed"
	}
	// Conversation and provider errors can include credentials, local paths,
	// or internal diagnostics. Keep those details on the server side.
	return "remote request failed"
}
