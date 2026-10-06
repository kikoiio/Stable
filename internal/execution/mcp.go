package execution

import "context"

// MCPToolSchema is the minimal view of an MCP tool exposed to the model side.
type MCPToolSchema struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// MCPCaller is implemented by the MCP Manager held by the conversation service
// and injected into the executor via WithMCPCaller. The implementation lives in
// the internal/mcp package (which imports the MCP SDK); this package only sees
// the interface and never depends on the SDK.
type MCPCaller interface {
	// CallTool is shared by direct call and dispatch: server and tool are the
	// normalized names, and args have already been coerced by the caller
	// against the target schema. It returns the text output (multiple entries
	// already merged); a server-side business error is flagged with isError,
	// while a transport-layer failure is returned as err.
	CallTool(ctx context.Context, server, tool string, args map[string]any) (output string, isError bool, err error)
	// EagerSchemas returns the provider schemas of the eager-tier tools
	// (fixed after connection, unchanged at runtime, sorted by name).
	EagerSchemas() []MCPToolSchema
	// DispatchTools returns the dispatch-tier tool list (for tool_search
	// retrieval, sorted by name).
	DispatchTools() []MCPToolSchema
	// InputSchema returns the raw input schema of the target tool (the basis
	// for argument coercion), looked up by the normalized server/tool names.
	InputSchema(server, tool string) (map[string]any, bool)
	// ResolveTarget resolves a model-provided target into the normalized
	// server/tool pair. Acceptable query forms:
	//   - the full name "mcp__srv__tool" as exposed to the model;
	//   - the server/tool pair "srv__tool" (a caller holding separate strings
	//     joins them with "__");
	//   - a bare tool name matched uniquely as a suffix against all tools.
	// An unknown or ambiguous query returns an error carrying the full list
	// of available tools as guidance.
	ResolveTarget(query string) (server, tool string, err error)
	// Instructions returns the text merged from each server's handshake
	// instructions, deduplicated per server (may be empty).
	Instructions() string
}
