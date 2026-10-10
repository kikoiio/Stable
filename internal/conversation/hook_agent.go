package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"stable/internal/agent"
	"stable/internal/hooks"
	"stable/internal/sessionlog"
)

const hookAgentMaxDuration = 3 * time.Minute

type HookAgentInvocation struct {
	Parent      agent.ParentRun
	HookID      string
	Instruction string
	Event       hooks.Context
	Timeout     time.Duration
}

type HookAgentCoordinator struct {
	Delegator  agent.Delegator
	Credential string
}

func (c HookAgentCoordinator) Execute(ctx context.Context, invocation HookAgentInvocation) hooks.Result {
	result := hooks.Result{HookID: invocation.HookID}
	if c.Delegator == nil {
		return hookAgentFailure(result, "hook agent delegation service is unavailable", false)
	}
	instruction := strings.TrimSpace(invocation.Instruction)
	if instruction == "" {
		return hookAgentFailure(result, "agent action requires message or command", false)
	}
	eventPayload, err := json.Marshal(struct {
		Event    hooks.Event    `json:"event"`
		ToolName string         `json:"tool_name,omitempty"`
		ToolArgs map[string]any `json:"tool_args,omitempty"`
		FilePath string         `json:"file_path,omitempty"`
		Message  string         `json:"message,omitempty"`
	}{
		Event: invocation.Event.Event, ToolName: invocation.Event.ToolName,
		ToolArgs: invocation.Event.ToolArgs, FilePath: invocation.Event.FilePath,
		Message: invocation.Event.Message,
	})
	if err != nil {
		return hookAgentFailure(result, "could not encode hook event context", false)
	}
	taskID, err := sessionlog.NewID()
	if err != nil {
		return hookAgentFailure(result, "could not create hook child task ID", false)
	}
	name := "hook agent"
	if invocation.HookID != "" {
		name += ": " + invocation.HookID
	}
	instruction = redactRunCredential(instruction, c.Credential)
	eventPayload = []byte(redactRunCredential(string(eventPayload), c.Credential))
	task := agent.DelegationTask{
		ID: taskID, Name: name,
		Instruction: "Hook agent instruction:\n" + instruction + "\n\nCurrent hook event (JSON):\n" + string(eventPayload),
	}
	encoded, err := json.Marshal(task)
	if err != nil || len(encoded) > agent.DefaultDelegationLimits().MaxInputBytes {
		return hookAgentFailure(result, fmt.Sprintf("hook agent input exceeds %d bytes", agent.DefaultDelegationLimits().MaxInputBytes), false)
	}

	timeout := invocation.Timeout
	if timeout <= 0 || timeout > hookAgentMaxDuration {
		timeout = hookAgentMaxDuration
	}
	if ctx == nil {
		ctx = context.Background()
	}
	childCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	delegated, runErr := c.Delegator.RunTask(childCtx, invocation.Parent, task)
	result.ChildRunID = delegated.ChildRunID
	if runErr != nil {
		return hookAgentFailure(result, runErr.Error(), errors.Is(childCtx.Err(), context.DeadlineExceeded))
	}
	result.Output = delegated.Summary
	if delegated.Status == agent.DelegationSucceeded {
		result.Success = true
	} else {
		message := delegated.Error
		if message == "" {
			message = string(delegated.Status)
		}
		result.Success = false
		result.TimedOut = errors.Is(childCtx.Err(), context.DeadlineExceeded)
		if result.Output == "" {
			result.Output = message
		} else if delegated.Error != "" {
			result.Output += "\nError: " + delegated.Error
		}
	}
	return result
}

func hookAgentFailure(result hooks.Result, message string, timedOut bool) hooks.Result {
	result.Output = message
	result.Success = false
	result.TimedOut = timedOut
	return result
}

func (g *HookGate) runAgent(ctx context.Context, parent agent.ParentRun, hook hooks.Hook, event hooks.Context) hooks.Result {
	service := g.service
	if service == nil {
		result := hookAgentFailure(hooks.Result{HookID: hook.ID}, "hook agent service is unavailable", false)
		result.Rejected = hook.Reject || hook.OnError == "reject"
		return result
	}
	instruction := hook.Action.Message
	if strings.TrimSpace(instruction) == "" {
		instruction = hook.Action.Command
	}
	result := (HookAgentCoordinator{Delegator: service.deps.Delegator, Credential: service.deps.ProviderCredential}).Execute(ctx, HookAgentInvocation{
		Parent: parent, HookID: hook.ID, Instruction: instruction,
		Event: event, Timeout: hook.Action.Timeout,
	})
	result.Rejected = hook.Reject || (!result.Success && hook.OnError == "reject")
	return result
}
