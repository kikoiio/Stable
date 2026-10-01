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
	p, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "prop-1", Criteria: criteria, RawText: "ERC 全过，J1 连上", SessionID: "session-123"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != core.ProposalPending {
		t.Fatalf("status: %q", p.Status)
	}
	got, err := s.GetProposal(ctx, "prop-1")
	if err != nil || got.GoalID != "" || got.SessionID != "session-123" {
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

func TestConfirmGoalCriteria(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	criteria := []core.Criterion{
		{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)},
		{ID: "conn", Kind: core.CriterionKindConnectionPresent, Payload: json.RawMessage(`{"endpoint_a":"RT1.2","endpoint_b":"J1.2"}`)},
	}
	// Existing verified conclusion with evidence that the confirmation must
	// invalidate without rewriting the original result.
	rev0 := 0
	if _, err := s.UpdateStatus(ctx, "goal-1", 1, core.GoalVerified, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEvidence(ctx, core.Evidence{ID: "ev-old", GoalID: "goal-1", CriterionID: "erc", ArtifactID: "sha", Kind: "kicad.erc", Result: "pass", ReportPath: "/tmp/old.json", CriteriaRevision: &rev0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "prop-new", GoalID: "goal-1", Criteria: criteria, RawText: "ERC 全过且连线"}); err != nil {
		t.Fatal(err)
	}

	conf, err := s.ConfirmGoalCriteria(ctx, "prop-new")
	if err != nil {
		t.Fatal(err)
	}
	if conf.Proposal.Status != core.ProposalConfirmed || conf.Proposal.ID != "prop-new" {
		t.Fatalf("proposal: %+v", conf.Proposal)
	}
	if conf.Goal.CriteriaRevision != 1 || conf.Goal.Status != core.GoalPendingReverification || conf.Goal.Reason == "" {
		t.Fatalf("goal: %+v", conf.Goal)
	}
	if len(conf.Goal.Criteria) != 2 {
		t.Fatalf("criteria not swapped: %+v", conf.Goal.Criteria)
	}
	if conf.Event.ID != "criteria-confirm-prop-new" || conf.Event.Kind != core.EventKindCriteriaUpdate || conf.Event.Status != "pending" || conf.Event.GoalID != "goal-1" {
		t.Fatalf("event: %+v", conf.Event)
	}

	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.Status != core.GoalPendingReverification || snap.Goal.CriteriaRevision != 1 {
		t.Fatalf("snapshot goal: %+v", snap.Goal)
	}
	if len(snap.Events) != 1 {
		t.Fatalf("events: %+v", snap.Events)
	}
	if len(snap.Evidence) != 1 || snap.Evidence[0].Result != "pass" || snap.Evidence[0].InvalidatedReason == "" {
		t.Fatalf("old evidence not invalidated in place: %+v", snap.Evidence)
	}

	// Repeating the confirmation changes nothing and returns no second event.
	if _, err = s.ConfirmGoalCriteria(ctx, "prop-new"); err == nil {
		t.Fatal("duplicate confirmation accepted")
	}
	snap, _ = s.GetGoalSnapshot(ctx, "goal-1")
	if len(snap.Events) != 1 || snap.Goal.CriteriaRevision != 1 {
		t.Fatalf("duplicate confirmation mutated goal: %+v", snap.Goal)
	}
}

func TestConfirmGoalCriteriaRejections(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	valid := []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)}}
	if _, err := s.ConfirmGoalCriteria(ctx, "missing"); err == nil {
		t.Fatal("missing proposal confirmed")
	}
	// Session-level proposal without a goal cannot drive an existing goal.
	if _, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "prop-free", Criteria: valid}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmGoalCriteria(ctx, "prop-free"); err == nil {
		t.Fatal("unattached proposal confirmed")
	}
	// Out-of-vocabulary criteria leave the goal untouched.
	bad := []core.Criterion{{ID: "x", Kind: "kicad.beautiful"}}
	if _, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "prop-bad", GoalID: "goal-1", Criteria: bad}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmGoalCriteria(ctx, "prop-bad"); err == nil {
		t.Fatal("invalid criteria confirmed")
	}
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.CriteriaRevision != 0 || snap.Goal.Status != core.GoalActive || len(snap.Events) != 0 {
		t.Fatalf("rejected confirmation mutated goal: %+v", snap.Goal)
	}
	p, _ := s.GetProposal(ctx, "prop-bad")
	if p.Status != core.ProposalPending {
		t.Fatalf("invalid proposal status changed: %q", p.Status)
	}
}

func TestConfirmGoalCriteriaSupersedes(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	criteria := []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)}}
	if _, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "prop-a", GoalID: "goal-1", Criteria: criteria}); err != nil {
		t.Fatal(err)
	}
	first, err := s.ConfirmGoalCriteria(ctx, "prop-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "prop-b", GoalID: "goal-1", Criteria: criteria}); err != nil {
		t.Fatal(err)
	}
	second, err := s.ConfirmGoalCriteria(ctx, "prop-b")
	if err != nil {
		t.Fatal(err)
	}
	if second.Goal.CriteriaRevision != first.Goal.CriteriaRevision+1 {
		t.Fatalf("revision not bumped: %d -> %d", first.Goal.CriteriaRevision, second.Goal.CriteriaRevision)
	}
	if second.Event.ID == first.Event.ID {
		t.Fatal("second confirmation reused the first event")
	}
	old, _ := s.GetProposal(ctx, "prop-a")
	if old.Status != core.ProposalSuperseded {
		t.Fatalf("previous proposal not superseded: %q", old.Status)
	}
	snap, _ := s.GetGoalSnapshot(ctx, "goal-1")
	if len(snap.Events) != 2 {
		t.Fatalf("events: %+v", snap.Events)
	}
}

func TestConfirmGoalCriteriaCrashBoundary(t *testing.T) {
	s, path := newGoalStore(t)
	ctx := context.Background()
	criteria := []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)}}
	if err := s.RecordEvidence(ctx, core.Evidence{ID: "ev-1", GoalID: "goal-1", CriterionID: "erc", ArtifactID: "sha", Kind: "kicad.erc", Result: "pass", ReportPath: "/tmp/r.json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "prop-1", GoalID: "goal-1", Criteria: criteria}); err != nil {
		t.Fatal(err)
	}
	conf, err := s.ConfirmGoalCriteria(ctx, "prop-1")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	invalidated := snap.Evidence[0].InvalidatedReason
	if invalidated == "" {
		t.Fatal("evidence not invalidated")
	}
	// Process dies after commit; on restart the persisted event is replayable
	// and a retried confirmation is rejected without side effects.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	unprocessed, err := s.UnprocessedEvents(ctx)
	if err != nil || len(unprocessed) != 1 || unprocessed[0].ID != conf.Event.ID {
		t.Fatalf("event not replayable after restart: %+v %v", unprocessed, err)
	}
	if _, err = s.ConfirmGoalCriteria(ctx, "prop-1"); err == nil {
		t.Fatal("confirmation replayed after restart")
	}
	snap, err = s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.CriteriaRevision != 1 || len(snap.Events) != 1 {
		t.Fatalf("replay mutated goal: %+v events=%d", snap.Goal, len(snap.Events))
	}
	if snap.Evidence[0].InvalidatedReason != invalidated {
		t.Fatalf("invalidation reason changed on replay: %q -> %q", invalidated, snap.Evidence[0].InvalidatedReason)
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
