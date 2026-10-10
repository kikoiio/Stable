package conversation

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

const (
	agentTaskPrivacyCredential = "sk-agent-task-private-fixture-0123456789"
	agentTaskPrivacyRoleBody   = "PRIVATE ROLE BODY: inspect the unreleased parser roadmap."
	agentTaskPrivacyTranscript = "PRIVATE CHILD TRANSCRIPT: user pasted internal customer records."
)

// The child receives its full role and transcript in memory, while the parent
// session only retains the sanitized result summary needed for replay.
func TestAgentTaskSessionReplayKeepsSummaryButNotCredentialRoleOrChildTranscript(t *testing.T) {
	role := "---\nname: privacy-reviewer\ndescription: bounded privacy fixture\n---\n" + agentTaskPrivacyRoleBody + "\n"
	runner := agentTaskTestRunner(func(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
		if !strings.Contains(input.RoleInstruction, agentTaskPrivacyRoleBody) || !strings.Contains(input.Task.Instruction, agentTaskPrivacyRoleBody) {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "fixture did not receive role body"}
		}
		// This private transcript is intentionally never returned as a result.
		childTranscript := agentTaskPrivacyTranscript
		if childTranscript == "" {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "missing private transcript"}
		}
		return agent.ChildRunResult{
			Status:  agent.DelegationSucceeded,
			Summary: "Parser entry point identified. " + agentTaskPrivacyRoleBody + " credential=" + agentTaskPrivacyCredential,
		}
	})
	svc, root, sessionID := newAgentTaskTestService(t, runner, role)
	svc.deps.ProviderCredential = agentTaskPrivacyCredential
	parent := agentTaskTestParent(t, svc, sessionID, "privacy-parent-run")
	task, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "privacy-reviewer", Instruction: "Review parser entry points."})
	if err != nil {
		t.Fatalf("start child task: %v", err)
	}
	got := waitAgentTaskTerminal(t, svc, parent, task.ID)
	if got.Status != agent.DelegationSucceeded {
		t.Fatalf("child task status = %q, want succeeded", got.Status)
	}
	if !strings.Contains(got.Summary, "Parser entry point identified.") || !strings.Contains(got.Summary, "[credential redacted]") {
		t.Fatalf("replayed task lost its safe summary context: %+v", got)
	}

	path, err := sessionlog.SessionPath(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := json.Marshal(transcript.Events)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := svc.listAgentTasks(sessionID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	for label, data := range map[string][]byte{"raw session log": raw, "replayed events": events, "agent task projection": projected} {
		for _, secret := range []string{agentTaskPrivacyCredential, agentTaskPrivacyRoleBody, agentTaskPrivacyTranscript} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s contains private child content %q", label, secret)
			}
		}
		if !strings.Contains(string(data), "Parser entry point identified.") {
			t.Errorf("%s lost the legitimate child summary", label)
		}
	}
}
