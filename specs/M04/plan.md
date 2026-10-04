# M04 基础工具 Plan

> 状态：已批准（2026-10-04）。输入：已批准的 [spec.md](spec.md)。模块路径前缀 `stable/internal/...`。

## 架构概览

五层，自下而上：

1. **工具实现层 `internal/tools`**（自 mewcode-golang 移植）：纯 stdlib 的工具行为本体与注册表——`Tool` 接口、`Registry`、ReadFile/WriteFile/EditFile/Glob/Grep、`FileStateCache`（读后改纪律）、`BuildDiff`（diff 摘要）、`IsSafeCommand`。不含权限、沙箱、候选语义。同一套代码：宿主侧用于 schema 生成，客内 helper 进程用于实际执行。
2. **模型接口层 `internal/llm`**：`Message` 增加 tool_use / tool_result 表示，`Request` 增加 `Tools` 定义；anthropic / openai / openai-compatible 三个 provider 适配各自线上格式；零值字段保持既有序列化（向后兼容）。
3. **执行循环层 `internal/agent`**：`StreamingRunner` 从单趟流改为多轮工具循环；新增 `RunExecutor`/`ExecutorFactory` 接口与 `FakeExecutor`；新事件种类（tool_exec_start / tool_exec_result / awaiting_approval / budget_exhausted）与新终态 `budget_exhausted`。
4. **受控执行层 `internal/execution`**：沙箱工具执行器——权限评估（复用 M03 门）、两段式审批等待、模型路径→宿主/客内路径映射、bwrap profile 构造、输出截断与脱敏、候选区生命周期（惰性创建 / 冻结 / 登记）。文件工具经 agentworker 客内 helper 执行，命令工具直接在沙箱内 `bash -c` 执行。
5. **编排与持久层 `internal/conversation`、`internal/sessionlog`、`internal/tui`**：工具工作区 system prompt、运行结束候选定稿、工具调用持久记录（复用 `EventToolCall/EventToolResult`）、跨运行历史重建扩展、终态枚举扩展、TUI 新事件标签与状态文案。

一句话数据流：模型流 → 收集工具调用 → 预算检查 → 执行器（权限 → 审批等待 → bwrap 沙箱）→ 结果回填对话 → 下一轮 → 终态 → 候选冻结登记 → 既有 review/accept。

## 核心数据结构

### internal/llm 扩展（provider.go）

```go
type ToolSchema struct {
    Name        string         `json:"name"`
    Description string         `json:"description"`
    InputSchema map[string]any `json:"input_schema"`
}

type ToolUse struct { // assistant 消息携带：模型发起的工具调用
    ID        string          `json:"id"`
    Name      string          `json:"name"`
    Arguments json.RawMessage `json:"arguments,omitempty"`
}

type ToolResultPart struct { // user 消息携带：回填的工具结果
    ToolUseID string `json:"tool_use_id"`
    Content   string `json:"content"`
    IsError   bool   `json:"is_error,omitempty"`
}

type Message struct {
    Role        string           `json:"role"`
    Content     string           `json:"content"`
    ToolUses    []ToolUse        `json:"tool_uses,omitempty"`
    ToolResults []ToolResultPart `json:"tool_results,omitempty"`
}

type Request struct {
    Model     string       `json:"model"`
    Messages  []Message    `json:"messages"`
    Tools     []ToolSchema `json:"tools,omitempty"`
    MaxTokens int          `json:"max_tokens,omitempty"`
}
```

Provider 序列化约定：
- **anthropic**：assistant 消息 → content blocks（`text` + `tool_use`）；user 消息的 `ToolResults` → content blocks（`tool_result`，带 `is_error`）；`Request.Tools` → `tools:[{name, description, input_schema}]`。
- **openai / openai-compatible**：assistant 消息 → `tool_calls:[{id,type:"function",function:{name,arguments}}]`；每条工具结果 → 独立 `{role:"tool", tool_call_id, content}` 消息；`Request.Tools` → `tools:[{type:"function",function:{name,description,parameters}}]`。
- 所有新字段为零值时，序列化与现状逐字节兼容。

