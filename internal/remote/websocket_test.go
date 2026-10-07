package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/llm"
	"stable/internal/platform/ipc"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

type remoteFixtureProvider struct{}

func (remoteFixtureProvider) Stream(ctx context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
	events := make(chan llm.Event, 2)
	errs := make(chan error)
	lastMessage := ""
	if len(request.Messages) > 0 {
		lastMessage = request.Messages[len(request.Messages)-1].Content
	}
	go func() {
		defer close(events)
		defer close(errs)
		if lastMessage == "cancel this" {
			events <- llm.Event{Kind: llm.TextDelta, Text: "partial"}
			<-ctx.Done()
			return
		}
		if lastMessage == "keep running" {
			events <- llm.Event{Kind: llm.TextDelta, Text: "waiting"}
			<-ctx.Done()
			return
		}
		events <- llm.Event{Kind: llm.TextDelta, Text: "remote answer"}
		events <- llm.Event{Kind: llm.StreamEnd}
	}()
	return events, errs
}

func TestWebSocketAccessGrantAndScopedConversationBridge(t *testing.T) {
	chatPath := filepath.Join(t.TempDir(), "chat.sock")
	listener, err := ipc.ListenPrivate(chatPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	projectRoot := t.TempDir()
	releaseReceived := make(chan struct{}, 1)
	bridgeErr := make(chan error, 1)
	go func() {
		for i := 0; i < 3; i++ {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				bridgeErr <- acceptErr
				return
			}
			var request conversation.ClientMsg
			if decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); decodeErr != nil {
				_ = conn.Close()
				bridgeErr <- decodeErr
				return
			}
			switch request.Op {
			case "remote_access_request":
				if request.ProjectRoot != projectRoot || request.RemoteConnectionID == "" || request.RemoteAccessRequestID == "" {
					bridgeErr <- context.Canceled
					_ = conn.Close()
					return
				}
				_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "remote_grant", RemoteGrant: &conversation.RemoteGrant{ID: "grant-1", ProjectRoot: projectRoot}})
				_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "done"})
			case "session_list":
				if request.RemoteGrantID != "grant-1" || request.RemoteConnectionID == "" || request.ProjectRoot != projectRoot {
					bridgeErr <- context.Canceled
					_ = conn.Close()
					return
				}
				_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "sessions", Sessions: nil})
				_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "done"})
			case "remote_access_release":
				if request.RemoteGrantID != "grant-1" || request.RemoteConnectionID == "" {
					bridgeErr <- context.Canceled
					_ = conn.Close()
					return
				}
				releaseReceived <- struct{}{}
				_ = json.NewEncoder(conn).Encode(conversation.ServerMsg{Type: "done"})
			}
			_ = conn.Close()
		}
	}()

	manager := NewManager(chatPath)
	started, err := manager.Start(context.Background(), Config{ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "http://" + started.ListenAddr
	token, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	pairURL, _ := url.Parse(baseURL + "/api/pair")
	pairBody := strings.NewReader(`{"token":"` + token + `"}`)
	request, _ := http.NewRequest(http.MethodPost, pairURL.String(), pairBody)
	request.Header.Set("Origin", baseURL)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("pair status = %d", response.StatusCode)
	}
	var cookie *http.Cookie
	for _, candidate := range response.Cookies() {
		if candidate.Name == pairingCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("pair response did not set browser session cookie")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := http.Header{"Cookie": []string{cookie.Name + "=" + cookie.Value}}
	header.Set("Origin", baseURL)
	ws, handshake, err := websocket.Dial(ctx, "ws://"+started.ListenAddr+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if handshake != nil {
			t.Fatalf("%v: status %d", err, handshake.StatusCode)
		}
		t.Fatal(err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "test complete")
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"id":"access","message":{"op":"remote_access_request","project_root":"`+projectRoot+`"}}`)); err != nil {
		t.Fatal(err)
	}
	var grantSeen bool
	for i := 0; i < 2; i++ {
		var result wsResponse
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.Message.Type == "remote_grant" && result.Message.RemoteGrant != nil && result.Message.RemoteGrant.ID == "grant-1" {
			grantSeen = true
		}
	}
	if !grantSeen {
		t.Fatal("browser did not receive approved directory grant")
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"id":"sessions","message":{"op":"session_list","project_root":"/forged/path"}}`)); err != nil {
		t.Fatal(err)
	}
	var done bool
	for i := 0; i < 2; i++ {
		var result wsResponse
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.ID != "sessions" {
			t.Fatalf("response ID = %q, want sessions", result.ID)
		}
		if result.Message.Type == "done" {
			done = true
		}
	}
	if !done {
		t.Fatal("session bridge did not finish request")
	}
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-releaseReceived:
	case err := <-bridgeErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("remote grant was not released during shutdown")
	}
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("WebSocket remained open after remote shutdown")
	}
}

