package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"stable/internal/appconfig"
	"stable/internal/execution"
)

const (
	defaultConnectTimeout = 15 * time.Second
	maxServerTools        = 200
	maxMCPOutput          = 50_000
)

// ServerStatus is the service-facing status of one configured MCP server.
type ServerStatus struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	ToolCount int    `json:"tool_count"`
}

// Event describes a lifecycle change. It is intentionally independent of
// sessionlog so the manager can be used by tests and other hosts.
type Event struct {
	Name      string
	Source    string
	State     string
	Error     string
	ToolCount int
}

// ManagerOptions customizes lifecycle behavior without exposing SDK types to
// the conversation package.
type ManagerOptions struct {
	ConnectTimeout time.Duration
	OnEvent        func(Event)
	UserConfigPath string
}

// Manager owns all MCP connections for one conversation service.
type Manager struct {
	mu             sync.RWMutex
	user           []appconfig.MCPServerConfig
	userConfigPath string
	projectPath    string
	connectTimeout time.Duration
	onEvent        func(Event)
	servers        map[string]*serverConn
	statuses       []ServerStatus
	rejections     Rejections
	eager          []execution.MCPToolSchema
	dispatch       []execution.MCPToolSchema
	tools          map[string]toolRef
	lastProjectMod time.Time
	lastUserMod    time.Time
	loaded         bool
	revision       uint64
}

type serverConn struct {
	cfg          appconfig.MCPServerConfig
	source       string
	session      *sdkmcp.ClientSession
	tools        []toolRef
	instructions string
}

type toolRef struct {
	server        string
	serverDisplay string
	tool          string
	toolDisplay   string
	fullName      string
	description   string
	inputSchema   map[string]any
	session       *sdkmcp.ClientSession
}

// NewManager creates a manager from the user-level entries and project-level
// .stable/mcp.yaml path. Connections are established by ConnectAll.
func NewManager(user []appconfig.MCPServerConfig, projectPath string, options ...ManagerOptions) *Manager {
	opt := ManagerOptions{ConnectTimeout: defaultConnectTimeout}
	if len(options) > 0 {
		opt = options[0]
		if opt.ConnectTimeout <= 0 {
			opt.ConnectTimeout = defaultConnectTimeout
		}
	}
	return &Manager{
		user:           append([]appconfig.MCPServerConfig(nil), user...),
		projectPath:    projectPath,
		connectTimeout: opt.ConnectTimeout,
		onEvent:        opt.OnEvent,
		userConfigPath: opt.UserConfigPath,
		servers:        make(map[string]*serverConn),
		tools:          make(map[string]toolRef),
	}
}

// Load returns the latest merged configuration and rejection report.
func (m *Manager) Load() ([]appconfig.MCPServerConfig, Rejections) {
	merged, rejections := m.loadConfigs()
	m.mu.Lock()
	m.rejections = append(Rejections(nil), rejections...)
	m.mu.Unlock()
	return merged, rejections
}

func (m *Manager) loadConfigs() ([]appconfig.MCPServerConfig, Rejections) {
	merged, rejections := LoadMerge(m.user, m.projectPath)
	return merged, rejections
}

// ConnectAll connects every valid server. A failed server is retained in the
// status list and does not prevent other servers from connecting.
func (m *Manager) ConnectAll(ctx context.Context) error {
	configs, rejections := m.loadConfigs()
	m.closeConnections()
	connected := make(map[string]*serverConn, len(configs))
	statuses := make([]ServerStatus, 0, len(configs))
	for _, cfg := range configs {
		source := m.sourceFor(cfg)
		conn, err := m.connectOne(ctx, cfg, source)
		if err != nil {
			status := ServerStatus{Name: cfg.Name, Source: source, State: "reload-failed", Error: safeError(err)}
			statuses = append(statuses, status)
			m.emit(Event{Name: status.Name, Source: status.Source, State: status.State, Error: status.Error})
			continue
		}
		connected[cfg.Name] = conn
		status := ServerStatus{Name: cfg.Name, Source: source, State: "connected", ToolCount: len(conn.tools)}
		statuses = append(statuses, status)
		m.emit(Event{Name: status.Name, Source: status.Source, State: status.State, ToolCount: status.ToolCount})
	}
	m.mu.Lock()
	m.servers = connected
	m.statuses = statuses
	m.rejections = append(Rejections(nil), rejections...)
	m.rebuildIndexesLocked()
	m.lastProjectMod = m.projectModTime()
	m.lastUserMod = m.userModTime()
	m.loaded = true
	m.revision++
	m.mu.Unlock()
	return nil
}

