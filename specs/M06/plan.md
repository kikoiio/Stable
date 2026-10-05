# M06 计划与任务交互 Plan

> 状态:已批准(2026-10-05)。基于已批准的 [spec.md](spec.md)。技术路线:对照源端全量迁移 + Stable 服务边界适配。

## 架构概览

M06 不引入新进程或新通信方式,在现有「TUI ⇄ conversation 服务 ⇄ agent run 执行」三层上增量扩展:

- **命令层**(新模块 `internal/commands`):纯库——注册表、`.md` 加载器、解析与展开;内置命令由 TUI 侧注册(闭包触发本地动作或服务 op),自定义命令统一为「提示词」类型,展开后走普通 chat 通路。
- **计划层**(新模块 `internal/planfile` + permission/execution 扩展):计划文件按会话命名存 `.stable/plans/<sessionID>.md`;`/plan` 切换的会话运行时状态保存在 conversation Service;`ModePlan` 在权限策略中获得真实语义(仅计划文件放行写);agent 仍用 `write_file`/`edit_file` 写计划,executor 识别该路径后主机侧直写(不进沙箱、不进候选区)。
- **计划审批**(conversation 新增 PlanApprovalService):独立的请求/状态机/广播,交互复制现有权限审批弹窗模式;三选项决策落会话事件并插入普通消息进入上下文。
- **提问链路**(execution 新增 `ask_user` 分支 + conversation 问询适配器):工具调用 → sink → 追加 M05 的 `EventQuestion` 并广播 → TUI 弹层 → `reply` op → 适配器轮询会话日志中的 `EventReply` 返回给阻塞中的工具;运行结束后遗留问题的答复由 `replyQuestion` 附加一条用户消息排队。
- **todo**(新模块 `internal/todo`):`TaskList`(进程内互斥 + JSON 存取 `.stable/tasks/<sessionID>.json`)+ 4 个工具的 schema 与 executor 执行分支;每次变更追加全量快照事件。
- **会话日志**(sessionlog 扩展):新增 `plan_mode`、`plan_approval`、`todo_update` 三种事件 + 校验钩子 + transcript 投影。
- **TUI**:统一待决策弹层队列(优先级:权限审批 > 提问 > 计划审批 > 提案确认),新增三个弹层组件,slash 补全切换到注册表,`/plan`、`/help` 成为内置命令。

新增工具(`ask_user`、`exit_plan_mode`、`task_create/get/list/update`)都不进沙箱 helper,`cmd/agentworker` 不改;schema 在 execution 层定义,两处白名单(`chatserveToolSchemas`/`runtimeToolSchemas`)追加。

## 核心数据结构

### internal/commands —— 命令注册表(纯库,不依赖 tui/conversation)

```go
type Kind int  // KindPrompt(展开为用户消息) / KindLocal(注册方闭包自行处理)

type Command struct {
    Name, Description, ArgPrompt string
    Aliases  []string
    Kind     Kind
    Local    func(args string)  // 仅 KindLocal 使用
}

type Registry struct{ /* name -> *Command,含别名索引 */ }
func (r *Registry) Register(c *Command)                // 内置注册;冲突 panic(仅内置使用)
func (r *Registry) RegisterOptional(c *Command) bool   // 自定义注册;同名冲突返回 false(内置优先,可观察)
func (r *Registry) Find(name string) (*Command, bool)
func (r *Registry) List() []*Command                   // 按名排序,/help 与补全共用

type Meta struct {                                     // .md frontmatter
    Description  string   `yaml:"description"`
    ArgumentHint string   `yaml:"argument-hint"`
    Aliases      []string `yaml:"aliases"`
}

type Loader struct{ /* 目录 mtime 缓存 + 最近结果,互斥保护 */ }
func NewLoader(projectDir, userDir string) *Loader
func (l *Loader) Commands() ([]*Command, []string, error) // 命令、被拒文件报告;目录 mtime 未变则返回缓存(热更新)
func ExpandPrompt(body, args string) string               // $ARGUMENTS 替换;无占位符且有参数时追加 "## User Request" 段
```

### internal/planfile

```go
const DirName = ".stable/plans"
func PlanPath(projectRoot, sessionID string) string   // <root>/.stable/plans/<sessionID>.md
func Ensure(projectRoot, sessionID string) (path string, existed bool, err error)
func Exists(projectRoot, sessionID string) bool
```

### internal/todo