### internal/agent 执行器接口（新文件 executor.go）

```go
type ToolExecStatus string
const (
    ToolSucceeded ToolExecStatus = "succeeded"
    ToolFailed    ToolExecStatus = "failed"    // 工具报错（含非零退出的语义化判定）
    ToolDenied    ToolExecStatus = "denied"    // 权限拒绝/审批拒绝/过期
    ToolTimeout   ToolExecStatus = "timeout"
)

type DiffSummary struct {
    Additions int    `json:"additions"`
    Removals  int    `json:"removals"`
    Text      string `json:"text"` // BuildDiff 截断后的 diff 文本
}

type ToolOutcome struct {
    CallID      string         `json:"call_id"`
    ToolName    string         `json:"tool_name"`
    Content     string         `json:"content"` // 回填对话的结果文本（已截断）
    IsError     bool           `json:"is_error"`
    Status      ToolExecStatus `json:"status"`
    Elapsed     time.Duration  `json:"elapsed"`
    OutputBytes int            `json:"output_bytes"`
    Diff        *DiffSummary   `json:"diff,omitempty"` // write/edit 类
}

type RunExecutor interface {
    // Execute 阻塞执行一次模型工具调用；权限询问在内部等待，ctx 取消须终止等待与隔离进程。
    // 工具自身的失败、拒绝和超时写入 ToolOutcome，不作为 Go error 返回。
    // 实现方负责写入成对的 sessionlog 工具调用/结果记录。
    Execute(ctx context.Context, call llm.ToolUse) (ToolOutcome, error)
}

type ExecutorFactory interface {
    // ForRun 每次运行调用一次，绑定可信运行归属和权限边界；候选区按首次调用惰性创建。
    ForRun(request ExecutionRequest) (RunExecutor, error)
}
```

`RunnerOptions` 增加 `ExecutorFactory ExecutorFactory` 与 `Budget ResourceBounds`；`ExecutorFactory` 为 nil 时保持现状（检测到工具调用 → `RunAwaitingTools` 终态），旧测试与 M02 语义不破坏。

### internal/agent 资源边界（新文件 budget.go）

```go
type ResourceBounds struct {
    MaxToolRounds    int           `json:"max_tool_rounds"`    // 默认 40 个模型工具回合
    MaxTotalDuration time.Duration `json:"max_total_duration"` // 默认 15m
}
const MaxCommandTimeout = 600 * time.Second // 命令单次上限（源端语义）
```

一个模型响应中含一个或多个工具调用时，`MaxToolRounds` 只增加一次；同一响应内的工具调用串行执行。预算从运行开始计时，空/非法 `ExecutionRequest.ResourceBounds` 回退默认值。

### internal/agent 新事件与终态（events.go）

```go
EventToolExecStart    EventKind = "tool_exec_start"    // {call_id, tool_name, seq}
EventToolExecResult   EventKind = "tool_exec_result"   // ToolOutcome 全量
EventAwaitingApproval EventKind = "awaiting_approval"  // {call_id, tool_name, approval_id, reason}
EventBudgetExhausted  EventKind = "budget_exhausted"   // {reason}
RunBudgetExhausted    RunStatus = "budget_exhausted"
```

终态集合变为 `completed | cancelled | failed | budget_exhausted`；`awaiting_tools` 不再作为终态发出（枚举保留，兼容读旧日志）。

### internal/tools 移植面

保留：`Tool` 接口、`Registry`（`Register/Get/ListTools/GetAllSchemas`）、`ToolResult`、`ToolCategory`、`IsConcurrencySafe`、`SkipDirs`、`MaxOutputChars=50000`、六个描述常量、`ReadFileTool`、`WriteFileTool`、`EditFileTool`、`GlobTool`、`GrepTool`、`FileStateCache`、`BuildDiff/DiffResult`、`IsSafeCommand`、`intArg` 等包级辅助。
移除：`MCPTool`、`DeferrableTool`、`McpLoadingMode`、`ToolSearch`/`mcp_call`/`ask_user`/`exit_plan_mode`/`media_input`/`enter/exit_worktree`/`synthetic_output` 相关文件与字段、`ToolResult.ContentBlocks`。
行为适配（仅两处）：`WriteFileTool`/`EditFileTool` 删除 `FileHistory` 字段（filehistory 归 M05）；`WriteFileTool` 对已存在文件计算 diff（先读旧内容 → `BuildDiff`），新文件无 diff。

