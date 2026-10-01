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
	"stable/internal/appconfig"
	"stable/internal/artifact"
	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/dependency"
	"stable/internal/execution"
	"stable/internal/goalrun"
	"stable/internal/policy"
	"stable/internal/store"
)

func main() {
	dbPath := flag.String("db", "run/state.db", "business SQLite database")
	runRoot := flag.String("run-root", "run", "authorized run root")
	address := flag.String("temporal", "localhost:7233", "Temporal server address")
	projectRoot := flag.String("project-root", ".", "project root containing workers and schemas")
	appMode := flag.Bool("app-config", false, "use user model configuration instead of development Codex adapter")
	flag.Parse()
	if err := runConfigured(*dbPath, *runRoot, *address, *projectRoot, *appMode); err != nil {
		log.Fatal(err)
	}
}

func run(dbPath, runRoot, address, projectRoot string) error {
	return runConfigured(dbPath, runRoot, address, projectRoot, false)
}

func runConfigured(dbPath, runRoot, address, projectRoot string, appMode bool) error {
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
	if appMode {
		if err = state.InterruptStartedModelCalls(context.Background()); err != nil {
			return err
		}
	}
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
	if marker := os.Getenv("STABLE_CRASH_AFTER_REPAIR_MARKER"); marker != "" {
		coordinator.AfterCapabilityEffect = func() {
			file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err == nil {
				file.Close()
				os.Exit(86)
			}
		}
	}
	var decider core.DecisionMaker
	if appMode {
		cfg, err := appconfig.Load()
		if err != nil {
			return err
		}
		if err = cfg.Validate(true); err != nil {
			return err
		}
		provider, err := decision.NewProvider(cfg.Model)
		if err != nil {
			return err
		}
		schema, err := os.ReadFile(filepath.Join(projectRoot, "schemas/next_action.schema.json"))
		if err != nil {
			return err
		}
		decider = decision.ProviderDecider{Provider: provider, Schema: schema}
	} else {
		decider = &decision.Codex{SchemaPath: filepath.Join(projectRoot, "schemas/next_action.schema.json"), Workdir: projectRoot, Timeout: 90 * time.Second, Attempts: 2}
	}
	refresher := &dependency.Refresher{State: state, Collector: dependency.KiCadCollector{Kicad: kicad}, Wake: func(ctx context.Context, goalID, eventID string) error {
		return goalrun.WakeGoal(ctx, address, goalID, eventID)
	}}
	activities := &core.Activities{State: state, Artifacts: artifacts, Kicad: kicad, Computer: computer, Decider: decider, Policy: policyEngine, Executor: coordinator, Refresher: refresher}
	connection, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		return err
	}
	defer connection.Close()
	w := worker.New(connection, core.TaskQueue, worker.Options{})
	w.RegisterWorkflow(core.GoalWorkflow)
	w.RegisterActivityWithOptions(activities.CheckInterval, activity.RegisterOptions{Name: "CheckInterval"})
	w.RegisterActivityWithOptions(activities.WaitingForHuman, activity.RegisterOptions{Name: "WaitingForHuman"})
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
	events, err := state.UnprocessedEvents(ctx)
	if err != nil {
		return err
	}
	replayEvents(ctx, events,
		func(ctx context.Context, goalID, eventID string) error {
			return goalrun.WakeGoal(ctx, address, goalID, eventID)
		},
		func(ctx context.Context, id string) error {
			return state.SetEventStatus(ctx, id, "signaled")
		}, log.Printf)
	fmt.Printf("agent worker ready: temporal=%s db=%s run=%s\n", address, dbPath, runRoot)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	return nil
}

// replayEvents re-delivers every unprocessed event through wake. Only a
// successful delivery advances the event to signaled; a failure is logged and
// the event keeps its state so the next restart retries it.
func replayEvents(ctx context.Context, events []core.Event, wake func(context.Context, string, string) error, markSignaled func(context.Context, string) error, logf func(string, ...any)) (delivered, failed int) {
	for _, event := range events {
		if err := wake(ctx, event.GoalID, event.ID); err != nil {
			logf("event %s (%s) not delivered: %v", event.ID, event.Status, err)
			failed++
			continue
		}
		delivered++
		if err := markSignaled(ctx, event.ID); err != nil {
			logf("event %s status: %v", event.ID, err)
		}
	}
	return delivered, failed
}
