package execution

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"stable/internal/todo"
)

func schemaObject(t *testing.T, container map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := container[key].(map[string]any)
	if !ok {
		t.Fatalf("field %q is not an object", key)
	}
	return value
}

func schemaStrings(t *testing.T, container map[string]any, key string) []string {
	t.Helper()
	raw, ok := container[key].([]string)
	if !ok {
		t.Fatalf("field %q is not a string list", key)
	}
	return raw
}

func TestAskUserSchemaShape(t *testing.T) {
	if got, want := AskUserSchema["name"], "ask_user"; got != want {
		t.Fatalf("name = %v, want %v", got, want)
	}
	input := schemaObject(t, AskUserSchema, "input_schema")
	if got, want := input["type"], "object"; got != want {
		t.Fatalf("input type = %v, want %v", got, want)
	}
	if required := schemaStrings(t, input, "required"); !reflect.DeepEqual(required, []string{"questions"}) {
		t.Fatalf("required = %v, want [questions]", required)
	}
	properties := schemaObject(t, input, "properties")
	questions := schemaObject(t, properties, "questions")
	if got, want := questions["type"], "array"; got != want {
		t.Fatalf("questions type = %v, want %v", got, want)
	}
	if got, want := questions["minItems"], 1; got != want {
		t.Fatalf("questions minItems = %v, want %v", got, want)
	}
	if got, want := questions["maxItems"], 4; got != want {
		t.Fatalf("questions maxItems = %v, want %v", got, want)
	}
	item := schemaObject(t, questions, "items")
	itemRequired := schemaStrings(t, item, "required")
	wantRequired := []string{"question", "header", "options", "multiSelect"}
	if !reflect.DeepEqual(itemRequired, wantRequired) {
		t.Fatalf("question required = %v, want %v", itemRequired, wantRequired)
	}
	itemProperties := schemaObject(t, item, "properties")
	header := schemaObject(t, itemProperties, "header")
	if got, want := header["maxLength"], 12; got != want {
		t.Fatalf("header maxLength = %v, want %v", got, want)
	}
	options := schemaObject(t, itemProperties, "options")
	if got, want := options["minItems"], 2; got != want {
		t.Fatalf("options minItems = %v, want %v", got, want)
	}
	if got, want := options["maxItems"], 4; got != want {
		t.Fatalf("options maxItems = %v, want %v", got, want)
	}
	option := schemaObject(t, options, "items")
	if optionRequired := schemaStrings(t, option, "required"); !reflect.DeepEqual(optionRequired, []string{"label", "description"}) {
		t.Fatalf("option required = %v, want [label description]", optionRequired)
	}
	optionProperties := schemaObject(t, option, "properties")
	for _, field := range []string{"label", "description"} {
		if fieldSchema := schemaObject(t, optionProperties, field); fieldSchema["type"] != "string" {
			t.Fatalf("option %q type = %v, want string", field, fieldSchema["type"])
		}
	}
	multiSelect := schemaObject(t, itemProperties, "multiSelect")
	if got, want := multiSelect["type"], "boolean"; got != want {
		t.Fatalf("multiSelect type = %v, want %v", got, want)
	}
}

func TestExitPlanModeSchemaShape(t *testing.T) {
	if got, want := ExitPlanModeSchema["name"], "exit_plan_mode"; got != want {
		t.Fatalf("name = %v, want %v", got, want)
	}
	description, _ := ExitPlanModeSchema["description"].(string)
	if description == "" {
		t.Fatal("description is empty")
	}
	input := schemaObject(t, ExitPlanModeSchema, "input_schema")
	if got, want := input["type"], "object"; got != want {
		t.Fatalf("input type = %v, want %v", got, want)
	}
	properties := schemaObject(t, input, "properties")
	if len(properties) != 0 {
		t.Fatalf("properties = %v, want empty", properties)
	}
}

