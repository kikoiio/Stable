package paths

import (
	"errors"
	"os"
	"path/filepath"

	"stable/internal/platform/secfile"
)

type Paths struct {
	Root, Bin, Libexec, Share, State, Goals, Database, TemporalDB            string
	Socket, ChatSocket, Lock, SupervisorLog, TemporalLog, WorkerLog, ChatLog string
}

// Resolve derives install and state layout paths from the absolute state directory.
func Resolve(stateDir string) (Paths, error) {
	var p Paths
	exe, err := os.Executable()
	if err != nil {
		return p, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return p, err
	}
	p.Bin = filepath.Dir(exe)
	p.Root = filepath.Dir(p.Bin)
	p.Libexec = filepath.Join(p.Root, "libexec")
	p.Share = filepath.Join(p.Root, "share")
	p.State, err = filepath.Abs(stateDir)
	if err != nil {
		return p, err
	}
	if p.State == "/" {
		return p, errors.New("state directory cannot be root")
	}
	p.Goals = filepath.Join(p.State, "goals")
	p.Database = filepath.Join(p.State, "state.db")
	p.TemporalDB = filepath.Join(p.State, "temporal.db")
	p.Socket = filepath.Join(p.State, "control.sock")
	p.ChatSocket = filepath.Join(p.State, "chat.sock")
	p.Lock = filepath.Join(p.State, "supervisor.lock")
	p.SupervisorLog = filepath.Join(p.State, "supervisor.log")
	p.TemporalLog = filepath.Join(p.State, "temporal.log")
	p.WorkerLog = filepath.Join(p.State, "worker.log")
	p.ChatLog = filepath.Join(p.State, "chat.log")
	return p, nil
}

func (p Paths) Prepare() error { return secfile.MkdirAllPrivate(p.Goals, 0700) }

func (p Paths) HelperBinary(name string) string {
	return filepath.Join(p.Libexec, name+exeSuffix)
}

func (p Paths) HelperBinaryResolved(name string) string {
	candidates := []string{p.HelperBinary(name), filepath.Join(p.Root, "dev-install", "libexec", name+exeSuffix), filepath.Join(p.Bin, name+exeSuffix)}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return candidates[0]
}
