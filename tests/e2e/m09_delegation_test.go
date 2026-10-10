package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
)

func TestM09DelegationParentChildResultsReturnToParent(t *testing.T) {
	root, db := m06NewProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	provider := &m06Agent{}
	provider.streamErr = func(request llm.Request) error {
		if len(request.Messages) > 0 && request.Messages[len(request.Messages)-1].Content == "Summarize session recovery." {
			return errors.New("controlled child provider failure")
		}
		return nil
	}
	provider.respond = func(_ int, request llm.Request) []llm.Event {
		parentTools := false
		for _, tool := range request.Tools {
			if tool.Name == "delegate_tasks" {
				parentTools = true
				break
			}
		}
		last := request.Messages[len(request.Messages)-1]
		if parentTools {
			if len(last.ToolResults) > 0 {
				return m06TextRound("Two read-only investigations completed.")
			}
			return m06ToolRound("m09-delegate", "delegate_tasks", map[string]any{"tasks": []any{
				map[string]any{"id": "config", "name": "Find configuration", "instruction": "Find the configuration entry point."},
				map[string]any{"id": "recovery", "name": "Inspect recovery", "instruction": "Summarize session recovery."},
			}})
		}
		if len(last.ToolResults) > 0 {
			return m06TextRound("Child read-only summary.")
		}
		return m06ToolRound(fmt.Sprintf("m09-glob-%d", time.Now().UnixNano()), "glob", map[string]any{"pattern": "*.go"})
	}

	reporter := conversation.NewDelegationEventReporter()
	delegator, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), agent.StreamingChildRunner{}, reporter)
	if err != nil {
		t.Fatal(err)
	}
	helper := m06HelperPath(t)
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Sandbox: sandboxNewForM09(), Gate: execution.StorePermissionGate{Store: db},
		Approvals: db, Candidates: db, HelperPath: helper, SessionRoot: root,
		Now: time.Now, Provider: provider,
	}, execution.WithDelegator(delegator, provider))
	schemas := m06ToolSchemas()
	schemas = append(schemas, execution.DelegationToolSchemas()...)
	runner := agent.NewRunner(provider, agent.RunnerOptions{ExecutorFactory: factory, ToolSchemas: schemas, MaxRetries: -1})
	socket := filepath.Join(filepath.Dir(root), fmt.Sprintf("chat-m09-%d.sock", time.Now().UnixNano()))
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ChatProvider: provider, Runner: runner, ExecutorFactory: factory,
		ToolSchemas: schemas, ProviderName: "fixture", Model: "fixture", ProjectRoot: root,
		SocketPath: socket, PollEvery: 100 * time.Millisecond,
	})
	if err != nil {
		delegator.Close()
		cancel()
		t.Fatal(err)
	}
	reporter.Bind(svc)
	t.Cleanup(func() { svc.Close(); delegator.Close(); cancel() })
	sessionID := m06SessionCreate(t, ctx, &m06Env{root: root, db: db, agent: provider, socket: socket, svc: svc, cancel: cancel})
	stream := m06StartRunWithText(t, ctx, &m06Env{root: root, db: db, agent: provider, socket: socket, svc: svc, cancel: cancel}, sessionID, "m09-parent", "/delegate inspect project")
	outcome := stream.waitOutcome(t, 10*time.Second)
	if outcome.Status != agent.RunCompleted {
		t.Fatalf("parent outcome=%+v", outcome)
	}
	if provider.roundCount() != 5 {
		t.Fatalf("provider rounds=%d, want parent tool call, one child tool/summary pair, one child failure, and parent summary", provider.roundCount())
	}
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var delegationEvents int
	var toolResult string
	for _, event := range transcript.Events {
		if event.Type == sessionlog.EventRunEvent {
			var runEvent sessionlog.RunEvent
			m06Decode(t, event.Data, &runEvent)
			if runEvent.Kind == string(agent.EventDelegation) {
				delegationEvents++
			}
		}
		if event.Type == sessionlog.EventToolResult {
			var result sessionlog.ToolResult
			m06Decode(t, event.Data, &result)
			if result.CallID == "m09-delegate" {
				toolResult, _ = result.Result.(string)
			}
		}
	}
	var results []agent.DelegationResult
	if err := json.Unmarshal([]byte(toolResult), &results); err != nil {
		t.Fatalf("decode parent delegation result %q: %v", toolResult, err)
	}
	if delegationEvents < 6 || len(results) != 2 || results[0].Status != agent.DelegationSucceeded || results[1].Status != agent.DelegationFailed || results[1].Error == "" {
		t.Fatalf("delegation events=%d results=%+v", delegationEvents, results)
	}
	allowed := map[string]bool{"read_file": true, "glob": true, "grep": true}
	childSchemas := provider.roundTools(2)
	if len(childSchemas) != 3 {
		t.Fatalf("child tool schema count=%d, want 3: %+v", len(childSchemas), childSchemas)
	}
	for _, schema := range childSchemas {
		if !allowed[schema.Name] {
			t.Fatalf("child tool schema escaped read-only allowlist: %+v", childSchemas)
		}
	}
}

func sandboxNewForM09() sandbox.SandboxManager { return sandbox.New() }
