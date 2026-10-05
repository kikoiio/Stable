package runtime

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"stable/internal/appconfig"
	"stable/internal/platform/paths"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func Doctor(c appconfig.AppConfig, p paths.Paths) []Check {
	var checks []Check
	for name, path := range map[string]string{"agentctl": p.HelperBinary("agentctl"), "agentworker": p.HelperBinary("agentworker"), "temporal": p.HelperBinary("temporal"), "fixture": filepath.Join(p.Share, "fixtures/sensor_board/sensor.kicad_sch"), "project fixture": filepath.Join(p.Share, "fixtures/sensor_board/sensor.kicad_pro"), "schema": filepath.Join(p.Share, "schemas/next_action.schema.json"), "criteria schema": filepath.Join(p.Share, "schemas/criteria_proposal.schema.json"), "KiCad bridge": filepath.Join(p.Share, "workers/kicad/bridge.py"), "computer bridge": filepath.Join(p.Share, "workers/computer/bridge.py")} {
		st, err := os.Stat(path)
		checks = append(checks, Check{Name: name, OK: err == nil && st.Mode().IsRegular(), Detail: path})
	}
	for _, name := range []string{"python3", "kicad-cli", "kicad", "eeschema", "Xvfb", "xvfb-run", "xprop", "xwininfo", "import"} {
		path, err := exec.LookPath(name)
		if err != nil {
			path = "install system package providing " + name
		}
		checks = append(checks, Check{Name: name, OK: err == nil, Detail: path})
	}
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

func Missing(checks []Check) string {
	var names []string
	for _, c := range checks {
		if !c.OK {
			names = append(names, c.Name)
		}
	}
	return strings.Join(names, ", ")
}
