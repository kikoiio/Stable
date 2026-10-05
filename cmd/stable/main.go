package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"stable/internal/appconfig"
	"stable/internal/platform/paths"
	"stable/internal/prototype"
	"stable/internal/runtime"
	"stable/internal/sandbox"
	"stable/internal/tui"
)

// Overridden by -ldflags "-X main.version=..." in release builds (see VERSION).
var version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`stable ` + version + `
Usage:
  stable                (start runtime if needed, enter the production TUI)
  stable demo           (quickstart: run one built-in goal, export, stop)
  stable prototype      (launch the isolated interactive TUI prototype)
  stable doctor
  stable config init
  stable config check
  stable up | down | runtime status
  stable goal create --from FILE [--goal ID]
  stable goal status --goal ID
  stable goal notify --goal ID --event ID --kind design_changed|external_check_failed
  stable goal export --goal ID --out DIRECTORY
  stable logs
  stable version
`)
}

func run(args []string) error {
	if len(args) >= 4 && args[0] == "--stable-sandbox-proxy" && args[2] == "--" {
		// Trusted wrapper mounted read-only inside the sandbox: it forwards the
		// wrapped process's network through the private proxy socket.
		code, err := sandbox.RunProxyCommand(context.Background(), args[1], args[3:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "isolated proxy wrapper failed:", err)
		}
		os.Exit(code)
	}
	if len(args) == 0 {
		stdinInfo, statErr := os.Stdin.Stat()
		if statErr != nil || stdinInfo.Mode()&os.ModeCharDevice == 0 {
			return errors.New("stable requires an interactive terminal; run it in a terminal to open the TUI")
		}
		c, err := appconfig.Load()
		if err != nil {
			return err
		}
		p, err := paths.Resolve(c)
		if err != nil {
			return err
		}
		if err := c.Validate(true); err != nil {
			return fmt.Errorf("%w\nrun `stable config init` to create the config file, fill in your model and key, then run stable again", err)
		}
		fmt.Println(c.Summary())
		if s, err := runtime.Control(p, "status"); err != nil || !s.Running {
			fmt.Println("starting local runtime...")
			if _, err := runtime.Up(context.Background(), c, p); err != nil {
				return err
			}
		}
		// The chat session outlives the terminal on purpose: goals keep
		// running after the user quits, so the runtime is left up.
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		return tui.Run(p.ChatSocket, root)
	}
	// Keep the demo prototype isolated from production configuration and runtime
	// initialization. This branch intentionally precedes appconfig.Load.
	if args[0] == "prototype" {
		if len(args) != 1 {
			return errors.New("usage: stable prototype")
		}
		return prototype.Run()
	}
	if args[0] == "chat" {
		return errors.New("stable chat was removed; run stable in an interactive terminal to open the TUI")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		usage()
		return nil
	}
	if args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	if len(args) == 2 && args[0] == "config" && args[1] == "init" {
		path, created, err := appconfig.Init()
		if err != nil {
			return err
		}
		if created {
			fmt.Printf("created %s\nedit it: set provider (openai, anthropic, gemini, openai-compatible), model and api_key, then run stable config check\n", path)
		} else {
			fmt.Printf("%s already exists; not changed\n", path)
		}
		return nil
	}
	c, err := appconfig.Load()
	if err != nil {
		return err
	}
	p, err := paths.Resolve(c)
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
			return errors.New("usage: stable config check|init")
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
			if !strings.Contains(err.Error(), "not running") {
				return err
			}
			fmt.Println("runtime is not running")
		} else {
			fmt.Printf("stopping runtime pid=%d; data kept at %s\n", s.PID, s.StateDir)
		}
		if n := runtime.StopSessionProcesses(p); n > 0 {
			fmt.Printf("stopped %d leftover GUI session process(es)\n", n)
		}
		return nil
	case "runtime":
		if len(args) != 2 || args[1] != "status" {
			return errors.New("usage: stable runtime status")
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
		fmt.Printf("supervisor: %s\ntemporal: %s\nworker: %s\nchat: %s\n", p.SupervisorLog, p.TemporalLog, p.WorkerLog, p.ChatLog)
		return nil
	case "goal":
		return goal(args[1:], c, p)
	case "demo":
		return runOnce(c, p)
	case "chatserve":
		return chatserve(args[1:])
	default:
		return fmt.Errorf("unknown command %q; run stable help", args[0])
	}
}

func goal(args []string, c appconfig.AppConfig, p paths.Paths) error {
	if len(args) == 0 {
		return errors.New("usage: stable goal create|status|notify|export")
	}
	cmd := args[0]
	switch cmd {
	case "create", "status", "notify", "export":
	default:
		return fmt.Errorf("unknown goal command %q; goal start was removed: use goal create --from FILE or stable chat", cmd)
	}
	if cmd == "create" {
		if _, err := runtime.Control(p, "status"); err != nil {
			return errors.New("runtime is not running; run stable up")
		}
	}
	if err := p.Prepare(); err != nil {
		return err
	}
	forward := []string{cmd, "--run-root", p.Goals, "--db", p.Database, "--temporal", "127.0.0.1:" + strconv.Itoa(c.TemporalPort), "--project-root", p.Share}
	forward = append(forward, args[1:]...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, p.HelperBinary("agentctl"), forward...)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Stdin = os.Stdin
	if err := child.Run(); err != nil {
		return err
	}
	return nil
}
