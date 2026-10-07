package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"stable/internal/agent"
	"stable/internal/appconfig"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/decision"
	"stable/internal/dependency"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/mcp"
	"stable/internal/memory"
	"stable/internal/permission"
	"stable/internal/platform/ipc"
	"stable/internal/platform/lock"
	"stable/internal/platform/paths"
	"stable/internal/platform/proc"
	"stable/internal/platform/sandbox"
	"stable/internal/platform/secfile"
	"stable/internal/sessioncontext"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/tools"
)

type Status struct {
	Running         bool   `json:"running"`
	PID             int    `json:"pid"`
	TemporalPID     int    `json:"temporal_pid"`
	WorkerPID       int    `json:"worker_pid"`
	TemporalAddress string `json:"temporal_address"`
	StateDir        string `json:"state_dir"`
	TemporalLog     string `json:"temporal_log"`
	WorkerLog       string `json:"worker_log"`
}

func Control(p paths.Paths, cmd string) (Status, error) {
	var s Status
	conn, err := ipc.DialPrivate(p.Socket, 500*time.Millisecond)
	if err != nil {
		return s, errors.New("runtime is not running")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = fmt.Fprintln(conn, cmd); err != nil {
		return s, err
	}
	if err = json.NewDecoder(conn).Decode(&s); err != nil {
		return s, err
	}
	return s, nil
}

func Up(ctx context.Context, c appconfig.AppConfig, p paths.Paths) (Status, error) {
	if s, err := Control(p, "status"); err == nil && s.Running {
		return s, nil
	}
	if err := p.Prepare(); err != nil {
		return Status{}, err
	}
	f, err := secfile.OpenFilePrivate(p.SupervisorLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return Status{}, err
	}
	defer f.Close()
	cmd := exec.Command(filepath.Join(p.Bin, "stable"), "supervise")
	cmd.Env = os.Environ()
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	if err = cmd.Start(); err != nil {
		return Status{}, err
	}
	go cmd.Wait()
	deadline := time.NewTimer(35 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return Status{}, ctx.Err()
		case <-deadline.C:
			return Status{}, fmt.Errorf("runtime startup timed out; see %s", p.SupervisorLog)
		case <-tick.C:
			if s, err := Control(p, "status"); err == nil && s.Running {
				return s, nil
			}
			if cmd.ProcessState != nil {
				return Status{}, fmt.Errorf("runtime startup failed; see %s", p.SupervisorLog)
			}
		}
	}
}

// ReconcileBeforeDispatch settles candidate acceptances that were interrupted
// before the last shutdown. It runs before Temporal or the worker start so no
// new action is dispatched until every prepared/swapped acceptance is either
// finalized, retried, or recorded as blocked.
func ReconcileBeforeDispatch(ctx context.Context, dbPath string) error {
	s, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.ReconcileAcceptances(ctx); err != nil {
		return err
	}
	return s.ReconcileRewinds(ctx)
}

