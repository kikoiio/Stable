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
	"stable/internal/mcp"
	"stable/internal/permission"
	"stable/internal/platform/paths"
	"stable/internal/platform/sandbox"
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
	sbx := sandbox.New()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = chatserveRecovery(ctx, s); err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}
	refresher, err := dependency.NewKiCadRefresher(s, *runRoot, *projectRoot, *temporal, sbx)
	if err != nil {
		return err
	}
	p, err := paths.Resolve(c.StateDir)
	if err != nil {
		return err
	}
	helperPath := p.HelperBinaryResolved("agentworker")
	var runner agent.Runner
	var executorFactory agent.ExecutorFactory
	var toolSchemas []llm.ToolSchema
	var runnerError string
	var snapshotStore *candidate.SnapshotStore
	delegationReporter := conversation.NewDelegationEventReporter()
	var delegator *agent.PoolDelegator
	// The interaction sinks need the conversation service, which only exists
	// once Serve returns, so they are built unbound here and bound right
	// after Serve — no run can reach a tool call before that.
	askSink := conversation.NewAskAdapter(nil)
	todoProvider := conversation.NewTodoProvider(nil)
	planSink := conversation.NewPlanApprovalSink(nil)
	// The skill gate reads the same directories the TUI lists; Serve binds it
	// to the service so its event appends share the service event mutex.
	skillGate := conversation.NewSkillGate(nil, userSkillsDir(), filepath.Join(*projectRoot, ".stable", "skills"))
	hookGate := conversation.NewHookGate(nil, userHooksPath(), filepath.Join(*projectRoot, ".stable", "hooks.yaml"))
	mcpManager := mcp.NewManager(c.MCPServers, filepath.Join(*projectRoot, ".stable", "mcp.yaml"))
	if configPath, configErr := appconfig.ConfigPath(); configErr == nil {
		mcpManager.SetUserConfigPath(configPath)
	}
	if err := mcpManager.ConnectAll(ctx); err != nil {
		log.Printf("MCP startup: %v", err)
	}
	defer mcpManager.Shutdown()
	if streamingProvider, streamErr := llm.NewProvider(c.Model); streamErr == nil {
		delegator, err = agent.NewPoolDelegator(agent.DefaultDelegationLimits(), agent.StreamingChildRunner{}, delegationReporter)
		if err != nil {
			return fmt.Errorf("delegation coordinator: %w", err)
		}
		defer delegator.Close()
		var credentials []string
		if c.Model.APIKey != "" {
			credentials = []string{c.Model.APIKey}
		}
		snapshotStore, err = candidate.NewSnapshotStore(*projectRoot, c.Snapshots.ProjectBytes(), c.Snapshots.ManifestsPerCandidate(), credentials)
		if err != nil {
			return fmt.Errorf("candidate snapshot store: %w", err)
		}
		executorFactory = execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
			Sandbox:            sbx,
			Gate:               execution.StorePermissionGate{Store: s},
			Approvals:          s,
			Candidates:         s,
			HelperPath:         helperPath,
			SessionRoot:        *projectRoot,
			ProviderCredential: c.Model.APIKey,
			Snapshots:          snapshotStore,
			QuestionSink:       askSink,
			TodoProvider:       todoProvider,
			Provider:           streamingProvider,
		}, execution.WithPlanSink(planSink), execution.WithSkillProvider(skillGate), execution.WithHookRunner(hookGate), execution.WithMCPCaller(mcpManager), execution.WithDelegator(delegator, streamingProvider))
		toolSchemas = chatserveToolSchemasWithDelegation(delegator, mcpManager)
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
		CandidateCheckers:   chatCandidateCheckers(*runRoot, sbx),
		ContextWindowTokens: c.Model.ContextWindowTokens,
		Snapshots:           snapshotStore,
		Skills:              skillGate,
		Hooks:               hookGate,
		MCP:                 mcpManager,
	})
	if err != nil {
		return err
	}
	askSink.Bind(svc)
	todoProvider.Bind(svc)
	planSink.Bind(svc)
	delegationReporter.Bind(svc)
	defer svc.Close()
	<-ctx.Done()
	return nil
}

// userSkillsDir returns the user-level skill directory for the skill gate;
// an unavailable config base simply contributes no user skills.
func userHooksPath() string {
	path, err := appconfig.UserHooksPath()
	if err != nil {
		return ""
	}
	return path
}

func userSkillsDir() string {
	dir, err := appconfig.UserSkillsDir()
	if err != nil {
		return ""
	}
	return dir
}

func chatserveToolSchemas(callers ...execution.MCPCaller) []llm.ToolSchema {
	return chatserveToolSchemasWithDelegation(nil, callers...)
}

func chatserveToolSchemasWithDelegation(delegator agent.Delegator, callers ...execution.MCPCaller) []llm.ToolSchema {
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
		"load_skill":     "load_skill",
	}
	registry := tools.CreateDefaultTools().Registry
	// M06/M07 tools live outside the default registry; the copy keeps append
	// from aliasing the registry slice.
	sources := append(append(append([]map[string]any{}, registry.GetAllSchemas()...), execution.M06ToolSchemas()...), execution.SkillToolSchemas()...)
	schemas := make([]llm.ToolSchema, 0, len(nameMap)+2)
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
	if delegator != nil {
		schemas = append(schemas, execution.DelegationToolSchemas()...)
	}
	if len(callers) > 0 && callers[0] != nil {
		if manager, ok := callers[0].(interface{ Configured() bool }); ok && manager.Configured() {
			for _, schema := range execution.MCPToolSchemas() {
				name, _ := schema["name"].(string)
				description, _ := schema["description"].(string)
				input, _ := schema["input_schema"].(map[string]any)
				schemas = append(schemas, llm.ToolSchema{Name: name, Description: description, InputSchema: input})
			}
			for _, schema := range callers[0].EagerSchemas() {
				schemas = append(schemas, llm.ToolSchema{Name: schema.Name, Description: schema.Description, InputSchema: schema.InputSchema})
			}
		}
	}
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return schemas
}

// chatCandidateCheckers wires the independent candidate checkers the trusted
// review entry runs inside the verified Linux sandbox. Without one every
// preview would carry an unavailable finding and normal acceptance would be
// blocked.
func chatCandidateCheckers(runRoot string, sbx sandbox.SandboxManager) []candidate.Checker {
	return []candidate.Checker{candidate.KicadERCChecker{Sandbox: sbx, RunRoot: runRoot}}
}

// chatserveRecovery settles candidate acceptances that were interrupted before
// the last shutdown so no trusted decision is served against stale journal
// state.
func chatserveRecovery(ctx context.Context, s *store.Store) error {
	if err := s.ReconcileAcceptances(ctx); err != nil {
		return err
	}
	return s.ReconcileRewinds(ctx)
}
