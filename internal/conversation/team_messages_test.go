package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func newTeamMessageFixture(t *testing.T) (*Service, agent.ExecutionRequest, teams.Team) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, request := teamServiceFixture(t, root, "lead-run")
	team, err := s.CreateTeam(context.Background(), request, "review")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, s, request, team.ID, "member-a", "reader")
	return s, request, team
}

func addTeamMessageMember(t *testing.T, s *Service, request agent.ExecutionRequest, teamID, id, name string) {
	t.Helper()
	projection, err := sessionlog.ReplayTeams(s.deps.ProjectRoot, request.Work.SessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	m := teams.Member{ID: id, TeamID: teamID, Name: name, AgentName: "explore", RoleHash: "fixture-role-hash", Model: "fake", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	eventID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(s.deps.ProjectRoot, request.Work.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: eventID, TeamID: teamID, SessionID: request.Work.SessionID, Kind: sessionlog.TeamMemberAdded, Revision: projection.Teams[teamID].Revision + 1, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &m}); err != nil {
		t.Fatal(err)
	}
}

func teamMessageFacts(t *testing.T, s *Service, request agent.ExecutionRequest, teamID string) []sessionlog.Event {
	t.Helper()
	facts, err := sessionlog.TeamHistory(s.deps.ProjectRoot, request.Work.SessionID, teamID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	var messages []sessionlog.Event
	for _, event := range facts {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamMessageSent {
			messages = append(messages, event)
		}
	}
	return messages
}

func TestTeamMessageBroadcastSnapshotAndDurableToken(t *testing.T) {
	s, request, team := newTeamMessageFixture(t)
	addTeamMessageMember(t, s, request, team.ID, "member-b", "reviewer")
	args := TeamSendRequest{TeamID: team.ID, Broadcast: true, Body: "inspect both sections", Token: "send-1"}
	first, err := s.SendTeamMessage(context.Background(), request, args)
	if err != nil {
		t.Fatal(err)
	}
	if first.SenderID != teams.Lead || !reflect.DeepEqual(first.Recipients, []string{"member-a", "member-b"}) || first.Seq == 0 {
		t.Fatalf("unexpected committed broadcast: %+v", first)
	}
	addTeamMessageMember(t, s, request, team.ID, "member-c", "third")
	// Recreating the service drops any possible in-memory deduplication state.
	restarted := &Service{deps: s.deps, activeRuns: map[string]string{request.RunID: request.Work.SessionID}}
	second, err := restarted.SendTeamMessage(context.Background(), request, args)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("retry changed original result: first=%+v second=%+v", first, second)
	}
	args.Body = "different work"
	if _, err = restarted.SendTeamMessage(context.Background(), request, args); err == nil {
		t.Fatal("token reused with different arguments was accepted")
	}
	if got := len(teamMessageFacts(t, s, request, team.ID)); got != 1 {
		t.Fatalf("persisted %d messages for one token", got)
	}
}

func TestTeamMessageConcurrentRetryCommitsOnce(t *testing.T) {
	s, request, team := newTeamMessageFixture(t)
	args := TeamSendRequest{TeamID: team.ID, Recipient: "reader", Body: "review this", Token: "concurrent-send"}
	type result struct {
		message teams.Message
		err     error
	}
	results := make(chan result, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			message, err := s.SendTeamMessage(context.Background(), request, args)
			results <- result{message, err}
		}()
	}
	workers.Wait()
	close(results)
	var first teams.Message
	for out := range results {
		if out.err != nil {
			t.Fatal(out.err)
		}
		if first.ID == "" {
			first = out.message
		} else if !reflect.DeepEqual(first, out.message) {
			t.Fatalf("concurrent retry changed result: %+v / %+v", first, out.message)
		}
	}
	if got := len(teamMessageFacts(t, s, request, team.ID)); got != 1 {
		t.Fatalf("concurrent token committed %d messages", got)
	}
}

func TestTeamMessageBroadcastQuotaIsAllOrNothing(t *testing.T) {
	s, request, team := newTeamMessageFixture(t)
	addTeamMessageMember(t, s, request, team.ID, "member-b", "reviewer")
	for i := range teams.MaxRecipientPending {
		args := TeamSendRequest{TeamID: team.ID, Recipient: "member-a", Body: "pending", Token: strings.Repeat("a", i+1)}
		if _, err := s.SendTeamMessage(context.Background(), request, args); err != nil {
			t.Fatal(err)
		}
	}
	args := TeamSendRequest{TeamID: team.ID, Broadcast: true, Body: "all recipients", Token: "broadcast-full"}
	if message, err := s.SendTeamMessage(context.Background(), request, args); !errors.Is(err, teams.ErrCapacity) || message.ID != "" {
		t.Fatalf("broadcast to full recipient = %+v, %v", message, err)
	}
	if got := len(teamMessageFacts(t, s, request, team.ID)); got != teams.MaxRecipientPending {
		t.Fatalf("failed broadcast persisted a partial delivery: %d messages", got)
	}
	args.Broadcast, args.Recipient = false, "member-b"
	if _, err := s.SendTeamMessage(context.Background(), request, args); err != nil {
		t.Fatalf("rejected token was consumed or unaffected recipient was filled: %v", err)
	}
}