func (m *Manager) connectOne(parent context.Context, cfg appconfig.MCPServerConfig, source string) (*serverConn, error) {
	ctx, cancel := context.WithTimeout(parent, m.connectTimeout)
	defer cancel()
	transport, err := m.transport(cfg)
	if err != nil {
		return nil, err
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "stable", Version: "m07c"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	result, err := session.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	if len(result.Tools) > maxServerTools {
		_ = session.Close()
		return nil, fmt.Errorf("server returned %d tools; limit is %d", len(result.Tools), maxServerTools)
	}
	conn := &serverConn{cfg: cfg, source: source, session: session}
	for _, tool := range result.Tools {
		if tool == nil || strings.TrimSpace(tool.Name) == "" {
			continue
		}
		conn.tools = append(conn.tools, toolRef{
			server:        SanitizeName(cfg.Name),
			serverDisplay: cfg.Name,
			tool:          SanitizeName(tool.Name),
			toolDisplay:   tool.Name,
			fullName:      ToolName(cfg.Name, tool.Name),
			description:   tool.Description,
			inputSchema:   schemaMap(tool.InputSchema),
			session:       session,
		})
	}
	if init := session.InitializeResult(); init != nil {
		conn.instructions = strings.TrimSpace(init.Instructions)
	}
	return conn, nil
}

func (m *Manager) transport(cfg appconfig.MCPServerConfig) (sdkmcp.Transport, error) {
	if strings.TrimSpace(cfg.Command) != "" {
		cmd := exec.Command(cfg.Command, cfg.Args...)
		env := append([]string(nil), os.Environ()...)
		for key, value := range cfg.Env {
			env = append(env, key+"="+os.ExpandEnv(value))
		}
		cmd.Env = env
		return &discoverTimeoutTransport{Transport: &sdkmcp.CommandTransport{Command: cmd}}, nil
	}
	endpoint := os.ExpandEnv(strings.TrimSpace(cfg.URL))
	if endpoint == "" {
		return nil, errors.New("MCP server has no command or url")
	}
	httpClient := &http.Client{Transport: &headerRoundTripper{base: http.DefaultTransport, headers: expandedHeaders(cfg.Headers)}, Timeout: m.connectTimeout}
	if strings.EqualFold(cfg.Transport, "sse") {
		return &sdkmcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: httpClient}, nil
	}
	return &sdkmcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true}, nil
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	copyReq := req.Clone(req.Context())
	for key, value := range t.headers {
		copyReq.Header.Set(key, value)
	}
	return base.RoundTrip(copyReq)
}

func expandedHeaders(headers map[string]string) map[string]string {
	result := make(map[string]string, len(headers))
	for key, value := range headers {
		result[key] = os.ExpandEnv(value)
	}
	return result
}

func schemaMap(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if schema, ok := value.(map[string]any); ok {
		return schema
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return map[string]any{}
	}
	return schema
}

func (m *Manager) rebuildIndexesLocked() {
	m.tools = make(map[string]toolRef)
	var all []execution.MCPToolSchema
	for _, server := range m.servers {
		for _, tool := range server.tools {
			// Later registration wins, matching the approved duplicate semantics.
			m.tools[tool.fullName] = tool
			all = append(all, execution.MCPToolSchema{Name: tool.fullName, Description: tool.description, InputSchema: tool.inputSchema})
		}
	}
	m.eager, m.dispatch = Apply(all)
}

func (m *Manager) sourceFor(cfg appconfig.MCPServerConfig) string {
	project, _ := loadProjectServers(m.projectPath)
	for _, entry := range project {
		if entry.Name == cfg.Name && reflect.DeepEqual(entry, cfg) {
			return "project"
		}
	}
	return "user"
}

func (m *Manager) closeConnections() {
	m.mu.Lock()
	old := m.servers
	m.servers = make(map[string]*serverConn)
	m.eager, m.dispatch = nil, nil
	m.tools = make(map[string]toolRef)
	m.mu.Unlock()
	for _, server := range old {
		closeServer(server, m)
	}
}