```go
type Status string  // pending / in_progress / completed

type Task struct {
    ID, Subject, Description, ActiveForm string
    Status     Status
    Owner      string
    Blocks, BlockedBy []string
    Metadata   map[string]string
}

type UpdatePatch struct{ /* Subject/Description/ActiveForm/Status/Owner/Blocks/BlockedBy 可选修改;Status=="deleted" 表示移除 */ }

type TaskList struct{ /* sync.Mutex + *Store + onChange 回调 */ }
func NewTaskList(projectRoot, sessionID string, onChange func(tasks []Task) error) *TaskList
func (tl *TaskList) Create(subject, description, activeForm string, metadata map[string]string) (Task, error)
func (tl *TaskList) Get(id string) (Task, error)
func (tl *TaskList) List() ([]Task, error)
func (tl *TaskList) Update(id string, patch UpdatePatch) (Task, error) // deleted 同步清理 Blocks/BlockedBy 悬空引用
```

### internal/execution —— 工厂新依赖(可选注入)

```go
type OptionSpec struct{ Label, Description string }
type QuestionSpec struct{ Question, Header string; Options []OptionSpec; MultiSelect bool }
type AskRequest  struct{ SessionID, RunID, WorkRef string; Questions []QuestionSpec }
type AskResponse struct{ Answers [][]string }  // 每题:选中 label(多选多项)或自由文本,长度与题数一致
type QuestionSink interface{ Ask(ctx context.Context, req AskRequest) (AskResponse, error) }

type PlanSink interface {
    // 提交计划审批并阻塞等待;"auto"/"manual" 正常返回;feedback/cancel 以特定 error 类型返回
    SubmitPlan(ctx context.Context, sessionID, runID, planPath string) (choice string, err error)
}

type TodoProvider interface{ For(sessionID string) *todo.TaskList }
```

### internal/permission / internal/sessionlog / internal/conversation

```go
// permission/model.go —— Authority 增加字段
type Authority struct { /* 现有字段不变 */
    PlanFilePath string  // 非空 = 该会话处于计划模式
}
// permission/policy.go —— Decide 新分支:
//   Mode==ModePlan 且 OpWrite 且目标解析后 == Authority.PlanFilePath → allow(唯一 plan 模式免询问写)
//   ModePlan 其余行为与 ModeDefault 一致(读放行,写/命令 ask)

// sessionlog/events.go —— 新事件
EventPlanMode     = "plan_mode"      // {Mode: "plan"|"default", Reason: "user_toggle"|"plan_approved"|"plan_cancelled", At}
EventPlanApproval = "plan_approval"  // {RequestID, RunID, PlanPath, Status: "submitted"|"approved_auto"|"approved_manual"|"feedback"|"cancelled", Feedback, CreatedAt, ResolvedAt}
EventTodo         = "todo_update"    // {Revision int, Tasks []todo.Task}  全量快照
// validate.go:plan_approval 单次终态;todo Revision 严格递增、任务数 ≤ 100;plan_mode 无跨事件约束

// conversation —— 会话运行时计划状态与新服务
type PlanState struct{ Mode string; PlanPath string; ExecutionMode string; Runs int64 }
    // Service 内 map[sessionID]*PlanState,互斥保护;重启清空
    // Mode: "plan"|"default";ExecutionMode: 计划批准后的后续运行模式("acceptEdits"|"default"|"")
type PlanApproval struct {
    ID, SessionID, RunID, PlanPath, Status, Feedback string
    CreatedAt, ResolvedAt time.Time
}
```

## 模块设计

### internal/commands(新建,纯库零内部依赖)

**职责:** 注册表、`.md` 加载(热更新)、`$ARGUMENTS` 展开、`Parse`(name/args 切分)。
**要点:** 项目 `.stable/commands/` + 用户 `~/.config/stable/commands/`,项目覆盖用户;子目录 `命名空间:命令名`(相对路径小写、空格→`-`);frontmatter 三字段手写解析(不新增 yaml 依赖);界限常量:目录深度 3、单文件 256KB、命令总数 200;非法文件跳过并出报告;热更新 = 目录 mtime 惰性重扫。
**依赖:** 无。

### internal/planfile(新建)

**职责:** 计划路径计算、创建/复用、存在性检查。
**依赖:** 无。

### internal/todo(新建)

**职责:** Task 模型、TaskList(锁内 读→改→存→onChange 回调)、JSON Store(写入前脱敏)、4 个工具 schema 常量;`deleted` 移除任务并清理依赖悬空引用;ID 用 `RandomID("task")` 风格生成。
**依赖:** internal/redact。

### internal/redact(新建)

**职责:** 从 inputhistory/conversation/execution 提取共用的 ReplaceAll 脱敏函数(第 4 处复用前先提取);迁移既有三处调用。
**依赖:** 无。

### internal/permission(修改)

**职责:** `Authority.PlanFilePath` 字段;`Policy.Decide` 增加 ModePlan 分支——仅当写目标解析后等于 `PlanFilePath` 时放行,其余行为与 default 一致。
**依赖:** 无。

### internal/execution(修改)

