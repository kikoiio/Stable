// Package mcp provides naming helpers for tools surfaced from MCP servers.
//
// Tool names are folded onto a restricted alphabet so they stay valid,
// unambiguous identifiers across model APIs and remain filterable by
// server through a shared prefix.
package mcp

import "regexp"

// nonAlphanumeric matches every character outside [A-Za-z0-9]. Both server
// and tool names are sanitized with it, so dashes, dots and spaces collapse
// into underscores.
var nonAlphanumeric = regexp.MustCompile(`[^A-Za-z0-9]`)

// SanitizeName replaces every character outside [A-Za-z0-9] in s with an
// underscore. Case is preserved: "chrome-devtools" becomes
// "chrome_devtools", while "TakeScreenshot" stays untouched apart from any
// punctuation. Sanitize is idempotent and safe on the empty string.
func SanitizeName(s string) string {
	return nonAlphanumeric.ReplaceAllString(s, "_")
}

// MCPToolNamePrefix returns the common prefix shared by every tool name
// exposed by the given server. Filter tools by server with this prefix
// instead of concatenating strings by hand: the server name is sanitized
// here, so punctuation such as dashes becomes underscores and a hand-built
// prefix would fail to match the sanitized tool names.
func MCPToolNamePrefix(server string) string {
	return "mcp__" + SanitizeName(server) + "__"
}

// ToolName returns the fully qualified name of a tool on the given server:
// the sanitized server prefix followed by the sanitized tool name. Server
// and tool are sanitized independently, so punctuation in either is folded
// to underscores.
func ToolName(server, tool string) string {
	return MCPToolNamePrefix(server) + SanitizeName(tool)
}
