package tools

import (
	"context"
	"sort"
	"strings"
)

// SkipDirs contains directories that are noisy or unsafe to scan recursively.
var SkipDirs = map[string]bool{
	".git": true, ".venv": true, "node_modules": true,
	"__pycache__": true, ".tox": true, ".mypy_cache": true,
}

func privateMetadataName(name string) bool {
	return strings.EqualFold(name, ".git") || strings.EqualFold(name, ".stable") || strings.EqualFold(name, ".mewcode")
}

// MaxOutputChars is the maximum tool-result size passed to callers.
const MaxOutputChars = 50000

type ToolResult struct {
	Output    string
	IsError   bool
	Additions int
	Removals  int
	DiffText  string
}

type ToolCategory string

const (
	CategoryRead    ToolCategory = "read"
	CategoryWrite   ToolCategory = "write"
	CategoryCommand ToolCategory = "command"
)

type Tool interface {
	Name() string
	Description() string
	Category() ToolCategory
	Schema() map[string]any
	Execute(ctx context.Context, args map[string]any) ToolResult
}

// ConcurrencySafeTool lets a tool classify an invocation based on its arguments.
type ConcurrencySafeTool interface {
	IsConcurrencySafe(args map[string]any) bool
}

func IsConcurrencySafe(t Tool, args map[string]any) bool {
	if cs, ok := t.(ConcurrencySafeTool); ok {
		return cs.IsConcurrencySafe(args)
	}
	return t.Category() == CategoryRead
}

type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(t Tool) {
	if t != nil {
		r.tools[t.Name()] = t
	}
}

func (r *Registry) Get(name string) Tool {
	return r.tools[name]
}

func (r *Registry) ListTools() []Tool {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]Tool, 0, len(names))
	for _, name := range names {
		result = append(result, r.tools[name])
	}
	return result
}

// ModelSchema returns the provider-facing schema for a tool. Internal tool
// implementations keep their historical names so helper dispatch remains
// unchanged, while models receive the snake_case names understood by the
// controlled executor.
func ModelSchema(t Tool) map[string]any {
	if t == nil {
		return nil
	}
	schema := cloneSchema(t.Schema())
	schema["name"] = modelToolName(t.Name())
	return schema
}

func modelToolName(name string) string {
	switch name {
	case "ReadFile":
		return "read_file"
	case "WriteFile":
		return "write_file"
	case "EditFile":
		return "edit_file"
	case "Glob":
		return "glob"
	case "Grep":
		return "grep"
	default:
		return name
	}
}

// CommandSchema is the provider-facing schema for the controlled command
// executor, which intentionally has no internal tools.Tool implementation.
func CommandSchema() map[string]any {
	return map[string]any{
		"name":        "command",
		"description": BashDescription,
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command to execute"},
				"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds", "default": 120, "minimum": 1, "maximum": 600},
			},
			"required": []string{"command"},
		},
	}
}

// GetAllSchemas returns provider-ready schemas in stable model-name order so
// serialized requests are deterministic and names match executor dispatch.
func (r *Registry) GetAllSchemas() []map[string]any {
	tools := r.ListTools()
	schemas := make([]map[string]any, 0, len(tools)+1)
	for _, t := range tools {
		schemas = append(schemas, ModelSchema(t))
	}
	schemas = append(schemas, CommandSchema())
	sort.Slice(schemas, func(i, j int) bool {
		left, _ := schemas[i]["name"].(string)
		right, _ := schemas[j]["name"].(string)
		return left < right
	})
	return schemas
}

func cloneSchema(schema map[string]any) map[string]any {
	clone := make(map[string]any, len(schema))
	for key, value := range schema {
		clone[key] = value
	}
	return clone
}

type DefaultTools struct {
	Registry  *Registry
	WriteFile *WriteFileTool
	EditFile  *EditFileTool
}

func CreateDefaultRegistry() *Registry {
	return CreateDefaultTools().Registry
}

func CreateDefaultToolsWithWorkDir(workDir string) DefaultTools {
	fsc := NewFileStateCache()
	wf := &WriteFileTool{FileStateCache: fsc}
	ef := &EditFileTool{FileStateCache: fsc}
	reg := NewRegistry()
	reg.Register(&ReadFileTool{FileStateCache: fsc})
	reg.Register(wf)
	reg.Register(ef)
	_ = workDir // command execution is provided by internal/execution
	reg.Register(&GlobTool{})
	reg.Register(&GrepTool{})
	return DefaultTools{Registry: reg, WriteFile: wf, EditFile: ef}
}

func CreateDefaultTools() DefaultTools {
	return CreateDefaultToolsWithWorkDir("")
}
