package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"proactive-agent/internal/appconfig"
	"proactive-agent/internal/runtime"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`proactive-agent ` + version + `
Usage:
  proactive-agent doctor
  proactive-agent config check
  proactive-agent up | down | runtime status
  proactive-agent goal start [--goal ID] [--interval SECONDS]
  proactive-agent goal status --goal ID
  proactive-agent goal notify --goal ID --event ID --kind design_changed|external_check_failed
  proactive-agent goal export --goal ID --out DIRECTORY
  proactive-agent logs
  proactive-agent version
`)
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		usage()
		return nil
	}
	if args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	c, err := appconfig.Load()
	if err != nil {
		return err
	}
	p, err := runtime.Resolve(c)
	if err != nil {
		return err
	}
	switch args[0] {
	case "doctor":
		if len(args) != 1 {
			return errors.New("doctor takes no arguments")
		}
		checks := runtime.Doctor(c, p)
		for _, check := range checks {
			mark := "OK"
			if !check.OK {
				mark = "MISSING"
			}
			fmt.Printf("%s %s: %s\n", mark, check.Name, check.Detail)
		}
		if missing := runtime.Missing(checks); missing != "" {
			return fmt.Errorf("missing requirements: %s", missing)
		}
		return nil
	case "config":
		if len(args) != 2 || args[1] != "check" {
			return errors.New("usage: proactive-agent config check")
		}
		if err = c.Validate(true); err != nil {
			return err
		}
		fmt.Println(c.Summary())
		return nil
	case "supervise":
		if len(args) != 1 {
			return errors.New("supervise takes no arguments")
		}
		if err = c.Validate(true); err != nil {
			return err
		}
		return runtime.Supervise(c, p)
	case "up":
		if len(args) != 1 {
			return errors.New("up takes no arguments")
		}
		if err = c.Validate(true); err != nil {
			return err
		}
		s, err := runtime.Up(context.Background(), c, p)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(s)
	case "down":
		if len(args) != 1 {
			return errors.New("down takes no arguments")
		}
		s, err := runtime.Control(p, "down")
		if err != nil {
			return err
		}
		fmt.Printf("stopping runtime pid=%d; data kept at %s\n", s.PID, s.StateDir)
		return nil
	case "runtime":
		if len(args) != 2 || args[1] != "status" {
			return errors.New("usage: proactive-agent runtime status")
		}
		s, err := runtime.Control(p, "status")
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(s)
	case "logs":
		if len(args) != 1 {
			return errors.New("logs takes no arguments")
		}
		fmt.Printf("supervisor: %s\ntemporal: %s\nworker: %s\n", p.SupervisorLog, p.TemporalLog, p.WorkerLog)
		return nil
	case "goal":
		return goal(args[1:], c, p)
	default:
		return fmt.Errorf("unknown command %q; run proactive-agent help", args[0])
	}
}

func goal(args []string, c appconfig.AppConfig, p runtime.Paths) error {
	if len(args) == 0 {
		return errors.New("usage: proactive-agent goal start|status|notify|export")
	}
	cmd := args[0]
	switch cmd {
	case "start", "status", "notify", "export":
	default:
		return errors.New("unknown goal command")
	}
	if cmd == "start" {
		if _, err := runtime.Control(p, "status"); err != nil {
			return errors.New("runtime is not running; run proactive-agent up")
		}
	}
	if err := p.Prepare(); err != nil {
		return err
	}
	forward := []string{cmd, "--run-root", p.Goals, "--db", p.Database, "--temporal", "127.0.0.1:" + strconv.Itoa(c.TemporalPort)}
	if cmd == "start" {
		forward = append(forward, "--project-root", p.Share)
	}
	forward = append(forward, args[1:]...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, filepath.Join(p.Libexec, "agentctl"), forward...)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Stdin = os.Stdin
	if err := child.Run(); err != nil {
		return err
	}
	return nil
}
