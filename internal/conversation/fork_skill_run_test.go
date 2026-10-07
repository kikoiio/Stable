package conversation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
)

type forkSkillFixtureProvider struct{}

func (forkSkillFixtureProvider) Stream(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
	events := make(chan llm.Event)
	errors := make(chan error)
	close(events)
	close(errors)
	return events, errors
}

type forkSkillFixtureExecutorFactory struct{}

func (forkSkillFixtureExecutorFactory) ForRun(agent.ExecutionRequest) (agent.RunExecutor, error) {
	return &agent.FakeExecutor{}, nil
}

type reportingForkSkillFixtureDelegator struct{ reporter *DelegationEventReporter }

func (d reportingForkSkillFixtureDelegator) RunBatch(ctx context.Context, parent agent.ParentRun, tasks []agent.DelegationTask) ([]agent.DelegationResult, error) {
	for _, status := range []agent.DelegationStatus{agent.DelegationQueued, agent.DelegationRunning, agent.DelegationSucceeded} {
		if err := d.reporter.Publish(parent.RunID, agent.DelegationEvent{
			BatchID: "batch", TaskID: tasks[0].ID, TaskName: tasks[0].Name, Status: status,
			Summary: "review complete",
		}); err != nil {
			return nil, err
		}
	}
	return []agent.DelegationResult{{
		TaskID: tasks[0].ID, ChildRunID: "child-run", Name: tasks[0].Name, Status: agent.DelegationSucceeded, Summary: "review complete",
	}}, nil
}

type returningForkSkillFixtureDelegator struct{}

func (returningForkSkillFixtureDelegator) RunBatch(_ context.Context, _ agent.ParentRun, tasks []agent.DelegationTask) ([]agent.DelegationResult, error) {
	return []agent.DelegationResult{{
		TaskID: tasks[0].ID, ChildRunID: "child-load-run", Name: tasks[0].Name,
		Status: agent.DelegationSucceeded, Summary: "found three relevant files",
	}}, nil
}

func TestSlashForkSkillRunPersistsAndStreamsProgressAndTerminal(t *testing.T) {
	svc, gate, root, sessionID, _, _ := newSkillFixture(t, nil, map[string]map[string]string{
		"review": {"SKILL.md": "---\nname: review\nmode: fork\nfork_context: none\n---\n\nReview the requested files.\n"},
	})
	svc.lifeCtx = context.Background()
	reporter := NewDelegationEventReporter()
	reporter.Bind(svc)
	svc.deps.Delegator = reportingForkSkillFixtureDelegator{reporter: reporter}
	svc.deps.ForkProvider = forkSkillFixtureProvider{}
	svc.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	svc.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}

	updates := make(chan ServerMsg, 16)
	svc.mu.Lock()
	svc.clients[updates] = &clientSubscription{ch: updates}
	svc.mu.Unlock()
	if err := svc.invokeSkill(context.Background(), ClientMsg{
		Op: "skill_invoke", SessionID: sessionID, SkillName: "review", SkillArgs: "src/",
	}, updates); err != nil {
		t.Fatal(err)
	}

	var runID string
	deadline := time.After(3 * time.Second)
	for {
		select {
		case message := <-updates:
			if message.Type == "run_started" {
				runID = message.RunID
			}
			if message.Type == "run_outcome" {
				if runID == "" {
					runID = message.RunID
				}
				if message.Outcome == nil || message.Outcome.Status != agent.RunCompleted {
					t.Fatalf("fork outcome = %+v", message.Outcome)
				}
				goto completed
			}
		case <-deadline:
			t.Fatal("timed out waiting for fork run terminal")
		}
	}

completed:
	if runID == "" {
		t.Fatal("fork run did not expose a run ID")
	}
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var started, invoked, progress, terminal bool
	for _, event := range transcript.Events {
		switch event.Type {
		case sessionlog.EventRunStarted:
			var value sessionlog.RunStarted
			if decodeSessionData(event.Data, &value) == nil && value.RunID == runID && value.ForkSkill == "review" {
				started = true
			}
		case sessionlog.EventSkillInvoked:
			var value sessionlog.SkillInvoked
			if decodeSessionData(event.Data, &value) == nil && value.RunID == runID && value.Mode == sessionlog.SkillModeFork {
				invoked = true
			}
		case sessionlog.EventRunEvent:
			var value sessionlog.RunEvent
			if decodeSessionData(event.Data, &value) != nil || value.RunID != runID {
				continue
			}
			if value.Kind == string(agent.EventDelegation) {
				progress = true
			}
			if value.Kind == string(agent.EventTerminal) {
				var payload struct {
					Status     agent.RunStatus `json:"status"`
					ChildRunID string          `json:"child_run_id"`
					Summary    string          `json:"summary"`
				}
				if decodeSessionData(value.Payload, &payload) == nil && payload.Status == agent.RunCompleted && payload.ChildRunID == "child-run" && payload.Summary == "review complete" {
					terminal = true
				}
			}
		}
	}
	if !started || !invoked || !progress || !terminal {
		t.Fatalf("fork persisted state started=%v invoked=%v progress=%v terminal=%v", started, invoked, progress, terminal)
	}
	infos, _ := gate.List(sessionID)
	if len(infos) == 0 {
		t.Fatal("fork skill should remain discoverable in the session")
	}
}

func TestLoadSkillForkReturnsChildResultAndAudit(t *testing.T) {
	svc, gate, root, sessionID, _, _ := newSkillFixture(t, nil, map[string]map[string]string{
		"review": {"SKILL.md": "---\nname: review\nmode: fork\nfork_context: none\n---\n\nsecret skill body\n"},
	})
	svc.deps.Delegator = returningForkSkillFixtureDelegator{}
	parent := agent.ParentRun{
		RunID: "parent-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		ProjectRoot: root, Provider: forkSkillFixtureProvider{}, ProviderName: "fixture", Model: "fixture",
	}
	ctx := agent.WithForkSkillParentRun(context.Background(), parent)
	resultText, err := gate.LoadSkill(ctx, sessionID, "review", "inspect src/")
	if err != nil {
		t.Fatal(err)
	}
	var result ForkSkillResult
	if err = json.Unmarshal([]byte(resultText), &result); err != nil {
		t.Fatalf("fork tool result is not JSON: %q: %v", resultText, err)
	}
	if result.ChildRunID != "child-load-run" || result.Status != agent.DelegationSucceeded || result.Summary != "found three relevant files" {
		t.Fatalf("fork tool result = %+v", result)
	}
	if strings.Contains(resultText, "secret skill body") {
		t.Fatal("fork skill body was returned to the parent tool result")
	}
	var invoked int
	for _, event := range skillEvents(t, root, sessionID) {
		if event.Type != sessionlog.EventSkillInvoked {
			continue
		}
		var audit sessionlog.SkillInvoked
		if err = decodeSessionData(event.Data, &audit); err != nil {
			t.Fatal(err)
		}
		if audit.Name == "review" && audit.Entry == sessionlog.SkillEntryTool &&
			audit.Mode == sessionlog.SkillModeFork && audit.RunID == parent.RunID {
			invoked++
		}
	}
	if invoked != 1 {
		t.Fatalf("fork tool audit count = %d", invoked)
	}
	_, activated := gate.List(sessionID)
	if len(activated) != 1 || activated[0] != "review" {
		t.Fatalf("fork skill activation state = %v", activated)
	}
}
