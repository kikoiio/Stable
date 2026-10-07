package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"stable/internal/conversation"
	"stable/internal/permission"
	"stable/internal/platform/ipc"
	"stable/internal/sessionlog"
)

func TestWebSocketForwardsPermissionQuestionAndPlanInteractions(t *testing.T) {
	chatPath := filepath.Join(t.TempDir(), "chat.sock")
	listener, err := ipc.ListenPrivate(chatPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	root := t.TempDir()
	serverErr := make(chan error, 1)
	released := make(chan struct{}, 1)
	go func() {
		for i := 0; i < 7; i++ {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverErr <- acceptErr
				return
			}
			var request conversation.ClientMsg
			if decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); decodeErr != nil {
				_ = conn.Close()
				serverErr <- decodeErr
				return
			}
			if request.Op != "remote_access_request" && request.Op != "remote_access_release" && (request.RemoteGrantID != "approved-grant" || request.RemoteConnectionID == "" || request.ProjectRoot != root) {
				_ = conn.Close()
				serverErr <- errors.New("conversation request was not bound to the approved grant and root")
				return
			}
			var response conversation.ServerMsg
			switch request.Op {
			case "remote_access_request":
				response = conversation.ServerMsg{Type: "remote_grant", RemoteGrant: &conversation.RemoteGrant{ID: "approved-grant", ProjectRoot: root}}
			case "approval_list":
				response = conversation.ServerMsg{Type: "approvals", Approvals: []permission.ApprovalPrompt{{ID: "approval-1", Name: "write_file", Reason: "confirm write"}}}
			case "approval_resolve":
				if request.ApprovalID != "approval-1" || request.ApprovalChoice != string(permission.ChoiceAllowOnce) {
					_ = conn.Close()
					serverErr <- errors.New("permission decision did not reach the service")
					return
				}
				response = conversation.ServerMsg{Type: "approval_resolved"}
			case "question_list":
				response = conversation.ServerMsg{Type: "questions", Questions: []sessionlog.PendingQuestion{{QuestionID: "question-1", Prompt: "Continue?", Status: sessionlog.QuestionPending}}}
			case "reply":
				if request.QuestionID != "question-1" || request.Text != "yes" {
					_ = conn.Close()
					serverErr <- errors.New("question answer did not reach the service")
					return
				}
				response = conversation.ServerMsg{Type: "questions"}
			case "plan_resolve":
				if request.ApprovalChoice != conversation.PlanResolveAuto {
					_ = conn.Close()
					serverErr <- errors.New("plan decision did not reach the service")
					return
				}
				response = conversation.ServerMsg{Type: "plan_state", PlanState: &conversation.PlanState{Mode: "default"}}
			case "remote_access_release":
				released <- struct{}{}
				response = conversation.ServerMsg{Type: "done"}
			default:
				_ = conn.Close()
				serverErr <- errors.New("unexpected conversation operation: " + request.Op)
				return
			}
			_ = json.NewEncoder(conn).Encode(response)
			if request.Op != "remote_access_release" {
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
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = manager.Stop(ctx)
	}()
	token, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.store.Consume(token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	baseURL := "http://" + started.ListenAddr
	header := http.Header{"Origin": []string{baseURL}, "Cookie": []string{pairingCookieName + "=" + session.Credential}}
	ws, _, err := websocket.Dial(ctx, "ws://"+started.ListenAddr+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
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
	send := func(id string, msg conversation.ClientMsg) {
		t.Helper()
		data, marshalErr := json.Marshal(wsEnvelope{ID: id, Message: msg})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := ws.Write(ctx, websocket.MessageText, data); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	projectRequest := conversation.ClientMsg{Op: "remote_access_request", ProjectRoot: root}
	send("access", projectRequest)
	if grant, done := read(), read(); grant.Message.Type != "remote_grant" || done.Message.Type != "done" {
		t.Fatalf("access response = %+v, %+v", grant, done)
	}
	steps := []struct {
		id       string
		request  conversation.ClientMsg
		response string
	}{
		{"approvals", conversation.ClientMsg{Op: "approval_list", SessionID: "session-1"}, "approvals"},
		{"approval-decision", conversation.ClientMsg{Op: "approval_resolve", SessionID: "session-1", ApprovalID: "approval-1", ApprovalChoice: string(permission.ChoiceAllowOnce)}, "approval_resolved"},
		{"questions", conversation.ClientMsg{Op: "question_list", SessionID: "session-1"}, "questions"},
		{"answer", conversation.ClientMsg{Op: "reply", SessionID: "session-1", QuestionID: "question-1", Text: "yes"}, "questions"},
		{"plan", conversation.ClientMsg{Op: "plan_resolve", SessionID: "session-1", ApprovalChoice: conversation.PlanResolveAuto}, "plan_state"},
	}
	for _, step := range steps {
		send(step.id, step.request)
		response, done := read(), read()
		if response.ID != step.id || response.Message.Type != step.response || done.ID != step.id || done.Message.Type != "done" {
			t.Fatalf("%s round trip = %+v, %+v", step.id, response, done)
		}
	}
	_ = ws.Close(websocket.StatusNormalClosure, "interaction test complete")
	select {
	case <-released:
	case err := <-serverErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("approved remote grant was not released")
	}
}
