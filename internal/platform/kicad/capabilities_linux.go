//go:build linux

package kicad

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"stable/internal/platform/sandbox"
)

var kicadLookPath = exec.LookPath
var kicadTemplateSearch = defaultTemplateSearch

type executableSpec struct {
	name string
	args []string
}

func discoverCapabilities(ctx context.Context, manager sandbox.SandboxManager, profile sandbox.SandboxProfile) (Capabilities, error) {
	var capabilities Capabilities
	if err := ctx.Err(); err != nil {
		return capabilities, err
	}

	probeErr := errors.New("sandbox manager is nil")
	if manager != nil {
		probeErr = manager.Probe(ctx, sandboxProfileForProbe(profile))
	}
	capabilities.Sandbox = SandboxCapability{
		Name:      "bubblewrap",
		Available: probeErr == nil,
		Detail:    detailForProbe(probeErr),
	}

	capabilities.Python = discoverTool(ctx, manager, profile, probeErr, executableSpec{name: "python3", args: []string{"--version"}})
	capabilities.CLI = discoverTool(ctx, manager, profile, probeErr, executableSpec{name: "kicad-cli", args: []string{"--version"}})
	capabilities.GUI = discoverTool(ctx, manager, profile, probeErr, executableSpec{name: "eeschema", args: []string{"--version"}})
	capabilities.Display = discoverTool(ctx, manager, profile, probeErr, executableSpec{name: "Xvfb", args: []string{"--version"}})
	capabilities.Screenshot = discoverTool(ctx, manager, profile, probeErr, executableSpec{name: "import", args: []string{"--version"}})
	capabilities.Window = discoverWindowTools(ctx, manager, profile, probeErr)
	capabilities.Template, capabilities.TemplateRoot = discoverTemplate(ctx, manager, profile, probeErr)
	return capabilities, ctx.Err()
}

func discoverTool(ctx context.Context, manager sandbox.SandboxManager, profile sandbox.SandboxProfile, probeErr error, spec executableSpec) ToolStatus {
	status := ToolStatus{Name: spec.name}
	path, err := kicadLookPath(spec.name)
	if err != nil {
		status.Detail = fmt.Sprintf("%s is not available on host: %v", spec.name, err)
		return status
	}
	status.Path, status.HostAvailable = path, true
	if probeErr != nil {
		status.Detail = fmt.Sprintf("host %s is available but isolated sandbox is unavailable: %v", spec.name, probeErr)
		return status
	}
	if manager == nil {
		status.Detail = fmt.Sprintf("host %s is available but isolated sandbox manager is nil", spec.name)
		return status
	}
	result, runErr := manager.RunIsolated(ctx, sandboxProfileForProbe(profile), append([]string{path}, spec.args...), nil)
	if runErr != nil {
		status.Detail = fmt.Sprintf("%s is not visible in isolated sandbox: %v", spec.name, runErr)
		return status
	}
	// A command-not-found exit is distinguishable from tools such as Xvfb
	// returning a usage error for --version.  The latter still proves visibility.
	if result.ExitCode == 127 {
		status.Detail = fmt.Sprintf("%s is not visible in isolated sandbox (exit 127)", spec.name)
		return status
	}
	status.IsolatedAvailable, status.Available = true, true
	status.Version = firstOutputLine(result.Stdout, result.Stderr)
	if status.Version == "" {
		status.Detail = fmt.Sprintf("%s is visible in isolated sandbox but did not report a version", spec.name)
	}
	return status
}

// sandboxProfileForProbe preserves the caller's roots while replacing only
// fields that could accidentally grant network or session access.  The
// caller's profile is already validated by the initial Probe.
func sandboxProfileForProbe(profile sandbox.SandboxProfile) sandbox.SandboxProfile {
	profile.NetworkGrants = nil
	profile.ProxyHelperPath = ""
	profile.ReadOnlyFiles = nil
	mounts := profile.ReadOnlyMounts[:0]
	for _, mount := range profile.ReadOnlyMounts {
		if mount.GuestPath == "/run/stable-network" {
			continue
		}
		mounts = append(mounts, mount)
	}
	profile.ReadOnlyMounts = mounts
	return profile
}

func discoverWindowTools(ctx context.Context, manager sandbox.SandboxManager, profile sandbox.SandboxProfile, probeErr error) ToolStatus {
	status := ToolStatus{Name: "window"}
	first, firstErr := kicadLookPath("xprop")
	second, secondErr := kicadLookPath("xwininfo")
	if firstErr != nil || secondErr != nil {
		status.Detail = fmt.Sprintf("window probe tools unavailable (xprop: %v, xwininfo: %v)", firstErr, secondErr)
		return status
	}
	status.Path = first + "," + second
	status.HostAvailable = true
	if probeErr != nil || manager == nil {
		status.Detail = fmt.Sprintf("window probe tools are host-visible but isolated sandbox is unavailable: %v", detailForProbe(probeErr))
		return status
	}
	for _, path := range []string{first, second} {
		result, err := manager.RunIsolated(ctx, sandboxProfileForProbe(profile), []string{path, "--version"}, nil)
		if err != nil || result.ExitCode == 127 {
			if err == nil {
				err = fmt.Errorf("exit %d", result.ExitCode)
			}
			status.Detail = fmt.Sprintf("window probe tool %s is not isolated: %v", filepath.Base(path), err)
			return status
		}
		if status.Version == "" {
			status.Version = firstOutputLine(result.Stdout, result.Stderr)
		}
	}
	status.IsolatedAvailable, status.Available = true, true
	return status
}

func discoverTemplate(ctx context.Context, manager sandbox.SandboxManager, profile sandbox.SandboxProfile, probeErr error) (ToolStatus, string) {
	status := ToolStatus{Name: "template"}
	for _, candidate := range templateCandidates() {
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		status.Path, status.HostAvailable = candidate, true
		if probeErr != nil || manager == nil {
			status.Detail = fmt.Sprintf("KiCad template root is host-visible but isolated sandbox is unavailable: %v", detailForProbe(probeErr))
			return status, ""
		}
		result, runErr := manager.RunIsolated(ctx, sandboxProfileForProbe(profile), []string{"/bin/sh", "-c", "test -d \"$1\"", "kicad-template", candidate}, nil)
		if runErr != nil || result.ExitCode != 0 {
			if runErr != nil {
				status.Detail = fmt.Sprintf("KiCad template root is not visible in isolated sandbox: %v", runErr)
			} else {
				status.Detail = fmt.Sprintf("KiCad template root is not visible in isolated sandbox (exit %d)", result.ExitCode)
			}
			return status, ""
		}
		status.IsolatedAvailable, status.Available = true, true
		return status, candidate
	}
	status.Detail = "KiCad template root is missing on host"
	return status, ""
}

func templateCandidates() []string {
	return kicadTemplateSearch()
}

func defaultTemplateSearch() []string {
	var candidates []string
	for _, key := range []string{"KICAD_TEMPLATE_DIR", "KICAD_USER_TEMPLATE_DIR"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			candidates = append(candidates, filepath.Clean(value))
		}
	}
	candidates = append(candidates,
		"/usr/share/kicad/template",
		"/usr/share/kicad/templates",
		"/usr/local/share/kicad/template",
	)
	seen := make(map[string]struct{}, len(candidates))
	result := candidates[:0]
	for _, candidate := range candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		result = append(result, candidate)
	}
	return result
}

func firstOutputLine(outputs ...[]byte) string {
	for _, output := range outputs {
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	return ""
}

func detailForProbe(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
