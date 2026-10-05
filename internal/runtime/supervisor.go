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
	"syscall"
	"time"

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

func Control(p Paths, cmd string) (Status, error) {
	var s Status
	conn, err := net.DialTimeout("unix", p.Socket, 500*time.Millisecond)
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

func Up(ctx context.Context, c appconfig.AppConfig, p Paths) (Status, error) {
	if s, err := Control(p, "status"); err == nil && s.Running {
		return s, nil
	}
	if err := p.Prepare(); err != nil {
		return Status{}, err
	}
	f, err := os.OpenFile(p.SupervisorLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return Status{}, err
	}
	defer f.Close()
	cmd := exec.Command(p.Root+"/bin/stable", "supervise")
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
	return s.ReconcileAcceptances(ctx)
}

func Supervise(c appconfig.AppConfig, p Paths) error {
	if err := p.Prepare(); err != nil {
		return err
	}
	lock, err := os.OpenFile(p.Lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	// A previous supervisor may still be releasing children after a crash or
	// down; wait for the lock instead of failing the fresh start outright.
	lockDeadline := time.Now().Add(20 * time.Second)
	for {
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
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
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if _, err := Control(p, "status"); err == nil {
		return errors.New("runtime already running")
	}
	if err := ReconcileBeforeDispatch(context.Background(), p.Database); err != nil {
		return fmt.Errorf("reconcile interrupted acceptances: %w", err)
	}
	_ = os.Remove(p.Socket)
	address := "127.0.0.1:" + strconv.Itoa(c.TemporalPort)
	if conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("Temporal port %d is occupied", c.TemporalPort)
	}
	listener, err := net.Listen("unix", p.Socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(p.Socket)
	defer os.Remove(p.ChatSocket)
	if err = os.Chmod(p.Socket, 0600); err != nil {
		return err
	}
	temporalLog, err := os.OpenFile(p.TemporalLog, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer temporalLog.Close()
	workerLog, err := os.OpenFile(p.WorkerLog, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer workerLog.Close()
	temporal := exec.Command(filepath.Join(p.Libexec, "temporal"), "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", strconv.Itoa(c.TemporalPort), "--db-filename", p.TemporalDB)
	temporal.Stdout = temporalLog
	temporal.Stderr = temporalLog
	if err = temporal.Start(); err != nil {
		return err
	}
	defer stopProcess(temporal)
	if err = waitPort(address, temporal, 20*time.Second); err != nil {
		return fmt.Errorf("Temporal startup: %w; see %s", err, p.TemporalLog)
	}
	worker := exec.Command(filepath.Join(p.Libexec, "agentworker"), "--app-config", "--db", p.Database, "--run-root", p.Goals, "--temporal", address, "--project-root", p.Share, "--chat-socket", p.ChatSocket)
	worker.Env = os.Environ()
	worker.Stdout = workerLog
	worker.Stderr = workerLog
	if err = worker.Start(); err != nil {
		return err
	}
	defer stopProcess(worker)
	if err = waitReady(p.WorkerLog, worker, 15*time.Second); err != nil {
		return fmt.Errorf("Worker startup: %w; see %s", err, p.WorkerLog)
	}
	chatDone := startChatService(c, p, address)
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
				if !processAlive(temporal) || !processAlive(worker) {
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
		if !processAlive(temporal) || !processAlive(worker) {
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

func runtimeToolSchemas() []llm.ToolSchema {
	nameMap := map[string]string{
		"read_file":  "read_file",
		"write_file": "write_file",
		"edit_file":  "edit_file",
		"glob":       "glob",
		"grep":       "grep",
	}
	registry := tools.CreateDefaultTools().Registry
	schemas := make([]llm.ToolSchema, 0, len(nameMap)+1)
	for _, schema := range registry.GetAllSchemas() {
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

// startChatService runs the persistent conversation service inside the
// supervisor process. It returns a channel that closes when the service stops.
func startChatService(c appconfig.AppConfig, p Paths, address string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := runChatService(c, p, address); err != nil {
			if f, ferr := os.OpenFile(p.ChatLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); ferr == nil {
				fmt.Fprintf(f, "%s chat session service: %v\n", time.Now().UTC().Format(time.RFC3339), err)
				f.Close()
			}
		}
	}()
	return done
}

func runChatService(c appconfig.AppConfig, p Paths, address string) error {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refresher, err := dependency.NewKiCadRefresher(s, p.Goals, p.Share, address)
	if err != nil {
		return err
	}
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
	if streamingProvider, streamErr := llm.NewProvider(c.Model); streamErr == nil {
		snapshotStore, err = candidate.NewSnapshotStore(p.Share, c.Snapshots.ProjectBytes(), c.Snapshots.ManifestsPerCandidate(), snapshotCredentials(c.Model.APIKey))
		if err != nil {
			return fmt.Errorf("candidate snapshot store: %w", err)
		}
		executorFactory = execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
			Sandbox:            sandbox.LinuxManager{},
			Gate:               execution.StorePermissionGate{Store: s},
			Approvals:          s,
			Candidates:         s,
			HelperPath:         filepath.Join(p.Libexec, "agentworker"),
			SessionRoot:        p.Share,
			ProviderCredential: c.Model.APIKey,
			Snapshots:          snapshotStore,
			QuestionSink:       askSink,
			TodoProvider:       todoProvider,
		}, execution.WithPlanSink(planSink))
		toolSchemas = runtimeToolSchemas()
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
		CandidateCheckers:   []candidate.Checker{candidate.KicadERCChecker{Sandbox: sandbox.LinuxManager{}, RunRoot: p.Goals}},
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
	select {}
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
			conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				return nil
			}
		}
	}
}

func processAlive(cmd *exec.Cmd) bool {
	if cmd.Process == nil || cmd.Process.Signal(syscall.Signal(0)) != nil {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	return len(fields) > 2 && fields[2] != "Z"
}
func stopProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}
func waitPort(address string, cmd *exec.Cmd, limit time.Duration) error {
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		if !processAlive(cmd) {
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
		if !processAlive(cmd) {
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
