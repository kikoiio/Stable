package conversation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
)

func recoverAgentTaskRuns(root string) error {
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
		transcript, err := sessionlog.Replay(root, sessionID)
		if err != nil {
			continue
		} // Preserve service availability for unrelated corrupt logs.
		session := transcript.Session
		records, err := sessionlog.AgentTasks(transcript)
		if err != nil {
			return err
		}
		for _, r := range records {
			if r.RunStatus != "" {
				continue
			}
			seq := map[string]uint64{r.Started.RunID: r.LastRunSeq}
			status := agent.DelegationStatus(r.Delegation.Status)
			if !status.IsTerminal() {
				batch := r.Delegation.BatchID
				if batch == "" {
					batch, err = sessionlog.NewID()
					if err != nil {
						return err
					}
				}
				progress := agent.DelegationEvent{BatchID: batch, TaskID: r.Started.AgentTaskID, TaskName: r.Started.AgentName, Status: agent.DelegationInterrupted, Error: "service restarted before the agent task completed"}
				if err = appendDelegationRunEvent(root, session.ID, r.Started.RunID, &seq, progress); err != nil {
					return err
				}
				status = agent.DelegationInterrupted
			}
			outcome := agent.RunInterrupted
			switch status {
			case agent.DelegationSucceeded:
				outcome = agent.RunCompleted
			case agent.DelegationFailed:
				outcome = agent.RunFailed
			case agent.DelegationCanceled:
				outcome = agent.RunCancelled
			}
			id, err := sessionlog.NewID()
			if err != nil {
				return err
			}
			if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, sessionlog.RunEvent{ID: id, RunID: r.Started.RunID, SessionID: session.ID, RunSeq: seq[r.Started.RunID] + 1, At: time.Now().UTC(), Kind: string(agent.EventTerminal), Payload: map[string]any{"status": outcome, "reason": "recovered agent task outcome"}}); err != nil {
				return err
			}
		}

		if err := recoverAgentTaskCalls(root, session.ID); err != nil {
			return err
		}
	}
	return nil
}

// Called under eventMu; durable destination references count as delivered only
// when that destination has its own run_started fact.
func (s *Service) agentTaskNotifications(request agent.ExecutionRequest) ([]llm.Message, error) {
	transcript, err := sessionlog.Replay(s.sessionProjectRoot(request.Work.SessionID), request.Work.SessionID)
	if err != nil {
		return nil, err
	}
	records, err := sessionlog.AgentTasks(transcript)
	if err != nil {
		return nil, err
	}
	refs, err := sessionlog.AgentTaskNotifications(transcript)
	if err != nil {
		return nil, err
	}
	starts := map[string]bool{}
	for _, event := range transcript.Events {
		if event.Type == sessionlog.EventRunStarted {
			var started sessionlog.RunStarted
			if decodeSessionData(event.Data, &started) == nil {
				starts[started.RunID] = true
			}
		}
	}
	delivered := map[string]bool{}
	for _, ref := range refs {
		if starts[ref.DestinationRunID] {
			delivered[fmt.Sprintf("%s/%d", ref.TaskID, ref.TerminalSeq)] = true
		}
	}
	var messages []llm.Message
	totalBytes := 0
	for _, r := range records {
		if r.TerminalSeq == 0 || r.Started.WorkKind != string(request.Work.Kind) || r.Started.GoalID != request.Work.GoalID || r.Started.WorkItemID != request.Work.WorkItemID {
			continue
		}
		key := fmt.Sprintf("%s/%d", r.Started.AgentTaskID, r.TerminalSeq)
		if delivered[key] {
			continue
		}
		snapshot := taskSnapshot(request.Work.SessionID, r)
		encoded, _ := json.Marshal(snapshot)
		content := "Agent task result (reference data):\n" + strings.TrimSpace(string(encoded))
		if len(messages) >= 20 || totalBytes+len(content) > 64<<10 {
			break
		}
		totalBytes += len(content)
		reference := sessionlog.AgentTaskNotification{TaskID: r.Started.AgentTaskID, TerminalSeq: r.TerminalSeq, DestinationRunID: request.RunID}
		if _, err = sessionlog.Append(s.deps.ProjectRoot, request.Work.SessionID, sessionlog.EventAgentTaskNotification, reference); err != nil {
			return nil, err
		}
		messages = append(messages, llm.Message{Role: "user", Content: content})
	}
	return messages, nil
}

func recoverAgentTaskCalls(root, sessionID string) error {
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		return err
	}
	records, err := sessionlog.AgentTasks(transcript)
	if err != nil {
		return err
	}
	pending := map[string]recoveredToolCall{}
	started := map[string]sessionlog.RunStarted{}
	seq := map[string]uint64{}
	terminal := map[string]bool{}
	current := ""
	for _, event := range transcript.Events {
		switch event.Type {
		case sessionlog.EventRunStarted:
			var run sessionlog.RunStarted
			if decodeSessionData(event.Data, &run) == nil {
				started[run.RunID] = run
				if run.AgentTaskID == "" {
					current = run.RunID
				}
			}
		case sessionlog.EventToolCall:
			var call sessionlog.ToolCall
			if decodeSessionData(event.Data, &call) == nil && (call.Name == "run_agent" || call.Name == "task_output" || call.Name == "task_stop") {
				owner := call.RunID
				if owner == "" {
					owner = current
				}
				pending[call.CallID] = recoveredToolCall{call: call, runID: owner, seq: event.Seq}
			}
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			if decodeSessionData(event.Data, &result) == nil {
				delete(pending, result.CallID)
			}
		case sessionlog.EventRunEvent:
			var run sessionlog.RunEvent
			if decodeSessionData(event.Data, &run) == nil {
				seq[run.RunID] = run.RunSeq
				if run.Kind == string(agent.EventTerminal) {
					terminal[run.RunID] = true
				}
			}
		}
	}
	calls := []recoveredToolCall{}
	for _, call := range pending {
		calls = append(calls, call)
	}
	// Preserve call order so repeated recovery and result pairing are stable.
	sort.Slice(calls, func(i, j int) bool { return calls[i].seq < calls[j].seq })
	for _, call := range calls {
		var result *agent.AgentTaskSnapshot
		raw, _ := json.Marshal(call.call.Input)
		var args struct {
			TaskID string `json:"task_id"`
		}
		_ = json.Unmarshal(raw, &args)
		for _, r := range records {
			if (call.call.Name == "run_agent" && r.Started.OriginRunID == call.runID && r.StartSeq > call.seq && (r.Started.OriginCallID == call.call.CallID || r.Started.OriginCallID == "")) || (call.call.Name != "run_agent" && r.Started.AgentTaskID == args.TaskID) {
				snapshot := taskSnapshot(sessionID, r)
				result = &snapshot
				break
			}
		}
		content := "agent task operation interrupted by service restart"
		if result != nil {
			encoded, _ := json.Marshal(result)
			content = string(encoded)
		}
		if _, err = sessionlog.Append(root, sessionID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: call.call.CallID, Result: content, Error: "agent task operation interrupted by service restart"}); err != nil {
			return err
		}
		if started[call.runID].RunID != "" && !terminal[call.runID] {
			if err = appendRunTerminal(root, sessionID, call.runID, &seq); err != nil {
				return err
			}
			terminal[call.runID] = true
		}
	}
	return nil
}
