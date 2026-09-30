package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"go.temporal.io/sdk/client"
	"stable/internal/core"
	"stable/internal/goalrun"
	"stable/internal/report"
	"stable/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fatal("usage: agentctl create|notify|status|export [flags]")
	}
	var err error
	switch os.Args[1] {
	case "create":
		err = create(os.Args[2:])
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

func create(args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	runRoot, dbPath, address := common(fs)
	projectRoot := fs.String("project-root", ".", "project root containing fixtures")
	goalID := fs.String("goal", "", "stable goal ID (generated when empty)")
	definition := fs.String("definition", "", "goal definition JSON file (objective, criteria, check_interval_seconds)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *definition == "" {
		return errors.New("goal definition file required")
	}
	def, err := goalrun.LoadDefinition(*definition)
	if err != nil {
		return err
	}
	root, db, err := paths(*runRoot, *dbPath)
	if err != nil {
		return err
	}
	s, err := store.Open(db)
	if err != nil {
		return err
	}
	defer s.Close()
	spec := def.Spec(*goalID)
	goal, err := goalrun.Create(context.Background(), s, root, *address, *projectRoot, spec)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", goal.ID)
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