**职责:** 工厂 options 注入 `QuestionSink`/`PlanSink`/`TodoProvider`;6 个新工具 schema 定义;`Execute` 旁路分支:
- `ask_user`:校验 1–4 题/2–4 选项 → sink 阻塞等待,ctx 取消返回 "Question cancelled";
- `exit_plan_mode`:非计划模式报错 → `PlanSink.SubmitPlan` 阻塞等待决策 → 结果文本指示结束回合;
- `task_create/get/list/update` → TodoProvider;
- 计划文件直写:解析目标 == `Authority.PlanFilePath` 时主机侧写(先脱敏),不进沙箱、不进候选区。
**依赖:** internal/todo、internal/permission、internal/redact。

### internal/conversation(修改)

**职责:**
- `PlanState` 会话运行时状态(重启清空);
- 新 op `plan_mode`(切换 + Ensure 计划文件 + `EventPlanMode`)、`plan_resolve`(auto/manual/feedback/cancel);
- `PlanApprovalService`:提交→广播→决策→终态事件→插入普通消息;`PlanSink` 实现等待决策(500ms 轮询);
- 问询适配器(QuestionSink 实现):追加 `EventQuestion`→广播→轮询 `EventReply`;维护 sessionID→活跃等待计数;
- `replyQuestion` 扩展:无活跃等待者时附加一条用户消息排队;
- poll 循环扩展:广播 pending 提问与计划审批;
- `BuildAuthority` 参数化(模式 + 计划文件路径);
- run 开始时向上下文 prepend 计划提醒(首次全文,此后每 5 次运行精简一次);
- session_load 响应附带 PlanState;
- TodoProvider 实现(onChange 追加 `EventTodo` + 广播 `ServerMsg{Type:"todo"}`)。
**依赖:** internal/sessionlog、internal/planfile、internal/todo、internal/prompt、internal/redact。

### internal/sessionlog(修改)

**职责:** 三种新事件 + payload struct + validate 钩子(`plan_approval` 单次终态;`todo_update` revision 单调、任务数 ≤ 100)+ 投影(`ItemPlanMode`/`ItemPlanApproval`/`ItemTodo`)。
**依赖:** internal/todo(Task 类型)。

### internal/prompt(修改)

**职责:** 计划工作流提醒文本(改编源端五阶段:去子代理表述,调研改为直接读文件,Phase 3 引用 ask_user;全文/精简两版)。
**依赖:** 无。

### internal/tui(修改)

**职责:** 内置命令全部注册进 Registry(`/plan /help` 新增);`refreshCompletions` 改走 Registry + Loader;三个新弹层组件(question/plan/proposal)与统一优先级函数;`handleResult`/`applyRunMessage` 新 case(questions/plan_approvals/todo/proposal 弹层触发);状态栏显示计划模式。
**依赖:** internal/commands、internal/conversation 协议类型。

### 白名单与配置

`cmd/stable/chatserve.go` 与 `internal/runtime/supervisor.go` 的 nameMap 追加 6 个工具名。`internal/appconfig` 不改(N5 界限用常量)。

## 模块交互(关键链路)

1. **自定义命令**:输入 `/git:log 最近改动` → TUI Parse → Loader 刷新(mtime 变化才重扫)→ Registry.Find → KindPrompt → `ExpandPrompt` → 走普通 chat 通路;KindLocal 命令闭包直接动作。
2. **进入计划模式**:`/plan` → `plan_mode` op → Service 切换 PlanState + Ensure 计划文件 + `EventPlanMode` → 状态栏更新。下一次 run:`BuildAuthority(ModePlan, PlanFilePath)`,上下文 prepend 计划提醒。
3. **计划审批**:agent 调 `exit_plan_mode` → executor → `PlanSink.SubmitPlan` → PlanApprovalService 建请求(submitted)+ 事件 + 广播 → TUI 按优先级弹层 → 用户决策 → `plan_resolve` op → 终态事件 + 插入用户消息(批准文案或反馈文本)→ 广播 → 等待侧 500ms 轮询拿到决策 → 工具结果指示结束回合;Service 同步更新 PlanState(auto 批准 → 后续 run 用 acceptEdits 语义)。
4. **提问**:agent 调 `ask_user` → executor 校验 → `QuestionSink.Ask` → 追加 `EventQuestion` + 广播 → 弹层 → 弹层或 `/reply` 答复 → `replyQuestion` 置 replied + `EventReply` → 适配器轮询到 → 结构化答案作为工具结果 → agent 继续。运行结束后仍 pending 的问题:`/reply` 答复时无活跃等待者 → 附加用户消息排队,下次运行进上下文。
5. **todo**:工具 → TaskList(锁内改 JSON)→ onChange 追加 `EventTodo` 快照 + 广播 `ServerMsg{Type:"todo"}` → transcript 投影渲染 + 实时更新。
6. **提案弹窗**:`create_goal` 响应(现有 proposal ServerMsg)→ 弹层(优先级最低)→ 确认/拒绝走现有 `confirm`/`reject` op;Esc 稍后。
7. **弹层优先级**:权限审批 > 提问 > 计划审批 > review(用户主动)> 提案确认。
8. **模式生效时机**:模式变更只影响后续运行;批准后工具结果指示结束回合(Authority 不可变,照源端 end-turn 约定)。

