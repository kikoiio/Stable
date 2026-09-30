package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"stable/internal/core"
)

func TestMessageLifecycle(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)

	m1, err := s.InsertMessage(ctx, core.SessionMessage{ID: "msg-1", GoalID: "goal-1", Role: core.MessageRoleUser, Kind: core.MessageKindText, Text: "focus on J1", CreatedAt: base})
	if err != nil {
		t.Fatal(err)
	}
	if !m1.CreatedAt.Equal(base) {
		t.Fatalf("created_at overwritten: %v", m1.CreatedAt)
	}
	if _, err = s.InsertMessage(ctx, core.SessionMessage{ID: "msg-q", GoalID: "goal-1", Role: core.MessageRoleAgent, Kind: core.MessageKindQuestion, Text: "which net?", CreatedAt: base.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.InsertMessage(ctx, core.SessionMessage{ID: "msg-bad", Role: "robot", Kind: core.MessageKindText}); err == nil {
		t.Fatal("invalid role accepted")
	}
	if _, err = s.InsertMessage(ctx, core.SessionMessage{ID: "msg-bad2", Role: core.MessageRoleUser, Kind: "poem"}); err == nil {
		t.Fatal("invalid kind accepted")
	}

	all, err := s.ListMessages(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %d %v", len(all), err)
	}
	undelivered, err := s.UndeliveredMessages(ctx, "goal-1")
	if err != nil || len(undelivered) != 1 || undelivered[0].ID != "msg-1" {
		t.Fatalf("undelivered: %+v %v", undelivered, err)
	}
	if err = s.MarkMessagesDelivered(ctx, []string{"msg-1", "msg-1"}); err != nil {
		t.Fatal(err)
	}
	undelivered, _ = s.UndeliveredMessages(ctx, "goal-1")
	if len(undelivered) != 0 {
		t.Fatalf("still undelivered: %+v", undelivered)
	}

	q, found, err := s.UnansweredQuestion(ctx, "goal-1")
	if err != nil || !found || q.ID != "msg-q" {
		t.Fatalf("question: %v %v %v", q, found, err)
	}
	if _, err = s.InsertMessage(ctx, core.SessionMessage{ID: "msg-r", GoalID: "goal-1", Role: core.MessageRoleUser, Kind: core.MessageKindReply, Text: "J1.2", CreatedAt: base.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ = s.UnansweredQuestion(ctx, "goal-1"); found {
		t.Fatal("question still unanswered after reply")
	}
}

func TestProposalLifecycle(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	criteria := []core.Criterion{
		{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)},
		{ID: "conn", Kind: core.CriterionKindConnectionPresent, Payload: json.RawMessage(`{"endpoint_a":"RT1.2","endpoint_b":"J1.2"}`)},
	}

	// Session-level proposal precedes goal creation: goal_id stays empty.
	p, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "prop-1", Criteria: criteria, RawText: "ERC 全过，J1 连上"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != core.ProposalPending {
		t.Fatalf("status: %q", p.Status)
	}
	got, err := s.GetProposal(ctx, "prop-1")
	if err != nil || got.GoalID != "" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err = s.SetProposalStatus(ctx, "prop-1", core.ProposalSuperseded); err == nil {
		t.Fatal("direct supersede accepted")
	}
	if _, err = s.SetProposalStatus(ctx, "prop-1", core.ProposalConfirmed); err != nil {
		t.Fatal(err)
	}
	if err = s.AttachProposalGoal(ctx, "prop-1", "goal-1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetProposal(ctx, "prop-1")
	if got.GoalID != "goal-1" || got.Status != core.ProposalConfirmed {
		t.Fatalf("attached: %+v", got)
	}

	// A second confirmation for the same goal supersedes the first.
	if _, err = s.InsertProposal(ctx, core.CriteriaProposal{ID: "prop-2", GoalID: "goal-1", Criteria: criteria[:1], RawText: "只要 ERC"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetProposalStatus(ctx, "prop-2", core.ProposalConfirmed); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetProposal(ctx, "prop-1")
	if got.Status != core.ProposalSuperseded {
		t.Fatalf("old proposal not superseded: %q", got.Status)
	}
	proposals, err := s.GoalProposals(ctx, "goal-1")
	if err != nil || len(proposals) != 2 {
		t.Fatalf("goal proposals: %d %v", len(proposals), err)
	}
	if _, err = s.SetProposalStatus(ctx, "prop-2", core.ProposalRejected); err == nil {
		t.Fatal("re-confirm accepted")
	}
	if _, err = s.SetProposalStatus(ctx, "missing", core.ProposalRejected); err == nil {
		t.Fatal("missing proposal accepted")
	}
}

func TestUpdateGoalCriteria(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	criteria := []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)}}
	rev, err := s.UpdateGoalCriteria(ctx, "goal-1", criteria)
	if err != nil || rev != 1 {
		t.Fatalf("update: %d %v", rev, err)
	}
	rev, err = s.UpdateGoalCriteria(ctx, "goal-1", criteria)
	if err != nil || rev != 2 {
		t.Fatalf("bump: %d %v", rev, err)
	}
	g, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Goal.CriteriaRevision != 2 || len(g.Goal.Criteria) != 1 {
		t.Fatalf("snapshot: %+v", g.Goal)
	}
	if _, err = s.UpdateGoalCriteria(ctx, "goal-1", []core.Criterion{{ID: "x", Kind: "kicad.beautiful"}}); err == nil {
		t.Fatal("out-of-vocabulary criteria accepted")
	}
	if _, err = s.UpdateGoalCriteria(ctx, "missing", criteria); err == nil {
		t.Fatal("missing goal accepted")
	}
}