func closeServer(server *serverConn, manager *Manager) {
	if server == nil || server.session == nil {
		return
	}
	_ = server.session.Close()
	manager.emit(Event{Name: server.cfg.Name, Source: server.source, State: "disconnected", ToolCount: len(server.tools)})
}

// Reload rebuilds the server set from both configuration layers.
func (m *Manager) Reload() (before, after int, rejections []string, err error) {
	m.mu.RLock()
	before = len(m.statuses)
	old := make(map[string]*serverConn, len(m.servers))
	for name, server := range m.servers {
		old[name] = server
	}
	m.mu.RUnlock()
	configs, configRejections := m.loadConfigs()
	connected := make(map[string]*serverConn, len(configs))
	statuses := make([]ServerStatus, 0, len(configs))
	kept := make(map[string]bool, len(old))
	for _, cfg := range configs {
		source := m.sourceFor(cfg)
		if previous := old[cfg.Name]; previous != nil && reflect.DeepEqual(previous.cfg, cfg) {
			connected[cfg.Name] = previous
			kept[cfg.Name] = true
			statuses = append(statuses, ServerStatus{Name: cfg.Name, Source: source, State: "connected", ToolCount: len(previous.tools)})
			continue
		}
		conn, connectErr := m.connectOne(context.Background(), cfg, source)
		if connectErr != nil {
			status := ServerStatus{Name: cfg.Name, Source: source, State: "reload-failed", Error: safeError(connectErr)}
			statuses = append(statuses, status)
			m.emit(Event{Name: status.Name, Source: status.Source, State: status.State, Error: status.Error})
			continue
		}
		connected[cfg.Name] = conn
		status := ServerStatus{Name: cfg.Name, Source: source, State: "connected", ToolCount: len(conn.tools)}
		statuses = append(statuses, status)
		m.emit(Event{Name: status.Name, Source: status.Source, State: status.State, ToolCount: status.ToolCount})
	}
	for name, previous := range old {
		if !kept[name] {
			closeServer(previous, m)
		}
	}
	m.mu.Lock()
	m.servers = connected
	m.statuses = statuses
	m.rejections = append(Rejections(nil), configRejections...)
	m.rebuildIndexesLocked()
	m.lastProjectMod = m.projectModTime()
	m.lastUserMod = m.userModTime()
	m.loaded = true
	m.revision++
	m.mu.Unlock()
	m.mu.RLock()
	after = len(m.statuses)
	rejections = append([]string(nil), m.rejections...)
	m.mu.RUnlock()
	return before, after, rejections, err
}

// EnsureFresh reloads when the project configuration mtime changes.
func (m *Manager) EnsureFresh() error {
	projectMod, userMod := m.projectModTime(), m.userModTime()
	m.mu.RLock()
	changed := !projectMod.Equal(m.lastProjectMod) || !userMod.Equal(m.lastUserMod)
	initialized := m.loaded
	userPath := m.userConfigPath
	lastUserMod := m.lastUserMod
	m.mu.RUnlock()
	if initialized && !changed {
		return nil
	}
	if !initialized {
		return m.ConnectAll(context.Background())
	}
	if !userMod.Equal(lastUserMod) && userPath != "" {
		config, err := appconfig.Load()
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.user = append([]appconfig.MCPServerConfig(nil), config.MCPServers...)
		m.mu.Unlock()
	}
	_, _, _, err := m.Reload()
	return err
}

// SetUserConfigPath enables mtime based refresh of the user-level config.
// Runtime hosts call this with appconfig.ConfigPath; tests can omit it and
// supply the initial user entries directly.
func (m *Manager) SetUserConfigPath(path string) { m.userConfigPath = path }

func (m *Manager) projectModTime() time.Time {
	if m.projectPath == "" {
		return time.Time{}
	}
	info, err := os.Stat(m.projectPath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func (m *Manager) userModTime() time.Time {
	if m.userConfigPath == "" {
		return time.Time{}
	}
	info, err := os.Stat(m.userConfigPath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// Shutdown closes all SDK sessions and releases stdio child processes.
func (m *Manager) Shutdown() {
	m.closeConnections()
	m.mu.Lock()
	for i := range m.statuses {
		if m.statuses[i].State == "connected" {
			m.statuses[i].State = "disconnected"
		}
	}
	m.mu.Unlock()
}

// Status returns a stable snapshot sorted by configured order.
func (m *Manager) Status() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]ServerStatus(nil), m.statuses...)
}

// Rejections returns the last configuration rejection report.
func (m *Manager) Rejections() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.rejections...)
}

