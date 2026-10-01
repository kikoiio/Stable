package conversation

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/appconfig"
	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestRedactProviderCredential(t *testing.T) {
	provider := &decision.HTTPProvider{Config: appconfig.ModelConfig{APIKey: "test-secret-token"}}
	got := redactProviderCredential("before test-secret-token after", provider)
	if got != "before [credential redacted] after" {
		t.Fatalf("credential was not redacted: %q", got)
	}
}

type recordingChat struct{ requests [][]decision.ChatMessage }

func (p *recordingChat) GenerateChat(_ context.Context, m []decision.ChatMessage) (string, error) {
	p.requests = append(p.requests, append([]decision.ChatMessage(nil), m...))
	return "response", nil
}

type proposalStub struct{}

func (proposalStub) Descriptor() core.ModelDescriptor { return core.ModelDescriptor{Provider: "fake"} }
func (proposalStub) GenerateStructured(context.Context, string, decision.SchemaID) (decision.StructuredOutput, error) {
	return decision.StructuredOutput{Data: json.RawMessage(`{"status":"ok","criteria":[{"id":"erc","kind":"kicad.erc_clean","payload":{"max_violations":0}}],"reason":"verified"}`)}, nil
}

func TestSessionChatIsolatedFromOtherSessionsAndLegacyRows(t *testing.T) {
	root := t.TempDir()
	s, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.InsertMessage(context.Background(), core.SessionMessage{ID: "legacy", Role: core.MessageRoleUser, Kind: core.MessageKindText, Text: "legacy text", Ref: "chat"}); err != nil {
		t.Fatal(err)
	}
	a, err := sessionlog.Create(root, "A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sessionlog.Create(root, "B")
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingChat{}
	svc := &Service{deps: Deps{Store: s, ChatProvider: provider}}
	if _, err = svc.handle(context.Background(), ClientMsg{Op: "chat", ProjectRoot: root, SessionID: a.ID, Text: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.handle(context.Background(), ClientMsg{Op: "chat", ProjectRoot: root, SessionID: b.ID, Text: "beta"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("requests=%d", len(provider.requests))
	}
	for _, msg := range provider.requests[0] {
		if strings.Contains(msg.Content, "beta") || strings.Contains(msg.Content, "legacy text") {
			t.Fatalf("session A leaked history: %+v", provider.requests[0])
		}
	}
	for _, msg := range provider.requests[1] {
		if strings.Contains(msg.Content, "alpha") || strings.Contains(msg.Content, "legacy text") {
			t.Fatalf("session B leaked history: %+v", provider.requests[1])
		}
	}
	if len(provider.requests[0]) < 2 || provider.requests[0][len(provider.requests[0])-1].Content != "alpha" {
		t.Fatalf("A request=%+v", provider.requests[0])
	}
	r, err := sessionlog.Replay(root, a.ID)
	if err != nil || len(r.Events) != 4 {
		t.Fatalf("A transcript events=%d err=%v", len(r.Events), err)
	}
	legacy, err := s.ListMessages(context.Background())
	if err != nil || len(legacy) != 1 || legacy[0].ID != "legacy" {
		t.Fatalf("legacy SQLite row was changed: %+v %v", legacy, err)
	}
}

func TestProposalLifecycleIsSessionScopedAndSQLiteAuthoritative(t *testing.T) {
	root := t.TempDir()
	s, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := sessionlog.Create(root, "A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sessionlog.Create(root, "B")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{Store: s, Provider: proposalStub{}}}
	msgs, err := svc.handle(context.Background(), ClientMsg{Op: "create_goal", ProjectRoot: root, SessionID: a.ID, Text: "所有 ERC 违规为零"})
	if err != nil {
		t.Fatal(err)
	}
	var proposalID string
	for _, m := range msgs {
		if m.Proposal != nil {
			proposalID = m.Proposal.ID
			if m.Proposal.SessionID != a.ID {
				t.Fatalf("proposal source %q", m.Proposal.SessionID)
			}
		}
	}
	if proposalID == "" {
		t.Fatalf("proposal missing: %+v", msgs)
	}
	if _, err = svc.handle(context.Background(), ClientMsg{Op: "reject", SessionID: b.ID, ID: proposalID}); err == nil {
		t.Fatal("cross-session reject accepted")
	}
	if _, err = svc.handle(context.Background(), ClientMsg{Op: "reject", SessionID: a.ID, ID: proposalID}); err != nil {
		t.Fatal(err)
	}
	loaded, err := svc.handle(context.Background(), ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	transcript := loaded[0].Transcript
	if transcript == nil {
		t.Fatalf("transcript missing: %+v", loaded)
	}
	var restored core.CriteriaProposal
	for _, e := range transcript.Events {
		if e.Type == sessionlog.EventProposal {
			raw, _ := json.Marshal(e.Data)
			_ = json.Unmarshal(raw, &restored)
		}
	}
	if restored.ID != proposalID || restored.Status != core.ProposalRejected {
		t.Fatalf("SQLite proposal state not restored: %+v", restored)
	}
	goals, err := s.ListGoals(context.Background())
	if err != nil || len(goals) != 0 {
		t.Fatalf("rejected proposal created goal: %+v %v", goals, err)
	}
	other, err := sessionlog.Replay(root, b.ID)
	if err != nil || len(other.Events) != 1 {
		t.Fatalf("proposal leaked into session B: %+v %v", other, err)
	}
}

func TestGoalsStayGlobalAcrossProjectSessions(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.CreateGoal(context.Background(), core.Goal{ID: "global", Objective: "global objective", AllowedRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Create(rootA, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Create(rootB, ""); err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{Store: s}}
	for _, root := range []string{rootA, rootB} {
		msgs, e := svc.handle(context.Background(), ClientMsg{Op: "session_list", ProjectRoot: root})
		if e != nil || len(msgs) != 1 || len(msgs[0].Goals) != 1 || msgs[0].Goals[0].ID != "global" {
			t.Fatalf("global goal list for %s: %+v %v", root, msgs, e)
		}
	}
}
