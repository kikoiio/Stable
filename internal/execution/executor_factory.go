package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"stable/internal/platform/secfile"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/store"
	"stable/internal/todo"
	"stable/internal/workspace"
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
	CreateForPolicy(sessionID, candidateID, runID, label, candidateRoot, policy string) (candidate.FileSnapshot, error)
}

// HookRunner runs lifecycle hooks around individual tool calls. A rejected
// pre-hook can only block a call; it never alters permission decisions.
type HookRunner interface {
	PreToolUseRun(ctx context.Context, parent agent.ParentRun, sessionID, toolName string, args map[string]any) (rejected bool, hookID, message string)
	PostToolUseRun(ctx context.Context, parent agent.ParentRun, sessionID, toolName string, args map[string]any, result string)
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
	// SkillProvider, when set, serves skill activation for load_skill and the
	// per-session inventory used by run-context injection. Nil makes load_skill
	// unavailable; the nil behavior is defined by the M07-A tool dispatch.
	SkillProvider SkillProvider
	// MCP, when set, serves the M07-C MCP tool dispatch: direct
	// mcp__<server>__<tool> calls, the mcp_call bridge and the tool_search
	// index. Nil keeps all three entries on the plain unknown-tool path, so
	// deployments without MCP stay unchanged.
	MCP       MCPCaller
	Delegator agent.Delegator
	Provider  llm.Provider
	// AgentTasks serves named-agent launch, output and cancellation to parents.
	AgentTasks agent.AgentTaskService
	// TeamTools routes trusted team operations through the conversation service.
	TeamTools *agent.TeamToolHost
	// WorkspaceLease enables the isolated file-tool surface for a single
	// service-owned workspace generation. Command execution remains disabled.
	WorkspaceLease      *workspace.WriterLease
	WorkspaceAccounting workspace.WriterAccounting
	// ReadOnly restricts dispatch to project read/search/list tools. It is
	// applied by the executor as a hard allowlist, independently of schemas.
	ReadOnly bool
	// HookRunner runs optional pre/post tool-use hooks. A nil runner leaves
	// existing tool execution behavior unchanged.
	HookRunner HookRunner
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

// AskResponse holds the reply to one ask_user interaction. In the structured
// form, Answers is aligned with AskRequest.Questions and each entry lists the
// selected labels (several when multiSelect) or the free text typed for the
// auto-provided "Other" option. In the free-text form (FreeText is true) the
// user answered with one overall reply text — a /reply line or a dialog text
// answer — so Answers carries exactly one entry with the raw text and the
// renderer shows it once for the whole question set.
type AskResponse struct {
	Answers  [][]string
	FreeText bool
}

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

// SkillProvider resolves skill activations and the per-session skill
// inventory on the host. LoadSkill returns a rendered body for inline skills
// or a fork result for fork-mode skills. SkillInventory returns the stable
// inventory text for run-context injection plus, when new skills appeared
// since the last run, a one-shot delta reminder.
type SkillProvider interface {
	LoadSkill(ctx context.Context, sessionID, name, args string) (string, error)
	SkillInventory(ctx context.Context, sessionID string) (snapshotText, deltaReminder string, err error)
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

// WithSkillProvider injects the host skill provider used by load_skill and
// the run-context skill inventory.
func WithSkillProvider(provider SkillProvider) ToolExecutorOption {
	return func(deps *ToolExecutorDeps) { deps.SkillProvider = provider }
}

// WithMCPCaller injects the MCP caller backing the mcp__ direct-call prefix,
// the mcp_call bridge and tool_search.
func WithMCPCaller(caller MCPCaller) ToolExecutorOption {
	return func(deps *ToolExecutorDeps) { deps.MCP = caller }
}

func WithAgentTaskService(service agent.AgentTaskService) ToolExecutorOption {
	return func(deps *ToolExecutorDeps) { deps.AgentTasks = service }
}

func WithTeamToolHost(host *agent.TeamToolHost) ToolExecutorOption {
	return func(deps *ToolExecutorDeps) { deps.TeamTools = host }
}

func WithReadOnlyTools() ToolExecutorOption {
	return func(deps *ToolExecutorDeps) {
		deps.ReadOnly = true
		deps.MCP = nil
		deps.Candidates = nil
		deps.Snapshots = nil
		deps.QuestionSink = nil
		deps.PlanSink = nil
		deps.TodoProvider = nil
		deps.SkillProvider = nil
		deps.AgentTasks = nil
		deps.TeamTools = nil
		deps.HookRunner = nil
	}
}

func WithDelegator(delegator agent.Delegator, provider llm.Provider) ToolExecutorOption {
	return func(deps *ToolExecutorDeps) {
		deps.Delegator = delegator
		deps.Provider = provider
	}
}

// WorkspaceWriterExecutorFactory derives a constrained factory for one
// already-acquired workspace lease. It carries no lifecycle or export tools.
func WorkspaceWriterExecutorFactory(factory agent.ExecutorFactory, lease workspace.WriterLease, accounting workspace.WriterAccounting) agent.ExecutorFactory {
	base, ok := factory.(ToolExecutorFactory)
	if !ok {
		return nil
	}
	deps := base.deps
	leaseCopy := lease
	deps.WorkspaceLease = &leaseCopy
	deps.WorkspaceAccounting = accounting
	deps.SessionRoot = ""
	deps.Candidates = nil
	deps.Snapshots = nil
	deps.HookRunner = nil
	deps.AgentTasks = nil
	deps.TeamTools = nil
	deps.Delegator = nil
	deps.MCP = nil
	deps.QuestionSink = nil
	deps.PlanSink = nil
	deps.TodoProvider = nil
	deps.SkillProvider = nil
	deps.ReadOnly = false
	return NewToolExecutorFactory(deps)
}

// WithHookRunner injects the lifecycle hook runner for tool calls.
func WithHookRunner(runner HookRunner) ToolExecutorOption {
	return func(deps *ToolExecutorDeps) { deps.HookRunner = runner }
}

type ToolExecutorFactory struct{ deps ToolExecutorDeps }

// ReadOnlyExecutorFactory derives the child executor factory used by fork
// skills and delegation. It preserves the existing permission gate and
// sandbox wiring while applying the executor's hard read-only allowlist.
func ReadOnlyExecutorFactory(factory agent.ExecutorFactory) agent.ExecutorFactory {
	base, ok := factory.(ToolExecutorFactory)
	if !ok {
		return nil
	}
	deps := base.deps
	deps.SessionRoot = ""
	deps.HookRunner = nil // child tool calls must not recursively trigger parent hooks
	return NewToolExecutorFactory(deps, WithReadOnlyTools())
}

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
	if f.deps.WorkspaceLease != nil {
		lease := *f.deps.WorkspaceLease
		if lease.WorkspaceID == "" || lease.RunID != request.RunID || lease.Generation == 0 || lease.Authority.RunID != request.RunID || lease.Scope.Authority.RunID == request.RunID || lease.Scope.Work.SessionID != request.Work.SessionID || lease.Scope.Work.GoalID != request.Work.GoalID || lease.Scope.Work.WorkItemID != request.Work.WorkItemID || lease.Scope.Authority.SessionID != request.Work.SessionID || lease.Scope.Authority.GoalID != request.Work.GoalID || lease.Scope.Authority.WorkItemID != request.Work.WorkItemID || filepath.Clean(lease.Scope.Authority.FormalRoot) != filepath.Clean(authority.FormalRoot) || filepath.Clean(lease.Scope.Authority.AllowedRoot) != filepath.Clean(authority.FormalRoot) || filepath.Clean(lease.Paths.Root) == "." || filepath.Dir(lease.Paths.Baseline) != filepath.Clean(lease.Paths.Root) || filepath.Dir(lease.Paths.Repository) != filepath.Clean(lease.Paths.Root) || filepath.Dir(lease.Paths.Checkout) != filepath.Clean(lease.Paths.Root) || filepath.Dir(lease.Paths.Run) != filepath.Clean(lease.Paths.Root) || filepath.Clean(lease.Paths.Baseline) != authority.AllowedRoot || filepath.Clean(lease.Paths.Checkout) != authority.CandidateRoot || filepath.Clean(lease.Authority.FormalRoot) != filepath.Clean(authority.FormalRoot) || filepath.Clean(lease.Authority.AllowedRoot) != authority.AllowedRoot || filepath.Clean(lease.Authority.CandidateRoot) != authority.CandidateRoot || len(authority.Network) != 0 || authority.Mode == permission.ModePlan || f.deps.WorkspaceAccounting == nil {
			return nil, errors.New("workspace writer lease is invalid")
		}
		if lease.Scope.ProjectID == "" || lease.Scope.Validate() != nil {
			return nil, errors.New("workspace writer scope is invalid")
		}
		runRoot = lease.Paths.Run
	}
	if err = secfile.MkdirAllPrivate(runRoot, 0700); err != nil {
		return nil, err
	}
	runRoot, err = filepath.Abs(runRoot)
	if err != nil {
		return nil, err
	}
	if err = secfile.ChmodPrivate(runRoot, 0700); err != nil {
		return nil, err
	}
	return &toolRunExecutor{deps: f.deps, request: request, authority: authority, runRoot: runRoot}, nil
}
