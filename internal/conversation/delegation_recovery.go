package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

type recoveredToolCall struct {
	call  sessionlog.ToolCall
	runID string
	seq   uint64
}

type recoveredDelegationEvent struct {
	runID      string
	sessionSeq uint64
	event      agent.DelegationEvent
}

type recoveredForkSkill struct {
	name  string
	entry string
}

func recoverDelegationRuns(root string) error {
	dir, err := sessionlog.Prepare(root)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		sessionID := strings.TrimSuffix(name, filepath.Ext(name))
		if sessionlog.ValidateID(sessionID) != nil {
			continue
		}
		transcript, replayErr := sessionlog.Replay(root, sessionID)
		if replayErr != nil {
			// Recovery must not make the service unavailable because an
			// unrelated session log is already corrupt or unreadable.
			continue
		}
		if err = recoverSessionDelegation(root, sessionID, transcript.Events); err != nil {
			return fmt.Errorf("session %s: %w", sessionID, err)
		}
	}
	return nil
}

func recoverSessionDelegation(root, sessionID string, events []sessionlog.Event) error {
	var err error
	started := map[string]sessionlog.RunStarted{}
	forkSkillOwners := map[string]recoveredForkSkill{}
	lastSeq := map[string]uint64{}
	terminal := map[string]bool{}
	var delegations []recoveredDelegationEvent
	hasDelegations := map[string]bool{}
	var activeRun string
	pending := map[string]recoveredToolCall{}
	for _, event := range events {
		switch event.Type {
		case sessionlog.EventRunStarted:
			var run sessionlog.RunStarted
			if decodeSessionData(event.Data, &run) == nil && run.RunID != "" {
				started[run.RunID] = run
				activeRun = run.RunID
			}
		case sessionlog.EventToolCall:
			var call sessionlog.ToolCall
			if decodeSessionData(event.Data, &call) == nil && call.CallID != "" {
				pending[call.CallID] = recoveredToolCall{call: call, runID: activeRun, seq: event.Seq}
			}
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			if decodeSessionData(event.Data, &result) == nil {
				delete(pending, result.CallID)
			}
		case sessionlog.EventSkillInvoked:
			var invoked sessionlog.SkillInvoked
			if decodeSessionData(event.Data, &invoked) == nil && invoked.Mode == sessionlog.SkillModeFork && invoked.Entry == sessionlog.SkillEntryTool && invoked.RunID != "" {
				forkSkillOwners[invoked.RunID] = recoveredForkSkill{name: invoked.Name, entry: invoked.Entry}
			}
		case sessionlog.EventRunEvent:
			var runEvent sessionlog.RunEvent
			if decodeSessionData(event.Data, &runEvent) != nil {
				continue
			}
			if runEvent.RunSeq > lastSeq[runEvent.RunID] {
				lastSeq[runEvent.RunID] = runEvent.RunSeq
			}
			switch runEvent.Kind {
			case string(agent.EventTerminal):
				terminal[runEvent.RunID] = true
				if activeRun == runEvent.RunID {
					activeRun = ""
				}
			case string(agent.EventDelegation):
				var collaboration agent.DelegationEvent
				if decodeSessionData(runEvent.Payload, &collaboration) == nil && collaboration.BatchID != "" && collaboration.TaskID != "" {
					delegations = append(delegations, recoveredDelegationEvent{runID: runEvent.RunID, sessionSeq: event.Seq, event: collaboration})
					hasDelegations[runEvent.RunID] = true
				}
			}
		}
	}
	var openDelegations []recoveredToolCall
	var openForkSkills []recoveredToolCall
	for _, item := range pending {
		if item.call.Name == "delegate_tasks" && item.runID != "" && !terminal[item.runID] {
			openDelegations = append(openDelegations, item)
		}
		if item.call.Name == "load_skill" && item.runID != "" && !terminal[item.runID] {
			if _, ok := forkSkillOwners[item.runID]; ok {
				openForkSkills = append(openForkSkills, item)
			}
		}
	}
	interruptedRuns := map[string]bool{}
	for _, item := range openDelegations {
		run := started[item.runID]
		if run.RunID == "" || run.WorkKind != string(agent.WorkSession) {
			return errors.New("pending delegation tool call has no session run")
		}
		var args struct {
			Tasks []agent.DelegationTask `json:"tasks"`
		}
		input, _ := json.Marshal(item.call.Input)
		_ = json.Unmarshal(input, &args)
		states := map[string]agent.DelegationEvent{}
		currentBatch := ""
		var latestSeq uint64
		for _, stored := range delegations {
			if stored.runID != item.runID || stored.sessionSeq <= item.seq {
				continue
			}
			if stored.sessionSeq > latestSeq {
				latestSeq, currentBatch = stored.sessionSeq, stored.event.BatchID
			}
		}
		for _, stored := range delegations {
			if stored.runID == item.runID && stored.sessionSeq > item.seq && stored.event.BatchID == currentBatch {
				states[stored.event.TaskID] = stored.event
			}
		}
		if currentBatch == "" {
			currentBatch, err = sessionlog.NewID()
			if err != nil {
				return err
			}
		}
		resultByTask := map[string]agent.DelegationResult{}
		for _, task := range args.Tasks {
			state, exists := states[task.ID]
			if exists && state.Status.IsTerminal() {
				resultByTask[task.ID] = agent.DelegationResult{TaskID: task.ID, Name: task.Name, Status: state.Status, Summary: state.Summary, Error: state.Error}
				continue
			}
			if !exists {
				state = agent.DelegationEvent{BatchID: currentBatch, TaskID: task.ID, TaskName: task.Name, Status: agent.DelegationQueued}
			}
			state.Status = agent.DelegationInterrupted
			state.Error = "service restarted before the child task completed"
			if err = appendDelegationRunEvent(root, sessionID, item.runID, &lastSeq, state); err != nil {
				return err
			}
			resultByTask[task.ID] = agent.DelegationResult{TaskID: task.ID, Name: task.Name, Status: agent.DelegationInterrupted, Error: state.Error}
		}
		results := make([]agent.DelegationResult, 0, len(args.Tasks))
		for _, task := range args.Tasks {
			if result, ok := resultByTask[task.ID]; ok {
				results = append(results, result)
			} else {
				results = append(results, agent.DelegationResult{TaskID: task.ID, Name: task.Name, Status: agent.DelegationInterrupted, Error: "service restarted before the child task completed"})
			}
		}
		encoded, _ := json.Marshal(results)
		if _, err = sessionlog.Append(root, sessionID, sessionlog.EventToolResult, sessionlog.ToolResult{
			CallID: item.call.CallID, Result: string(encoded), Error: "delegation interrupted by service restart",
		}); err != nil {
			return err
		}
		interruptedRuns[item.runID] = true
	}
	for _, item := range openForkSkills {
		owner := forkSkillOwners[item.runID]
		reason := "service restarted before the fork skill completed"
		var latest *recoveredDelegationEvent
		for i := range delegations {
			stored := &delegations[i]
			if stored.runID == item.runID && stored.sessionSeq > item.seq && (latest == nil || stored.sessionSeq > latest.sessionSeq) {
				latest = stored
			}
		}
		if latest != nil && !latest.event.Status.IsTerminal() {
			latest.event.Status = agent.DelegationInterrupted
			latest.event.Error = reason
			if err = appendDelegationRunEvent(root, sessionID, item.runID, &lastSeq, latest.event); err != nil {
				return err
			}
		}
		result := ForkSkillResult{SkillName: owner.name, Entry: owner.entry, Status: agent.DelegationInterrupted, Error: reason}
		encoded, _ := json.Marshal(result)
		if _, err = sessionlog.Append(root, sessionID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: item.call.CallID, Result: string(encoded), Error: reason}); err != nil {
			return err
		}
		interruptedRuns[item.runID] = true
	}
	for runID, run := range started {
		if run.ForkSkill != "" && !terminal[runID] {
			var latest *recoveredDelegationEvent
			for i := range delegations {
				stored := &delegations[i]
				if stored.runID == runID && !stored.event.Status.IsTerminal() && (latest == nil || stored.sessionSeq > latest.sessionSeq) {
					latest = stored
				}
			}
			if latest != nil {
				latest.event.Status = agent.DelegationInterrupted
				latest.event.Error = "service restarted before the fork skill completed"
				if err = appendDelegationRunEvent(root, sessionID, runID, &lastSeq, latest.event); err != nil {
					return err
				}
			}
			interruptedRuns[runID] = true
		}
	}
	for runID := range forkSkillOwners {
		if !terminal[runID] {
			interruptedRuns[runID] = true
		}
	}
	for runID := range hasDelegations {
		if !terminal[runID] {
			interruptedRuns[runID] = true
		}
	}
	for runID := range interruptedRuns {
		if !terminal[runID] {
			if err = appendRunTerminal(root, sessionID, runID, &lastSeq); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendDelegationRunEvent(root, sessionID, runID string, seq *map[string]uint64, event agent.DelegationEvent) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	(*seq)[runID]++
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: id, RunID: runID, SessionID: sessionID, RunSeq: (*seq)[runID], At: time.Now().UTC(),
		Kind: string(agent.EventDelegation), Payload: event,
	})
	return err
}

func appendRunTerminal(root, sessionID, runID string, seq *map[string]uint64) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	(*seq)[runID]++
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: id, RunID: runID, SessionID: sessionID, RunSeq: (*seq)[runID], At: time.Now().UTC(),
		Kind: string(agent.EventTerminal), Payload: map[string]any{"status": agent.RunInterrupted, "reason": "service restarted during delegation"},
	})
	return err
}