### internal/execution 沙箱工具执行器（新文件 tool_executor.go）

```go
type ToolExecutorDeps struct {
    Sandbox    sandbox.SandboxManager
    Gate       PermissionGate                  // 复用现有接口；会话侧传 Service.AuthorizeOperation 适配器
    Approvals  permission.ApprovalRepository   // 审批状态轮询（store 实现）
    HelperPath string                          // agentworker 二进制（挂为客内 /workspace/runtime/agentworker）
    SessionRoot string                         // 会话日志根（写 EventToolCall/ToolResult）
    Now        func() time.Time
    PollEvery  time.Duration                   // 默认 500ms
}

func NewToolExecutorFactory(deps ToolExecutorDeps) agent.ExecutorFactory
// ForRun：解析 request.PermissionBounds → permission.Authority（受信来源，
// startRun 已用 BuildAuthority 覆写）；返回绑定该运行的 runExecutor。
```

绑定后的 runExecutor 保留以下运行级上下文：运行 ID、会话/目标归属、正式工程授权根、候选根、运行目录和正式工程基线摘要。路径映射规则固定为：

- `read_file`、`glob`、`grep`：相对路径映射到正式工程只读根。
- `write_file`、`edit_file`：相对路径映射到候选根。
- `command`：在沙箱中同时挂载正式工程只读根和候选根，命令可读取正式工程并只能写候选区。

`runExecutor.Execute` 必须接受 `context.Context` 取消；不得向调用方暴露宿主绝对路径，也不得允许调用参数改变正式工程挂载为只读或扩大授权根。

runExecutor.Execute 流程见「模块设计」。

### 客内 helper 请求/响应（cmd/agentworker 新入口 `--stable-tool-exec`）

```go
// stdin 单行 JSON
type HelperRequest struct {
    Tool      string         `json:"tool"`      // read_file|write_file|edit_file|glob|grep
    Args      map[string]any `json:"args"`
    Workspace string         `json:"workspace"` // 当前工具可访问的客内根
}
// stdout 单行 JSON
type HelperResponse struct {
    Output    string `json:"output"`
    IsError   bool   `json:"is_error"`
    Additions int    `json:"additions,omitempty"`
    Removals  int    `json:"removals,omitempty"`
    DiffText  string `json:"diff_text,omitempty"`
}
```

helper 进程内实例化 `internal/tools` 的 Registry（五个文件工具 + 共享 `FileStateCache`，不注册命令工具），chdir 到当前挂载的工具根后 `Execute`。读取工具的 helper 工作根为正式工程只读挂载；写入/编辑工具的 helper 工作根为候选挂载。执行器不能通过模型参数选择或重映射挂载根。edit 的 diff 由 `BuildDiff` 计算，write 对已存在文件先读旧内容再计算。

### 会话侧候选生命周期

复用 M03 状态机：`prepared → running → (ready | blocked)`；run 结束后由 conversation 定稿（见模块设计）。候选记录沿用 `store.CandidateRecord`（`ActionID` 为空、`SessionID` 归属），`verifyCandidateSession` 对无 goal 候选按会话归属放行（现状已支持）。

## 模块设计

### internal/tools（移植模块）
**职责：** 工具行为本体——文件读写、精确编辑、目录枚举、内容搜索、diff 计算、输出格式约定；只做文件系统与文本处理。
**对外接口：** `Registry`、`Tool`、六个工具构造、`BuildDiff`、`IsSafeCommand`、`MaxOutputChars`。
**依赖：** 仅 Go stdlib。
**移植原则：** 行为与源端逐字对齐（输出文案、错误约定、mtime 排序），便于 M11 源端对照；`GlobTool` 描述文案修正为「按修改时间倒序」以与实现一致。

