package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Event identifies a lifecycle point at which a hook may run.
type Event string

const (
	EventRunStart    Event = "run_start"
	EventRunEnd      Event = "run_end"
	EventPreToolUse  Event = "pre_tool_use"
	EventPostToolUse Event = "post_tool_use"
)

// Action describes the operation performed by a hook.
type Action struct {
	Type    string            `yaml:"type"`
	Command string            `yaml:"command,omitempty"`
	Message string            `yaml:"message,omitempty"`
	URL     string            `yaml:"url,omitempty"`
	Method  string            `yaml:"method,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Body    string            `yaml:"body,omitempty"`
	Timeout time.Duration     `yaml:"timeout,omitempty"`
}

type Hook struct {
	ID      string `yaml:"id,omitempty"`
	Event   Event  `yaml:"event"`
	If      string `yaml:"if,omitempty"`
	Action  Action `yaml:"action"`
	Reject  bool   `yaml:"reject,omitempty"`
	Once    bool   `yaml:"once,omitempty"`
	Async   bool   `yaml:"async,omitempty"`
	OnError string `yaml:"on_error,omitempty"`
	Source  string `yaml:"-"`
}

type Context struct {
	Event    Event
	ToolName string
	ToolArgs map[string]any
	FilePath string
	Message  string
}

type Result struct {
	HookID     string
	Output     string
	Success    bool
	Rejected   bool
	TimedOut   bool
	ChildRunID string
}

var validEvents = map[Event]bool{
	EventRunStart: true, EventRunEnd: true, EventPreToolUse: true, EventPostToolUse: true,
}

func Validate(list []Hook) error {
	var errs []error
	for i, h := range list {
		label := h.ID
		if label == "" {
			label = fmt.Sprintf("hook[%d]", i)
		}
		var fields []error
		if !validEvents[h.Event] {
			fields = append(fields, fmt.Errorf("event %q is not supported", h.Event))
		}
		if h.Action.Timeout < 0 {
			fields = append(fields, errors.New("timeout must be non-negative"))
		}
		switch h.OnError {
		case "", "fail", "ignore", "reject":
		default:
			fields = append(fields, fmt.Errorf("on_error %q must be fail, ignore, or reject", h.OnError))
		}
		switch strings.ToLower(h.Action.Type) {
		case "command":
			if strings.TrimSpace(h.Action.Command) == "" {
				fields = append(fields, errors.New("command action requires command"))
			}
		case "prompt":
			if strings.TrimSpace(h.Action.Message) == "" {
				fields = append(fields, errors.New("prompt action requires message"))
			}
		case "http":
			u, err := parseHTTPURL(h.Action.URL)
			if err != nil {
				fields = append(fields, err)
			} else if u == "" {
				fields = append(fields, errors.New("http action requires url"))
			}
		case "agent":
			if strings.TrimSpace(h.Action.Message) == "" && strings.TrimSpace(h.Action.Command) == "" {
				fields = append(fields, errors.New("agent action requires message or command"))
			}
		default:
			fields = append(fields, fmt.Errorf("action type %q is not supported", h.Action.Type))
		}
		if len(fields) > 0 {
			for _, err := range fields {
				errs = append(errs, fmt.Errorf("%s: %w", label, err))
			}
		}
	}
	return errors.Join(errs...)
}

func parseHTTPURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("http url must use http or https")
	}
	return raw, nil
}

func EvaluateCondition(cond string, ctx Context) bool {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return true
	}
	return evalExpr(cond, ctx)
}

func evalExpr(s string, ctx Context) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	if s[0] == '(' && matchingParen(s, 0) == len(s)-1 {
		return evalExpr(s[1:len(s)-1], ctx)
	}
	if parts := splitTop(s, "||"); len(parts) > 1 {
		for _, p := range parts {
			if evalExpr(p, ctx) {
				return true
			}
		}
		return false
	}
	if parts := splitTop(s, "&&"); len(parts) > 1 {
		for _, p := range parts {
			if !evalExpr(p, ctx) {
				return false
			}
		}
		return true
	}
	if strings.HasPrefix(s, "!") {
		return !evalExpr(strings.TrimSpace(s[1:]), ctx)
	}
	for _, op := range []string{"=~", "=*", "==", "!="} {
		if i := findOperator(s, op); i >= 0 {
			left, right := strings.TrimSpace(s[:i]), unquote(strings.TrimSpace(s[i+len(op):]))
			value := resolveVar(left, ctx)
			switch op {
			case "==":
				return value == right
			case "!=":
				return value != right
			case "=~":
				ok, err := regexp.MatchString(right, value)
				return err == nil && ok
			case "=*":
				ok, err := filepath.Match(right, value)
				return err == nil && ok
			}
		}
	}
	return truthy(resolveVar(s, ctx))
}

func matchingParen(s string, start int) int {
	depth := 0
	for i, r := range s[start:] {
		if r == '(' {
			depth++
		}
		if r == ')' {
			depth--
			if depth == 0 {
				return start + i
			}
		}
	}
	return -1
}
func findOperator(s, op string) int {
	depth := 0
	quote := byte(0)
	for i := 0; i+len(op) <= len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote && (i == 0 || s[i-1] != '\\') {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			depth--
		}
		if depth == 0 && s[i:i+len(op)] == op {
			return i
		}
	}
	return -1
}
func splitTop(s, op string) []string {
	var out []string
	depth, start := 0, 0
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote && (i == 0 || s[i-1] != '\\') {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			depth--
		}
		if depth == 0 && strings.HasPrefix(s[i:], op) {
			out = append(out, s[start:i])
			start = i + len(op)
			i += len(op) - 1
		}
	}
	if len(out) == 0 {
		return nil
	}
	return append(out, s[start:])
}
func unquote(s string) string {
	if len(s) >= 2 && ((s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"')) {
		return s[1 : len(s)-1]
	}
	return s
}
func truthy(s string) bool { return s != "" && s != "0" && strings.ToLower(s) != "false" }
func resolveVar(name string, ctx Context) string {
	switch name {
	case "tool":
		return ctx.ToolName
	case "event":
		return string(ctx.Event)
	case "file_path":
		return ctx.FilePath
	case "message":
		return ctx.Message
	}
	if strings.HasPrefix(name, "args.") {
		if v, ok := ctx.ToolArgs[strings.TrimPrefix(name, "args.")]; ok {
			return fmt.Sprint(v)
		}
	}
	return name
}

func FireOne(h Hook, ctx Context) Result {
	r := Result{HookID: h.ID}
	timeout := h.Action.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	var output string
	var err error
	switch strings.ToLower(h.Action.Type) {
	case "prompt":
		output = h.Action.Message
	case "http":
		output, err = runHTTPAction(h.Action, timeout)
	case "agent":
		err = errors.New("agent action must run through the HookGate coordinator")
	case "command":
		ctxTimeout, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctxTimeout, "bash", "-c", h.Action.Command)
		cmd.Env = append(os.Environ(), "STABLE_EVENT="+string(ctx.Event), "STABLE_TOOL="+ctx.ToolName, "STABLE_FILE_PATH="+ctx.FilePath)
		b, runErr := cmd.CombinedOutput()
		output = strings.TrimSpace(string(b))
		err = runErr
		if ctxTimeout.Err() == context.DeadlineExceeded {
			r.TimedOut = true
			if err == nil {
				err = ctxTimeout.Err()
			}
		}
	default:
		err = fmt.Errorf("unsupported action %q", h.Action.Type)
	}
	r.Output, r.Success = output, err == nil
	r.Rejected = h.Reject || (!r.Success && h.OnError == "reject")
	if err != nil && output == "" {
		r.Output = err.Error()
	}
	return r
}

func Fire(list []Hook, ctx Context) []Result {
	results := make([]Result, 0)
	for _, h := range list {
		if h.Event == ctx.Event && EvaluateCondition(h.If, ctx) {
			results = append(results, FireOne(h, ctx))
		}
	}
	return results
}
