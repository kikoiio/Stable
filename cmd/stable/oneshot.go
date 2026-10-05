package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"stable/internal/appconfig"
	"stable/internal/platform/paths"
	"stable/internal/runtime"
)

const oneshotTimeout = 15 * time.Minute

func agentctl(p paths.Paths, c appconfig.AppConfig, cmd string, extra ...string) (string, error) {
	args := []string{cmd, "--run-root", p.Goals, "--db", p.Database, "--temporal", "127.0.0.1:" + strconv.Itoa(c.TemporalPort), "--project-root", p.Share}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, p.HelperBinary("agentctl"), append(args, extra...)...)
	var out, errOut bytes.Buffer
	child.Stdout, child.Stderr = &out, &errOut
	if err := child.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// runOnce starts the runtime if needed, runs one goal to a stopping point, exports the result, and stops what it started.
func runOnce(c appconfig.AppConfig, p paths.Paths) error {
	if err := c.Validate(true); err != nil {
		return fmt.Errorf("%w\nrun `stable config init` to create the config file, fill in your model and key, then run stable again", err)
	}
	fmt.Println(c.Summary())

	startedRuntime := false
	if s, err := runtime.Control(p, "status"); err != nil || !s.Running {
		fmt.Println("starting local runtime...")
		if _, err = runtime.Up(context.Background(), c, p); err != nil {
			return err
		}
		startedRuntime = true
	}
	stop := func() {
		if startedRuntime {
			_, _ = runtime.Control(p, "down")
			if n := runtime.StopSessionProcesses(p); n > 0 {
				fmt.Printf("stopped %d leftover GUI session process(es)\n", n)
			}
			fmt.Println("runtime stopped; data kept at", p.State)
		}
	}
	defer stop()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)

	if err := p.Prepare(); err != nil {
		return err
	}
	goalID := "stable-" + time.Now().Format("20060102-150405")
	defPath := filepath.Join(p.Goals, goalID+"-definition.json")
	// Quickstart demo target: the same objective written out explicitly so no
	// built-in goal exists anywhere in the system.
	def := `{"objective":"Repair the sensor connector and obtain a clean KiCad ERC",` +
		`"criteria":[{"id":"erc-clean","kind":"kicad.erc_clean","payload":{"max_violations":0}}],` +
		`"check_interval_seconds":2}`
	if err := os.WriteFile(defPath, []byte(def), 0644); err != nil {
		return err
	}
	if _, err := agentctl(p, c, "create", "--goal", goalID, "--from", defPath); err != nil {
		return err
	}
	fmt.Println("goal started:", goalID)

	last := ""
	deadline := time.After(oneshotTimeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-interrupt:
			return fmt.Errorf("interrupted; goal %s is kept and can be inspected with `stable goal status --goal %s`", goalID, goalID)
		case <-deadline:
			return fmt.Errorf("goal %s did not finish within %s; check `stable logs`", goalID, oneshotTimeout)
		case <-tick.C:
		}
		raw, err := agentctl(p, c, "status", "--goal", goalID)
		if err != nil {
			continue
		}
		var s struct {
			Snapshot struct {
				Goal struct {
					Status string `json:"status"`
					Reason string `json:"reason"`
				} `json:"goal"`
			} `json:"snapshot"`
			Verified bool `json:"verified"`
		}
		if json.Unmarshal([]byte(raw), &s) != nil {
			continue
		}
		state := s.Snapshot.Goal.Status
		if state != last {
			fmt.Println("status:", state)
			last = state
		}
		if s.Verified || state == "needs_human" {
			out, err := filepath.Abs(filepath.Join("stable-out", goalID))
			if err != nil {
				return err
			}
			summary, err := agentctl(p, c, "export", "--goal", goalID, "--out", out)
			if err != nil {
				return err
			}
			fmt.Println(summary)
			fmt.Println("delivery:", out)
			if !s.Verified {
				return errors.New("goal needs human attention: " + s.Snapshot.Goal.Reason)
			}
			return nil
		}
	}
}