### internal/agent（执行循环）
**职责：** 多轮循环、消息累积、事件发布、预算强制、取消与重试（重试语义保持：仅首轮未发出任何内容时重试）。
**改造（runner.go execute）：**
1. 循环体：`provider.Stream(ctx, Request{Model, Messages, Tools})` → 消费流事件并发布（现状逻辑）→ 流结束后：
   - 无工具调用 → 组装 assistant 消息（text/thinking）追加进 `messages` → 终态 `RunCompleted`；
   - 有工具调用 → 组装 assistant 消息（含 `ToolUses`）追加；预算检查（轮数 +1、总时长）；逐个**串行**执行工具调用；
   - 每次调用：发布 `tool_exec_start` → `executor.Execute` → 发布 `tool_exec_result`（payload 为 `ToolOutcome`）→ 组装 user 消息（`ToolResults`）追加；
   - 执行器错误（非工具结果错误）→ 终态 `RunFailed`；预算超限 → 发布 `budget_exhausted` 事件 → 终态 `RunBudgetExhausted`（事件与状态都说明原因）；
   - ctx 取消 → 终态 `RunCancelled`（沙箱进程组由执行器随 ctx 清理）。
2. `Tools` 来自 `Registry.GetAllSchemas()` 的稳定字典序（由 factory 提供，runner 存 `ToolSchema` 列表于 `RunnerOptions.ToolSchemas` 或由 factory 暴露）。
**依赖：** `llm.Provider`、`ExecutorFactory`（可选）。
**接口不变：** `Start/Cancel` 签名不变。

### internal/execution（受控执行）
**职责：** 单次工具调用的权限评估、审批等待、路径映射与校验、沙箱执行、输出规整（截断/脱敏/diff 透传）、候选区惰性创建、工具调用持久记录。
**runExecutor.Execute 流程：**
1. 工具名 → `permission.Operation` 映射：`read_file`/`glob`/`grep` → `OpRead`（`Target` = 宿主正式工程路径）；`write_file`/`edit_file` → `OpWrite`（`Target` = 宿主候选区路径）；`command` → `OpCommand`（`Parameters` = `{command, timeout}` JSON；`Name` = "Command"）。未知工具直接返回 failed 结果（`Error: unknown tool`），不打断循环。
2. 路径处理：模型传相对路径 → `candidate.CleanRelative` 校验（拒绝绝对路径与 `..`）→ 读/搜/列映射授权正式工程只读视图，写/编辑映射宿主候选区绝对路径。命令工具使用沙箱内按 M03 profile 挂载的正式工程只读视图与候选区。
3. `Gate.Authorize(authority, operation)`：
   - `allow` → 继续；`deny` → 返回 denied 结果（含原因）；
   - `ask` → 发布 `awaiting_approval`（经 runner 事件通道；ApprovalID 由 `AuthorizeOperation` 已推送的 `approval_pending` 对应）→ 每 500ms 轮询 `Approvals.GetApprovalForOperation` 直到状态离开 pending（allowed_once/saved/denied/cancelled/expired）或 ctx 结束 → 重新 `Authorize`（allowed_once 在此被消费）→ 循环至 allow/deny。审批 TTL 过期按 denied 处理。
4. 首次任一工具调用前：`candidate.CreateCandidate(authority 派生候选 ID, AllowedRoot, parent)` + 运行目录创建 + `SaveCandidate(prepared)` → `TransitionCandidate(running)`；创建失败（含同设备校验失败）→ denied 结果并说明。
5. 执行：
   - **文件工具**：构造 `SandboxProfile{ProjectRoot: authority.AllowedRoot, CandidateRoot, RunRoot: <run 目录>, Timeout: 120s, OutputLimit: 1MiB}`，`ReadOnlyFiles` 追加 helper 挂载；`RunIsolated(ctx, profile, [helper, "--stable-tool-exec"], HelperRequest JSON)`；解析 `HelperResponse`；
   - **命令工具**：`SandboxProfile.Timeout = min(args.timeout, 600s)`；`RunIsolated(ctx, profile, ["bash","-c",command])`；stdout+stderr 合并；保留源端退出码语义（grep/diff/find/test 等阈值内非零退出不算 error，输出带 hint）；
   - 沙箱返回 `ErrUnavailable` → denied 结果（明确「隔离不可用」原因），**无宿主回退**。
