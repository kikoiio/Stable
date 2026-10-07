// Package kicad describes the tools and isolation capabilities required by
// the KiCad workers.  Discovery is deliberately kept separate from the ERC
// and GUI workflows so all callers use the same host and sandbox checks.
package kicad

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"stable/internal/platform/sandbox"
)

// ErrUnsupported identifies a platform for which the protected KiCad
// workflow has not been implemented.
var ErrUnsupported = errors.New("KiCad capabilities are unsupported on this platform")

// ErrUnavailable identifies a capability that is not usable in the protected
// execution environment.  The status Detail field carries the concrete
// reason (for example, a missing executable or a failed isolation probe).
var ErrUnavailable = errors.New("KiCad capability is unavailable")

// CapabilityKind names a workflow's required capability set.
type CapabilityKind string

const (
	KindERC        CapabilityKind = "erc"
	KindGUI        CapabilityKind = "gui"
	KindScreenshot CapabilityKind = "screenshot"
)

// ToolStatus describes one executable and whether it can be used from the
// isolated environment.  HostAvailable and IsolatedAvailable are retained
// separately so diagnostics do not report a host PATH false positive.
type ToolStatus struct {
	Name              string
	Path              string
	Version           string
	Available         bool
	HostAvailable     bool
	IsolatedAvailable bool
	Detail            string
}

// SandboxCapability describes the isolation prerequisite used by KiCad.
type SandboxCapability struct {
	Name      string
	Available bool
	Detail    string
}

// Capabilities is the complete KiCad toolchain and display capability set.
// TemplateRoot is empty when no system KiCad template directory is visible.
type Capabilities struct {
	Python       ToolStatus
	CLI          ToolStatus
	GUI          ToolStatus
	Display      ToolStatus
	Window       ToolStatus
	Screenshot   ToolStatus
	Template     ToolStatus
	TemplateRoot string
	Sandbox      SandboxCapability
}

// Discover checks host tool lookup and visibility from the supplied sandbox.
// It returns a capability report even when one or more capabilities are
// unavailable; callers can use the individual Detail values for diagnostics.
func Discover(ctx context.Context, manager sandbox.SandboxManager, profile sandbox.SandboxProfile) (Capabilities, error) {
	return discoverCapabilities(ctx, manager, profile)
}

// Environment returns the private XDG and KiCad configuration environment for
// a single run.  The caller is responsible for creating the directories in
// runRoot before launching the worker.
func Environment(runRoot string) []string {
	if strings.TrimSpace(runRoot) == "" {
		return nil
	}
	runRoot = filepath.Clean(runRoot)
	config := filepath.Join(runRoot, "config")
	cache := filepath.Join(runRoot, "cache")
	data := filepath.Join(runRoot, "data")
	templates := filepath.Join(runRoot, "templates")
	return []string{
		"XDG_CONFIG_HOME=" + config,
		"XDG_CACHE_HOME=" + cache,
		"XDG_DATA_HOME=" + data,
		"KICAD_CONFIG_HOME=" + filepath.Join(config, "kicad"),
		"KICAD_USER_TEMPLATE_DIR=" + templates,
	}
}

// Environment returns the private configuration environment for this
// capability set.  It exists as a method as well as the package helper so a
// resolver result can be passed directly to a worker profile builder.
func (c Capabilities) Environment(runRoot string) []string { return Environment(runRoot) }

// RequiredFor returns the names required by a workflow.  Unknown workflow
// names intentionally return nil so callers fail closed when checking
// AvailableFor rather than accidentally treating an unknown mode as ERC.
func RequiredFor(kind string) []string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case string(KindERC), "kicad.erc", "headless":
		return []string{"sandbox", "python3", "kicad-cli", "template"}
	case string(KindGUI), "computer":
		return []string{"sandbox", "python3", "eeschema", "display", "window"}
	case string(KindScreenshot), "screen", "capture":
		return []string{"sandbox", "display", "window", "screenshot"}
	default:
		return nil
	}
}

// RequiredCapabilities is an explicit alias useful to callers that want to
// avoid the shorter RequiredFor name in diagnostics code.
func RequiredCapabilities(kind string) []string { return RequiredFor(kind) }

// AvailableFor reports whether every prerequisite for kind is available.
func (c Capabilities) AvailableFor(kind string) bool {
	missing := c.Missing(kind)
	return len(RequiredFor(kind)) > 0 && len(missing) == 0
}

// Missing returns the stable capability names that prevent kind from running.
func (c Capabilities) Missing(kind string) []string {
	missing := make([]string, 0)
	for _, name := range RequiredFor(kind) {
		if !c.has(name) {
			missing = append(missing, name)
		}
	}
	return missing
}

func (c Capabilities) has(name string) bool {
	switch name {
	case "sandbox":
		return c.Sandbox.Available
	case "python3":
		return c.Python.Available
	case "kicad-cli":
		return c.CLI.Available
	case "eeschema":
		return c.GUI.Available
	case "display":
		return c.Display.Available
	case "window":
		return c.Window.Available
	case "screenshot":
		return c.Screenshot.Available
	case "template":
		return c.Template.Available && c.TemplateRoot != ""
	default:
		return false
	}
}