func TestTaskSchemasReuseTodoConstants(t *testing.T) {
	wrapped := map[string]map[string]any{
		"task_create": TaskCreateSchema,
		"task_get":    TaskGetSchema,
		"task_list":   TaskListSchema,
		"task_update": TaskUpdateSchema,
	}
	constants := map[string]map[string]any{
		"task_create": todo.TaskCreateSchema,
		"task_get":    todo.TaskGetSchema,
		"task_list":   todo.TaskListSchema,
		"task_update": todo.TaskUpdateSchema,
	}
	for name, schema := range wrapped {
		if got := schema["name"]; got != name {
			t.Fatalf("%s: name = %v, want %v", name, got, name)
		}
		description, _ := schema["description"].(string)
		if prefix := "Manage the current session task list. "; len(description) <= len(prefix) || description[:len(prefix)] != prefix {
			t.Fatalf("%s: description %q lacks task-list prefix", name, description)
		}
		base := constants[name]
		if got, want := schema["input_schema"], base["input_schema"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: input_schema does not match the todo constant", name)
		}
		shared := reflect.ValueOf(schema["input_schema"]).Pointer() == reflect.ValueOf(base["input_schema"]).Pointer()
		if !shared {
			t.Fatalf("%s: input_schema is not the shared todo constant", name)
		}
	}
	input := schemaObject(t, TaskCreateSchema, "input_schema")
	if required := schemaStrings(t, input, "required"); !reflect.DeepEqual(required, []string{"subject", "description"}) {
		t.Fatalf("task_create required = %v, want [subject description]", required)
	}
	updateInput := schemaObject(t, TaskUpdateSchema, "input_schema")
	if required := schemaStrings(t, updateInput, "required"); !reflect.DeepEqual(required, []string{"taskId"}) {
		t.Fatalf("task_update required = %v, want [taskId]", required)
	}
	updateProperties := schemaObject(t, updateInput, "properties")
	status := schemaObject(t, updateProperties, "status")
	enum, ok := status["enum"].([]string)
	if !ok {
		t.Fatal("task_update status enum is not a string list")
	}
	wantEnum := []string{"pending", "in_progress", "completed", "deleted"}
	if !reflect.DeepEqual(enum, wantEnum) {
		t.Fatalf("task_update status enum = %v, want %v", enum, wantEnum)
	}
}

func TestM06ToolSchemasOrder(t *testing.T) {
	schemas := M06ToolSchemas()
	want := []string{"ask_user", "exit_plan_mode", "task_create", "task_get", "task_list", "task_update"}
	if len(schemas) != len(want) {
		t.Fatalf("schema count = %d, want %d", len(schemas), len(want))
	}
	seen := map[string]bool{}
	for i, schema := range schemas {
		name, _ := schema["name"].(string)
		if name != want[i] {
			t.Fatalf("schemas[%d] = %q, want %q", i, name, want[i])
		}
		if seen[name] {
			t.Fatalf("duplicate schema name %q", name)
		}
		seen[name] = true
	}
}

type testQuestionSink struct{ request AskRequest }

func (s *testQuestionSink) Ask(_ context.Context, req AskRequest) (AskResponse, error) {
	s.request = req
	return AskResponse{Answers: [][]string{{"one"}}}, nil
}

type testPlanSink struct{ planPath string }

func (s *testPlanSink) SubmitPlan(_ context.Context, _, _, planPath string) (string, error) {
	s.planPath = planPath
	return PlanChoiceAuto, nil
}

type testTodoProvider struct{ list *todo.TaskList }

func (p *testTodoProvider) For(sessionID string) *todo.TaskList { return p.list }

func TestNewToolExecutorFactoryDefaultsToNilM06Deps(t *testing.T) {
	factory := NewToolExecutorFactory(ToolExecutorDeps{Now: time.Now}).(ToolExecutorFactory)
	if factory.deps.QuestionSink != nil || factory.deps.PlanSink != nil || factory.deps.TodoProvider != nil {
		t.Fatal("factory built without options must keep the M06 deps nil")
	}
	request := executorRequest(t, t.TempDir(), t.TempDir())
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	executor := runner.(*toolRunExecutor)
	if executor.deps.QuestionSink != nil || executor.deps.PlanSink != nil || executor.deps.TodoProvider != nil {
		t.Fatal("run executor must see nil M06 deps when none are injected")
	}
}