6. 输出规整：`Output` 超过 50000 字符截断并注明；命令输出按源端 `"$ cmd\n<output>\nExit code N"` 格式；扫描并脱敏 ProviderCredential 标记（沿用 M03 `redactProviderCredential` 思路）。
7. 写 sessionlog：`EventToolCall{CallID, Name, Input(脱敏后)}` 在执行前、`EventToolResult{CallID, Result(截断预览), Error}` 在完成后（`sessionlog.Append` 按文件互斥锁，安全）。
8. 返回 `ToolOutcome`（含 `Elapsed`、`OutputBytes`、`Diff`）。

**依赖：** `sandbox`、`permission`、`candidate`、`sessionlog`、`agent`（接口）。

### cmd/agentworker（客内 helper 入口）
**职责：** `--stable-tool-exec`：stdin 读 `HelperRequest` → 实例化移植版 Registry（仅五个文件工具）→ chdir Workspace → Execute → stdout 写 `HelperResponse`。进程结束即退出；不访问网络、不持有凭据。
**复用：** agentworker 二进制已被沙箱挂为 `/workspace/runtime/agentworker`（M03 网络代理同款机制），无需新增挂载来源。

### internal/conversation（编排）
**职责增量：**
- `startRun`：system prompt 更新——说明工作区为正式工程的候选副本、路径一律相对工作区根、写入只落在候选区、工具结果即执行结果（替换现有「工具调用会由后续受控执行流程处理」占位）；跨运行历史重建（`sessionConversationMessages`）扩展：纳入上一运行的 `EventToolCall/EventToolResult` 投影（与现有 20 条消息上限一致地截断）。
- 运行结束定稿 `finalizeRunCandidate`：`consumeRun` 收到终态后——候选未创建则跳过；重算候选 manifest 与基线 digest 比对：一致 → 删除候选目录（纯调查运行零残留）；有变更 → `FreezeCandidate` + `SaveCandidateRecord(ready)` + `TransitionCandidate`；随后既有 `reviewCandidate`/`acceptReviewedCandidate` 全程复用。
- `Deps` 增加 `ExecutorFactory`、`ToolSchemas` 的装配（supervise/chatserve 注入）。

### internal/sessionlog
**职责增量：** 终态枚举两处（log.go 校验点）增加 `budget_exhausted`；`EventToolCall/EventToolResult` 配对校验复用。

### internal/tui
**职责增量：** `transcript.go` 标签 map 增加 `tool_exec_start`（"工具执行"）、`tool_exec_result`（"工具结果"）、`awaiting_approval`（"等待授权"）、`budget_exhausted`（"预算耗尽"）；`model.go` 更新运行状态文案（`budget_exhausted` 显示停止原因；`awaiting_tools` 文案随语义退役）。工具调用流事件（tool_call_start 等）的既有渲染不变。

### 测试基建（F8）
- `internal/agent.FakeExecutor`：脚本化 `ToolOutcome` 序列的导出实现，供 runner 单测、conversation 集成测试使用；配套 fake `llm.Provider`（扩展现有 runner_test 模式）产生含工具调用的流。
- `internal/execution` 单测：fake `sandbox.SandboxManager`（接口已存在，可 mock）+ fake `PermissionGate` + 临时目录候选区，覆盖权限矩阵、审批等待、路径逃逸、截断、ErrUnavailable。
- e2e：`tests/e2e/m04_tools.sh`（真实 bwrap：调查 → 编辑 → 命令 → 候选 → 接收）+ `make m04-e2e` Go 场景套件（真实沙箱正例 + 受限环境拒绝分支 + 预算注入）。
- S01 案例运行器重写：`tests/cases` 按「会话绑定目标（goalrun + SourceSessionID）→ 目标工作项运行使用工具 → 候选 → 接收 → 断言」重排，复用 `tests/e2e/lib.sh` 资源隔离。

