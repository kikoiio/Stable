package tui

import (
	"strings"
	"testing"

	"stable/internal/conversation"
	"stable/internal/teams"
)

func TestTeamCommandsUseSessionScopeAndPreserveParentRun(t *testing.T) {
	cases := []struct {
		line, op, teamID, runID, text, recipient, decision string
		memberID, memberName, agentName                    string
		after, limit                                       uint64
		broadcast                                          bool
		planRequired                                       bool
		coordinatorOn                                      bool
		acceptRoleChange                                   bool
	}{
		{line: "/teams list", op: "team_list"},
		{line: "/teams coordinator on", op: "team_coordinator", coordinatorOn: true},
		{line: "/teams create squad", op: "team_create", runID: "parent"},
		{line: "/teams close team-1", op: "team_close", teamID: "team-1"},
		{line: "/team team-1 get", op: "team_get", teamID: "team-1"},
		{line: "/team team-1 tasks", op: "team_task_list", teamID: "team-1"},
		{line: "/team team-1 tasks create write docs", op: "team_task_create", teamID: "team-1", text: "write docs"},
		{line: "/team team-1 messages after 12 7", op: "team_messages", teamID: "team-1", after: 12, limit: 7},
		{line: "/team team-1 requests", op: "team_request_list", teamID: "team-1"},
		{line: "/team team-1 send member-1 hello team", op: "team_send", teamID: "team-1", text: "hello team", recipient: "member-1"},
		{line: "/team team-1 send all hello team", op: "team_send", teamID: "team-1", text: "hello team", broadcast: true},
		{line: "/team team-1 respond req-1 3 approve looks good", op: "team_request_respond", teamID: "team-1", decision: string(teams.RequestApproved)},
		{line: "/team team-1 shutdown member-1", op: "team_shutdown_request", teamID: "team-1"},
		{line: "/team team-1 spawn reader explore inspect changes --plan", op: "team_member_spawn", teamID: "team-1", runID: "parent", memberName: "reader", agentName: "explore", text: "inspect changes", planRequired: true},
		{line: "/team team-1 resume member-1", op: "team_member_resume", teamID: "team-1", runID: "parent", memberID: "member-1"},
		{line: "/team team-1 resume member-1 --accept-role-change", op: "team_member_resume", teamID: "team-1", runID: "parent", memberID: "member-1", acceptRoleChange: true},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			m := New("", t.TempDir())
			m.ActiveSession = "session"
			m.Pending, m.ActiveRunID, m.LastCursor = true, "parent", 17
			parentStream := &conversation.StreamClient{}
			m.stream = parentStream
			socket, requests := agentSocketFixture(t, conversation.ServerMsg{Type: tc.op})
			m.Socket = socket
			m.Composer.SetValue(tc.line)
			updated, command := m.submitComposer()
			got := updated.(Model)
			if command == nil || !got.Pending || got.ActiveRunID != "parent" || got.LastCursor != 17 || got.stream != parentStream {
				t.Fatalf("team command changed parent run state: pending=%v run=%q cursor=%d stream=%p", got.Pending, got.ActiveRunID, got.LastCursor, got.stream)
			}
			result := command().(resultMsg)
			if result.err != nil {
				t.Fatal(result.err)
			}
			request := agentFixtureRequest(t, requests)
			if request.Op != tc.op || request.SessionID != "session" || request.TeamID != tc.teamID || request.Run != nil {
				t.Fatalf("request=%+v", request)
			}
			if request.RunID != tc.runID {
				t.Fatalf("run ID=%q, want %q", request.RunID, tc.runID)
			}
			if request.CoordinatorOn != tc.coordinatorOn {
				t.Fatalf("coordinator mode=%v, want %v", request.CoordinatorOn, tc.coordinatorOn)
			}
			if tc.op != "team_create" && tc.op != "team_member_spawn" && tc.op != "team_member_resume" && request.RunID != "" {
				t.Fatalf("session operation borrowed run ID: %+v", request)
			}
			if tc.op == "team_member_spawn" && (request.TeamMemberName != tc.memberName || request.AgentName != tc.agentName || request.Text != tc.text || request.TeamPlanRequired != tc.planRequired) {
				t.Fatalf("member spawn request=%+v", request)
			}
			if tc.op == "team_member_resume" && request.TeamMemberID != tc.memberID {
				t.Fatalf("member resume request=%+v", request)
			}
			if request.TeamAcceptRoleChange != tc.acceptRoleChange {
				t.Fatalf("role change acceptance=%v, want %v", request.TeamAcceptRoleChange, tc.acceptRoleChange)
			}
			if tc.op == "team_send" {
				if request.Text != tc.text || request.TeamRecipient != tc.recipient || request.TeamBroadcast != tc.broadcast || request.TeamToken == "" {
					t.Fatalf("send request=%+v", request)
				}
			}
			if tc.op == "team_messages" && (request.AfterSeq != tc.after || request.Limit != int(tc.limit)) {
				t.Fatalf("message page=%+v", request)
			}
			if tc.op == "team_task_create" && (request.TaskTitle == nil || *request.TaskTitle != tc.text) {
				t.Fatalf("task title=%v", request.TaskTitle)
			}
			if tc.op == "team_request_respond" && request.TeamDecision != tc.decision {
				t.Fatalf("decision=%q", request.TeamDecision)
			}
			updated, _ = got.handleResult(resultMsg{op: tc.op, sessionID: "session", msgs: []conversation.ServerMsg{{Type: tc.op}}})
			got = updated.(Model)
			if !got.Pending || got.ActiveRunID != "parent" || got.LastCursor != 17 || got.stream != parentStream {
				t.Fatal("one-shot team result ended the parent run")
			}
		})
	}
}

func TestTeamCommandsRejectInvalidUsageAndRequireRunForCreate(t *testing.T) {
	for _, line := range []string{"/teams", "/teams create", "/teams close", "/teams coordinator", "/teams coordinator yes", "/team", "/team team-1 tasks update task-1 0 status completed", "/team team-1 messages 0 999", "/team team-1 send all", "/team team-1 respond req-1 0 approve", "/team team-1 shutdown", "/team team-1 spawn reader explore inspect", "/team team-1 resume member-1"} {
		m := New("", t.TempDir())
		m.ActiveSession = "session"
		m.Composer.SetValue(line)
		updated, command := m.submitComposer()
		if command != nil || !strings.Contains(updated.(Model).Status, "用法") {
			t.Fatalf("invalid command accepted: %s status=%q", line, updated.(Model).Status)
		}
	}
	m := New("", t.TempDir())
	m.ActiveSession = "session"
	m.Composer.SetValue("/teams create squad")
	updated, command := m.submitComposer()
	if command != nil || !strings.Contains(updated.(Model).Status, "活动的 lead run") {
		t.Fatal("team create without active lead run was accepted")
	}
}
