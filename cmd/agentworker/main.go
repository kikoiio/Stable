package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"proactive-agent/internal/artifact"
	"proactive-agent/internal/core"
	"proactive-agent/internal/decision"
	"proactive-agent/internal/execution"
	"proactive-agent/internal/policy"
	"proactive-agent/internal/store"
)

func main() {
	dbPath := flag.String("db", "run/state.db", "business SQLite database")
	runRoot := flag.String("run-root", "run", "authorized run root")
	address := flag.String("temporal", "localhost:7233", "Temporal server address")
	projectRoot := flag.String("project-root", ".", "project root containing workers and schemas")
	flag.Parse()
	if err := run(*dbPath, *runRoot, *address, *projectRoot); err != nil {
		log.Fatal(err)
	}
}

func run(dbPath, runRoot, address, projectRoot string) error {
	var err error
	runRoot, err = filepath.Abs(runRoot)
	if err != nil {
		return err
	}
	projectRoot, err = filepath.Abs(projectRoot)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return err
	}
	state, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer state.Close()
	artifacts, err := artifact.New(runRoot)
	if err != nil {
		return err
	}
	policyEngine := policy.Policy{Declared: map[string]core.CapabilityDescriptor{
		"kicad.repair_connection": {Name: "kicad.repair_connection", PostconditionKind: "sensor.connection_present"},
		"computer.ensure_open":    {Name: "computer.ensure_open", PostconditionKind: "computer.open"},
	}}
	kicad := &execution.PythonBridge{Script: filepath.Join(projectRoot, "workers/kicad/bridge.py"), AllowedRoot: runRoot, Timeout: 90 * time.Second}
	computer := &execution.PythonBridge{Script: filepath.Join(projectRoot, "workers/computer/bridge.py"), AllowedRoot: runRoot, Timeout: 90 * time.Second}
	coordinator := &execution.Coordinator{Store: state, Artifacts: artifacts, Policy: policyEngine,
		Capabilities:   map[string]execution.Caller{"kicad.repair_connection": kicad},
		Postconditions: map[string]json.RawMessage{"kicad.repair_connection": json.RawMessage(`{"sensor.connection_present":true}`)}}
	if marker := os.Getenv("PROACTIVE_CRASH_AFTER_REPAIR_MARKER"); marker != "" {
		coordinator.AfterCapabilityEffect = func() {
			file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err == nil {
				file.Close()
				os.Exit(86)
			}
		}
	}
	decider := &decision.Codex{SchemaPath: filepath.Join(projectRoot, "schemas/next_action.schema.json"), Workdir: projectRoot, Timeout: 90 * time.Second, Attempts: 2}
	activities := &core.Activities{State: state, Artifacts: artifacts, Kicad: kicad, Computer: computer, Decider: decider, Policy: policyEngine, Executor: coordinator}
	connection, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		return err
	}
	defer connection.Close()
	w := worker.New(connection, core.TaskQueue, worker.Options{})
	w.RegisterWorkflow(core.GoalWorkflow)
	w.RegisterActivityWithOptions(activities.CheckInterval, activity.RegisterOptions{Name: "CheckInterval"})
	w.RegisterActivityWithOptions(func(ctx context.Context, goalID, eventID string) (bool, error) {
		activity.RecordHeartbeat(ctx)
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					activity.RecordHeartbeat(ctx)
				case <-stop:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
		return activities.EvaluateGoal(ctx, goalID, eventID)
	}, activity.RegisterOptions{Name: "EvaluateGoal"})
	if err = w.Start(); err != nil {
		return err
	}
	defer w.Stop()
	ctx := context.Background()
	pending, err := state.PendingEvents(ctx)
	if err != nil {
		return err
	}
	for _, event := range pending {
		if err = connection.SignalWorkflow(ctx, event.GoalID, "", core.GoalEventSignal, event.ID); err != nil {
			log.Printf("pending event %s not signaled yet: %v", event.ID, err)
			continue
		}
		if err = state.SetEventStatus(ctx, event.ID, "signaled"); err != nil {
			log.Printf("event %s status: %v", event.ID, err)
		}
	}
	fmt.Printf("agent worker ready: temporal=%s db=%s run=%s\n", address, dbPath, runRoot)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	return nil
}
