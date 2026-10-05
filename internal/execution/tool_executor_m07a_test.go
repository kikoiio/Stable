package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"stable/internal/permission"
)

// fakeSkillProvider stands in for the conversation-side SkillGate in
// executor dispatch tests.
type fakeSkillProvider struct {
	body      string
	err       error
	sessionID string
	name      string
	args      string
	calls     int
	snapshot  string
	delta     string
	invErr    error
	invCalls  int
	invSid    string
}

func (p *fakeSkillProvider) LoadSkill(_ context.Context, sessionID, name, args string) (string, error) {
	p.calls++
	p.sessionID, p.name, p.args = sessionID, name, args
	if p.err != nil {
		return "", p.err
	}
	return p.body, nil
}

func (p *fakeSkillProvider) SkillInventory(_ context.Context, sessionID string) (string, string, error) {
	p.invCalls++
	p.invSid = sessionID
	return p.snapshot, p.delta, p.invErr
}

func TestLoadSkillSchemaShape(t *testing.T) {
	if got, want := LoadSkillSchema["name"], "load_skill"; got != want {
		t.Fatalf("name = %v, want %v", got, want)
	}
	description, _ := LoadSkillSchema["description"].(string)
	if description == "" {
		t.Fatal("description is empty")
	}
	input := schemaObject(t, LoadSkillSchema, "input_schema")
	if got, want := input["type"], "object"; got != want {
		t.Fatalf("input type = %v, want %v", got, want)
	}
	if required := schemaStrings(t, input, "required"); !reflect.DeepEqual(required, []string{"name"}) {
		t.Fatalf("required = %v, want [name]", required)
	}
	properties := schemaObject(t, input, "properties")
	nameSchema := schemaObject(t, properties, "name")
	if got, want := nameSchema["type"], "string"; got != want {
		t.Fatalf("name type = %v, want %v", got, want)
	}
	argsSchema := schemaObject(t, properties, "args")
	if got, want := argsSchema["type"], "string"; got != want {
		t.Fatalf("args type = %v, want %v", got, want)
	}
}

func TestSkillToolSchemasOrder(t *testing.T) {
	schemas := SkillToolSchemas()
	if len(schemas) != 1 {
		t.Fatalf("schema count = %d, want 1", len(schemas))
	}
	if got, want := schemas[0]["name"], "load_skill"; got != want {
		t.Fatalf("schemas[0] name = %v, want %v", got, want)
	}
}

func TestLoadSkillReturnsBodyWithHeader(t *testing.T) {
	skills := &fakeSkillProvider{body: "1. read the board\n2. run ERC"}
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	runner := m06Runner(t, authority, ToolExecutorDeps{SkillProvider: skills})

	arguments, err := json.Marshal(map[string]any{"name": "code-review", "args": "focus on timing"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, execErr := runner.Execute(context.Background(), m06Call("load_skill", string(arguments)))
	requireOutcome(t, outcome, execErr, false, "# Skill: code-review", "1. read the board")

	if skills.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", skills.calls)
	}
	if got, want := skills.sessionID, authority.SessionID; got != want {
		t.Fatalf("sessionID = %q, want %q", got, want)
	}
	if got, want := skills.name, "code-review"; got != want {
		t.Fatalf("name = %q, want %q", got, want)
	}
	if got, want := skills.args, "focus on timing"; got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestLoadSkillRequiresName(t *testing.T) {
	skills := &fakeSkillProvider{body: "unused"}
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	runner := m06Runner(t, authority, ToolExecutorDeps{SkillProvider: skills})

	outcome, err := runner.Execute(context.Background(), m06Call("load_skill", `{"args":"x"}`))
	requireOutcome(t, outcome, err, true, "name is required")
	if skills.calls != 0 {
		t.Fatalf("provider must not be called without a name, calls = %d", skills.calls)
	}
}

func TestLoadSkillNilProvider(t *testing.T) {
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	runner := m06Runner(t, authority, ToolExecutorDeps{})

	outcome, err := runner.Execute(context.Background(), m06Call("load_skill", `{"name":"x"}`))
	requireOutcome(t, outcome, err, true, "技能通道不可用")
}

func TestLoadSkillProviderErrorSurfaces(t *testing.T) {
	skills := &fakeSkillProvider{err: errors.New("unknown skill: nope (available: a, b)")}
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	runner := m06Runner(t, authority, ToolExecutorDeps{SkillProvider: skills})

	outcome, err := runner.Execute(context.Background(), m06Call("load_skill", `{"name":"nope"}`))
	requireOutcome(t, outcome, err, true, "unknown skill: nope", "available: a, b")
}

func TestLoadSkillBypassesCandidate(t *testing.T) {
	skills := &fakeSkillProvider{body: "read-only body"}
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	runner := m06Runner(t, authority, ToolExecutorDeps{SkillProvider: skills})

	outcome, err := runner.Execute(context.Background(), m06Call("load_skill", `{"name":"a"}`))
	requireOutcome(t, outcome, err, false, "# Skill: a")

	// A read-only host tool must never create a candidate or a snapshot.
	if _, statErr := os.Stat(authority.CandidateRoot); !os.IsNotExist(statErr) {
		t.Fatalf("candidate root was created for load_skill: %v", statErr)
	}
	if outcome.Diff != nil || len(outcome.Snapshots) != 0 {
		t.Fatalf("load_skill produced diff or snapshots: %#v", outcome)
	}
}

func TestNewToolExecutorFactoryInjectsSkillProvider(t *testing.T) {
	skills := &fakeSkillProvider{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{}, WithSkillProvider(skills)).(ToolExecutorFactory)
	if factory.deps.SkillProvider == nil {
		t.Fatal("factory option must populate the skill provider")
	}
	request := executorRequest(t, t.TempDir(), t.TempDir())
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	executor := runner.(*toolRunExecutor)
	if executor.deps.SkillProvider != skills {
		t.Fatal("run executor must share the injected skill provider")
	}
}
