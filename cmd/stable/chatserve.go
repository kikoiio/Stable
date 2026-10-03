package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"stable/internal/agent"
	"stable/internal/appconfig"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/decision"
	"stable/internal/dependency"
	"stable/internal/llm"
	"stable/internal/sandbox"
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
	if err = chatserveRecovery(ctx, s); err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}
	refresher, err := dependency.NewKiCadRefresher(s, *runRoot, *projectRoot, *temporal)
	if err != nil {
		return err
	}
	var runner agent.Runner
	var runnerError string
	if streamingProvider, streamErr := llm.NewProvider(c.Model); streamErr == nil {
		runner = agent.NewRunner(streamingProvider, agent.RunnerOptions{})
	} else {
		runnerError = streamErr.Error()
	}
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: s, Provider: provider, ChatProvider: model.(decision.ChatProvider), Runner: runner, RunnerError: runnerError, ProviderCredential: c.Model.APIKey, ProviderName: c.Model.Provider, Model: c.Model.Model, Temporal: *temporal, ProjectRoot: *projectRoot, RunRoot: *runRoot, SocketPath: *socket,
		Refresher:         refresher,
		CandidateCheckers: chatCandidateCheckers(*runRoot),
	})
	if err != nil {
		return err
	}
	defer svc.Close()
	<-ctx.Done()
	return nil
}

// chatCandidateCheckers wires the independent candidate checkers the trusted
// review entry runs inside the verified Linux sandbox. Without one every
// preview would carry an unavailable finding and normal acceptance would be
// blocked.
func chatCandidateCheckers(runRoot string) []candidate.Checker {
	return []candidate.Checker{candidate.KicadERCChecker{Sandbox: sandbox.LinuxManager{}, RunRoot: runRoot}}
}

// chatserveRecovery settles candidate acceptances that were interrupted before
// the last shutdown so no trusted decision is served against stale journal
// state.
func chatserveRecovery(ctx context.Context, s *store.Store) error {
	return s.ReconcileAcceptances(ctx)
}
