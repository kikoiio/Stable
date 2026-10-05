package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"stable/internal/agent"
	"stable/internal/appconfig"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/decision"
	"stable/internal/dependency"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sandbox"
	"stable/internal/sessioncontext"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/tools"
)

// chatserve runs the persistent conversation service standalone. It is a
// development/test entry point; the installed runtime embeds the same service
// inside `stable supervise`.
func chatserve(args []string) error {
	fs := flag.NewFlagSet("chatserve", flag.ContinueOnError)
	db := fs.String("db", "", "state database")
	socket := fs.String("socket", "", "chat unix socket path")
	temporal := fs.String("temporal", "127.0.0.1:7233", "temporal address")
	projectRoot := fs.String("project-root", ".", "project root containing fixtures")
	runRoot := fs.String("run-root", "", "goal run directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" || *socket == "" || *runRoot == "" {
		return errors.New("chatserve requires --db, --socket and --run-root")
	}
	c, err := appconfig.Load()
	if err != nil {
		return err
	}
	model, err := decision.NewProvider(c.Model)
	if err != nil {
		return fmt.Errorf("model provider: %w", err)
	}
	provider, ok := model.(decision.StructuredProvider)
	if !ok {
		return errors.New("model provider does not support structured output")
	}
	s, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = chatserveRecovery(ctx, s); err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}
	refresher, err := dependency.NewKiCadRefresher(s, *runRoot, *projectRoot, *temporal)
	if err != nil {
		return err
	}
	helperPath, err := chatserveHelperPath()
	if err != nil {
		return err
	}
	var runner agent.Runner
	var executorFactory agent.ExecutorFactory
	var toolSchemas []llm.ToolSchema
	var runnerError string
	var snapshotStore *candidate.SnapshotStore
	// The interaction sinks need the conversation service, which only exists
	// once Serve returns, so they are built unbound here and bound right
	// after Serve — no run can reach a tool call before that.
	askSink := conversation.NewAskAdapter(nil)
	todoProvider := conversation.NewTodoProvider(nil)
	planSink := conversation.NewPlanApprovalSink(nil)
	if streamingProvider, streamErr := llm.NewProvider(c.Model); streamErr == nil {
		var credentials []string
		if c.Model.APIKey != "" {
			credentials = []string{c.Model.APIKey}
		}
		snapshotStore, err = candidate.NewSnapshotStore(*projectRoot, c.Snapshots.ProjectBytes(), c.Snapshots.ManifestsPerCandidate(), credentials)
		if err != nil {
			return fmt.Errorf("candidate snapshot store: %w", err)
		}
		executorFactory = execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
			Sandbox:            sandbox.LinuxManager{},
			Gate:               execution.StorePermissionGate{Store: s},
			Approvals:          s,
			Candidates:         s,
			HelperPath:         helperPath,
			SessionRoot:        *projectRoot,
			ProviderCredential: c.Model.APIKey,
			Snapshots:          snapshotStore,
			QuestionSink:       askSink,
			TodoProvider:       todoProvider,
		}, execution.WithPlanSink(planSink))
		toolSchemas = chatserveToolSchemas()
		contextManager, fellBack := sessioncontext.NewManager(c.Model.ContextWindowTokens, model.(decision.ChatProvider))
		if fellBack {
			log.Printf("invalid context_window_tokens %d; using default %d", c.Model.ContextWindowTokens, sessioncontext.DefaultWindowTokens)
		}
		runner = agent.NewRunner(streamingProvider, agent.RunnerOptions{ExecutorFactory: executorFactory, ToolSchemas: toolSchemas, ContextManager: contextManager})
	} else {
		runnerError = streamErr.Error()
	}
	permissionService := &permission.PermissionService{Repository: s, NewID: func() string { id, _ := sessionlog.NewID(); return id }}
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: s, Provider: provider, ChatProvider: model.(decision.ChatProvider), Runner: runner, ExecutorFactory: executorFactory, ToolSchemas: toolSchemas, PermissionService: permissionService, RunnerError: runnerError, ProviderCredential: c.Model.APIKey, ProviderName: c.Model.Provider, Model: c.Model.Model, Temporal: *temporal, ProjectRoot: *projectRoot, RunRoot: *runRoot, SocketPath: *socket,
		Refresher:           refresher,
		CandidateCheckers:   chatCandidateCheckers(*runRoot),
		ContextWindowTokens: c.Model.ContextWindowTokens,
		Snapshots:           snapshotStore,
	})
	if err != nil {
		return err
	}
	askSink.Bind(svc)
	todoProvider.Bind(svc)
	planSink.Bind(svc)
	defer svc.Close()
	<-ctx.Done()
	return nil
}

func chatserveHelperPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find chatserve executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve chatserve executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("resolve chatserve executable path: %w", err)
	}
	root := filepath.Dir(filepath.Dir(exe))
	candidates := []string{
		filepath.Join(root, "libexec", "agentworker"),
		filepath.Join(root, "dev-install", "libexec", "agentworker"),
		filepath.Join(filepath.Dir(exe), "agentworker"),
	}
	for _, candidate := range candidates {
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return candidates[0], nil
}

func chatserveToolSchemas() []llm.ToolSchema {
	nameMap := map[string]string{
		"read_file":      "read_file",
		"write_file":     "write_file",
		"edit_file":      "edit_file",
		"glob":           "glob",
		"grep":           "grep",
		"ask_user":       "ask_user",
		"exit_plan_mode": "exit_plan_mode",
		"task_create":    "task_create",
		"task_get":       "task_get",
		"task_list":      "task_list",
		"task_update":    "task_update",
	}
	registry := tools.CreateDefaultTools().Registry
	// M06 tools live outside the default registry; the copy keeps append from
	// aliasing the registry slice.
	sources := append(append([]map[string]any{}, registry.GetAllSchemas()...), execution.M06ToolSchemas()...)
	schemas := make([]llm.ToolSchema, 0, len(nameMap)+1)
	for _, schema := range sources {
		internalName, _ := schema["name"].(string)
		name, ok := nameMap[internalName]
		if !ok {
			continue
		}
		description, _ := schema["description"].(string)
		input, _ := schema["input_schema"].(map[string]any)
		schemas = append(schemas, llm.ToolSchema{Name: name, Description: description, InputSchema: input})
	}
	schemas = append(schemas, llm.ToolSchema{
		Name:        "command",
		Description: tools.BashDescription,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string", "description": "Shell command to execute"}, "timeout": map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "description": "Timeout in seconds"}}, "required": []string{"command"}},
	})
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return schemas
}

// chatCandidateCheckers wires the independent candidate checkers the trusted
// review entry runs inside the verified Linux sandbox. Without one every
// preview would carry an unavailable finding and normal acceptance would be
// blocked.
func chatCandidateCheckers(runRoot string) []candidate.Checker {
	return []candidate.Checker{candidate.KicadERCChecker{Sandbox: sandbox.LinuxManager{}, RunRoot: runRoot}}
}

// chatserveRecovery settles candidate acceptances that were interrupted before
// the last shutdown so no trusted decision is served against stale journal
// state.
func chatserveRecovery(ctx context.Context, s *store.Store) error {
	return s.ReconcileAcceptances(ctx)
}
