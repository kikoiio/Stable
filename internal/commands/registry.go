// Package commands provides the slash command registry and the loader for
// file-based custom prompt commands. Built-in commands are registered by the
// TUI with their own handlers; custom commands are markdown templates loaded
// from the project and user command directories that expand into user
// messages on the ordinary chat path.
package commands

import (
	"fmt"
	"sort"
	"strings"
)

// Kind distinguishes how a command is executed.
type Kind int

const (
	// KindPrompt commands expand into a user message via ExpandPrompt and
	// enter the ordinary chat path.
	KindPrompt Kind = iota
	// KindLocal commands are handled by the registering component through
	// the Local closure; they never reach the model.
	KindLocal
)

// Command is one slash command with its presentation metadata.
type Command struct {
	Name        string
	Description string
	ArgPrompt   string
	Aliases     []string
	Kind        Kind
	// Body is the raw prompt template of a KindPrompt command; it is
	// rendered with ExpandPrompt at dispatch time.
	Body string
	// Local handles the command in place; only used for KindLocal.
	Local func(args string)
}

// Registry holds commands with an alias index. The zero value is ready to
// use, but built-in registration should go through NewRegistry and Register.
type Registry struct {
	commands map[string]*Command
	aliases  map[string]string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		commands: make(map[string]*Command),
		aliases:  make(map[string]string),
	}
}

// Register adds a built-in command. It panics when the name or any alias
// collides with an already registered command or alias; this is a
// programming error in the fixed built-in set and must surface at startup.
func (r *Registry) Register(c *Command) {
	if err := r.conflict(c); err != nil {
		panic(err.Error())
	}
	r.insert(c)
}

// RegisterOptional adds a command unless its name or one of its aliases
// collides with something already registered, in which case nothing is
// inserted and it returns false. Dynamic sources (custom commands loaded
// from disk) use it so built-in commands keep priority.
func (r *Registry) RegisterOptional(c *Command) bool {
	if r.conflict(c) != nil {
		return false
	}
	r.insert(c)
	return true
}

// Find resolves a command by its name or one of its aliases.
func (r *Registry) Find(name string) (*Command, bool) {
	if c, ok := r.commands[name]; ok {
		return c, true
	}
	if canonical, ok := r.aliases[name]; ok {
		c, ok := r.commands[canonical]
		return c, ok
	}
	return nil, false
}

// List returns every command sorted by name; /help and completion share it.
func (r *Registry) List() []*Command {
	out := make([]*Command, 0, len(r.commands))
	for _, c := range r.commands {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// conflict returns a non-nil error when c cannot be inserted without
// colliding with an existing command name or alias.
func (r *Registry) conflict(c *Command) error {
	if c == nil {
		return fmt.Errorf("commands: nil command")
	}
	if c.Name == "" {
		return fmt.Errorf("commands: command name is required")
	}
	if _, exists := r.commands[c.Name]; exists {
		return fmt.Errorf("commands: duplicate command name %q", c.Name)
	}
	if owner, exists := r.aliases[c.Name]; exists {
		return fmt.Errorf("commands: command name %q collides with alias of %q", c.Name, owner)
	}
	for _, alias := range c.Aliases {
		if _, exists := r.commands[alias]; exists {
			return fmt.Errorf("commands: alias %q of %q collides with existing command name", alias, c.Name)
		}
		if owner, exists := r.aliases[alias]; exists {
			return fmt.Errorf("commands: alias %q of %q already registered by %q", alias, c.Name, owner)
		}
	}
	return nil
}

// insert stores the command and its alias index entries; it must only be
// called after conflict reported no collision.
func (r *Registry) insert(c *Command) {
	if r.commands == nil {
		r.commands = make(map[string]*Command)
	}
	if r.aliases == nil {
		r.aliases = make(map[string]string)
	}
	r.commands[c.Name] = c
	for _, alias := range c.Aliases {
		r.aliases[alias] = c.Name
	}
}

// Parse splits a raw input line into a command name and its arguments. Input
// that does not start with "/" is not a command and yields empty values. The
// name is lowercased so lookups match the naming convention of loaded files.
func Parse(input string) (name, args string) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return "", ""
	}
	rest := input[1:]
	parts := strings.SplitN(rest, " ", 2)
	name = strings.ToLower(parts[0])
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return name, args
}
