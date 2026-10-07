package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
)

type testDelegationProvider struct{}

func (testDelegationProvider) Stream(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
	return make(chan llm.Event), make(chan error)
}

type delegationStub struct {
	results []agent.DelegationResult
	parent  agent.ParentRun
	tasks   []agent.DelegationTask
	err     error
}

func (s *delegationStub) RunBatch(_ context.Context, parent agent.ParentRun, tasks []agent.DelegationTask) ([]agent.DelegationResult, error) {
	s.parent, s.tasks = parent, append([]agent.DelegationTask(nil), tasks...)
	return s.results, s.err
}

func TestDelegateTasksSchemaAndToolDispatch(t *testing.T) {
	schema := DelegationTasksSchema
	if schema["name"] != "delegate_tasks" {
		t.Fatalf("schema name=%v", schema["name"])
	}
	provider := testDelegationProvider{}
	delegator := &delegationStub{results: []agent.DelegationResult{{TaskID: "a", Name: "find config", Status: agent.DelegationSucceeded, Summary: "config.yaml"}}}
	formal := t.TempDir()
	authority := permission.Authority{RunID: "run-1", SessionID: "0123456789abcdef0123456789abcdef", AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(t.TempDir(), "candidate"), Mode: permission.ModeBypass}
	request := m06Request(t, authority)
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: policyGate{policy: permission.Policy{}}, Provider: provider}, WithDelegator(delegator, provider))
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolUse{ID: "call-delegate", Name: "delegate_tasks", Arguments: json.RawMessage(`{"tasks":[{"id":"a","name":"find config","instruction":"find config files"}]}`)}
	outcome, err := executor.Execute(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolSucceeded || outcome.IsError {
		t.Fatalf("outcome=%+v", outcome)
	}
	if len(delegator.tasks) != 1 || delegator.tasks[0].Instruction != "find config files" {
		t.Fatalf("delegator tasks=%+v", delegator.tasks)
	}
	if delegator.parent.RunID != request.RunID || delegator.parent.Model != request.Model || delegator.parent.ExecutorFactory == nil {
		t.Fatalf("parent context not forwarded: %+v", delegator.parent)
	}
	var authorityCopy permission.Authority
	if err = json.Unmarshal(delegator.parent.PermissionBounds, &authorityCopy); err != nil || authorityCopy.RunID != request.RunID || authorityCopy.AllowedRoot != formal {
		t.Fatalf("permission bounds=%+v err=%v", authorityCopy, err)
	}
}

func TestReadOnlyToolSchemasContainOnlyInspectionTools(t *testing.T) {
	schemas := ReadOnlyToolSchemas()
	if len(schemas) != 3 {
		t.Fatalf("read-only schemas=%+v", schemas)
	}
	want := map[string]bool{"read_file": true, "glob": true, "grep": true}
	for _, schema := range schemas {
		if !want[schema.Name] {
			t.Fatalf("unexpected child schema %q", schema.Name)
		}
		delete(want, schema.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing child schemas: %v", want)
	}
}

func TestReadOnlyExecutorRejectsWritesAndCommands(t *testing.T) {
	formal := t.TempDir()
	sentinel := filepath.Join(formal, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	authority := permission.Authority{RunID: "run-1", SessionID: "0123456789abcdef0123456789abcdef", AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidateRoot, Mode: permission.ModeBypass}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: policyGate{policy: permission.Policy{}}, Now: time.Now}, WithReadOnlyTools())
	executor, err := factory.ForRun(m06Request(t, authority))
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []llm.ToolUse{
		{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"keep.txt","content":"changed"}`)},
		{ID: "command", Name: "command", Arguments: json.RawMessage(`{"command":"touch x"}`)},
		{ID: "delegate", Name: "delegate_tasks", Arguments: json.RawMessage(`{"tasks":[]}`)},
		{ID: "mcp", Name: "mcp_call", Arguments: json.RawMessage(`{"tool":"remote"}`)},
		{ID: "network", Name: "http_request", Arguments: json.RawMessage(`{"url":"https://example.invalid"}`)},
	} {
		outcome, execErr := executor.Execute(context.Background(), call)
		if execErr != nil {
			t.Fatal(execErr)
		}
		if !outcome.IsError || outcome.Status != agent.ToolFailed {
			t.Fatalf("%s was not rejected: %+v", call.Name, outcome)
		}
	}
	if _, err = os.Stat(candidateRoot); !os.IsNotExist(err) {
		t.Fatalf("read-only executor created candidate path: err=%v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "unchanged" {
		t.Fatalf("read-only executor changed authorized root: content=%q err=%v", got, err)
	}
}
