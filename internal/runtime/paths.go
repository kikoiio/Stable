package runtime

import (
	"errors"
	"os"
	"path/filepath"

	"stable/internal/appconfig"
)

type Paths struct{ Root, Bin, Libexec, Share, State, Goals, Database, TemporalDB, Socket, Lock, SupervisorLog, TemporalLog, WorkerLog string }

func Resolve(c appconfig.AppConfig) (Paths, error) {
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
	p.State, err = filepath.Abs(c.StateDir)
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
	p.Lock = filepath.Join(p.State, "supervisor.lock")
	p.SupervisorLog = filepath.Join(p.State, "supervisor.log")
	p.TemporalLog = filepath.Join(p.State, "temporal.log")
	p.WorkerLog = filepath.Join(p.State, "worker.log")
	return p, nil
}

func (p Paths) Prepare() error { return os.MkdirAll(p.Goals, 0700) }