// Configured reports whether either configuration layer declares at least one
// valid server. It is used to keep the provider schema unchanged for users who
// do not enable MCP.
func (m *Manager) Configured() bool {
	configs, _ := m.loadConfigs()
	return len(configs) > 0
}

// Revision increments after each configuration load that rebuilds the server
// set. Conversation services use it to journal automatic refreshes once.
func (m *Manager) Revision() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.revision
}

func (m *Manager) EagerSchemas() []execution.MCPToolSchema {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]execution.MCPToolSchema(nil), m.eager...)
}

func (m *Manager) DispatchTools() []execution.MCPToolSchema {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]execution.MCPToolSchema(nil), m.dispatch...)
}

func (m *Manager) Instructions() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var parts []string
	for _, server := range m.servers {
		if server.instructions != "" {
			parts = append(parts, fmt.Sprintf("MCP server %s:\n%s", server.cfg.Name, server.instructions))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n\n")
}

func (m *Manager) ResolveTarget(query string) (string, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	query = strings.TrimSpace(query)
	if strings.HasPrefix(query, "mcp__") {
		if tool, ok := m.tools[query]; ok {
			return tool.server, tool.tool, nil
		}
	} else if strings.Count(query, "__") == 1 {
		parts := strings.SplitN(query, "__", 2)
		key := "mcp__" + parts[0] + "__" + parts[1]
		if tool, ok := m.tools[key]; ok {
			return tool.server, tool.tool, nil
		}
	} else if query != "" {
		var matches []toolRef
		for _, tool := range m.tools {
			if tool.tool == SanitizeName(query) || tool.toolDisplay == query {
				matches = append(matches, tool)
			}
		}
		if len(matches) == 1 {
			return matches[0].server, matches[0].tool, nil
		}
		if len(matches) > 1 {
			return "", "", fmt.Errorf("ambiguous MCP tool %q; choose one of: %s", query, m.availableToolsLocked())
		}
	}
	return "", "", fmt.Errorf("unknown MCP tool %q (available: %s)", query, m.availableToolsLocked())
}

func (m *Manager) availableToolsLocked() string {
	names := make([]string, 0, len(m.tools))
	for name := range m.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (m *Manager) InputSchema(server, tool string) (map[string]any, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := "mcp__" + SanitizeName(server) + "__" + SanitizeName(tool)
	ref, ok := m.tools[key]
	if !ok {
		return nil, false
	}
	return cloneMap(ref.inputSchema), true
}

func (m *Manager) CallTool(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	m.mu.RLock()
	key := "mcp__" + SanitizeName(server) + "__" + SanitizeName(tool)
	ref, ok := m.tools[key]
	m.mu.RUnlock()
	if !ok || ref.session == nil {
		return "", false, fmt.Errorf("unknown MCP tool %q", key)
	}
	result, err := ref.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: ref.toolDisplay, Arguments: args})
	if err != nil {
		return "", false, err
	}
	if result == nil {
		return "(no output)", false, nil
	}
	var lines []string
	for _, content := range result.Content {
		if text, ok := content.(*sdkmcp.TextContent); ok && text.Text != "" {
			lines = append(lines, text.Text)
		}
	}
	output := strings.Join(lines, "\n")
	if output == "" {
		output = "(no output)"
	}
	if runes := []rune(output); len(runes) > maxMCPOutput {
		output = string(runes[:maxMCPOutput]) + "...[truncated]"
	}
	return output, result.IsError, nil
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return input
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return input
	}
	return out
}

func (m *Manager) emit(event Event) {
	if m.onEvent != nil {
		m.onEvent(event)
	}
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if len([]rune(text)) > 8*1024 {
		return string([]rune(text)[:8*1024]) + "...[truncated]"
	}
	return text
}

var _ execution.MCPCaller = (*Manager)(nil)
