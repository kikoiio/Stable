package conversation

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// recoverTeamRuns closes accepted turns left by a process crash. Recovery
// records an interrupted outcome and never calls the provider or replays the
// model. A later explicit resume creates a new turn that can reference the
// interrupted turn and its undelivered messages.
func recoverTeamRuns(root string) error {
	dir, err := sessionlog.Prepare(root)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
		if sessionlog.ValidateID(sessionID) != nil {
			continue
		}
		transcript, replayErr := sessionlog.Replay(root, sessionID)
		if replayErr != nil {
			// Do not make an unrelated corrupt session prevent service startup.
			continue
		}
		projection, projectErr := sessionlog.ProjectTeams(transcript)
		if projectErr != nil {
			continue
		}
		if err := recoverTeamSession(root, sessionID, transcript.Events, projection); err != nil {
			return fmt.Errorf("recover team session %s: %w", sessionID, err)
		}
	}
	return nil
}

func recoverTeamSession(root, sessionID string, events []sessionlog.Event, projection sessionlog.TeamProjection) error {
	started := make(map[string]sessionlog.RunStarted)
	terminal := make(map[string]bool)
	lastRunSeq := make(map[string]uint64)
	delegations := make(map[string]sessionlog.AgentTaskDelegation)
	runStartedAt := make(map[string]time.Time)
	runFinishedAt := make(map[string]time.Time)
	for _, event := range events {
		switch event.Type {
		case sessionlog.EventRunStarted:
			var run sessionlog.RunStarted
			if decodeSessionData(event.Data, &run) == nil {
				started[run.RunID] = run
			}
		case sessionlog.EventRunEvent:
			var runEvent sessionlog.RunEvent
			if decodeSessionData(event.Data, &runEvent) != nil {
				continue
			}
			if runEvent.RunSeq > lastRunSeq[runEvent.RunID] {
				lastRunSeq[runEvent.RunID] = runEvent.RunSeq
			}
			if runEvent.Kind == string(agent.EventTerminal) {
				terminal[runEvent.RunID] = true
			}
			if runEvent.Kind == string(agent.EventDelegation) {
				var delegation sessionlog.AgentTaskDelegation
				if decodeSessionData(runEvent.Payload, &delegation) == nil {
					delegations[runEvent.RunID] = delegation
					if delegation.Status == "running" && runStartedAt[runEvent.RunID].IsZero() {
						runStartedAt[runEvent.RunID] = runEvent.At
					}
					if teamTerminalStatus(delegation.Status) {
						runFinishedAt[runEvent.RunID] = runEvent.At
					}
				}
			}
		}
	}
	// Capacity waiters are durable intent only: they have no accepted child
	// run and must never trigger a model call during startup. Convert them to
	// interrupted so a lead can explicitly resume after service recovery.
	for _, member := range projection.Members {
		if member.Status != teams.MemberWaitingCapacity {
			continue
		}
		if _, exists := projection.Teams[member.TeamID]; !exists {
			return fmt.Errorf("capacity-waiting member %s has no team", member.ID)
		}
		if projection.Teams[member.TeamID].Status == teams.TeamClosing {
			member.Status = teams.MemberStopped
		} else {
			member.Status = teams.MemberInterrupted
		}
		member.Revision++
		if err := appendTeamFactLocked(root, sessionID, member.TeamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: member.OriginRunID, Member: &member}); err != nil {
			return err
		}
	}
	intentIDs := make([]string, 0, len(projection.Turns))
	turnIDs := make([]string, 0, len(projection.Turns))
	reconciledTurnIDs := make(map[string]bool, len(projection.Turns))
	for id, turn := range projection.Turns {
		if turn.Status == "intent" {
			intentIDs = append(intentIDs, id)
		} else if turn.Status == "queued" || turn.Status == "running" {
			turnIDs = append(turnIDs, id)
		}
	}
	for _, turnID := range intentIDs {
		turn := projection.Turns[turnID]
		teamID := teamForTurn(projection, turnID)
		team, ok := projection.Teams[teamID]
		if !ok {
			return fmt.Errorf("unaccepted turn intent %s has no team", turnID)
		}
		member, ok := projection.Members[turn.MemberID]
		if !ok || member.TeamID != team.ID {
			return fmt.Errorf("unaccepted turn intent %s has no member", turnID)
		}
		turn.Status = "aborted"
		if err := appendTeamFactLocked(root, sessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAborted, ActorID: "service", ActorRunID: turn.OriginRunID, Turn: &turn}); err != nil {
			return err
		}
		if member.Status == teams.MemberCreated {
			member.Status = teams.MemberInterrupted
			member.Revision++
			if err := appendTeamFactLocked(root, sessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: turn.OriginRunID, Member: &member}); err != nil {
				return err
			}
		}
	}
	for _, turnID := range turnIDs {
		turn := projection.Turns[turnID]
		team, ok := projection.Teams[teamForTurn(projection, turnID)]
		if !ok {
			return fmt.Errorf("accepted turn %s has no team", turnID)
		}
		member, ok := projection.Members[turn.MemberID]
		if !ok || member.TeamID != team.ID {
			return fmt.Errorf("accepted turn %s has no member", turnID)
		}
		reconciledTurnIDs[turnID] = true
		hadDurableChildTerminal := terminal[turn.RunID]
		if _, ok := started[turn.RunID]; !ok {
			work := sessionlog.RunStarted{RunID: turn.RunID, WorkKind: team.Scope.WorkKind, GoalID: team.Scope.GoalID, WorkItemID: team.Scope.WorkItemID, Intent: "recovered interrupted team turn", TeamID: team.ID, TeamMemberID: member.ID, TeamTurnID: turn.ID, WorkspaceID: turn.WorkspaceID, WorkspaceGeneration: turn.WorkspaceGeneration, OriginRunID: turn.OriginRunID, OriginCallID: turn.OriginCallID}
			if _, err := sessionlog.Append(root, sessionID, sessionlog.EventRunStarted, work); err != nil {
				return err
			}
			started[turn.RunID] = work
		}
		prior := delegations[turn.RunID]
		if !terminal[turn.RunID] {
			seq := lastRunSeq[turn.RunID]
			if prior.BatchID == "" {
				batchID, err := sessionlog.NewID()
				if err != nil {
					return err
				}
				prior = sessionlog.AgentTaskDelegation{SessionID: sessionID, BatchID: batchID, TaskID: turn.TaskID, TaskName: member.Name, Status: "queued", UpdatedAt: time.Now().UTC()}
				if err := appendRecoveredTeamDelegation(root, sessionID, turn.RunID, &seq, prior); err != nil {
					return err
				}
			}
			if !teamTerminalStatus(prior.Status) {
				prior.Status = string(agent.DelegationInterrupted)
				prior.Error = "service restarted before team turn completed"
				prior.UpdatedAt = time.Now().UTC()
				if err := appendRecoveredTeamDelegation(root, sessionID, turn.RunID, &seq, prior); err != nil {
					return err
				}
			}
			id, err := sessionlog.NewID()
			if err != nil {
				return err
			}
			runStatus := string(agent.RunInterrupted)
			switch prior.Status {
			case string(agent.DelegationSucceeded):
				runStatus = string(agent.RunCompleted)
			case string(agent.DelegationFailed):
				runStatus = string(agent.RunFailed)
			case string(agent.DelegationCanceled):
				runStatus = string(agent.RunCancelled)
			}
			payload := map[string]string{"status": runStatus, "reason": "service restarted before team turn completed"}
			event := sessionlog.RunEvent{ID: id, RunID: turn.RunID, SessionID: sessionID, RunSeq: seq + 1, At: time.Now().UTC(), Kind: string(agent.EventTerminal), Payload: payload}
			if _, err := sessionlog.Append(root, sessionID, sessionlog.EventRunEvent, event); err != nil {
				return err
			}
			terminal[turn.RunID] = true
			lastRunSeq[turn.RunID] = seq + 1
		}
		turn.Status, turn.Error = prior.Status, prior.Error
		if !runStartedAt[turn.RunID].IsZero() {
			end := runFinishedAt[turn.RunID]
			if end.IsZero() || end.Before(runStartedAt[turn.RunID]) {
				end = time.Now().UTC()
			}
			elapsed := end.Sub(runStartedAt[turn.RunID])
			if elapsed < 0 {
				elapsed = 0
			}
			if remaining := member.Budget.RemainingDuration(); elapsed > remaining {
				elapsed = remaining
			}
			turn.Elapsed = elapsed
		}
		if err := appendTeamFactLocked(root, sessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnTerminal, ActorID: "service", ActorRunID: turn.OriginRunID, Turn: &turn}); err != nil {
			return err
		}
		projection, err := sessionlog.ReplayTeams(root, sessionID, team.ID)
		if err != nil {
			return err
		}
		member = projection.Members[member.ID]
		if member.Status == teams.MemberStopping && hadDurableChildTerminal {
			// The child may have durably exited just before the process stopped,
			// while its watcher had not yet applied the already accepted stop.
			// Preserve that decision when reconciling the terminal turn.
			member.Status = teams.MemberStopped
		} else if turn.Status == string(agent.DelegationSucceeded) {
			member.Status = teams.MemberIdle
		} else {
			member.Status = teams.MemberInterrupted
		}
		if member.Status == teams.MemberIdle && member.PlanRequired && !member.PlanApproved {
			member.Status = teams.MemberAwaitingPlan
		}
		if member.Budget.CanAccept() != nil && !member.Status.IsTerminal() {
			member.Status = teams.MemberBudgetExhausted
		}
		member.Revision++
		member.RunID, member.TurnID = turn.RunID, turn.ID
		if err := appendTeamFactLocked(root, sessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: turn.OriginRunID, Member: &member}); err != nil {
			return err
		}
		projection, err = sessionlog.ReplayTeams(root, sessionID, team.ID)
		if err != nil {
			return err
		}
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID)
	if err != nil {
		return err
	}
	// A crash can land after run/turn terminal facts but before the member idle
	// or stopped fact. Reconcile those gaps independently so repeated startup
	// recovery never leaves a terminal turn displayed as queued/running.
	for _, member := range projection.Members {
		if !member.Status.HasTurn() {
			if reconciledTurnIDs[member.TurnID] {
				// The turn loop above already reconciled this member from the
				// durable child outcome. Do not reinterpret its newly idle state
				// as a live member that needs interruption.
				continue
			}
			if member.Status != teams.MemberIdle && member.Status != teams.MemberAwaitingPlan && member.Status != teams.MemberCreated {
				continue
			}
			member.Status = teams.MemberInterrupted
			member.Revision++
			if err := appendTeamFactLocked(root, sessionID, member.TeamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: member.OriginRunID, Member: &member}); err != nil {
				return err
			}
			continue
		}
		turn, ok := projection.Turns[member.TurnID]
		if !ok || !turnTerminalTeamStatus(turn.Status) {
			continue
		}
		switch {
		case member.Status == teams.MemberStopping:
			member.Status = teams.MemberStopped
		case turn.Status == string(agent.DelegationSucceeded):
			member.Status = teams.MemberIdle
			if member.PlanRequired && !member.PlanApproved {
				member.Status = teams.MemberAwaitingPlan
			}
		default:
			member.Status = teams.MemberInterrupted
		}
		if member.Budget.CanAccept() != nil && member.Status != teams.MemberStopped {
			member.Status = teams.MemberBudgetExhausted
		}
		member.Revision++
		if err := appendTeamFactLocked(root, sessionID, member.TeamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: turn.OriginRunID, Member: &member}); err != nil {
			return err
		}
	}
	projection, err = sessionlog.ReplayTeams(root, sessionID)
	if err != nil {
		return err
	}
	teamIDs := make([]string, 0, len(projection.Teams))
	for id := range projection.Teams {
		teamIDs = append(teamIDs, id)
	}
	sort.Strings(teamIDs)
	for _, teamID := range teamIDs {
		team := projection.Teams[teamID]
		if team.Status != teams.TeamClosing {
			continue
		}
		teamProjection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err != nil {
			return err
		}
		active := false
		for _, member := range teamProjection.Members {
			if member.TeamID != teamID || member.Status.IsTerminal() {
				continue
			}
			if member.Status.HasTurn() {
				active = true
				continue
			}
			member.Status = teams.MemberStopped
			member.Revision++
			if err := appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: member.OriginRunID, Member: &member}); err != nil {
				return err
			}
		}
		teamProjection, err = sessionlog.ReplayTeams(root, sessionID, teamID)
		if err != nil {
			return err
		}
		for _, member := range teamProjection.Members {
			if member.TeamID == teamID && member.Status.HasTurn() {
				active = true
			}
		}
		if active {
			continue
		}
		team = teamProjection.Teams[teamID]
		team.Status = teams.TeamClosed
		team.Revision++
		id, err := sessionlog.NewID()
		if err != nil {
			return err
		}
		if _, err := sessionlog.Append(root, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: id, TeamID: teamID, SessionID: sessionID, Kind: sessionlog.TeamClosed, Revision: team.Revision, ActorID: "service", Team: &team}); err != nil {
			return err
		}
	}
	return nil
}

func appendRecoveredTeamDelegation(root, sessionID, runID string, seq *uint64, delegation sessionlog.AgentTaskDelegation) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	*seq++
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{ID: id, RunID: runID, SessionID: sessionID, RunSeq: *seq, At: time.Now().UTC(), Kind: string(agent.EventDelegation), Payload: delegation})
	return err
}

func teamForTurn(projection sessionlog.TeamProjection, turnID string) string {
	turn := projection.Turns[turnID]
	if member, ok := projection.Members[turn.MemberID]; ok {
		return member.TeamID
	}
	return ""
}
