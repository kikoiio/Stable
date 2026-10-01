package demo

import "testing"

func TestNewStateReturnsIndependentFixtures(t *testing.T) {
	a, b := NewState(), NewState()
	a.Sessions[0].Messages[0].Text = "changed"
	a.Sessions[0].Pending.Criteria[0].Name = "changed"
	a.Goals[0].Steering = append(a.Goals[0].Steering, Steering{Text: "changed"})
	if b.Sessions[0].Messages[0].Text == "changed" || b.Sessions[0].Pending.Criteria[0].Name == "changed" || len(b.Goals[0].Steering) != 0 {
		t.Fatal("fixture state shares mutable slices")
	}
}

func TestSessionSwitchKeepsGlobalGoalsAndPendingProposalIsolated(t *testing.T) {
	s := NewState()
	goalCount := len(s.Goals)
	proposalID := s.Sessions[0].Pending.ID
	s = Apply(s, Action{Type: SelectSession, ID: "session-notes"})
	if len(s.Goals) != goalCount || s.ActiveSessionID != "session-notes" {
		t.Fatalf("switch changed global goals or failed to change session: %+v", s)
	}
	if s.Sessions[1].Pending != nil || s.Sessions[0].Pending.ID != proposalID {
		t.Fatal("pending proposal leaked across sessions")
	}
	unchanged := Apply(s, Action{Type: SelectSession, ID: "missing"})
	if unchanged.ActiveSessionID != s.ActiveSessionID || len(unchanged.Goals) != goalCount {
		t.Fatal("unknown session changed state")
	}
}

func TestConfirmProposalCreatesOneGlobalGoalWithSource(t *testing.T) {
	s := NewState()
	before := len(s.Goals)
	proposalID := s.Sessions[0].Pending.ID
	s = Apply(s, Action{Type: ConfirmProposal, ID: proposalID})
	if len(s.Goals) != before+1 {
		t.Fatalf("got %d goals, want %d", len(s.Goals), before+1)
	}
	created := s.Goals[len(s.Goals)-1]
	if !created.Demo || created.SourceSessionID != "session-electronics" {
		t.Fatalf("bad source or demo marker: %+v", created)
	}
	s = Apply(s, Action{Type: ConfirmProposal, ID: proposalID})
	if len(s.Goals) != before+1 {
		t.Fatal("repeated confirmation created a duplicate goal")
	}
}

func TestRejectProposalDoesNotCreateGoal(t *testing.T) {
	s := NewState()
	before := len(s.Goals)
	s = Apply(s, Action{Type: RejectProposal, ID: s.Sessions[0].Pending.ID})
	if len(s.Goals) != before || s.Sessions[0].Pending != nil {
		t.Fatal("rejection did not close the proposal cleanly")
	}
	if got := s.Sessions[0].Messages[len(s.Sessions[0].Messages)-1]; !got.Demo || got.Role != "system" {
		t.Fatalf("missing demo rejection record: %+v", got)
	}
}

func TestSubmitProposalAndSteeringKeepSessionAndGoalAttribution(t *testing.T) {
	s := NewState()
	s = Apply(s, Action{Type: SubmitGoal, Text: "  新目标  "})
	proposal := s.Sessions[0].Pending
	if proposal == nil || proposal.Description != "新目标" || proposal.SourceSessionID != s.ActiveSessionID || !proposal.Demo {
		t.Fatalf("unexpected proposal: %+v", proposal)
	}
	s = Apply(s, Action{Type: ConfirmProposal, ID: proposal.ID})
	goal := s.Goals[len(s.Goals)-1]
	s = Apply(s, Action{Type: SendSteering, Text: "优先检查连接器"})
	updatedGoal := findGoal(&s, goal.ID)
	if updatedGoal == nil || len(updatedGoal.Steering) != 1 || updatedGoal.Steering[0].SourceSessionID != "session-electronics" || !updatedGoal.Steering[0].Demo {
		t.Fatalf("steering attribution is incorrect: %+v", updatedGoal)
	}
	if got := s.Sessions[0].Messages[len(s.Sessions[0].Messages)-1]; got.GoalID != updatedGoal.ID || !got.Demo {
		t.Fatalf("chat steering message is not linked to goal: %+v", got)
	}
}

func TestSteeringWithoutValidGoalDoesNothing(t *testing.T) {
	s := NewState()
	s.SelectedGoalID = "missing"
	before := len(s.Sessions[0].Messages)
	s = Apply(s, Action{Type: SendSteering, Text: "ignored"})
	if len(s.Sessions[0].Messages) != before {
		t.Fatal("steering was recorded without a valid selected goal")
	}
	for _, goal := range s.Goals {
		if len(goal.Steering) != 0 {
			t.Fatal("goal changed without a valid selection")
		}
	}
}