func TestNewToolExecutorFactoryInjectsM06Deps(t *testing.T) {
	questions := &testQuestionSink{}
	plans := &testPlanSink{}
	todos := &testTodoProvider{list: todo.NewTaskList(t.TempDir(), "0123456789abcdef0123456789abcdef", nil)}
	factory := NewToolExecutorFactory(ToolExecutorDeps{}, WithQuestionSink(questions), WithPlanSink(plans), WithTodoProvider(todos)).(ToolExecutorFactory)
	if factory.deps.QuestionSink == nil || factory.deps.PlanSink == nil || factory.deps.TodoProvider == nil {
		t.Fatal("factory options must populate the M06 deps")
	}
	request := executorRequest(t, t.TempDir(), t.TempDir())
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	executor := runner.(*toolRunExecutor)
	if executor.deps.QuestionSink != questions {
		t.Fatal("run executor must share the injected question sink")
	}
	if executor.deps.PlanSink != plans {
		t.Fatal("run executor must share the injected plan sink")
	}
	if executor.deps.TodoProvider != todos {
		t.Fatal("run executor must share the injected todo provider")
	}
}

func TestPlanSinkSentinelErrors(t *testing.T) {
	feedback := PlanFeedbackError{Text: "use a table"}
	var reported PlanFeedbackError
	if !errors.As(error(feedback), &reported) || reported.Text != "use a table" {
		t.Fatalf("errors.As(PlanFeedbackError) = %v %q", reported, reported.Text)
	}
	if got, want := feedback.Error(), "plan feedback: use a table"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	var cancelled PlanCancelledError
	if !errors.As(error(PlanCancelledError{}), &cancelled) {
		t.Fatal("errors.As should match PlanCancelledError")
	}
	if cancelled.Error() == "" {
		t.Fatal("PlanCancelledError message is empty")
	}
	if PlanChoiceAuto != "auto" || PlanChoiceManual != "manual" {
		t.Fatalf("plan choices = %q/%q, want auto/manual", PlanChoiceAuto, PlanChoiceManual)
	}
}

func TestMCPCallSchemaShape(t *testing.T) {
	if got, want := MCPCallSchema["name"], "mcp_call"; got != want {
		t.Fatalf("name = %v, want %v", got, want)
	}
	description, _ := MCPCallSchema["description"].(string)
	if !strings.Contains(description, "tool_search") {
		t.Fatalf("description %q must explain the tool_search discovery flow", description)
	}
	input := schemaObject(t, MCPCallSchema, "input_schema")
	if got, want := input["type"], "object"; got != want {
		t.Fatalf("input type = %v, want %v", got, want)
	}
	if required := schemaStrings(t, input, "required"); !reflect.DeepEqual(required, []string{"server", "tool"}) {
		t.Fatalf("required = %v, want [server tool]", required)
	}
	properties := schemaObject(t, input, "properties")
	for _, field := range []string{"server", "tool"} {
		if fieldSchema := schemaObject(t, properties, field); fieldSchema["type"] != "string" {
			t.Fatalf("%s type = %v, want string", field, fieldSchema["type"])
		}
	}
	arguments := schemaObject(t, properties, "arguments")
	if got, want := arguments["type"], "object"; got != want {
		t.Fatalf("arguments type = %v, want %v", got, want)
	}
	if _, optional := arguments["required"]; optional {
		t.Fatal("arguments must stay optional")
	}
}

func TestToolSearchSchemaShape(t *testing.T) {
	if got, want := ToolSearchSchema["name"], "tool_search"; got != want {
		t.Fatalf("name = %v, want %v", got, want)
	}
	description, _ := ToolSearchSchema["description"].(string)
	if !strings.Contains(description, "read-only") {
		t.Fatalf("description %q must document the read-only guarantee", description)
	}
	input := schemaObject(t, ToolSearchSchema, "input_schema")
	if _, hasRequired := input["required"]; hasRequired {
		t.Fatal("tool_search must have no required arguments")
	}
	properties := schemaObject(t, input, "properties")
	if query := schemaObject(t, properties, "query"); query["type"] != "string" {
		t.Fatalf("query type = %v, want string", query["type"])
	}
	limit := schemaObject(t, properties, "limit")
	if got, want := limit["type"], "integer"; got != want {
		t.Fatalf("limit type = %v, want %v", got, want)
	}
	if got, want := limit["maximum"], 20; got != want {
		t.Fatalf("limit maximum = %v, want %v", got, want)
	}
}
