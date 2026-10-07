// Two-level configuration loading for MCP servers: user-level entries come
// from the app config, project-level entries from the project's
// .stable/mcp.yaml. Both levels share one validation rule set, and project
// entries override user entries with the same original name while keeping
// their project-section position. The shape (rejections, size and count
// caps, symlink refusal) mirrors internal/hooks/loader.go.

package mcp

import (
	"fmt"
	"os"
	"strings"

	"stable/internal/appconfig"

	"gopkg.in/yaml.v3"
)

const (
	// MaxMCPServersPerFile caps how many server entries one project file may
	// declare; entries beyond the cap are rejected individually. Mirrors the
	// hooks loader's per-file cap.
	MaxMCPServersPerFile = 100
	// MaxMCPFileBytes caps the size of the project-level mcp.yaml file,
	// mirroring MaxFileBytes in the hooks loader.
	MaxMCPFileBytes = 256 * 1024
)

// Rejections lists entries skipped while loading, one string per entry in
// "source: reason" form: "user:<name>: ...", "project:<name>: ..." for
// per-entry problems, or "project:<path>: ..." for whole-file problems.
type Rejections []string

// validTransports is the closed set of transports a server entry may declare.
// The empty string is handled separately: it is legal and means the default
// (streamable).
var validTransports = map[string]bool{
	"sse":        true,
	"http":       true,
	"streamable": true,
}

// Validate checks one server entry's semantics: the name must be non-empty,
// exactly one of command and url must be set, and transport, when non-empty,
// must be one of "sse", "http" or "streamable" (empty means the default).
// Args, Headers and Env are unconstrained and may be empty. Whitespace-only
// fields count as empty for validation, but values are passed through to
// callers untouched.
func Validate(s appconfig.MCPServerConfig) error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("missing name")
	}
	hasCommand := strings.TrimSpace(s.Command) != ""
	hasURL := strings.TrimSpace(s.URL) != ""
	switch {
	case hasCommand && hasURL:
		return fmt.Errorf("command and url are mutually exclusive: set exactly one")
	case !hasCommand && !hasURL:
		return fmt.Errorf("command and url are both empty: set exactly one")
	}
	if s.Transport != "" && !validTransports[s.Transport] {
		return fmt.Errorf("invalid transport %q: must be one of sse, http, streamable", s.Transport)
	}
	return nil
}

// LoadMerge merges the user-level server list with the project-level file at
// projectPath (the project's .stable/mcp.yaml) and returns the combined list
// followed by every rejected entry.
//
// User entries are validated in place; invalid ones are skipped and recorded
// as "user:<name>: reason". A missing project file is silently treated as
// empty, a symbolic link is refused, and the size and entry-count caps match
// the hooks loader (MaxMCPFileBytes, MaxMCPServersPerFile). Project entries
// override user entries that share the same original (unsanitized) name: the
// user slot is dropped and the project entry stays in the project section, so
// the merged list keeps user entries first and project entries last.
func LoadMerge(user []appconfig.MCPServerConfig, projectPath string) ([]appconfig.MCPServerConfig, Rejections) {
	kept, userRej := filterValidServers(user, "user")
	project, projectRej := loadProjectServers(projectPath)
	merged, mergeRej := mergeServerLists(kept, project)
	return merged, append(append(userRej, projectRej...), mergeRej...)
}

// filterValidServers keeps the entries that pass Validate and records the
// others as rejections.
func filterValidServers(servers []appconfig.MCPServerConfig, source string) ([]appconfig.MCPServerConfig, Rejections) {
	var out []appconfig.MCPServerConfig
	var rejections Rejections
	for i, s := range servers {
		if err := Validate(s); err != nil {
			rejections = append(rejections, fmt.Sprintf("%s:%s: %v", source, serverEntryLabel(s.Name, i), err))
			continue
		}
		out = append(out, s)
	}
	return out, rejections
}

// loadProjectServers parses the project-level mcp.yaml. A missing file (or an
// empty path) is silently treated as empty; anything that makes the file
// unusable as a whole — symlink, oversize, unreadable, invalid YAML — rejects
// the whole file, while per-entry problems reject only the offending entry.
func loadProjectServers(path string) ([]appconfig.MCPServerConfig, Rejections) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, Rejections{fmt.Sprintf("project:%s: %v", path, err)}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, Rejections{fmt.Sprintf("project:%s: symbolic link refused", path)}
	}
	if info.Size() > MaxMCPFileBytes {
		return nil, Rejections{fmt.Sprintf("project:%s: file exceeds %d bytes", path, MaxMCPFileBytes)}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, Rejections{fmt.Sprintf("project:%s: %v", path, err)}
	}
	var doc []appconfig.MCPServerConfig
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, Rejections{fmt.Sprintf("project:%s: invalid YAML: %v", path, err)}
	}
	var out []appconfig.MCPServerConfig
	var rejections Rejections
	for i, s := range doc {
		n := i + 1
		if n > MaxMCPServersPerFile {
			rejections = append(rejections, fmt.Sprintf("project:%s: server %d exceeds per-file limit %d",
				serverEntryLabel(s.Name, i), n, MaxMCPServersPerFile))
			continue
		}
		if err := Validate(s); err != nil {
			rejections = append(rejections, fmt.Sprintf("project:%s: %v", serverEntryLabel(s.Name, i), err))
			continue
		}
		out = append(out, s)
	}
	return out, rejections
}

// mergeServerLists appends the project list after the user list, dropping
// user entries whose original name (exact, unsanitized match) also appears in
// the project list.
func mergeServerLists(user, project []appconfig.MCPServerConfig) ([]appconfig.MCPServerConfig, Rejections) {
	projectNames := make(map[string]bool, len(project))
	for _, s := range project {
		projectNames[s.Name] = true
	}
	var merged []appconfig.MCPServerConfig
	var rejections Rejections
	for _, s := range user {
		if projectNames[s.Name] {
			rejections = append(rejections, fmt.Sprintf("user:%s: overridden by project entry", s.Name))
			continue
		}
		merged = append(merged, s)
	}
	return append(merged, project...), rejections
}

// serverEntryLabel names a rejected entry in a rejection line. Entries
// without a name fall back to their one-based position so the reason stays
// traceable to a specific list element.
func serverEntryLabel(name string, index int) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return fmt.Sprintf("#%d", index+1)
}
