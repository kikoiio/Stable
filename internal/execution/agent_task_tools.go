package execution

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
)

var agentRoleNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Named-task tools stay in the host path to retain hooks and tool-call/result
// pairing. The service revalidates task ownership; these inputs never supply
// authority, project roots, provider credentials or a parent WorkRef.
func (e *toolRunExecutor) executeAgentTaskTool(ctx context.Context, call llm.ToolUse, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	if e.deps.AgentTasks == nil {
		outcome.Content = "Error: named agent task service is unavailable"
		return outcome
	}
	if e.request.Work.Kind != agent.WorkSession && e.request.Work.Kind != agent.WorkGoal {
		outcome.Content = "Error: named agent tasks require a session or goal parent"
		return outcome
	}
	var request agent.AgentTaskRequest
	var taskID string
	var wait time.Duration
	var err error
	switch call.Name {
	case "run_agent":
		request, err = parseAgentTaskRequest(args)
	case "task_output":
		taskID, wait, err = parseAgentTaskOutput(args)
	case "task_stop":
		if err = agentTaskFields(args, "task_id"); err == nil {
			taskID, err = agentTaskString(args, "task_id", true, 256)
		}
	}
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	if e.deps.Gate == nil {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: permission gate unavailable"
		return outcome
	}
	parameters, _ := json.Marshal(args)
	operation := permission.Operation{ID: call.ID, Kind: permission.OpRead, Name: call.Name, Target: e.authority.AllowedRoot, Parameters: parameters}
	decision, err := e.deps.Gate.Authorize(ctx, e.authority, operation)
	if err != nil {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: permission authorization failed"
		return outcome
	}
	if decision.Kind == permission.DecisionAsk {
		if e.approvalObserver != nil {
			e.approvalObserver()
		}
		decision, err = e.waitForApproval(ctx, operation)
		if err != nil {
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: named agent task approval unavailable or canceled"
			return outcome
		}
	}
	if decision.Kind != permission.DecisionAllow {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: "+safeReason(decision.Reason, "operation denied")
		return outcome
	}
	if ctx.Err() != nil {
		outcome.Content = "Error: parent run canceled"
		return outcome
	}
	parent, err := e.forkSkillParentRun()
	if err != nil {
		outcome.Content = "Error: could not derive named agent task context"
		return outcome
	}
	if call.Name == "run_agent" {
		parent.ToolCallID = call.ID
	}
	var result agent.AgentTaskSnapshot
	switch call.Name {
	case "run_agent":
		result, err = e.deps.AgentTasks.Run(ctx, parent, request)
	case "task_output":
		result, err = e.deps.AgentTasks.Output(ctx, parent, taskID, wait)
	case "task_stop":
		result, err = e.deps.AgentTasks.Stop(ctx, parent, taskID)
	}
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		outcome.Content = "Error: could not encode named agent task result"
		return outcome
	}
	outcome.Content = string(encoded)
	switch result.Status {
	case agent.DelegationQueued, agent.DelegationRunning, agent.DelegationSucceeded:
		outcome.Status, outcome.IsError = agent.ToolSucceeded, false
	default:
		outcome.Status, outcome.IsError = agent.ToolFailed, true
	}
	return outcome
}

func parseAgentTaskRequest(args map[string]any) (agent.AgentTaskRequest, error) {
	var request agent.AgentTaskRequest
	if err := agentTaskFields(args, "agent_name", "instruction", "background", "model", "timeout_ms"); err != nil {
		return request, err
	}
	var err error
	if request.AgentName, err = agentTaskString(args, "agent_name", true, 64); err != nil {
		return request, err
	}
	request.AgentName = strings.ToLower(request.AgentName)
	if !agentRoleNamePattern.MatchString(request.AgentName) {
		return request, errors.New("agent_name is invalid")
	}
	if request.Instruction, err = agentTaskString(args, "instruction", true, 65536); err != nil {
		return request, err
	}
	if request.Model, err = agentTaskString(args, "model", false, 256); err != nil {
		return request, err
	}
	if strings.IndexFunc(request.Model, unicode.IsSpace) >= 0 {
		return request, errors.New("model must not contain whitespace")
	}
	if value, present := args["background"]; present {
		var ok bool
		if request.Background, ok = value.(bool); !ok {
			return request, errors.New("background must be a boolean")
		}
	}
	if value, present := args["timeout_ms"]; present {
		ms, err := agentTaskMilliseconds(value, "timeout_ms", 1, 180000)
		if err != nil {
			return request, err
		}
		request.Timeout = time.Duration(ms) * time.Millisecond
	}
	return request, nil
}

func parseAgentTaskOutput(args map[string]any) (string, time.Duration, error) {
	if err := agentTaskFields(args, "task_id", "block", "wait_ms"); err != nil {
		return "", 0, err
	}
	id, err := agentTaskString(args, "task_id", true, 256)
	if err != nil {
		return "", 0, err
	}
	block := false
	if value, present := args["block"]; present {
		var ok bool
		if block, ok = value.(bool); !ok {
			return "", 0, errors.New("block must be a boolean")
		}
	}
	var wait time.Duration
	if block {
		wait = 30 * time.Second
	}
	if value, present := args["wait_ms"]; present {
		ms, err := agentTaskMilliseconds(value, "wait_ms", 0, 30000)
		if err != nil {
			return "", 0, err
		}
		if !block {
			return "", 0, errors.New("wait_ms requires block=true")
		}
		wait = time.Duration(ms) * time.Millisecond
	}
	return id, wait, nil
}

func agentTaskFields(args map[string]any, known ...string) error {
	for key := range args {
		found := false
		for _, name := range known {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return errors.New("unsupported named agent task argument")
		}
	}
	return nil
}

func agentTaskString(args map[string]any, key string, required bool, max int) (string, error) {
	value, present := args[key]
	if !present && !required {
		return "", nil
	}
	text, ok := value.(string)
	if !ok || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return "", errors.New(key + " must be a string without NUL")
	}
	text = strings.TrimSpace(text)
	if (required && text == "") || len(text) > max {
		return "", errors.New(key + " is empty or exceeds its byte limit")
	}
	if key != "instruction" && strings.IndexFunc(text, unicode.IsControl) >= 0 {
		return "", errors.New(key + " must not contain control characters")
	}
	return text, nil
}

func agentTaskMilliseconds(value any, key string, min, max int) (int, error) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < float64(min) || number > float64(max) {
		return 0, errors.New(key + " must be an integer within its allowed duration")
	}
	return int(number), nil
}
