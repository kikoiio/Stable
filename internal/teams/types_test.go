package teams

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNamesAndTrustedWorkScope(t *testing.T) {
	if name, err := NormalizeMemberName(" Investigator-1 "); err != nil || name != "investigator-1" {
		t.Fatalf("normalized name=%q err=%v", name, err)
	}
	for _, name := range []string{"lead", "LEAD", "", "../member", "member/name", "member name", strings.Repeat("x", 65), "角色"} {
		if _, err := NormalizeMemberName(name); err == nil {
			t.Fatalf("invalid member name %q accepted", name)
		}
	}
	scope := Scope{SessionID: "session-1", WorkKind: "goal", GoalID: "goal-1", WorkItemID: "item-1", ProjectRoot: filepath.Join(t.TempDir(), "project"), ProviderName: "fake"}
	if err := scope.Validate(); err != nil {
		t.Fatal(err)
	}
	if !scope.Matches(scope) {
		t.Fatal("valid scope does not match itself")
	}
	for _, change := range []func(*Scope){
		func(s *Scope) { s.SessionID = "session-2" },
		func(s *Scope) { s.GoalID = "goal-2" },
		func(s *Scope) { s.WorkItemID = "item-2" },
		func(s *Scope) { s.ProjectRoot = filepath.Join(filepath.Dir(s.ProjectRoot), "other") },
		func(s *Scope) { s.ProviderName = "other" },
	} {
		other := scope
		change(&other)
		if scope.Matches(other) {
			t.Fatalf("different authority matched: %+v", other)
		}
	}
	for _, bad := range []Scope{
		{SessionID: "session", WorkKind: "session", GoalID: "goal", ProjectRoot: scope.ProjectRoot},
		{SessionID: "session", WorkKind: "goal", GoalID: "goal", ProjectRoot: scope.ProjectRoot},
		{SessionID: "session", WorkKind: "session", ProjectRoot: "relative"},
		{SessionID: "session", WorkKind: "unknown", ProjectRoot: scope.ProjectRoot},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid scope accepted: %+v", bad)
		}
	}
}

func TestTeamAndMemberTransitionsRejectReopeningAndSkippedExecution(t *testing.T) {
	for _, transition := range [][2]TeamStatus{{"", TeamOpen}, {TeamOpen, TeamClosing}, {TeamClosing, TeamClosed}, {TeamClosed, TeamClosed}} {
		if err := ValidateTeamTransition(transition[0], transition[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, transition := range [][2]TeamStatus{{TeamClosed, TeamOpen}, {TeamOpen, TeamClosed}, {"", TeamClosed}, {"bad", TeamOpen}} {
		if err := ValidateTeamTransition(transition[0], transition[1]); err == nil {
			t.Fatalf("illegal team transition %v accepted", transition)
		}
	}
	for _, transition := range [][2]MemberStatus{{"", MemberCreated}, {MemberCreated, MemberQueued}, {MemberQueued, MemberRunning}, {MemberRunning, MemberIdle}, {MemberIdle, MemberWaitingCapacity}, {MemberWaitingCapacity, MemberQueued}, {MemberRunning, MemberAwaitingPlan}, {MemberAwaitingPlan, MemberQueued}, {MemberRunning, MemberStopping}, {MemberStopping, MemberStopped}, {MemberIdle, MemberInterrupted}, {MemberInterrupted, MemberQueued}, {MemberIdle, MemberBudgetExhausted}} {
		if err := ValidateMemberTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("%v: %v", transition, err)
		}
	}
	for _, transition := range [][2]MemberStatus{{MemberCreated, MemberIdle}, {MemberCreated, MemberRunning}, {MemberStopped, MemberQueued}, {MemberBudgetExhausted, MemberIdle}, {MemberStopping, MemberRunning}, {MemberRunning, MemberQueued}, {MemberInterrupted, MemberRunning}, {"bad", MemberIdle}} {
		if err := ValidateMemberTransition(transition[0], transition[1]); err == nil {
			t.Fatalf("illegal member transition %v accepted", transition)
		}
	}
}

func TestMemberBudgetIsBoundedAndResumeCannotResetAccounting(t *testing.T) {
	budget := Budget{AcceptedTurns: 15, Elapsed: 9 * time.Minute}
	if err := budget.CanAccept(); err != nil {
		t.Fatal(err)
	}
	if budget.RemainingDuration() != time.Minute {
		t.Fatalf("remaining turn duration=%s", budget.RemainingDuration())
	}
	budget.AcceptedTurns++
	if err := budget.CanAccept(); err != ErrBudgetExhausted {
		t.Fatalf("turn cap: %v", err)
	}
	budget = Budget{AcceptedTurns: 1, Elapsed: MaxMemberDuration}
	if err := budget.CanAccept(); err != ErrBudgetExhausted || budget.RemainingDuration() != 0 {
		t.Fatalf("elapsed cap: %v", err)
	}
	if err := (Budget{AcceptedTurns: -1}).CanAccept(); err == nil {
		t.Fatal("negative budget accepted")
	}
	if err := (Budget{Elapsed: -time.Second}).CanAccept(); err == nil {
		t.Fatal("negative elapsed accepted")
	}
	if remaining := (Budget{}).RemainingDuration(); remaining != MaxTurnDuration {
		t.Fatalf("default duration=%s", remaining)
	}
}

func TestCapacityMessagesAndQueryLimits(t *testing.T) {
	if err := CheckCapacity(1, 3, 7, 15, true, true); err != nil {
		t.Fatal(err)
	}
	for _, counts := range [][4]int{{2, 3, 7, 15}, {1, 4, 7, 15}, {1, 3, 8, 15}, {1, 3, 7, 16}} {
		if err := CheckCapacity(counts[0], counts[1], counts[2], counts[3], true, true); err != ErrCapacity {
			t.Fatalf("counts %v: %v", counts, err)
		}
	}
	usage := PendingUsage{Deliveries: MaxTeamPending - 2, Bytes: MaxTeamPendingBytes - 2*MaxMessageBytes, Recipients: map[string]int{"a": 63, "b": 63}}
	if err := usage.CheckMessage([]string{"a", "b"}, MaxMessageBytes); err != nil {
		t.Fatal(err)
	}
	usage.Recipients["b"] = 64
	if err := usage.CheckMessage([]string{"a", "b"}, MaxMessageBytes); err != ErrCapacity {
		t.Fatalf("recipient overflow: %v", err)
	}
	usage.Recipients["b"] = 63
	usage.Deliveries++
	if err := usage.CheckMessage([]string{"a", "b"}, MaxMessageBytes); err != ErrCapacity {
		t.Fatalf("broadcast deliveries overflow: %v", err)
	}
	usage.Deliveries--
	usage.Bytes++
	if err := usage.CheckMessage([]string{"a", "b"}, MaxMessageBytes); err != ErrCapacity {
		t.Fatalf("broadcast bytes overflow: %v", err)
	}
	if err := usage.CheckMessage([]string{"a", "a"}, 1); err == nil {
		t.Fatal("duplicate broadcast recipient accepted")
	}
	if PageSize(0) != 20 || PageSize(1000) != 100 || WaitDuration(time.Minute) != 30*time.Second || WaitDuration(-1) != 0 {
		t.Fatal("query limits are not bounded")
	}
}
