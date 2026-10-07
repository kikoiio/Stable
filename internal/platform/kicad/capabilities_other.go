//go:build !linux

package kicad

import (
	"context"

	"stable/internal/platform/sandbox"
)

// discoverCapabilities deliberately does not look up host tools on non-Linux
// systems.  S04 does not provide a protected KiCad workflow there, so a tool
// being installed must not be reported as usable.
func discoverCapabilities(ctx context.Context, _ sandbox.SandboxManager, _ sandbox.SandboxProfile) (Capabilities, error) {
	var capabilities Capabilities
	capabilities.Sandbox = SandboxCapability{
		Name:      "linux-bubblewrap",
		Available: false,
		Detail:    ErrUnsupported.Error(),
	}
	for _, item := range []struct {
		status *ToolStatus
		name   string
	}{
		{&capabilities.Python, "python3"},
		{&capabilities.CLI, "kicad-cli"},
		{&capabilities.GUI, "eeschema"},
		{&capabilities.Display, "Xvfb"},
		{&capabilities.Window, "window"},
		{&capabilities.Screenshot, "import"},
		{&capabilities.Template, "template"},
	} {
		item.status.Name = item.name
		item.status.Detail = ErrUnsupported.Error()
	}
	if err := ctx.Err(); err != nil {
		return capabilities, err
	}
	return capabilities, ErrUnsupported
}
