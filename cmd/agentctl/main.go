package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"proactive-agent/internal/artifact"
	"proactive-agent/internal/core"
	"proactive-agent/internal/report"
	"proactive-agent/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fatal("usage: agentctl start|notify|status|export [flags]")
	}
	var err error
	switch os.Args[1] {
	case "start":
		err = start(os.Args[2:])
	case "notify":
		err = notify(os.Args[2:])
	case "status":
		err = status(os.Args[2:])
	case "export":
		err = export(os.Args[2:])
	default:
		fatal("unknown command: " + os.Args[1])
	}
	if err != nil {
		fatal(err.Error())
	}
}

func fatal(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }

func common(fs *flag.FlagSet) (*string, *string, *string) {
	runRoot := fs.String("run-root", "run", "run directory")
	dbPath := fs.String("db", "", "business SQLite database (default run-root/state.db)")
	address := fs.String("temporal", "localhost:7233", "Temporal address")
	return runRoot, dbPath, address
}

func paths(root, db string) (string, string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	if db == "" {
		db = filepath.Join(abs, "state.db")
	}
	if err = os.MkdirAll(abs, 0755); err != nil {
		return "", "", err
	}
	return abs, db, nil
}

func start(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	runRoot, dbPath, address := common(fs)
	projectRoot := fs.String("project-root", ".", "project root containing fixtures")
	goalID := fs.String("goal", "", "stable goal ID (generated when empty)")
	objective := fs.String("objective", "Repair the sensor connector and obtain a clean KiCad ERC", "goal objective")
	interval := fs.Int("interval", 30, "periodic check interval in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *interval < 1 {
		return errors.New("interval must be positive")
	}
	root, db, err := paths(*runRoot, *dbPath)
	if err != nil {
		return err
	}
	id := *goalID
	if id == "" {
		id = randomID("goal")
	}
	if !validID(id) {
		return errors.New("goal ID must contain only letters, digits, dash, underscore")
	}
	dir := filepath.Join(root, id)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	project, err := filepath.Abs(*projectRoot)
	if err != nil {
		return err
	}
	fixture := filepath.Join(project, "fixtures/sensor_board")
	for _, name := range []string{"sensor.kicad_sch", "sensor.kicad_pro"} {
		target := filepath.Join(dir, name)
		if _, err = os.Stat(target); errors.Is(err, os.ErrNotExist) {
			if err = copyExclusive(filepath.Join(fixture, name), target); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	artifactPath := filepath.Join(dir, "sensor.kicad_sch")
	artifacts, err := artifact.New(root)
	if err != nil {
		return err
	}
	digest, err := artifacts.Digest(context.Background(), artifactPath)
	if err != nil {
		return err
	}
	s, err := store.Open(db)
	if err != nil {
		return err
	}
	defer s.Close()
	goal := core.Goal{ID: id, Objective: *objective, Criteria: []core.Criterion{{ID: "erc-clean", Kind: "kicad.erc_clean", Payload: json.RawMessage(`{"max_violations":0}`)}},
		AllowedRoot: dir, ArtifactPath: artifactPath, CheckIntervalSeconds: *interval,
		AllowedCapabilities: []string{"computer.ensure_open", "kicad.repair_connection", "kicad.run_erc", "inspect_design"},
		Status:              core.GoalActive, CurrentArtifactID: digest}
	snap, err := s.CreateGoal(context.Background(), goal)
	if err != nil {
		return err
	}
	connection, err := client.Dial(client.Options{HostPort: *address})
	if err != nil {
		return fmt.Errorf("goal %s persisted; Temporal connection: %w", id, err)
	}
	defer connection.Close()
	_, err = connection.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{ID: id, TaskQueue: core.TaskQueue}, core.GoalWorkflow, id)
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if !errors.As(err, &already) {
			return err
		}
	}
	fmt.Printf("%s\n", snap.Goal.ID)
	return nil
}

func notify(args []string) error {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	runRoot, dbPath, address := common(fs)
	goalID := fs.String("goal", "", "goal ID")
	eventID := fs.String("event", "", "stable event ID")
	kind := fs.String("kind", "", "design_changed or external_check_failed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *goalID == "" || *eventID == "" {
		return errors.New("goal and event IDs required")
	}
	if *kind != "design_changed" && *kind != "external_check_failed" {
		return errors.New("unsupported notification kind")
	}
	_, db, err := paths(*runRoot, *dbPath)
	if err != nil {
		return err
	}
	s, err := store.Open(db)
	if err != nil {
		return err
	}
	defer s.Close()
	e, inserted, err := s.InsertEventIfAbsent(context.Background(), core.Event{ID: *eventID, GoalID: *goalID, Kind: *kind, Payload: json.RawMessage(`{}`)})
	if err != nil {
		return err
	}
	if !inserted && e.Status == "processed" {
		fmt.Printf("event %s already processed\n", e.ID)
		return nil
	}
	connection, err := client.Dial(client.Options{HostPort: *address})
	if err != nil {
		fmt.Printf("event %s queued: %v\n", e.ID, err)
		return nil
	}
	defer connection.Close()
	if err = connection.SignalWorkflow(context.Background(), *goalID, "", core.GoalEventSignal, e.ID); err != nil {
		fmt.Printf("event %s queued: %v\n", e.ID, err)
		return nil
	}
	if e.Status == "pending" {
		if err = s.SetEventStatus(context.Background(), e.ID, "signaled"); err != nil {
			return err
		}
	}
	fmt.Printf("event %s signaled\n", e.ID)
	return nil
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	runRoot, dbPath, _ := common(fs)
	goalID := fs.String("goal", "", "goal ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *goalID == "" {
		return errors.New("goal ID required")
	}
	_, db, err := paths(*runRoot, *dbPath)
	if err != nil {
		return err
	}
	s, err := store.Open(db)
	if err != nil {
		return err
	}
	defer s.Close()
	snap, err := s.GetGoalSnapshot(context.Background(), *goalID)
	if err != nil {
		return err
	}
	r, err := report.BuildStatus(snap)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(r)
}

func export(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	runRoot, dbPath, _ := common(fs)
	goalID := fs.String("goal", "", "goal ID")
	out := fs.String("out", "", "new delivery directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *goalID == "" || *out == "" {
		return errors.New("goal ID and output directory required")
	}
	_, db, err := paths(*runRoot, *dbPath)
	if err != nil {
		return err
	}
	s, err := store.Open(db)
	if err != nil {
		return err
	}
	defer s.Close()
	snap, err := s.GetGoalSnapshot(context.Background(), *goalID)
	if err != nil {
		return err
	}
	r, err := report.Export(context.Background(), snap, *out)
	if err != nil {
		return err
	}
	fmt.Println(report.Summary(r))
	return nil
}

func copyExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func randomID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}
func validID(id string) bool {
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return id != ""
}
