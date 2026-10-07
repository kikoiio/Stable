// Package agentcatalog loads the read-only roles available to named agents.
package agentcatalog

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	MaxFileBytes        = 64 * 1024
	MaxNameBytes        = 64
	MaxDescriptionBytes = 256
	MaxTurns            = 8
)

// Definition is an immutable role snapshot. Instruction is deliberately never
// serialized: callers publish Metadata instead of the execution definition.
type Definition struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Model           string   `json:"model"`
	Instruction     string   `json:"-"`
	Source          string   `json:"source"`
	Tools           []string `json:"tools"`
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
	MaxTurns        int      `json:"max_turns"`
	Background      bool     `json:"background"`
}

// Metadata contains only the public role inventory, including effective tools.
type Metadata struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Model           string   `json:"model"`
	Source          string   `json:"source"`
	Tools           []string `json:"tools"`
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
	MaxTurns        int      `json:"max_turns"`
	Background      bool     `json:"background"`
	ReadOnly        bool     `json:"read_only"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Known names are the controlled host tools, not arbitrary MCP or source-side
// aliases. Recognizing a write tool here never grants it to a child.
var knownTools = map[string]bool{
	"read_file": true, "glob": true, "grep": true,
	"write_file": true, "edit_file": true, "command": true,
	"ask_user": true, "exit_plan_mode": true,
	"task_create": true, "task_get": true, "task_list": true, "task_update": true,
	"load_skill": true, "mcp_call": true, "tool_search": true,
	"delegate_tasks": true, "run_agent": true, "task_output": true, "task_stop": true,
}

// EffectiveTools intersects both definition rules with the host child allowlist.
// A nil tools list inherits that list; an explicitly empty list allows no tools.
func (d Definition) EffectiveTools() []string {
	allowed := map[string]bool{"read_file": true, "glob": true, "grep": true}
	if d.Tools != nil {
		requested := make(map[string]bool, len(d.Tools))
		for _, name := range d.Tools {
			requested[name] = true
		}
		for name := range allowed {
			if !requested[name] {
				delete(allowed, name)
			}
		}
	}
	for _, name := range d.DisallowedTools {
		delete(allowed, name)
	}
	out := make([]string, 0, len(allowed))
	for name := range allowed {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (d Definition) Metadata() Metadata {
	return Metadata{Name: d.Name, Description: d.Description, Model: d.Model, Source: d.Source,
		Tools: d.EffectiveTools(), DisallowedTools: cloneStrings(d.DisallowedTools),
		MaxTurns: d.MaxTurns, Background: d.Background, ReadOnly: true}
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append(make([]string, 0, len(values)), values...)
}

func cloneDefinition(d Definition) Definition {
	d.Tools = cloneStrings(d.Tools)
	d.DisallowedTools = cloneStrings(d.DisallowedTools)
	return d
}

// ParseDefinition strictly parses a Markdown frontmatter block. Error messages
// describe the invalid field without echoing YAML values or the role body.
func ParseDefinition(data []byte) (Definition, error) {
	var result Definition
	if len(data) > MaxFileBytes {
		return result, errors.New("definition exceeds 64 KiB")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return result, errors.New("definition must be UTF-8 text without NUL")
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return result, errors.New("definition requires Markdown YAML frontmatter")
	}
	end := 0
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end == 0 {
		return result, errors.New("frontmatter closing delimiter is missing")
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(strings.Join(lines[1:end], "\n")))
	if err := decoder.Decode(&doc); err != nil {
		return result, errors.New("invalid YAML frontmatter")
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF {
		return result, errors.New("frontmatter must contain one YAML document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return result, errors.New("frontmatter must be a mapping")
	}
	fields := map[string]*yaml.Node{}
	for i := 0; i < len(doc.Content[0].Content); i += 2 {
		key, value := doc.Content[0].Content[i], doc.Content[0].Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return result, errors.New("frontmatter field name must be a string")
		}
		switch key.Value {
		case "name", "description", "model", "tools", "disallowedTools", "maxTurns", "background":
		case "isolation", "permissionMode":
			return result, errors.New("isolation and permissionMode are unsupported in read-only M09-D; workspace and write capabilities require M09-F")
		case "team_name":
			return result, errors.New("team_name is unsupported in M09-D; team coordination requires M09-E")
		default:
			return result, errors.New("unsupported frontmatter field (only name, description, model, tools, disallowedTools, maxTurns and background are supported)")
		}
		if _, ok := fields[key.Value]; ok {
			return result, errors.New("duplicate frontmatter field")
		}
		fields[key.Value] = value
	}
	stringField := func(name string) (string, error) {
		node := fields[name]
		if node == nil {
			return "", nil
		}
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return "", errors.New(name + " must be a string")
		}
		return strings.TrimSpace(node.Value), nil
	}
	var err error
	if result.Name, err = stringField("name"); err != nil {
		return Definition{}, err
	}
	result.Name = strings.ToLower(result.Name)
	if len(result.Name) > MaxNameBytes || !namePattern.MatchString(result.Name) {
		return Definition{}, errors.New("name must be 1-64 ASCII letters, digits, underscores or hyphens, starting with a letter or digit")
	}
	if result.Description, err = stringField("description"); err != nil {
		return Definition{}, err
	}
	if result.Description == "" || len(result.Description) > MaxDescriptionBytes || hasControl(result.Description) {
		return Definition{}, errors.New("description must be 1-256 bytes without control characters")
	}
	if result.Model, err = stringField("model"); err != nil {
		return Definition{}, err
	}
	if result.Model == "" || strings.EqualFold(result.Model, "inherit") {
		result.Model = "inherit"
	}
	if len(result.Model) > MaxDescriptionBytes || strings.IndexFunc(result.Model, unicode.IsSpace) >= 0 || hasControl(result.Model) {
		return Definition{}, errors.New("model must be a name of at most 256 bytes without whitespace or controls")
	}
	if result.Tools, err = parseTools(fields["tools"]); err != nil {
		return Definition{}, errors.New("tools: " + err.Error())
	}
	if result.DisallowedTools, err = parseTools(fields["disallowedTools"]); err != nil {
		return Definition{}, errors.New("disallowedTools: " + err.Error())
	}
	result.MaxTurns = MaxTurns
	if node := fields["maxTurns"]; node != nil {
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" || node.Decode(&result.MaxTurns) != nil || result.MaxTurns < 1 || result.MaxTurns > MaxTurns {
			return Definition{}, errors.New("maxTurns must be an integer between 1 and 8")
		}
	}
	if node := fields["background"]; node != nil {
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" || node.Decode(&result.Background) != nil {
			return Definition{}, errors.New("background must be a boolean")
		}
	}
	result.Instruction = strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	if result.Instruction == "" {
		return Definition{}, errors.New("role instruction body is required")
	}
	return result, nil
}

func hasControl(value string) bool { return strings.IndexFunc(value, unicode.IsControl) >= 0 }

func parseTools(node *yaml.Node) ([]string, error) {
	if node == nil {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, errors.New("must be a list of known tool names")
	}
	seen := make(map[string]bool, len(node.Content))
	out := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
			return nil, errors.New("must contain only strings")
		}
		name := strings.ToLower(strings.TrimSpace(item.Value))
		if !knownTools[name] {
			return nil, errors.New("contains an unknown tool name")
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
