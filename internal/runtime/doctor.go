package runtime

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"stable/internal/appconfig"
	"stable/internal/platform/kicad"
	"stable/internal/platform/paths"
	"stable/internal/platform/sandbox"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func Doctor(c appconfig.AppConfig, p paths.Paths) []Check {
	return DoctorWithSandbox(c, p, sandbox.New())
}

// DoctorWithSandbox keeps capability discovery injectable for contract tests
// while the CLI uses the real platform manager. The temporary roots are
// private and are removed after the isolated probes complete.
func DoctorWithSandbox(c appconfig.AppConfig, p paths.Paths, manager sandbox.SandboxManager) []Check {
	var checks []Check
	for name, path := range map[string]string{"agentctl": p.HelperBinary("agentctl"), "agentworker": p.HelperBinary("agentworker"), "temporal": p.HelperBinary("temporal"), "fixture": filepath.Join(p.Share, "fixtures/sensor_board/sensor.kicad_sch"), "project fixture": filepath.Join(p.Share, "fixtures/sensor_board/sensor.kicad_pro"), "schema": filepath.Join(p.Share, "schemas/next_action.schema.json"), "criteria schema": filepath.Join(p.Share, "schemas/criteria_proposal.schema.json"), "KiCad bridge": filepath.Join(p.Share, "workers/kicad/bridge.py"), "computer bridge": filepath.Join(p.Share, "workers/computer/bridge.py")} {
		st, err := os.Stat(path)
		checks = append(checks, Check{Name: name, OK: err == nil && st.Mode().IsRegular(), Detail: path})
	}
	for _, name := range doctorTools {
		path, err := exec.LookPath(name)
		if err != nil {
			path = "install system package providing " + name
		}
		checks = append(checks, Check{Name: name, OK: err == nil, Detail: path})
	}
	for _, capability := range doctorCapabilities {
		checks = append(checks, Check{Name: capability.Name, OK: capability.OK, Detail: capability.Detail})
	}
	checks = append(checks, isolatedCapabilityChecks(manager)...)
	addr := fmt.Sprintf("127.0.0.1:%d", c.TemporalPort)
	conn, err := net.DialTimeout("tcp", addr, 200000000)
	free := err != nil
	if conn != nil {
		conn.Close()
	}
	if s, controlErr := Control(p, "status"); controlErr == nil && s.Running && s.TemporalAddress == addr {
		checks = append(checks, Check{Name: "Temporal port", OK: true, Detail: addr + " (used by Stable runtime)"})
	} else {
		checks = append(checks, Check{Name: "Temporal port", OK: free, Detail: addr + " (must be free before up)"})
	}
	return checks
}

func isolatedCapabilityChecks(manager sandbox.SandboxManager) []Check {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("", ".stable-doctor-")
	if err != nil {
		return []Check{{Name: "sandbox", OK: false, Detail: "create private capability probe root: " + err.Error()}}
	}
	defer os.RemoveAll(root)
	profile := sandbox.SandboxProfile{
		ProjectRoot:   filepath.Join(root, "project"),
		CandidateRoot: filepath.Join(root, "candidate"),
		RunRoot:       filepath.Join(root, "run"),
		Timeout:       10 * time.Second,
		OutputLimit:   16 << 10,
	}
	for _, path := range []string{profile.ProjectRoot, profile.CandidateRoot, profile.RunRoot} {
		if err := os.Mkdir(path, 0700); err != nil {
			return []Check{{Name: "sandbox", OK: false, Detail: "create private capability probe root: " + err.Error()}}
		}
	}
	caps, discoverErr := kicad.Discover(ctx, manager, profile)
	checks := []Check{{Name: "sandbox (isolated)", OK: caps.Sandbox.Available, Detail: caps.Sandbox.Detail}}
	for _, status := range []kicad.ToolStatus{caps.Python, caps.CLI, caps.GUI, caps.Display, caps.Window, caps.Screenshot, caps.Template} {
		detail := status.Detail
		if detail == "" {
			detail = status.Path
		}
		checks = append(checks, Check{Name: status.Name + " (isolated)", OK: status.Available, Detail: detail})
	}
	if discoverErr != nil {
		checks = append(checks, Check{Name: "KiCad capability discovery", OK: false, Detail: discoverErr.Error()})
	}
	return checks
}

func Missing(checks []Check) string {
	var names []string
	for _, c := range checks {
		if !c.OK {
			names = append(names, c.Name)
		}
	}
	return strings.Join(names, ", ")
}
