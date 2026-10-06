package runtime

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"stable/internal/platform/paths"
	"stable/internal/platform/proc"
)

// sessionHandle mirrors the runtime_handle JSON stored per computer session.
type sessionHandle struct {
	Path        string `json:"path"`
	Display     string `json:"display"`
	EeschemaPID int    `json:"eeschema_pid"`
	XvfbPID     int    `json:"xvfb_pid"`
}

// StopSessionProcesses SIGTERMs the GUI processes (eeschema/Xvfb) recorded in
// the state database. A pid is only signalled when /proc shows it still runs
// the exact recorded command on a design inside the state dir, so recycled
// pids and unrelated KiCad windows are never touched. Returns the number of
// processes signalled.
func StopSessionProcesses(p paths.Paths) int {
	if _, err := os.Stat(p.Database); err != nil {
		return 0
	}
	db, err := sql.Open("sqlite3", "file:"+p.Database+"?mode=ro&_busy_timeout=2000")
	if err != nil {
		return 0
	}
	defer db.Close()
	rows, err := db.Query(`SELECT runtime_handle FROM computer_sessions WHERE runtime_handle <> ''`)
	if err != nil {
		return 0
	}
	defer rows.Close()

	stateRoot := filepath.Clean(p.State) + string(os.PathSeparator)
	stopped := 0
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			continue
		}
		var h sessionHandle
		if json.Unmarshal([]byte(raw), &h) != nil {
			continue
		}
		design := filepath.Clean(h.Path)
		if !filepath.IsAbs(design) || !strings.HasPrefix(design, stateRoot) {
			continue
		}
		if terminateIf(h.EeschemaPID, "eeschema "+design) {
			stopped++
		}
		if terminateIf(h.XvfbPID, "Xvfb "+h.Display+" ") {
			stopped++
		}
	}
	return stopped
}

func terminateIf(pid int, wantPrefix string) bool {
	if pid <= 1 {
		return false
	}
	cmdline, err := proc.Cmdline(pid)
	if err != nil {
		return false
	}
	if !strings.HasPrefix(cmdline, wantPrefix) {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Terminate(process) == nil
}