func Supervise(c appconfig.AppConfig, p paths.Paths, sbx sandbox.SandboxManager) error {
	if err := p.Prepare(); err != nil {
		return err
	}
	// A previous supervisor may still be releasing children after a crash or
	// down; wait for the lock instead of failing the fresh start outright.
	lockDeadline := time.Now().Add(20 * time.Second)
	var guard lock.Guard
	for {
		acquired, acquireErr := lock.TryAcquire(p.Lock)
		if acquireErr == nil {
			guard = acquired
			break
		}
		if time.Now().After(lockDeadline) {
			return errors.New("runtime already starting or running")
		}
		if _, statErr := Control(p, "status"); statErr == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer guard.Release()
	if _, err := Control(p, "status"); err == nil {
		return errors.New("runtime already running")
	}
	if err := ReconcileBeforeDispatch(context.Background(), p.Database); err != nil {
		return fmt.Errorf("reconcile interrupted candidate transactions: %w", err)
	}
	address := "127.0.0.1:" + strconv.Itoa(c.TemporalPort)
	if conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("Temporal port %d is occupied", c.TemporalPort)
	}
	listener, err := ipc.ListenPrivate(p.Socket, true)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(p.Socket)
	defer os.Remove(p.ChatSocket)
	temporalLog, err := secfile.OpenFilePrivate(p.TemporalLog, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer temporalLog.Close()
	workerLog, err := secfile.OpenFilePrivate(p.WorkerLog, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer workerLog.Close()
	temporal := exec.Command(p.HelperBinary("temporal"), "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", strconv.Itoa(c.TemporalPort), "--db-filename", p.TemporalDB)
	temporal.Stdout = temporalLog
	temporal.Stderr = temporalLog
	if err = temporal.Start(); err != nil {
		return err
	}
	if err = proc.AdoptChild(temporal); err != nil {
		return fmt.Errorf("adopt Temporal process: %w", err)
	}
	defer proc.StopProcess(temporal, 5*time.Second)
	if err = waitPort(address, temporal, 20*time.Second); err != nil {
		return fmt.Errorf("Temporal startup: %w; see %s", err, p.TemporalLog)
	}
	worker := exec.Command(p.HelperBinary("agentworker"), "--app-config", "--db", p.Database, "--run-root", p.Goals, "--temporal", address, "--project-root", p.Share, "--chat-socket", p.ChatSocket)
	worker.Env = os.Environ()
	worker.Stdout = workerLog
	worker.Stderr = workerLog
	if err = worker.Start(); err != nil {
		return err
	}
	if err = proc.AdoptChild(worker); err != nil {
		return fmt.Errorf("adopt worker process: %w", err)
	}
	defer proc.StopProcess(worker, 5*time.Second)
	if err = waitReady(p.WorkerLog, worker, 15*time.Second); err != nil {
		return fmt.Errorf("Worker startup: %w; see %s", err, p.WorkerLog)
	}
	chatDone, chatCancel := startChatService(c, p, address, sbx)
	defer chatCancel()
	if err := waitChatSocket(p.ChatSocket, chatDone, 10*time.Second); err != nil {
		return fmt.Errorf("Chat startup: %w; see %s", err, p.ChatLog)
	}
	status := Status{Running: true, PID: os.Getpid(), TemporalPID: temporal.Process.Pid, WorkerPID: worker.Process.Pid, TemporalAddress: address, StateDir: p.State, TemporalLog: p.TemporalLog, WorkerLog: p.WorkerLog}
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-tick.C:
				if !proc.Alive(temporal) || !proc.Alive(worker) {
					_ = listener.Close()
					return
				}
				select {
				case <-chatDone:
					// The session service is part of the runtime; losing it
					// takes the whole supervise group down.
					_ = listener.Close()
					return
				default:
				}
			}
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		line, _ := bufio.NewReader(conn).ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "status" || line == "down" {
			_ = json.NewEncoder(conn).Encode(status)
			conn.Close()
			if line == "down" {
				return nil
			}
		} else {
			conn.Close()
		}
		if !proc.Alive(temporal) || !proc.Alive(worker) {
			return errors.New("runtime child exited unexpectedly")
		}
		select {
		case <-chatDone:
			return errors.New("chat session service exited unexpectedly; see " + p.ChatLog)
		default:
		}
	}
}

// snapshotCredentials lists the configured secrets that must never reach
// snapshot metadata. Empty entries are dropped.
func snapshotCredentials(apiKey string) []string {
	if apiKey == "" {
		return nil
	}
	return []string{apiKey}
}

func runtimeToolSchemas(callers ...execution.MCPCaller) []llm.ToolSchema {
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
		"memory_list":    "memory_list",
		"memory_read":    "memory_read",
		"memory_save":    "memory_save",
		"memory_delete":  "memory_delete",
	}
	registry := tools.CreateDefaultTools().Registry
	// M06/M07 tools live outside the default registry; the copy keeps append
	// from aliasing the registry slice.
	sources := append(append(append(append([]map[string]any{}, registry.GetAllSchemas()...), execution.M06ToolSchemas()...), execution.SkillToolSchemas()...), execution.MemoryToolSchemas()...)
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

// startChatService runs the persistent conversation service inside the
// supervisor process. It returns a channel that closes when the service stops.
func startChatService(c appconfig.AppConfig, p paths.Paths, address string, sbx sandbox.SandboxManager) (<-chan struct{}, context.CancelFunc) {
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(done)
		if err := runChatService(ctx, c, p, address, sbx); err != nil {
			if f, ferr := secfile.OpenFilePrivate(p.ChatLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); ferr == nil {
				fmt.Fprintf(f, "%s chat session service: %v\n", time.Now().UTC().Format(time.RFC3339), err)
				f.Close()
			}
		}
	}()
	return done, cancel
}