func TestRemoteConversationEndToEndWithLocalApprovalAndFakeProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	projectRoot := t.TempDir()
	stateDir := t.TempDir()
	db, err := store.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.CreateGoal(ctx, core.Goal{ID: "remote-goal", Objective: "Remote read-only target", AllowedRoot: projectRoot}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.InsertEventIfAbsent(ctx, core.Event{ID: "remote-event", GoalID: "remote-goal", Kind: "design_changed", Payload: json.RawMessage(`{"private":"must not be exposed"}`)}); err != nil {
		t.Fatal(err)
	}
	chatPath := filepath.Join(stateDir, "chat.sock")
	runner := agent.NewRunner(remoteFixtureProvider{}, agent.RunnerOptions{MaxRetries: -1})
	service, err := conversation.Serve(ctx, conversation.Deps{Store: db, Runner: runner, ProjectRoot: projectRoot, SocketPath: chatPath, ProviderName: "fixture", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	manager := NewManager(chatPath)
	started, err := manager.Start(ctx, Config{ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		_ = manager.Stop(stopCtx)
	}()
	token, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "http://" + started.ListenAddr
	body, _ := json.Marshal(map[string]string{"token": token})
	pairRequest, _ := http.NewRequest(http.MethodPost, baseURL+"/api/pair", strings.NewReader(string(body)))
	pairRequest.Header.Set("Origin", baseURL)
	pairRequest.Header.Set("Content-Type", "application/json")
	pairResponse, err := http.DefaultClient.Do(pairRequest)
	if err != nil {
		t.Fatal(err)
	}
	pairResponse.Body.Close()
	if pairResponse.StatusCode != http.StatusNoContent || len(pairResponse.Cookies()) != 1 {
		t.Fatalf("pair response = %d, cookies %v", pairResponse.StatusCode, pairResponse.Cookies())
	}
	cookie := pairResponse.Cookies()[0]
	header := http.Header{"Cookie": []string{cookie.Name + "=" + cookie.Value}, "Origin": []string{baseURL}}
	wsCtx, wsCancel := context.WithTimeout(ctx, 8*time.Second)
	defer wsCancel()
	ws, _, err := websocket.Dial(wsCtx, "ws://"+started.ListenAddr+"/ws", &websocket.DialOptions{HTTPHeader: header})
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
		if writeErr := ws.Write(wsCtx, websocket.MessageText, data); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	receive := func() wsResponse {
		t.Helper()
		_, data, readErr := ws.Read(wsCtx)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var response wsResponse
		if unmarshalErr := json.Unmarshal(data, &response); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return response
	}

	send("access", conversation.ClientMsg{Op: "remote_access_request", ProjectRoot: projectRoot, RemoteClientLabel: "fixture browser"})
	var pending conversation.RemoteAccessRequest
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := ipc.DialPrivate(chatPath, 300*time.Millisecond)
		if dialErr == nil {
			_ = json.NewEncoder(conn).Encode(conversation.ClientMsg{Op: "remote_access_list"})
			var response conversation.ServerMsg
			decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&response)
			_ = conn.Close()
			if decodeErr == nil && len(response.RemoteAccessRequests) > 0 {
				pending = response.RemoteAccessRequests[0]
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pending.ID == "" || pending.ClientLabel != "fixture browser" || pending.ProjectRoot != projectRoot {
		t.Fatalf("local TUI received unexpected approval request: %+v", pending)
	}
	local, err := ipc.DialPrivate(chatPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewEncoder(local).Encode(conversation.ClientMsg{Op: "remote_access_resolve", RemoteAccessRequestID: pending.ID, RemoteAccessDecision: "approve"})
	_ = local.Close()
	firstGrantResponse := receive()
	if firstGrantResponse.Message.Type != "remote_grant" || firstGrantResponse.Message.RemoteGrant == nil {
		t.Fatalf("directory approval response = %+v", firstGrantResponse)
	}
	if got := receive(); got.Message.Type != "done" {
		t.Fatalf("directory approval completion = %+v", got)
	}

	send("create", conversation.ClientMsg{Op: "session_create", ProjectRoot: projectRoot})
	created := receive()
	if created.ID != "create" || created.Message.Type != "session" || created.Message.Session == nil {
		t.Fatalf("session create response = %+v", created)
	}
	sessionID := created.Message.Session.ID
	if got := receive(); got.Message.Type != "done" {
		t.Fatalf("session create completion = %+v", got)
	}
	send("list", conversation.ClientMsg{Op: "session_list", ProjectRoot: projectRoot})
	listed := receive()
	if listed.ID != "list" || listed.Message.Type != "sessions" {
		t.Fatalf("session list response = %+v", listed)
	}
	foundSession := false
	for _, session := range listed.Message.Sessions {
		if session.ID == sessionID {
			foundSession = true
		}
	}
	if !foundSession {
		t.Fatalf("created session %q was absent from session list", sessionID)
	}
	if len(listed.Message.Goals) != 1 || listed.Message.Goals[0].ID != "remote-goal" || len(listed.Message.GoalEvents) != 1 || listed.Message.GoalEvents[0].Kind != "design_changed" {
		t.Fatalf("read-only goal summary/events = goals %+v events %+v", listed.Message.Goals, listed.Message.GoalEvents)
	}
	eventJSON, err := json.Marshal(listed.Message.GoalEvents)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(eventJSON), "must not be exposed") {
		t.Fatal("goal event payload leaked through the remote event summary")
	}
	if got := receive(); got.Message.Type != "done" {
		t.Fatalf("session list completion = %+v", got)
	}
	if _, err := sessionlog.Replay(projectRoot, sessionID); err != nil {
		t.Fatalf("created session was not persisted under approved root: %v", err)
	}

	send("run", conversation.ClientMsg{Op: "run_start", SessionID: sessionID, Run: &agent.ExecutionRequest{Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "Say hello", Messages: []llm.Message{{Role: "user", Content: "Say hello"}}}})
	var startedRun, gotText, gotOutcome bool
	for !gotOutcome {
		response := receive()
		if response.ID != "run" {
			t.Fatalf("run response ID = %q", response.ID)
		}
		switch response.Message.Type {
		case "run_started":
			startedRun = true
		case "run_event":
			if response.Message.RunEvent != nil && response.Message.RunEvent.Kind == string(agent.EventTextDelta) {
				gotText = true
			}
		case "run_outcome":
			gotOutcome = response.Message.Outcome != nil && response.Message.Outcome.Status == agent.RunCompleted
		case "error":
			t.Fatalf("remote run failed: %s", response.Message.Error)
		}
	}
	if !startedRun || !gotText {
		t.Fatalf("stream did not include start and text event: started=%v text=%v", startedRun, gotText)
	}
	send("load", conversation.ClientMsg{Op: "session_load", SessionID: sessionID, ProjectRoot: projectRoot})
	loaded := receive()
	if loaded.ID != "load" || loaded.Message.Type != "transcript" || loaded.Message.Transcript == nil {
		t.Fatalf("session load response = %+v", loaded)
	}
	if len(loaded.Message.Transcript.Events) < 3 {
		t.Fatalf("loaded transcript did not restore conversation: %+v", loaded.Message.Transcript.Events)
	}
	if got := receive(); got.Message.Type != "done" {
		t.Fatalf("session load completion = %+v", got)
	}

	send("cancel-run", conversation.ClientMsg{Op: "run_start", SessionID: sessionID, Run: &agent.ExecutionRequest{Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "cancel this", Messages: []llm.Message{{Role: "user", Content: "cancel this"}}}})
	var cancelRunID string
	var cancelSent, cancelDone, cancelledOutcome bool
	for !cancelDone || !cancelledOutcome {
		response := receive()
		switch response.Message.Type {
		case "run_started":
			if response.ID != "cancel-run" {
				t.Fatalf("cancel run start ID = %q", response.ID)
			}
			cancelRunID = response.Message.RunID
			if !cancelSent {
				send("cancel-action", conversation.ClientMsg{Op: "run_cancel", SessionID: sessionID, RunID: cancelRunID})
				cancelSent = true
			}
		case "run_outcome":
			if response.ID == "cancel-run" && response.Message.Outcome != nil {
				cancelledOutcome = response.Message.Outcome.Status == agent.RunCancelled
			}
		case "done":
			if response.ID == "cancel-action" {
				cancelDone = true
			}
		case "error":
			t.Fatalf("remote cancellation failed: %s", response.Message.Error)
		}
	}
	if cancelRunID == "" || !cancelSent || !cancelledOutcome {
		t.Fatalf("run was not cancelled: run=%q sent=%v outcome=%v", cancelRunID, cancelSent, cancelledOutcome)
	}

	send("resume-run", conversation.ClientMsg{Op: "run_subscribe", SessionID: sessionID, RunID: cancelRunID})
	var replayOutcome bool
	for !replayOutcome {
		response := receive()
		if response.ID != "resume-run" {
			t.Fatalf("resubscribe response ID = %q", response.ID)
		}
		if response.Message.Type == "run_outcome" {
			replayOutcome = response.Message.Outcome != nil && response.Message.Outcome.Status == agent.RunCancelled
		}
		if response.Message.Type == "error" {
			t.Fatalf("run resubscription failed: %s", response.Message.Error)
		}
	}
	send("live-run", conversation.ClientMsg{Op: "run_start", SessionID: sessionID, Run: &agent.ExecutionRequest{Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "keep running", Messages: []llm.Message{{Role: "user", Content: "keep running"}}}})
	var liveRunID string
	gotLiveEvent := false
	for liveRunID == "" || !gotLiveEvent {
		response := receive()
		if response.ID != "live-run" {
			t.Fatalf("live run response ID = %q", response.ID)
		}
		if response.Message.Type == "run_started" {
			liveRunID = response.Message.RunID
		}
		if response.Message.Type == "run_event" && response.Message.RunEvent != nil && response.Message.RunEvent.Kind == string(agent.EventTextDelta) {
			gotLiveEvent = true
		}
	}

	_ = ws.Close(websocket.StatusNormalClosure, "reconnect approval test")
	ws2, _, err := websocket.Dial(wsCtx, "ws://"+started.ListenAddr+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.Close(websocket.StatusNormalClosure, "test complete")
	accessData, _ := json.Marshal(wsEnvelope{ID: "reconnect-access", Message: conversation.ClientMsg{Op: "remote_access_request", ProjectRoot: projectRoot, RemoteClientLabel: "fixture browser reconnect"}})
	if err := ws2.Write(wsCtx, websocket.MessageText, accessData); err != nil {
		t.Fatal(err)
	}
	var reconnectRequest conversation.RemoteAccessRequest
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := ipc.DialPrivate(chatPath, 300*time.Millisecond)
		if dialErr == nil {
			_ = json.NewEncoder(conn).Encode(conversation.ClientMsg{Op: "remote_access_list"})
			var response conversation.ServerMsg
			decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&response)
			_ = conn.Close()
			if decodeErr == nil && len(response.RemoteAccessRequests) > 0 {
				reconnectRequest = response.RemoteAccessRequests[0]
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if reconnectRequest.ID == "" || reconnectRequest.ID == pending.ID || reconnectRequest.ClientLabel != "fixture browser reconnect" {
		t.Fatalf("reconnect did not request fresh local approval: %+v", reconnectRequest)
	}
	local, err = ipc.DialPrivate(chatPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewEncoder(local).Encode(conversation.ClientMsg{Op: "remote_access_resolve", RemoteAccessRequestID: reconnectRequest.ID, RemoteAccessDecision: "approve"})
	_ = local.Close()
	_, reconnectGrantData, err := ws2.Read(wsCtx)
	if err != nil {
		t.Fatal(err)
	}
	var reconnectGrant wsResponse
	if err := json.Unmarshal(reconnectGrantData, &reconnectGrant); err != nil {
		t.Fatal(err)
	}
	if reconnectGrant.Message.Type != "remote_grant" || reconnectGrant.Message.RemoteGrant == nil || reconnectGrant.Message.RemoteGrant.ID == firstGrantResponse.Message.RemoteGrant.ID {
		t.Fatalf("reconnect did not receive a fresh grant: %+v", reconnectGrant)
	}
	_, approvalDoneData, err := ws2.Read(wsCtx)
	if err != nil {
		t.Fatal(err)
	}
	var approvalDone wsResponse
	if err := json.Unmarshal(approvalDoneData, &approvalDone); err != nil {
		t.Fatal(err)
	}
	if approvalDone.ID != "reconnect-access" || approvalDone.Message.Type != "done" {
		t.Fatalf("reconnect approval completion = %+v", approvalDone)
	}
	resumeData, _ := json.Marshal(wsEnvelope{ID: "resume-live", Message: conversation.ClientMsg{Op: "run_subscribe", SessionID: sessionID, RunID: liveRunID}})
	if err := ws2.Write(wsCtx, websocket.MessageText, resumeData); err != nil {
		t.Fatal(err)
	}
	_, replayData, err := ws2.Read(wsCtx)
	if err != nil {
		t.Fatal(err)
	}
	var replayed wsResponse
	if err := json.Unmarshal(replayData, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.ID != "resume-live" || replayed.Message.Type != "run_event" {
		t.Fatalf("active run replay response = %+v", replayed)
	}
	cancelData, _ := json.Marshal(wsEnvelope{ID: "cancel-live", Message: conversation.ClientMsg{Op: "run_cancel", SessionID: sessionID, RunID: liveRunID}})
	if err := ws2.Write(wsCtx, websocket.MessageText, cancelData); err != nil {
		t.Fatal(err)
	}
	var resumedOutcome, cancelLiveDone bool
	for !resumedOutcome || !cancelLiveDone {
		_, data, err := ws2.Read(wsCtx)
		if err != nil {
			t.Fatal(err)
		}
		var response wsResponse
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
		if response.ID == "resume-live" && response.Message.Type == "run_outcome" {
			resumedOutcome = response.Message.Outcome != nil && response.Message.Outcome.Status == agent.RunCancelled
		}
		if response.ID == "cancel-live" && response.Message.Type == "done" {
			cancelLiveDone = true
		}
		if response.Message.Type == "error" {
			t.Fatalf("active run reconnect failed: %+v", response.Message)
		}
	}
}
