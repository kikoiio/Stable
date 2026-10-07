package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"stable/internal/agent"
	"stable/internal/appconfig"
	"stable/internal/conversation"
	"stable/internal/llm"
	"stable/internal/platform/paths"
	"stable/internal/runtime"
)

func runPrint(args []string, stdin io.Reader, stdout, stderr io.Writer) (retErr error) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	return runPrintWithInterrupts(args, stdin, stdout, stderr, interrupt)
}

func runPrintWithInterrupts(args []string, stdin io.Reader, stdout, stderr io.Writer, interrupt <-chan os.Signal) (retErr error) {
	input, err := readPrintInput(args, stdin)
	if err != nil {
		return err
	}
	c, err := appconfig.Load()
	if err != nil {
		return err
	}
	if err = validatePrintProvider(c.Model.Provider); err != nil {
		return err
	}
	if err = c.Validate(true); err != nil {
		return err
	}
	p, err := paths.Resolve(c.StateDir)
	if err != nil {
		return err
	}
	if status, statusErr := printRuntimeControl(p, "status"); statusErr != nil || !status.Running {
		if _, err = printRuntimeUp(context.Background(), c, p); err != nil {
			return fmt.Errorf("start runtime: %w", err)
		}
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	created, err := conversation.Request(context.Background(), p.ChatSocket, conversation.ClientMsg{Op: "session_create", ProjectRoot: root, Ephemeral: true})
	if err != nil {
		return fmt.Errorf("create temporary session: %w", err)
	}
	if len(created) == 0 || created[0].Session == nil {
		return errors.New("session service returned no temporary session")
	}
	sessionID := created[0].Session.ID
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cleanupErr := discardPrintSession(ctx, p.ChatSocket, root, sessionID)
		if cleanupErr != nil {
			msg := fmt.Errorf("discard temporary session: %w", cleanupErr)
			if retErr == nil {
				retErr = msg
			} else {
				retErr = errors.Join(retErr, msg)
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := agent.ExecutionRequest{
		Work:         agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent:       input,
		Messages:     []llm.Message{{Role: "user", Content: input}},
		ProviderName: c.Model.Provider,
		Model:        c.Model.Model,
	}
	stream, err := conversation.OpenRun(ctx, p.ChatSocket, request)
	if err != nil {
		return fmt.Errorf("start print run: %w", err)
	}
	defer stream.Close()
	type streamResult struct {
		message conversation.ServerMsg
		err     error
	}
	streamMessages := make(chan streamResult, 1)
	go func() {
		for {
			message, receiveErr := stream.Receive()
			select {
			case streamMessages <- streamResult{message: message, err: receiveErr}:
			case <-ctx.Done():
				return
			}
			if receiveErr != nil {
				return
			}
		}
	}()
	var runID string
	var interactionErr error
	cancelSent := false
	timer := time.NewTimer(30 * time.Minute)
	defer timer.Stop()
	var cancelDeadline <-chan time.Time
	cancelRun := func() {
		if runID != "" && !cancelSent {
			_ = stream.Cancel(sessionID, runID)
			cancelSent = true
			cancelDeadline = time.After(10 * time.Second)
		}
	}
	for {
		select {
		case <-interrupt:
			interactionErr = errors.New("print interrupted")
			cancelRun()
			interrupt = nil
		case <-cancelDeadline:
			return interactionErr
		case <-timer.C:
			interactionErr = errors.New("print timed out")
			cancelRun()
			cancelDeadline = time.After(10 * time.Second)
		case result := <-streamMessages:
			if result.err != nil {
				return fmt.Errorf("read print stream: %w", result.err)
			}
			message := result.message
			switch message.Type {
			case "run_started":
				runID = message.RunID
				if interactionErr != nil {
					cancelRun()
				}
			case "run_event":
				if message.RunEvent == nil {
					continue
				}
				switch message.RunEvent.Kind {
				case string(agent.EventTextDelta):
					var payload struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(mustJSON(message.RunEvent.Payload), &payload) == nil && payload.Text != "" {
						if _, err = io.WriteString(stdout, safePrintText(payload.Text, c.Model.APIKey)); err != nil {
							interactionErr = fmt.Errorf("write stdout: %w", err)
							cancelRun()
						}
					}
				case string(agent.EventToolExecStart):
					var payload struct {
						ToolName string `json:"tool_name"`
					}
					if json.Unmarshal(mustJSON(message.RunEvent.Payload), &payload) == nil && !printReadTool(payload.ToolName) {
						interactionErr = fmt.Errorf("tool %q is not allowed in print; only read-only tools are available", payload.ToolName)
						cancelRun()
					}
				case string(agent.EventAwaitingApproval):
					interactionErr = errors.New("print cannot wait for approval")
					cancelRun()
				}
			case "approval_pending", "approvals", "questions", "plan_approval_pending", "plan_approvals":
				interactionErr = fmt.Errorf("print cannot wait for user interaction (%s)", message.Type)
				cancelRun()
			case "error":
				if message.Error != "" {
					interactionErr = safePrintError(errors.New(message.Error), c.Model.APIKey)
					if runID == "" {
						return interactionErr
					}
					cancelRun()
				}
			case "run_outcome":
				if interactionErr != nil {
					return interactionErr
				}
				if message.Outcome == nil {
					return errors.New("print run returned no outcome")
				}
				if message.Outcome.Status != agent.RunCompleted {
					reason := string(message.Outcome.Status)
					if message.Outcome.Error != nil && message.Outcome.Error.Message != "" {
						reason += ": " + safePrintText(message.Outcome.Error.Message, c.Model.APIKey)
					}
					return fmt.Errorf("print run %s", reason)
				}
				return nil
			case "done":
				if interactionErr != nil {
					return interactionErr
				}
			}
		}
	}
}

func discardPrintSession(ctx context.Context, socket, root, sessionID string) error {
	request := conversation.ClientMsg{Op: "session_discard", ProjectRoot: root, SessionID: sessionID}
	var lastErr error
	for {
		_, err := conversation.Request(ctx, socket, request)
		if err == nil {
			return nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "active run") {
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func readPrintInput(args []string, stdin io.Reader) (string, error) {
	var input string
	if len(args) != 0 {
		input = strings.Join(args, " ")
	} else {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		input = string(data)
	}
	if strings.TrimSpace(input) == "" {
		return "", errors.New("print input is empty")
	}
	return input, nil
}

func validatePrintProvider(provider string) error {
	if provider == "gemini" {
		return errors.New("Gemini supports target decisions only and cannot run print")
	}
	if provider != "openai" && provider != "anthropic" && provider != "openai-compatible" {
		return fmt.Errorf("unsupported streaming provider %q", provider)
	}
	return nil
}

func printReadTool(name string) bool {
	return name == "read_file" || name == "glob" || name == "grep"
}

func safePrintError(err error, credential string) error {
	if err == nil || credential == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), credential, "[REDACTED]"))
}

func safePrintText(text, credential string) string {
	if credential == "" {
		return text
	}
	return strings.ReplaceAll(text, credential, "[REDACTED]")
}

func mustJSON(value any) []byte {
	b, _ := json.Marshal(value)
	return b
}

// These production entry points are indirect so the CLI lifecycle can be
// tested against a real conversation service without spawning supervisor
// processes and Temporal dependencies.
var printRuntimeControl = runtime.Control
var printRuntimeUp = runtime.Up
