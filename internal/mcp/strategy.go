// Tool-loading strategy for MCP tools: split the connected server tool set
// into two tiers, decided once per session right after discovery.
//
//	eager    — total schema volume stays under 10% of the model context
//	            window: every schema is rendered into tools[]. The context
//	            saved by deferring would not justify the extra indirection.
//	dispatch — everything else: MCP tools stay out of tools[] and are
//	            reached through a dispatch entry point backed by tool
//	            search.
//
// The tier must stay fixed for the whole session: tools[] renders between
// the system prompt and the message history, so any membership change
// invalidates the prompt cache for the entire conversation that follows.
// STABLE_MCP_LOADING=eager|dispatch forces the tier and skips the size
// check; any other value is ignored.
package mcp

import (
	"encoding/json"
	"os"
	"slices"
	"strings"

	"stable/internal/execution"
)

const (
	// DefaultEagerThresholdPercent is the share of the context window (in
	// estimated tokens) under which the whole tool set loads eager.
	DefaultEagerThresholdPercent = 10

	// CharsPerToken is the estimation ratio used when no real token count
	// is available. MCP schemas are JSON with high symbol density, so
	// characters per token run below natural-language text.
	CharsPerToken = 2.5

	// DefaultContextWindow is the fallback context window used to compute
	// the eager budget when no model metadata supplies one.
	DefaultContextWindow = 200_000

	// envLoadingOverride forces the tier regardless of schema size.
	envLoadingOverride = "STABLE_MCP_LOADING"
)

// mcpToolPrefix is the common name prefix of every MCP-exposed tool. It is
// also spelled out literally in MCPToolNamePrefix (sanitize.go).
const mcpToolPrefix = "mcp__"

// ContextWindow supplies the context window used for the eager budget when
// Apply is called without an explicit window. It is a package-level
// variable so wiring code can re-point it at real model metadata instead
// of editing the decision logic; callers that already know the window can
// pass it explicitly to applyWithWindow and bypass this variable.
//
// Hosts may replace ContextWindow with model metadata before calling Apply;
// the default remains a conservative fallback for callers without metadata.
var ContextWindow = func() int {
	return DefaultContextWindow
}

// EstimateSchemaTokens estimates the token count of schemaChars serialized
// characters. The int conversion truncates, matching the upstream
// implementation this is ported from.
func EstimateSchemaTokens(schemaChars int) int {
	return int(float64(schemaChars) / CharsPerToken)
}

// MeasureSchemaChars estimates the serialized character count used for the
// tier decision. Only tools whose Name carries the mcp__ prefix count.
//
// Each tool contributes the JSON length of a {name, description,
// input_schema} object — the same object shape the upstream
// implementation serializes through its tool Schema() — falling back to
// len(Name)+len(Description) when serialization fails, as upstream does.
func MeasureSchemaChars(tools []execution.MCPToolSchema) int {
	total := 0
	for _, t := range tools {
		if !strings.HasPrefix(t.Name, mcpToolPrefix) {
			continue
		}
		b, err := json.Marshal(map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.InputSchema,
		})
		if err != nil {
			total += len(t.Name) + len(t.Description)
			continue
		}
		total += len(b)
	}
	return total
}

// Decide reports whether the MCP tool set measuring schemaChars serialized
// characters loads eager (true) or behind dispatch (false) under the given
// context window. The rule is strict: estimated tokens must be strictly
// below the budget (contextWindow × DefaultEagerThresholdPercent) for
// eager, so a set landing exactly on the budget dispatches. An empty or
// measurement-less set is eager. STABLE_MCP_LOADING=eager|dispatch forces
// the answer before any size check; any other value is ignored.
func Decide(schemaChars, contextWindow int) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envLoadingOverride))) {
	case "eager":
		return true
	case "dispatch":
		return false
	}
	if schemaChars <= 0 {
		return true
	}
	budget := float64(contextWindow) * float64(DefaultEagerThresholdPercent) / 100
	return float64(EstimateSchemaTokens(schemaChars)) < budget
}

// Apply splits the MCP tool set into the eager and dispatch tiers. The
// tier is decided once for the whole set from the total serialized schema
// size under ContextWindow(); STABLE_MCP_LOADING overrides it. Only names
// carrying the mcp__ prefix enter either tier. Both lists are sorted by
// Name (stable: equal names keep their input order).
func Apply(tools []execution.MCPToolSchema) (eager, dispatch []execution.MCPToolSchema) {
	return applyWithWindow(tools, ContextWindow())
}

// applyWithWindow is Apply with an explicit context window, so callers
// holding model metadata (and tests) need not touch the ContextWindow
// variable.
func applyWithWindow(tools []execution.MCPToolSchema, contextWindow int) (eager, dispatch []execution.MCPToolSchema) {
	mcpTools := make([]execution.MCPToolSchema, 0, len(tools))
	for _, t := range tools {
		if strings.HasPrefix(t.Name, mcpToolPrefix) {
			mcpTools = append(mcpTools, t)
		}
	}
	slices.SortStableFunc(mcpTools, func(a, b execution.MCPToolSchema) int {
		return strings.Compare(a.Name, b.Name)
	})
	if Decide(MeasureSchemaChars(mcpTools), contextWindow) {
		return mcpTools, nil
	}
	return nil, mcpTools
}