## 模块交互（调用链）

```
TUI run_start(ExecutionRequest)
  → conversation.startRun：BuildAuthority（覆写 PermissionBounds）→ system prompt（正式工程只读/候选区写入）+ 历史重建
    → sessionlog EventRunStarted → agent.StreamingRunner.Start
  → runner 循环：
      llm.Provider.Stream(Request{Messages, Tools})
      → 无工具调用 → terminal(completed)
      → 有工具调用 → 预算检查 → ExecutorFactory.ForRun（惰性一次）
          → runExecutor.Execute(call)：
              permission gate → ask? → awaiting_approval 事件 + 审批轮询
              → 首次：candidate.CreateCandidate + Save(prepared)→running
              → sandbox.RunIsolated（helper --stable-tool-exec | bash -c）
              → sessionlog EventToolCall/EventToolResult
          ← ToolOutcome
      → 发布 tool_exec_start/tool_exec_result → 回填 ToolResults 消息 → 下一轮
      → 轮数/时长超限 → budget_exhausted 事件 + terminal(budget_exhausted)
  → conversation.consumeRun（持久化 run_event、广播 TUI）
  → terminal → finalizeRunCandidate（无变更：清理；有变更：冻结+登记 ready）
  → 既有 review/accept（TUI /review，受信接收，ExchangeProjectDir）

goal 路径：EvaluateGoal → GoalSocketClient.RunGoal → 同一 startRun/runner/executor
（authority 来自 store 目标记录；候选目录 goal-<gid>-<wid>）；M03 Coordinator 桥接路径不变。
```

## 文件组织

```
internal/tools/                  移植（适配后）
├── tool.go                      Tool 接口、Registry、ToolResult、IsConcurrencySafe（删 MCP/deferred）
├── descriptions.go              六类工具描述常量
├── read_file.go / glob.go / grep.go        原样移植
├── write_file.go / edit_file.go 移植（去 FileHistory；write 加 diff）
├── file_state_cache.go          原样移植
├── diff.go                      原样移植（BuildDiff）
├── safe_command.go              原样移植（IsSafeCommand）
└── tools_test.go                移植源端测试 + diff 断言
internal/llm/
├── provider.go                  Message/Request/ToolSchema/ToolUse/ToolResultPart 扩展
├── anthropic.go / openai.go / compatible.go  工具块序列化与 tools 参数
internal/agent/
├── runner.go                    多轮工具循环改造
├── executor.go                  RunExecutor/ExecutorFactory/ToolOutcome/FakeExecutor
├── budget.go                    ResourceBounds 解析与默认值
└── events.go                    新 EventKind/RunStatus
internal/execution/
└── tool_executor.go             沙箱工具执行器 + factory + 审批等待 + 路径映射 + 记录
cmd/agentworker/main.go          --stable-tool-exec 客内入口
internal/conversation/
├── run.go                       system prompt、历史重建扩展、finalizeRunCandidate
└── service.go                   Deps 装配（ExecutorFactory/ToolSchemas）
internal/sessionlog/log.go       终态枚举 + budget_exhausted（两处）
internal/tui/
├── transcript.go                新事件标签
└── model.go                     状态文案
tests/e2e/m04_tools.sh           真实沙箱 e2e 场景
tests/cases/                     S01 案例运行器重写
Makefile                         m04-e2e 目标
```

## 技术决策