## 文件组织

```
internal/
├── commands/                 (新)
│   ├── registry.go           Registry/Command/Kind/Parse
│   ├── loader.go             Loader/mtime 热更新/frontmatter/ExpandPrompt
│   └── *_test.go
├── planfile/                 (新)
│   └── planfile.go           PlanPath/Ensure/Exists(+测试)
├── todo/                     (新)
│   ├── task.go               Task/TaskList/UpdatePatch/校验
│   ├── store.go              JSON 存取 + 脱敏
│   ├── tools.go              4 工具 schema
│   └── *_test.go
├── redact/
│   └── redact.go             (新)共用脱敏;inputhistory/conversation/execution 改为调用
├── permission/
│   ├── model.go              (改)Authority.PlanFilePath
│   └── policy.go             (改)ModePlan 分支(+测试)
├── execution/
│   ├── executor_factory.go   (改)options 注入 sinks
│   ├── tool_executor.go      (改)ask_user/exit_plan_mode/task_*/plan 直写分支
│   ├── tools_schema.go       (新)6 个新工具 schema
│   └── *_test.go
├── conversation/
│   ├── protocol.go           (改)ClientMsg 新字段、validOp、ServerMsg todo/plan 类
│   ├── session.go            (改)handle 新 op、PlanState、session_load 附带
│   ├── plan.go               (新)PlanApprovalService、PlanSink 实现、reminder 注入
│   ├── questions.go          (改)AskAdapter、replyQuestion 排队扩展
│   ├── run.go                (改)BuildAuthority 参数化
│   ├── polls.go              (改)轮询广播 questions/plan approvals
│   └── *_test.go
├── sessionlog/
│   ├── events.go             (改)3 新事件
│   ├── validate.go           (改)新校验
│   └── projection.go         (改)ItemPlanMode/ItemPlanApproval/ItemTodo
├── prompt/
│   └── plan_mode.go          (新)提醒文本(全文/精简)
├── tui/
│   ├── model.go              (改)命令注册、弹层队列、key 分发、handleResult
│   ├── question_dialog.go    (新)
│   ├── plan_dialog.go        (新)
│   ├── proposal_dialog.go    (新)
│   ├── completion.go         (改)registry 数据源
│   └── *_test.go
├── cmd/stable/chatserve.go   (改)白名单
└── internal/runtime/supervisor.go (改)白名单
tests/e2e/m06_interaction.sh  (新)端到端
```

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 计划文件命名 | `<sessionID>.md`,一会话一文件 | 归属清晰(N3);无需全局单例,slug 仅美观 |
| 计划文件写入 | 复用 write_file/edit_file + 策略白名单 + 主机侧直写 | 照源端;不新增工具;计划文件不在候选视图,不能进沙箱 |
| 新工具执行位置 | executor 分支 + sink 接口,不进 agentworker | 提问/审批/todo 非文件命令操作,无沙箱语义 |
| 提问等待机制 | 阻塞 + 500ms 轮询会话日志 | 与 waitForApproval 同模式;复用 M05 校验与一次性消费 |
| 运行结束后的答复 | replyQuestion 附加用户消息排队 | 与 /say 下轮消费语义一致;不与 Temporal 耦合 |
| 计划审批 | 独立 PlanApprovalService,不复用 PermissionService | 无 op/scope digest 语义;避免污染权限状态机 |
| 审批决策进上下文 | 插入普通用户消息 | 零投影改动,审计与上下文天然一致 |
| plan reminder | 运行开始注入上下文,不落事件 | 照源端 ephemeral;transcript 不被刷屏 |
| todo 事件 | 全量快照 + revision 单调 | 事件失败下次变更自愈;投影简单 |
| 热更新 | 目录 mtime 惰性重扫 | 无守护协程;满足「无需重启」 |
| frontmatter 解析 | 手写三字段解析 | 避免新增 yaml 依赖 |
| 脱敏 | 提取 internal/redact 共用 | 避免第 4 处复制 |
| 命令界限 | 常量(深度 3/单文件 256KB/命令总数 200/任务数 100) | YAGNI,不进配置 |
| 内置命令归属 | tui 侧注册闭包 | commands 包保持纯库,不依赖 conversation/tui 类型 |
| 模式生效时机 | 只影响后续运行;批准后工具结果指示结束回合 | Authority 不可变;照源端「end turn」 |
| 新 ExecutionEvent Kind | 不新增;TUI 依赖 conversation 推送 | transcript 已显示工具调用本身;推送驱动弹层足够 |