func runChatService(ctx context.Context, c appconfig.AppConfig, p paths.Paths, address string, sbx sandbox.SandboxManager) error {
	s, err := store.Open(p.Database)
	if err != nil {
		return err
	}
	defer s.Close()
	var provider decision.StructuredProvider
	var chatProvider decision.ChatProvider
	if model, perr := decision.NewProvider(c.Model); perr == nil {
		provider = model.(decision.StructuredProvider)
		chatProvider = model.(decision.ChatProvider)
	}
	refresher, err := dependency.NewKiCadRefresher(s, p.Goals, p.Share, address, sbx)
	if err != nil {
		return err
	}
	userMemoryDir, err := appconfig.UserMemoryDir()
	if err != nil {
		return err
	}
	var memoryGate *conversation.MemoryGate
	memoryManager, err := memory.NewManager(memory.Options{
		ProjectRoot:   p.Share,
		UserConfigDir: filepath.Dir(filepath.Dir(userMemoryDir)),
		StateDir:      p.State,
		Model:         chatProvider,
		OnEvent: func(root string, event memory.BackgroundEvent) {
			if memoryGate != nil {
				memoryGate.RecordBackground(root, event)
			} else {
				conversation.AppendMemoryBackgroundEvent(root, event)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("memory manager: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if closeErr := memoryManager.Close(closeCtx); closeErr != nil {
			log.Printf("memory manager shutdown: %v", closeErr)
		}
	}()
	memoryGate = conversation.NewMemoryGate(memoryManager, p.Share)
	var runner agent.Runner
	var runnerError string
	var executorFactory agent.ExecutorFactory
	var toolSchemas []llm.ToolSchema
	var snapshotStore *candidate.SnapshotStore
	// The interaction sinks need the conversation service, which only exists
	// once Serve returns, so they are built unbound here and bound right
	// after Serve — no run can reach a tool call before that.
	askSink := conversation.NewAskAdapter(nil)
	todoProvider := conversation.NewTodoProvider(nil)
	planSink := conversation.NewPlanApprovalSink(nil)
	// The skill gate reads the same directories the TUI lists; Serve binds it
	// to the service so its event appends share the service event mutex.
	skillGate := conversation.NewSkillGate(nil, userSkillsDir(), filepath.Join(p.Share, ".stable", "skills"))
	hookGate := conversation.NewHookGate(nil, userHooksPath(), filepath.Join(p.Share, ".stable", "hooks.yaml"))
	mcpManager := mcp.NewManager(c.MCPServers, filepath.Join(p.Share, ".stable", "mcp.yaml"))
	if configPath, configErr := appconfig.ConfigPath(); configErr == nil {
		mcpManager.SetUserConfigPath(configPath)
	}
	if err := mcpManager.ConnectAll(ctx); err != nil {
		log.Printf("MCP startup: %v", err)
	}
	defer mcpManager.Shutdown()
	if streamingProvider, streamErr := llm.NewProvider(c.Model); streamErr == nil {
		snapshotStore, err = candidate.NewSnapshotStore(p.Share, c.Snapshots.ProjectBytes(), c.Snapshots.ManifestsPerCandidate(), snapshotCredentials(c.Model.APIKey))
		if err != nil {
			return fmt.Errorf("candidate snapshot store: %w", err)
		}
		executorFactory = execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
			Sandbox:            sbx,
			Gate:               execution.StorePermissionGate{Store: s},
			Approvals:          s,
			Candidates:         s,
			HelperPath:         p.HelperBinary("agentworker"),
			SessionRoot:        p.Share,
			ProviderCredential: c.Model.APIKey,
			Snapshots:          snapshotStore,
			QuestionSink:       askSink,
			TodoProvider:       todoProvider,
		}, execution.WithPlanSink(planSink), execution.WithSkillProvider(skillGate), execution.WithHookRunner(hookGate), execution.WithMCPCaller(mcpManager), execution.WithMemoryProvider(memoryGate))
		toolSchemas = runtimeToolSchemas(mcpManager)
		contextManager, fellBack := sessioncontext.NewManager(c.Model.ContextWindowTokens, chatProvider)
		if fellBack {
			log.Printf("invalid context_window_tokens %d; using default %d", c.Model.ContextWindowTokens, sessioncontext.DefaultWindowTokens)
		}
		runner = agent.NewRunner(streamingProvider, agent.RunnerOptions{ExecutorFactory: executorFactory, ToolSchemas: toolSchemas, ContextManager: contextManager})
	} else {
		runnerError = streamErr.Error()
	}
	permissionService := &permission.PermissionService{Repository: s, NewID: func() string { id, _ := sessionlog.NewID(); return id }}
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: s, Provider: provider, ChatProvider: chatProvider, Runner: runner, ExecutorFactory: executorFactory, ToolSchemas: toolSchemas, PermissionService: permissionService, RunnerError: runnerError, ProviderCredential: c.Model.APIKey, ProviderName: c.Model.Provider, Model: c.Model.Model, Temporal: address, ProjectRoot: p.Share, RunRoot: p.Goals, SocketPath: p.ChatSocket,
		Refresher:           refresher,
		CandidateCheckers:   []candidate.Checker{candidate.KicadERCChecker{Sandbox: sbx, RunRoot: p.Goals}},
		ContextWindowTokens: c.Model.ContextWindowTokens,
		Snapshots:           snapshotStore,
		Skills:              skillGate,
		Hooks:               hookGate,
		MCP:                 mcpManager,
		Memory:              memoryGate,
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

func waitChatSocket(path string, done <-chan struct{}, limit time.Duration) error {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return errors.New("session service exited")
		case <-deadline.C:
			return errors.New("session service readiness timed out")
		case <-tick.C:
			conn, err := ipc.DialPrivate(path, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				return nil
			}
		}
	}
}

func waitPort(address string, cmd *exec.Cmd, limit time.Duration) error {
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		if !proc.Alive(cmd) {
			return errors.New("process exited")
		}
		c, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("port readiness timed out")
}
func waitReady(path string, cmd *exec.Cmd, limit time.Duration) error {
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		if !proc.Alive(cmd) {
			return errors.New("process exited")
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "agent worker ready:") {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("worker readiness timed out")
}
