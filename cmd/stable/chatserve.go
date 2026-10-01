package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"stable/internal/appconfig"
	"stable/internal/conversation"
	"stable/internal/decision"
	"stable/internal/store"
)

// chatserve runs the persistent conversation service standalone. It is a
// development/test entry point; the installed runtime embeds the same service
// inside `stable supervise`.
func chatserve(args []string) error {
	fs := flag.NewFlagSet("chatserve", flag.ContinueOnError)
	db := fs.String("db", "", "state database")
	socket := fs.String("socket", "", "chat unix socket path")
	temporal := fs.String("temporal", "127.0.0.1:7233", "temporal address")
	projectRoot := fs.String("project-root", ".", "project root containing fixtures")
	runRoot := fs.String("run-root", "", "goal run directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" || *socket == "" || *runRoot == "" {
		return errors.New("chatserve requires --db, --socket and --run-root")
	}
	c, err := appconfig.Load()
	if err != nil {
		return err
	}
	model, err := decision.NewProvider(c.Model)
	if err != nil {
		return fmt.Errorf("model provider: %w", err)
	}
	provider, ok := model.(decision.StructuredProvider)
	if !ok {
		return errors.New("model provider does not support structured output")
	}
	s, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: s, Provider: provider, ChatProvider: model.(decision.ChatProvider), Temporal: *temporal, ProjectRoot: *projectRoot, RunRoot: *runRoot, SocketPath: *socket,
	})
	if err != nil {
		return err
	}
	defer svc.Close()
	<-ctx.Done()
	return nil
}