| 决策点 | 选择 | 理由 |
| --- | --- | --- |
| 工具执行位置 | 全部工具调用（含只读文件工具）经 bwrap `RunIsolated`，文件工具走客内 helper | spec F4/AC4 要求统一隔离边界与 sentinel 语义；文件工具与命令共享同一 mount 布局；避免「隔离不可用时只挡命令不挡读」的混合语义 |
| 移植范围 | tools 包 9 个文件移植，去 filehistory/mewcode-sandbox 两处依赖 | 用户选定方案 C；filehistory 归 M05；Stable 的 `SandboxManager` 是进程级隔离，强于 mewcode 的 Wrap 式沙箱 |
| bash.go 处理 | 不移植；命令工具在 Stable 侧按 Tool 接口实现，保留退出码语义与 `IsSafeCommand` | mewcode 的 Sandbox.Wrap 是命令包装式沙箱，与 Stable RunIsolated 模型不匹配 |
| 循环位置 | 扩展 `StreamingRunner.execute`，`ExecutorFactory` 注入，nil 保持旧行为 | M00 统一执行方向；`Start/Cancel` 接口不变；M02 既有测试不破坏 |
| llm 层 | 三 provider 同步扩展，新字段零值时序列化不变 | 现有请求/测试逐字节兼容 |
| 候选区创建 | 惰性：首次可能产生写入的工具调用前创建 | 纯对话与只读调查运行不产生候选目录；写入、编辑或可能改变候选的命令执行前创建候选区 |
| 候选粒度 | 每次运行独立候选目录 | 对齐 M00/M03 候选状态机与会话归属校验；跨运行工作区延续语义留给 M05 |
| 审批等待 | 执行器内 500ms 轮询 `GetApprovalForOperation`，重新 `Authorize` 消费 allowed_once | 复用 M03 两段式审批与 TTL；不新增通知通道；ctx 取消即时生效；session 运行无需 goal 式 wake |
| 一轮内工具调用 | 串行执行 | 候选区一致性简单、事件序号与预算语义清晰；并发批优化（IsSafeCommand）留后续 |
| 记录 | 双层：run 事件（实时 TUI）+ sessionlog `EventToolCall/EventToolResult`（持久/回放/历史重建） | 两类事件与配对校验已存在；sessionlog 按文件互斥锁支持并发追加 |
| diff | `BuildDiff`（200 行截断）+ 增删行数 | AC8 按「增删行数与变更内容一致」核对；与候选预览 `TextDiff` 格式不强制逐字节相同 |
| 输出上限 | `MaxOutputChars=50000` 截断 + 注明，不做磁盘溢写 | N2；溢写语义与 M05 会话/上下文耦合，本轮不做 |
| 预算 | 默认 40 轮 / 15 分钟，`ResourceBounds` 可配，新终态 `budget_exhausted` | V06A「超预算或超时自动停止并说明原因」的最小实现；事件与状态双重说明 |
| Glob 排序 | 保持源端实现（mtime 倒序），修正描述文案 | M11 源端对照以行为为准 |
| 路径约定 | 模型只见工具工作区相对路径；读/搜/列固定进入正式工程只读视图，写/编辑固定进入候选区；绝对路径、`..` 和符号链接逃逸拒绝 | 权限层分别绑定正式工程只读路径或候选路径；helper 与执行器双层校验 |

## spec 覆盖对照

| spec 需求 | plan 归属 |
| --- | --- |
| F1 工具循环 | internal/agent runner 多轮循环 + executor 接口 |
| F2a–F2c 读/列/搜 | internal/tools 移植 + helper |
| F2d–F2e 写/编辑 | internal/tools 移植 + 权限 OpWrite + 候选区 |
| F2f 受控命令 | internal/execution 命令工具 + RunIsolated + 600s 上限 |
| F2g diff 摘要 | BuildDiff + HelperResponse.DiffText + ToolOutcome.Diff |
| F3 权限边界 | permission gate + CleanRelative 双层校验 + 询问表复用 |
| F4 隔离执行 | RunIsolated 全量工具 + ErrUnavailable 拒绝 + 进程组清理 |
| F5 记录追溯 | run 事件 + sessionlog EventToolCall/ToolResult + 脱敏 |
| F6 预算 | ResourceBounds + budget_exhausted 终态 |
| F7 两类任务一致 | goal 与 session 同走 startRun/runner/executor |
| F8 测试基建 | FakeExecutor + fake sandbox/gate + e2e 场景 |
| F9 S01 运行器 | tests/cases 重写 |
