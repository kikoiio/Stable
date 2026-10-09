package sessionlog

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"stable/internal/teams"
)

type teamFixture struct {
	t             *testing.T
	root, session string
	revision      uint64
	team          teams.Team
	member        teams.Member
	turn          TurnFact
	runSeq        uint64
}

func newTeamFixture(t *testing.T) *teamFixture {
	t.Helper()
	root := t.TempDir()
	info, err := Create(root, "teams")
	if err != nil {
		t.Fatal(err)
	}
	f := &teamFixture{t: t, root: root, session: info.ID}
	if _, err = Append(root, info.ID, EventRunStarted, RunStarted{RunID: "parent", WorkKind: "session", Intent: "lead"}); err != nil {
		t.Fatal(err)
	}
	f.team = teams.Team{ID: "team-1", Name: "review", Scope: teams.Scope{SessionID: info.ID, WorkKind: "session", ProjectRoot: root}, CreatorRunID: "parent", Status: teams.TeamOpen, Revision: 1, CreatedAt: time.Now().UTC()}
	f.write(TeamCreated, func(e *TeamEvent) { e.Team = &f.team })
	f.member = teams.Member{ID: "member-1", TeamID: f.team.ID, Name: "reader", AgentName: "explore", RoleHash: "hash", Model: "fake", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	f.write(TeamMemberAdded, func(e *TeamEvent) { e.Member = &f.member })
	f.turn = TurnFact{ID: "turn-1", MemberID: f.member.ID, RunID: "child-1", TaskID: "pool-task-1", OriginRunID: "parent", Status: "intent"}
	return f
}
func (f *teamFixture) event(kind string, apply func(*TeamEvent)) TeamEvent {
	e := TeamEvent{ID: fmt.Sprintf("fact-%d", f.revision+1), TeamID: f.team.ID, SessionID: f.session, Kind: kind, Revision: f.revision + 1, ActorID: teams.Lead, ActorRunID: "parent"}
	apply(&e)
	return e
}
func (f *teamFixture) write(kind string, apply func(*TeamEvent)) Event {
	f.t.Helper()
	e := f.event(kind, apply)
	out, err := Append(f.root, f.session, EventTeam, e)
	if err != nil {
		f.t.Fatalf("append %s: %v", kind, err)
	}
	f.revision++
	return out
}
func (f *teamFixture) reject(kind string, apply func(*TeamEvent)) {
	f.t.Helper()
	if _, err := Append(f.root, f.session, EventTeam, f.event(kind, apply)); err == nil {
		f.t.Fatalf("accepted invalid %s", kind)
	}
}
func (f *teamFixture) accept() {
	f.write(TeamTurnIntent, func(e *TeamEvent) { e.Turn = &f.turn })
	f.turn.Status = "queued"
	f.write(TeamTurnAccepted, func(e *TeamEvent) { e.Turn = &f.turn })
	if _, err := Append(f.root, f.session, EventRunStarted, f.start()); err != nil {
		f.t.Fatal(err)
	}
}
func (f *teamFixture) start() RunStarted {
	return RunStarted{RunID: f.turn.RunID, WorkKind: "session", Intent: "investigate", TeamID: f.team.ID, TeamMemberID: f.member.ID, TeamTurnID: f.turn.ID, OriginRunID: f.turn.OriginRunID, OriginCallID: f.turn.OriginCallID}
}
func (f *teamFixture) run(kind string, payload any) {
	f.t.Helper()
	f.runSeq++
	if _, err := Append(f.root, f.session, EventRunEvent, RunEvent{ID: fmt.Sprintf("run-event-%s-%d", f.turn.ID, f.runSeq), RunID: f.turn.RunID, SessionID: f.session, RunSeq: f.runSeq, At: time.Now().UTC(), Kind: kind, Payload: payload}); err != nil {
		f.t.Fatal(err)
	}
}
func (f *teamFixture) delegation(status string) {
	f.run("delegation_event", AgentTaskDelegation{SessionID: f.session, BatchID: "batch-1", TaskID: f.turn.TaskID, TaskName: "inspect", Status: status, UpdatedAt: time.Now().UTC()})
}

func TestTeamStrictUnionAndRevision(t *testing.T) {
	f := newTeamFixture(t)
	f.reject(TeamMessageSent, func(e *TeamEvent) {
		e.Revision++
		e.Message = &teams.Message{ID: "message", TeamID: f.team.ID, SenderID: teams.Lead, Recipients: []string{f.member.ID}, Body: "hello", CreatedAt: time.Now().UTC()}
	})
	f.reject(TeamMemberState, func(e *TeamEvent) { m := f.member; m.Revision++; m.Status = teams.MemberIdle; e.Member = &m })
	f.reject(TeamMessageSent, func(e *TeamEvent) { e.Team = &f.team; e.Message = &teams.Message{} })
	e := f.event(TeamMessageSent, func(e *TeamEvent) {
		e.Message = &teams.Message{ID: "message", TeamID: f.team.ID, SenderID: teams.Lead, Recipients: []string{f.member.ID}, Body: "hello", CreatedAt: time.Now().UTC()}
	})
	raw, _ := json.Marshal(e)
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	data["role_body"] = "secret"
	if _, err := Append(f.root, f.session, EventTeam, data); err == nil {
		t.Fatal("unknown field accepted")
	}
	f.reject(TeamMessageSent, func(e *TeamEvent) { e.SessionID = "other"; e.Message = &teams.Message{} })
	f.reject("invented", func(e *TeamEvent) { e.Member = &f.member })
	f.reject(TeamMessageSent, func(e *TeamEvent) { e.ActorID = f.member.ID; e.ActorRunID = "parent"; e.Message = &teams.Message{} })
}

func TestTeamMessageAppendRejectsRecipientStoppingAtCommit(t *testing.T) {
	f := newTeamFixture(t)
	f.accept()
	projection, err := ReplayTeams(f.root, f.session, f.team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := projection.Members[f.member.ID]
	member.Status = teams.MemberStopping
	member.Revision++
	f.write(TeamMemberState, func(e *TeamEvent) { e.Member = &member })

	before, err := Replay(f.root, f.session)
	if err != nil {
		t.Fatal(err)
	}
	message := teams.Message{ID: "message-stopping-recipient", TeamID: f.team.ID, SenderID: teams.Lead, Recipients: []string{f.member.ID}, Body: "hello", CreatedAt: time.Now().UTC()}
	_, err = Append(f.root, f.session, EventTeam, f.event(TeamMessageSent, func(e *TeamEvent) { e.Message = &message }))
	if err == nil {
		t.Fatal("message to a stopping recipient was accepted")
	}
	after, replayErr := Replay(f.root, f.session)
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	if len(after.Events) != len(before.Events) {
		t.Fatalf("rejected message appended an event: before=%d after=%d", len(before.Events), len(after.Events))
	}
}

func TestTeamRunSourceRequiresAcceptedTurn(t *testing.T) {
	f := newTeamFixture(t)
	start := f.start()
	if _, err := Append(f.root, f.session, EventRunStarted, start); err == nil {
		t.Fatal("run before accepted fact")
	}
	f.write(TeamTurnIntent, func(e *TeamEvent) { e.Turn = &f.turn })
	f.turn.Status = "queued"
	f.write(TeamTurnAccepted, func(e *TeamEvent) { e.Turn = &f.turn })
	for _, change := range []func(*RunStarted){func(s *RunStarted) { s.TeamTurnID = "" }, func(s *RunStarted) { s.TeamMemberID = "other" }, func(s *RunStarted) { s.AgentTaskID = "task"; s.AgentName = "explore" }, func(s *RunStarted) { s.ForkSkill = "fork" }, func(s *RunStarted) { s.WorkKind = "goal"; s.GoalID = "goal"; s.WorkItemID = "item" }, func(s *RunStarted) { s.OriginRunID = "other" }} {
		bad := start
		change(&bad)
		if _, err := Append(f.root, f.session, EventRunStarted, bad); err == nil {
			t.Fatalf("invalid source accepted: %+v", bad)
		}
	}
	if _, err := Append(f.root, f.session, EventRunStarted, start); err != nil {
		t.Fatal(err)
	}
	f.reject(TeamTurnIntent, func(e *TeamEvent) {
		next := f.turn
		next.ID = "turn-2"
		next.RunID = "child-2"
		next.TaskID = "pool-task-2"
		next.Status = "intent"
		e.Turn = &next
	})
}

func TestTeamHandoffTerminalAndStream(t *testing.T) {
	f := newTeamFixture(t)
	message := teams.Message{ID: "message-1", TeamID: f.team.ID, SenderID: teams.Lead, Recipients: []string{f.member.ID}, Body: "inspect files", CreatedAt: time.Now().UTC()}
	f.write(TeamMessageSent, func(e *TeamEvent) { e.Message = &message })
	f.turn.MessageIDs = []string{message.ID}
	f.accept()
	h := HandoffFact{MessageID: message.ID, RecipientID: f.member.ID, DestinationRunID: f.turn.RunID, DestinationTurnID: f.turn.ID}
	f.reject(TeamMessageHandoff, func(e *TeamEvent) { e.Handoff = &h })
	f.delegation("queued")
	f.write(TeamMessageHandoff, func(e *TeamEvent) { e.Handoff = &h })
	f.reject(TeamMessageHandoff, func(e *TeamEvent) { e.Handoff = &h })
	f.turn.Status = "succeeded"
	f.turn.Elapsed = time.Second
	f.turn.Summary = "checked"
	f.reject(TeamTurnTerminal, func(e *TeamEvent) { e.Turn = &f.turn })
	f.reject(TeamMemberState, func(e *TeamEvent) {
		m := f.member
		m.Revision++
		m.Budget.AcceptedTurns = 1
		m.Status = teams.MemberIdle
		e.Member = &m
	})
	f.delegation("running")
	f.delegation("succeeded")
	f.run("terminal", map[string]string{"status": "completed"})
	terminal := f.write(TeamTurnTerminal, func(e *TeamEvent) { e.Turn = &f.turn })
	f.reject(TeamTurnTerminal, func(e *TeamEvent) { e.Turn = &f.turn })
	f.member.Revision++
	f.member.Status = teams.MemberIdle
	f.member.TurnID = f.turn.ID
	f.member.RunID = f.turn.RunID
	f.member.OriginRunID = "parent"
	f.member.Budget = teams.Budget{AcceptedTurns: 1, Elapsed: time.Second}
	f.member.Summary = "checked"
	f.write(TeamMemberState, func(e *TeamEvent) { e.Member = &f.member })
	transcript, err := Replay(f.root, f.session)
	if err != nil {
		t.Fatal(err)
	}
	all, err := ProjectTeams(transcript)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := ReplayTeams(f.root, f.session)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all.Members, stream.Members) || stream.TurnTerminalSeq[f.turn.ID] != terminal.Seq || len(stream.Messages) != 0 || len(all.Messages) != 1 {
		t.Fatalf("stream projection: %+v", stream)
	}
	page, err := TeamHistory(f.root, f.session, f.team.ID, 0, 2)
	if err != nil || len(page) != 2 {
		t.Fatalf("history page: %v, %v", page, err)
	}
	next, err := TeamHistory(f.root, f.session, f.team.ID, page[1].Seq, 2)
	if err != nil || len(next) != 2 || next[0].Seq <= page[1].Seq {
		t.Fatalf("history next: %v, %v", next, err)
	}
	// Compaction changes model context only; raw collaboration facts survive.
	if _, err := Append(f.root, f.session, EventBoundary, Boundary{FromSeq: 1, ToSeq: terminal.Seq, Summary: "summarized"}); err != nil {
		t.Fatal(err)
	}
	after, err := ReplayTeams(f.root, f.session)
	if err != nil || !reflect.DeepEqual(stream, after) {
		t.Fatalf("compaction changed teams: %v", err)
	}
}

func TestTeamRoleMetadataCanChangeOnlyOnExplicitLeadResume(t *testing.T) {
	f := newTeamFixture(t)
	f.accept()
	f.delegation("queued")
	f.delegation("running")
	f.delegation("succeeded")
	f.run("terminal", map[string]string{"status": "completed"})
	f.turn.Status = "succeeded"
	f.turn.Elapsed = time.Second
	f.write(TeamTurnTerminal, func(e *TeamEvent) { e.Turn = &f.turn })
	f.member.Revision++
	f.member.Status = teams.MemberIdle
	f.member.TurnID, f.member.RunID = f.turn.ID, f.turn.RunID
	f.member.OriginRunID = "parent"
	f.member.Budget = teams.Budget{AcceptedTurns: 1, Elapsed: time.Second}
	f.write(TeamMemberState, func(e *TeamEvent) { e.ActorID = "service"; e.ActorRunID = ""; e.Member = &f.member })

	updated := f.member
	updated.Revision++
	updated.RoleHash = "new-role-hash"
	updated.Tools = []string{"glob", "read_file"}
	f.write(TeamMemberState, func(e *TeamEvent) { e.Member = &updated })
	if _, err := ReplayTeams(f.root, f.session); err != nil {
		t.Fatalf("explicit role metadata update failed replay: %v", err)
	}
}

func TestTeamTaskCASAndDependencies(t *testing.T) {
	f := newTeamFixture(t)
	a := teams.Task{ID: "a", TeamID: f.team.ID, Title: "first", Status: teams.TaskPending, Revision: 1, CreatedBy: teams.Lead}
	f.write(TeamTaskCreated, func(e *TeamEvent) { e.Task = &a })
	b := teams.Task{ID: "b", TeamID: f.team.ID, Title: "second", Status: teams.TaskPending, BlockedBy: []string{"a"}, Revision: 1, CreatedBy: teams.Lead}
	f.write(TeamTaskCreated, func(e *TeamEvent) { e.Task = &b })
	f.reject(TeamTaskUpdated, func(e *TeamEvent) { bad := b; bad.Revision++; bad.Status = teams.TaskInProgress; e.Task = &bad })
	f.reject(TeamTaskUpdated, func(e *TeamEvent) { bad := a; bad.Revision++; bad.BlockedBy = []string{"b"}; e.Task = &bad })
	f.reject(TeamTaskUpdated, func(e *TeamEvent) { e.Task = &a })
	f.reject(TeamTaskUpdated, func(e *TeamEvent) { bad := a; bad.Revision++; bad.Blocks = []string{"b"}; e.Task = &bad })
	a.Revision++
	a.Status = teams.TaskCompleted
	f.write(TeamTaskUpdated, func(e *TeamEvent) { e.Task = &a })
	b.Revision++
	b.Status = teams.TaskInProgress
	f.write(TeamTaskUpdated, func(e *TeamEvent) { e.Task = &b })
	f.reject(TeamTaskUpdated, func(e *TeamEvent) { bad := a; bad.Revision++; bad.Status = teams.TaskPending; e.Task = &bad })
}

func TestTeamRequestActorAndExpiration(t *testing.T) {
	f := newTeamFixture(t)
	f.accept()
	f.delegation("queued")
	r := teams.Request{ID: "request-1", TeamID: f.team.ID, MemberID: f.member.ID, Type: teams.RequestPlan, Status: teams.RequestPending, RequesterID: f.member.ID, ResponderID: teams.Lead, Body: "inspect", ExpiresAt: time.Now().UTC().Add(time.Minute), Revision: 1}
	f.write(TeamRequestCreated, func(e *TeamEvent) { e.ActorID = f.member.ID; e.ActorRunID = f.turn.RunID; e.Request = &r })
	r.Revision++
	r.Status = teams.RequestApproved
	f.reject(TeamRequestResponded, func(e *TeamEvent) { e.ActorID = f.member.ID; e.ActorRunID = f.turn.RunID; e.Request = &r })
	f.write(TeamRequestResponded, func(e *TeamEvent) { e.Request = &r })
	f.reject(TeamRequestResponded, func(e *TeamEvent) { e.Request = &r })
	r.ID = "request-2"
	r.Revision = 1
	r.Status = teams.RequestPending
	f.write(TeamRequestCreated, func(e *TeamEvent) { e.ActorID = f.member.ID; e.ActorRunID = f.turn.RunID; e.Request = &r })
	tr, err := Replay(f.root, f.session)
	if err != nil {
		t.Fatal(err)
	}
	st, err := scanTeams(f.session, tr.Events)
	if err != nil {
		t.Fatal(err)
	}
	r.Revision++
	r.Status = teams.RequestExpired
	e := f.event(TeamRequestExpired, func(e *TeamEvent) { e.ActorID = "service"; e.ActorRunID = ""; e.Request = &r })
	if st.checkEvent(f.session, e, r.ExpiresAt.Add(-time.Second)) == nil {
		t.Fatal("premature expiration accepted")
	}
	if err := st.checkEvent(f.session, e, r.ExpiresAt); err != nil {
		t.Fatal(err)
	}
}

func TestTeamReplayRejectsCorruptTypedFact(t *testing.T) {
	f := newTeamFixture(t)
	path, err := SessionPath(f.root, f.session)
	if err != nil {
		t.Fatal(err)
	}
	e := f.event(TeamMemberState, func(e *TeamEvent) { m := f.member; m.Revision += 2; m.Status = teams.MemberStopped; e.Member = &m })
	raw, _ := json.Marshal(Event{SchemaVersion: SchemaVersion, SessionID: f.session, Seq: 5, At: time.Now().UTC(), Type: EventTeam, Data: e})
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.Write(append(raw, '\n'))
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Replay(f.root, f.session); err == nil {
		t.Fatal("corrupt revision replayed")
	}
	if _, err := ReplayTeams(f.root, f.session); err == nil {
		t.Fatal("corrupt revision streamed")
	}
}

func TestTeamCloseHistoryAndDurableToken(t *testing.T) {
	f := newTeamFixture(t)
	message := teams.Message{ID: "token-message", TeamID: f.team.ID, SenderID: teams.Lead, Recipients: []string{f.member.ID}, Body: "inspect", CreatedAt: time.Now().UTC()}
	f.write(TeamMessageSent, func(e *TeamEvent) { e.Token = "client-token"; e.ArgsDigest = "digest"; e.Message = &message })
	f.reject(TeamMessageSent, func(e *TeamEvent) {
		e.Token = "client-token"
		e.ArgsDigest = "other-digest"
		message.ID = "different-message"
		e.Message = &message
	})
	fact, found, err := LookupTeamMutation(f.root, f.session, f.team.ID, teams.Lead, "client-token")
	if err != nil || !found || fact.Message.ID != "token-message" || fact.ArgsDigest != "digest" {
		t.Fatalf("token lookup: %+v, %v, %v", fact, found, err)
	}
	f.team.Status = teams.TeamClosing
	f.team.Revision = f.revision + 1
	f.write(TeamClosing, func(e *TeamEvent) { e.Team = &f.team })
	f.reject(TeamMessageSent, func(e *TeamEvent) { e.Message = &message })
	f.team.Status = teams.TeamClosed
	f.team.Revision = f.revision + 1
	f.reject(TeamClosed, func(e *TeamEvent) { e.Team = &f.team })
	f.member.Revision++
	f.member.Status = teams.MemberStopped
	f.write(TeamMemberState, func(e *TeamEvent) { e.ActorID = "service"; e.ActorRunID = ""; e.Member = &f.member })
	f.team.Revision = f.revision + 1
	f.write(TeamClosed, func(e *TeamEvent) { e.Team = &f.team })
	active, err := ReplayTeams(f.root, f.session)
	if err != nil || len(active.Teams) != 0 {
		t.Fatalf("closed history in active cache: %+v, %v", active, err)
	}
	history, err := ReplayTeams(f.root, f.session, f.team.ID)
	if err != nil || history.Teams[f.team.ID].Status != teams.TeamClosed {
		t.Fatalf("closed projection: %+v, %v", history, err)
	}
}

func TestTeamInterruptedBatchRetriesAreExplicit(t *testing.T) {
	f := newTeamFixture(t)
	message := teams.Message{ID: "retry-message", TeamID: f.team.ID, SenderID: teams.Lead, Recipients: []string{f.member.ID}, Body: "inspect", CreatedAt: time.Now().UTC()}
	f.write(TeamMessageSent, func(e *TeamEvent) { e.Message = &message })
	f.turn.MessageIDs = []string{message.ID}
	previous := ""
	for i := 1; i <= 3; i++ {
		f.turn.ID = fmt.Sprintf("turn-%d", i)
		f.turn.RunID = fmt.Sprintf("child-%d", i)
		f.turn.TaskID = fmt.Sprintf("pool-task-%d", i)
		f.turn.Status = "intent"
		f.runSeq = 0
		f.accept()
		f.delegation("queued")
		h := HandoffFact{MessageID: message.ID, RecipientID: f.member.ID, DestinationRunID: f.turn.RunID, DestinationTurnID: f.turn.ID, RetryOfTurnID: previous}
		if i > 1 {
			f.reject(TeamMessageHandoff, func(e *TeamEvent) { bad := h; bad.RetryOfTurnID = ""; e.Handoff = &bad })
		}
		f.write(TeamMessageHandoff, func(e *TeamEvent) { e.Handoff = &h })
		f.delegation("interrupted")
		f.run("terminal", map[string]string{"status": "interrupted"})
		f.turn.Status = "interrupted"
		f.write(TeamTurnTerminal, func(e *TeamEvent) { e.ActorID = "service"; e.ActorRunID = ""; e.Turn = &f.turn })
		f.member.Revision++
		f.member.Status = teams.MemberInterrupted
		f.member.TurnID = f.turn.ID
		f.member.RunID = f.turn.RunID
		f.member.OriginRunID = "parent"
		f.member.Budget.AcceptedTurns = i
		f.write(TeamMemberState, func(e *TeamEvent) { e.ActorID = "service"; e.ActorRunID = ""; e.Member = &f.member })
		previous = f.turn.ID
	}
	projection, err := ReplayTeams(f.root, f.session)
	if err != nil || len(projection.Messages) != 1 || projection.Members[f.member.ID].Budget.AcceptedTurns != 3 {
		t.Fatalf("retry recovery projection: %+v, %v", projection, err)
	}
}

func TestTeamRunPreservesAgentTaskParentInference(t *testing.T) {
	f := newTeamFixture(t)
	f.accept()
	if _, err := Append(f.root, f.session, EventToolCall, ToolCall{CallID: "d-call", Name: "run_agent"}); err != nil {
		t.Fatal(err)
	}
	start := RunStarted{RunID: "d-child", WorkKind: "session", Intent: "inspect", AgentTaskID: "d-task", AgentName: "explore", OriginRunID: "parent", OriginCallID: "d-call"}
	if _, err := Append(f.root, f.session, EventRunStarted, start); err != nil {
		t.Fatalf("team child replaced trusted parent inference: %v", err)
	}
}
