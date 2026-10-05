package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/sandbox"
	"stable/internal/store"
	"stable/internal/todo"
)

// CandidateLifecycle is the existing persistence surface needed for lazy run candidates.
type CandidateLifecycle interface {
	SaveCandidate(context.Context, store.CandidateRecord) error
	TransitionCandidate(context.Context, string, string, string, string) error
}

// SnapshotCreator is the checkpoint surface used at tool write boundaries;
// *candidate.SnapshotStore implements it.
type SnapshotCreator interface {
	Create(sessionID, candidateID, runID, label, candidateRoot string) (candidate.FileSnapshot, error)
}

type ToolExecutorDeps struct {
	Sandbox            sandbox.SandboxManager
	Gate               PermissionGate
	Approvals          permission.ApprovalRepository
	HelperPath         string
	SessionRoot        string
	Now                func() time.Time
	PollEvery          time.Duration
	Candidates         CandidateLifecycle
	ProviderCredential string
	// Snapshots checkpoints the candidate around every file-changing tool
	// call. Nil disables checkpointing (tests and read-only tooling).
	Snapshots SnapshotCreator
	// QuestionSink, when set, receives ask_user questions and blocks until the
	// user answers. Nil makes ask_user unavailable; the nil behavior is
	// defined by the M06 tool dispatch.
	QuestionSink QuestionSink
	// PlanSink, when set, receives exit_plan_mode submissions and blocks until
	// the user approves, gives feedback, or cancels. Nil makes exit_plan_mode
	// unavailable; the nil behavior is defined by the M06 tool dispatch.
	PlanSink PlanSink
	// TodoProvider, when set, serves the per-session task lists used by the
	// task_create/task_get/task_list/task_update tools. Nil makes them
	// unavailable; the nil behavior is defined by the M06 tool dispatch.
	TodoProvider TodoProvider
}

// OptionSpec is one choice shown to the user for a question.
type OptionSpec struct{ Label, Description string }

// QuestionSpec is a single question presented to the user by ask_user.
type QuestionSpec struct {
	Question    string
	Header      string
	Options     []OptionSpec
	MultiSelect bool
}

// AskRequest carries the questions the model asked during one interaction.
type AskRequest struct {
	SessionID string
	RunID     string
	WorkRef   string
	Questions []QuestionSpec
}

// AskResponse holds one answer per question, aligned with AskRequest.Questions:
// each entry lists the selected labels (several when multiSelect) or the free
// text typed for the auto-provided "Other" option.
type AskResponse struct{ Answers [][]string }

// QuestionSink delivers ask_user questions to the host interface and blocks
// until the user answers or the run context is cancelled.
type QuestionSink interface {
	Ask(ctx context.Context, req AskRequest) (AskResponse, error)
}

// PlanChoiceAuto and PlanChoiceManual are the successful SubmitPlan choices:
// auto approves the plan and continues in accept-edits semantics, manual
// approves it and continues with explicit user confirmation.
const (
	PlanChoiceAuto   = "auto"
	PlanChoiceManual = "manual"
)

// PlanSink submits a completed plan for user approval and blocks until the
// user decides. "auto"/"manual" return normally; feedback and cancellation are
// reported as *PlanFeedbackError and PlanCancelledError respectively.
type PlanSink interface {
	SubmitPlan(ctx context.Context, sessionID, runID, planPath string) (choice string, err error)
}

// PlanFeedbackError reports that the user rejected the submitted plan and
// attached feedback text; the model should revise the plan.
type PlanFeedbackError struct{ Text string }

func (e PlanFeedbackError) Error() string { return "plan feedback: " + e.Text }

// PlanCancelledError reports that the user dismissed the plan approval.
type PlanCancelledError struct{}

func (PlanCancelledError) Error() string { return "plan approval cancelled" }

// TodoProvider serves the task list owned by one session. Returning nil means
// the session has no task list yet; the tool dispatch decides what to do.
type TodoProvider interface {
	For(sessionID string) *todo.TaskList
}

// ToolExecutorOption customizes the optional M06 tool dependencies at factory
// construction so existing deps literal call sites stay unchanged.
type ToolExecutorOption func(*ToolExecutorDeps)

// WithQuestionSink injects the interactive question sink used by ask_user.
func WithQuestionSink(sink QuestionSink) ToolExecutorOption {
	return func(deps *ToolExecutorDeps) { deps.QuestionSink = sink }
}

// WithPlanSink injects the plan approval sink used by exit_plan_mode.
func WithPlanSink(sink PlanSink) ToolExecutorOption {
	return func(deps *ToolExecutorDeps) { deps.PlanSink = sink }
}

// WithTodoProvider injects the per-session task list provider used by the
// task_create/task_get/task_list/task_update tools.
func WithTodoProvider(provider TodoProvider) ToolExecutorOption {
	return func(deps *ToolExecutorDeps) { deps.TodoProvider = provider }
}

type ToolExecutorFactory struct{ deps ToolExecutorDeps }

func NewToolExecutorFactory(deps ToolExecutorDeps, options ...ToolExecutorOption) agent.ExecutorFactory {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.PollEvery <= 0 {
		deps.PollEvery = defaultPoll
	}
	for _, option := range options {
		if option != nil {
			option(&deps)
		}
	}
	return ToolExecutorFactory{deps: deps}
}

func (f ToolExecutorFactory) ForRun(request agent.ExecutionRequest) (agent.RunExecutor, error) {
	if request.RunID == "" || request.Work.SessionID == "" {
		return nil, errors.New("tool execution requires run and session IDs")
	}
	var authority permission.Authority
	if err := json.Unmarshal(request.PermissionBounds, &authority); err != nil {
		return nil, fmt.Errorf("decode trusted permission bounds: %w", err)
	}
	if authority.RunID != request.RunID || authority.SessionID != request.Work.SessionID || authority.GoalID != request.Work.GoalID || authority.WorkItemID != request.Work.WorkItemID {
		return nil, errors.New("permission bounds do not match execution request")
	}
	if authority.AllowedRoot == "" || authority.CandidateRoot == "" {
		return nil, errors.New("permission bounds lack formal and candidate roots")
	}
	formal, err := filepath.Abs(authority.AllowedRoot)
	if err != nil {
		return nil, err
	}
	candidateRoot, err := filepath.Abs(authority.CandidateRoot)
	if err != nil {
		return nil, err
	}
	authority.AllowedRoot = filepath.Clean(formal)
	authority.CandidateRoot = filepath.Clean(candidateRoot)
	if authority.FormalRoot == "" {
		authority.FormalRoot = authority.AllowedRoot
	}
	runRoot := filepath.Join(filepath.Dir(authority.CandidateRoot), ".stable-runs", request.RunID)
	if err = os.MkdirAll(runRoot, 0700); err != nil {
		return nil, err
	}
	runRoot, err = filepath.Abs(runRoot)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(runRoot, 0700); err != nil {
		return nil, err
	}
	return &toolRunExecutor{deps: f.deps, request: request, authority: authority, runRoot: runRoot}, nil
}