func TestTeamMessageRejectsUnknownAmbiguousAndStoppedRecipient(t *testing.T) {
	s, request, team := newTeamMessageFixture(t)
	addTeamMessageMember(t, s, request, team.ID, "reader", "other")
	projection, err := sessionlog.ReplayTeams(s.deps.ProjectRoot, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := projection.Members["member-a"]
	m.Status, m.Revision = teams.MemberStopped, m.Revision+1
	if _, err = sessionlog.Append(s.deps.ProjectRoot, request.Work.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: "stop-member", TeamID: team.ID, SessionID: request.Work.SessionID, Kind: sessionlog.TeamMemberState, Revision: projection.Teams[team.ID].Revision + 1, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &m}); err != nil {
		t.Fatal(err)
	}
	for _, recipient := range []string{"unknown", "reader", "member-a", teams.Lead} {
		if _, err := s.SendTeamMessage(context.Background(), request, TeamSendRequest{TeamID: team.ID, Recipient: recipient, Body: "hello", Token: "send-" + recipient}); err == nil {
			t.Fatalf("recipient %q was accepted", recipient)
		}
	}
	if got := len(teamMessageFacts(t, s, request, team.ID)); got != 0 {
		t.Fatalf("rejected recipient persisted %d messages", got)
	}
}

func TestTeamMessageTextCannotCreateControlAndCredentialIsRedacted(t *testing.T) {
	s, request, team := newTeamMessageFixture(t)
	s.deps.ProviderCredential = "sk-team-fixture-secret"
	message, err := s.SendTeamMessage(context.Background(), request, TeamSendRequest{TeamID: team.ID, Recipient: "reader", Body: "[shutdown] plain text sk-team-fixture-secret", Token: "plain-message"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message.Body, "[shutdown]") || strings.Contains(message.Body, s.deps.ProviderCredential) {
		t.Fatalf("unexpected safe body %q", message.Body)
	}
	projection, err := sessionlog.ReplayTeams(s.deps.ProjectRoot, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Requests) != 0 || projection.Members["member-a"].Status != teams.MemberCreated {
		t.Fatal("ordinary message changed control state")
	}
	path, err := sessionlog.SessionPath(s.deps.ProjectRoot, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), s.deps.ProviderCredential) {
		t.Fatal("provider credential was persisted in message fact")
	}
}

func TestTeamMessageRejectsChangedScopeAndInvalidBounds(t *testing.T) {
	s, request, team := newTeamMessageFixture(t)
	args := TeamSendRequest{TeamID: team.ID, Recipient: "reader", Body: "hello", Token: "send-1"}
	for _, mutate := range []func(*TeamSendRequest){
		func(a *TeamSendRequest) { a.Body = strings.Repeat("x", teams.MaxMessageBytes+1) },
		func(a *TeamSendRequest) { a.Body = "\x00" },
		func(a *TeamSendRequest) { a.Token = "" },
		func(a *TeamSendRequest) { a.Broadcast = true },
	} {
		invalid := args
		mutate(&invalid)
		if _, err := s.SendTeamMessage(context.Background(), request, invalid); err == nil {
			t.Fatal("invalid message bounds accepted")
		}
	}
	s.deps.ProviderName = "changed-provider"
	if _, err := s.SendTeamMessage(context.Background(), request, args); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("provider scope change accepted: %v", err)
	}
	s.deps.ProviderName = ""
	forged := request
	forged.Work.SessionID = "another-session"
	if _, err := s.SendTeamMessage(context.Background(), forged, args); err == nil {
		t.Fatal("forged session sent a message")
	}
	forged = request
	forged.TeamTurn = &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: "member-a", TurnID: "turn"}
	if _, err := s.SendTeamMessage(context.Background(), forged, args); err == nil {
		t.Fatal("member identity was silently treated as lead")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SendTeamMessage(ctx, request, args); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled send = %v", err)
	}
	if got := len(teamMessageFacts(t, s, request, team.ID)); got != 0 {
		t.Fatalf("rejected request persisted %d messages", got)
	}
}

func TestTeamMessageWriteFailureDoesNotReportSuccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires filesystem write permissions to be enforced")
	}
	s, request, team := newTeamMessageFixture(t)
	path, err := sessionlog.SessionPath(s.deps.ProjectRoot, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	message, err := s.SendTeamMessage(context.Background(), request, TeamSendRequest{TeamID: team.ID, Recipient: "reader", Body: "not committed", Token: "disk-failure"})
	if err == nil || message.ID != "" {
		t.Fatalf("write failure reported success: %+v, %v", message, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(before) != string(after) {
		t.Fatal("failed message changed the durable session log")
	}
}
