# M04 基础工具 Tasks

> 状态：已批准（2026-10-04）。依据：已批准的 [spec.md](spec.md) 与已批准的 [plan.md](plan.md)。路径均相对仓库根。模块前缀 `stable/internal/...`。

## 文件清单

| 操作 | 文件 | 职责 |
| --- | --- | --- |
| 新建 | `internal/tools/`（tool.go、descriptions.go、read_file.go、glob.go、grep.go、write_file.go、edit_file.go、file_state_cache.go、diff.go、safe_command.go、tools_test.go） | 工具行为、schema、路径处理、文件读写与 diff |
| 修改 | `internal/llm/provider.go` | ToolSchema、ToolUse、ToolResultPart 与 Request/Message 扩展 |
| 修改 | `internal/llm/anthropic.go`、`openai.go`、`compatible.go` | 三类 provider 的工具协议序列化 |
| 新建 | `internal/agent/executor.go`、`budget.go` | 执行器契约、结果类型、FakeExecutor、预算 |
| 修改 | `internal/agent/events.go`、`runner.go` | 工具事件、预算终态和多轮循环 |
| 修改 | `internal/sessionlog/log.go` | budget_exhausted 终态与工具记录兼容 |
| 新建 | `internal/execution/toolhelper.go`、`tool_executor.go` | helper 协议、权限/授权/沙箱执行器与记录 |
| 修改 | `cmd/agentworker/main.go` | `--stable-tool-exec` 客内入口 |
| 修改 | `internal/conversation/run.go`、`service.go` | 工具上下文、历史投影、候选定稿和依赖装配 |
| 修改 | `cmd/stable/supervise.go`、`chatserve.go` | 真实执行器和工具 schema 装配 |
| 修改 | `internal/tui/transcript.go`、`model.go` | 新事件标签与状态文案 |
| 新建 | `tests/e2e/m04_tools.sh` | 普通任务与目标工作项完整流程 |
| 修改 | `tests/cases/`（运行器与 S01 案例） | 会话绑定目标与候选接收流程 |
| 修改 | `Makefile` | `m04-e2e` 验证入口 |

## 外部依赖与资源约束

1. M03 残留 1 已于 2026-10-04 在提交 `1d64bc0` 解决，写入和命令工具接入无需等待额外门槛。
2. T1–T19 主要为代码和定向测试；T20–T23 涉及真实沙箱、e2e、案例和全量回归，按顺序错峰执行。
3. 启动 T20–T23 前检查 `free -h`、`vmstat 1 5` 和 `/proc/pressure/memory`。真实沙箱、浏览器/服务、全量测试不得在同一批次并发启动。
4. 修改同一文件的任务串行执行；没有文件、接口或环境交集的任务可并行。

---

## T1: 工具核心契约与注册表

**文件：** `internal/tools/tool.go`、`descriptions.go`

**依赖：** 无

**步骤：**
1. 移植 `Tool`、`Registry`、`ToolResult`、`ToolCategory`、输出上限和并发安全标记。
2. 删除 MCP、deferred、tool search、媒体输入和工作树相关字段与方法。
3. 保留五个文件工具和命令工具所需的 schema 描述常量；保证 schema 名称稳定。
4. 为注册、查询、列出工具和生成 schema 增加定向测试。

**验证：** `go test ./internal/tools/...` 通过，注册表输出按名称稳定排序。

## T2: 只读文件工具

**文件：** `internal/tools/read_file.go`、`internal/tools/read_file` 相关测试

**依赖：** T1

**步骤：**
1. 移植按相对路径读取文本的实现。
2. 保持 1-based 行号、起始行和行数分段语义。
3. 覆盖文件不存在、目录、空文件和非法路径错误。
4. 断言工具本体不执行写入。

**验证：** `go test ./internal/tools/... -run Read` 通过，输出行号和分段边界符合 spec。

## T3: 目录匹配与内容搜索

**文件：** `internal/tools/glob.go`、`grep.go`、对应测试

**依赖：** T1

**步骤：**
1. 移植 glob，按修改时间倒序输出并跳过 `SkipDirs`。
2. 移植 grep，支持正则和 include 过滤，输出 `路径:行号:内容`。
3. 覆盖无匹配、非法正则、二进制/不可读文件和越界路径。
4. 修正文案，使 schema 与实际 mtime 排序行为一致。

**验证：** `go test ./internal/tools/... -run 'Glob|Grep'` 通过。

## T4: 文件状态缓存与 diff

**文件：** `internal/tools/file_state_cache.go`、`diff.go`、对应测试

**依赖：** T1

**步骤：**
1. 移植读后编辑状态缓存，记录已读取文件内容或摘要。
2. 移植 `BuildDiff` 和 `DiffResult`，保留增删行数和 diff 文本上限。
3. 覆盖新增、删除、替换、多段变更和 diff 截断。
4. 确认结果不包含候选外绝对路径或敏感环境值。

**验证：** `go test ./internal/tools/... -run 'Diff|State'` 通过。

## T5: 写入与精确编辑工具

**文件：** `internal/tools/write_file.go`、`edit_file.go`、`tools_test.go`

**依赖：** T2、T4

**步骤：**
1. 移除 `FileHistory` 依赖，保留读后改纪律。
2. 写入新文件时返回成功结果；覆盖已有文件前读取旧内容并附 diff。
3. 编辑要求目标已读取，且 `old_string` 唯一；失败时返回明确错误。
4. 增加新建、覆盖、未读先改、非唯一替换、diff 增删统计测试。

**验证：** `go test ./internal/tools/...` 通过，写入和编辑单元测试覆盖所有失败分支。

## T6: LLM 内部工具消息模型

**文件：** `internal/llm/provider.go`

**依赖：** 无

**步骤：**
1. 增加 `ToolSchema`、`ToolUse`、`ToolResultPart`。
2. 扩展 `Message` 的工具调用和工具结果字段。
3. 扩展 `Request` 的工具 schema 字段。
4. 保持新字段为零值时的既有结构和序列化兼容。

**验证：** `go test ./internal/llm/...` 通过，类型和零值 golden 测试通过。

## T7: Anthropic 工具协议适配

**文件：** `internal/llm/anthropic.go`、Anthropic 测试

**依赖：** T6

**步骤：**
1. 序列化 assistant 文本与 `tool_use` content block。
2. 序列化 user 的 `tool_result` block 和 `is_error`。
3. 序列化顶层 `tools` schema。
4. 保留未知 role 错误和无工具请求兼容。

**验证：** `go test ./internal/llm/... -run Anthropic` 通过，工具请求 JSON 与协议样例一致。

## T8: OpenAI 与兼容 provider 工具协议适配

**文件：** `internal/llm/openai.go`、`compatible.go`、对应测试

**依赖：** T6

**步骤：**
1. 序列化 assistant `tool_calls`。
2. 将每条工具结果序列化为独立 `tool` 消息并保留调用 ID。
3. 序列化 function 类型工具 schema。
4. 覆盖多工具调用、错误结果和零工具字段兼容。

**验证：** `go test ./internal/llm/... -run 'OpenAI|Compatible'` 通过。

## T9: Agent 执行器契约与 FakeExecutor

**文件：** `internal/agent/executor.go`

**依赖：** T6

**步骤：**
1. 定义工具执行状态、diff、`ToolOutcome`、`RunExecutor` 和 `ExecutorFactory`。
2. 让 `Execute` 接收 `llm.ToolUse`，区分工具级结果和执行器级 error。
3. 实现按脚本顺序返回结果的 `FakeExecutor`。
4. 覆盖脚本耗尽、注入延迟、工具结果错误和上下文取消。

**验证：** `go test ./internal/agent/... -run Executor` 通过。

## T10: Agent 预算与事件类型

**文件：** `internal/agent/budget.go`、`events.go`

**依赖：** 无

**步骤：**
1. 增加 `ResourceBounds`、默认值和 600 秒命令上限。
2. 解析空、非法、负数和部分配置；非法值回退默认值。
3. 增加工具开始、工具结果、等待授权和预算耗尽事件。
4. 增加 `RunBudgetExhausted` 并保持旧 `awaiting_tools` 记录可读取。

**验证：** `go test ./internal/agent/... -run 'Budget|Event'` 通过。

## T11: sessionlog 终态兼容

**文件：** `internal/sessionlog/log.go`、sessionlog 测试

**依赖：** 无

**步骤：**
1. 在终态校验和回放校验中加入 `budget_exhausted`。
2. 保留旧 `awaiting_tools` 记录的读取兼容。
3. 增加 budget 终态追加、回放和非法状态拒绝测试。

**验证：** `go test ./internal/sessionlog/...` 退出 0。

## T12: Agent 多轮工具循环

**文件：** `internal/agent/runner.go`、runner 测试

**依赖：** T7、T8、T9、T10、T11

**步骤：**
1. 将工具 schema 放入 provider request；保持现有首轮重试和流事件发布逻辑。
2. 无工具调用时追加 assistant 消息并完成；有工具调用时追加 assistant `ToolUses`。
3. 按模型工具回合检查回合数和总时长预算。
4. 通过 factory 初始化一次执行器，按顺序发布开始事件、执行、结果事件并回填 `ToolResults`。
5. 保留 nil factory 的旧 `awaiting_tools` 兼容行为。
6. 覆盖两轮收敛、多个调用串行、结果回填、事件序号、取消和预算耗尽。

**验证：** `go test ./internal/agent/...` 退出 0。

## T13: Helper 请求响应协议

**文件：** `internal/execution/toolhelper.go`

**依赖：** T5

**步骤：**
1. 定义单行 JSON 的 `HelperRequest` 和 `HelperResponse`。
2. 明确 workspace 由受控执行器选择，不能由模型改变挂载边界。
3. 覆盖成功、工具错误、diff 和响应截断字段的 JSON round-trip。

**验证：** `go test ./internal/execution/... -run Helper` 通过。

## T14: Agentworker 文件工具入口

**文件：** `cmd/agentworker/main.go`、入口测试

**依赖：** T1、T2、T3、T5、T13

**步骤：**
1. 增加 `--stable-tool-exec` 分支，读取单行请求并返回单行响应。
2. 校验 workspace 为受控客内目录，初始化五个文件工具和共享状态缓存。
3. 让读工具可在正式工程只读挂载运行，让写/编辑只在候选挂载运行；入口不提供命令工具。
4. 将错误和 panic 转为 `IsError` 响应并保持进程退出码为 0。
5. 覆盖五工具往返、路径逃逸、写入 diff 和错误响应。

**验证：** `go build ./cmd/agentworker/... && go test ./internal/execution/... -run Agentworker` 通过。

## T15: 受控执行器路径与权限映射

**文件：** `internal/execution/tool_executor.go`（第一阶段）

**依赖：** T9、T13、T14

**步骤：**
1. 定义 `ToolExecutorDeps` 和 `NewToolExecutorFactory`。
2. 解析可信运行请求并绑定运行、会话/目标、正式工程根、候选根和运行目录。
3. 将读/搜/列映射为正式工程只读路径，将写/编辑映射为候选路径，将命令映射为双挂载 profile。
4. 使用 `candidate.CleanRelative` 拒绝绝对路径、`..` 和符号链接逃逸。
5. 对未知工具返回工具级失败结果，不扩大权限。

**验证：** `go test ./internal/execution/... -run 'Path|Operation|Factory'` 通过。

## T16: 受控执行器授权与候选生命周期

**文件：** `internal/execution/tool_executor.go`（第二阶段）

**依赖：** T15

**步骤：**
1. 接入 M03 Gate 的 allow/ask/deny 结果。
2. ask 时发布等待授权事件，按配置轮询审批状态，取消和过期返回拒绝。
3. 重新授权并消费一次性批准，参数或范围变化不得复用旧决定。
4. 首次可能产生变更的写、编辑或命令调用前惰性创建候选并转为 running；纯对话和只读调查不创建候选。
5. 覆盖候选创建失败和状态迁移失败。

**验证：** fake Gate、fake ApprovalRepository 和临时目录测试通过；`go test ./internal/execution/... -run 'Approval|Candidate'` 退出 0。

## T17: 受控执行器隔离执行、结果规整与记录

**文件：** `internal/execution/tool_executor.go`（第三阶段）

**依赖：** T16

**步骤：**
1. 文件工具通过隔离 helper 执行，正式工程只读、候选区可写。
2. 命令通过隔离 shell 执行，挂载正式工程只读视图和候选区可写视图，限制单次超时。
3. 处理 `ErrUnavailable`、取消、超时、进程组清理和非零退出语义。
4. 截断超过 50000 字符的输出，脱敏模型凭据和宿主秘密，组装 diff 与 `ToolOutcome`。
5. 执行前后成对写入脱敏、截断的 `EventToolCall/EventToolResult`。
6. 覆盖 fake sandbox、截断、隔离不可用、超时清理和记录配对。

**验证：** `go test ./internal/execution/...` 退出 0；本任务只运行 fake sandbox，不启动真实重型 e2e。

## T18: Conversation 工具上下文与历史投影

**文件：** `internal/conversation/run.go`、相关测试

**依赖：** T12、T17

**步骤：**
1. 更新 system prompt，说明读/搜/列使用正式工程只读视图，写/编辑使用候选区，路径使用相对工作区路径。
2. 将工具 schema 和 ExecutorFactory 传入 runner。
3. 将工具调用和结果投影到跨运行历史，保持既有消息数量和结果截断限制。
4. 覆盖 fake provider + FakeExecutor 的多轮消息重建。

**验证：** `go test ./internal/conversation/... -run 'Prompt|History|Tool'` 通过。

## T19: Conversation 候选定稿与服务装配

**文件：** `internal/conversation/run.go`、`service.go`

**依赖：** T18

**步骤：**
1. 运行终态后检查候选是否存在及 manifest 是否相对正式工程基线发生变化。
2. 无变更时删除候选；有变更时冻结、登记 ready 并保留会话/目标归属。
3. 暴露 ExecutorFactory 和 ToolSchemas 的依赖注入点。
4. 集成测试断言候选登记、纯调查清理和 review 查询可见差异。

**验证：** `go test ./internal/conversation/...` 退出 0。

## T20: 真实进程装配与 TUI 事件渲染

**文件：** `cmd/stable/supervise.go`、`chatserve.go`、`internal/tui/transcript.go`、`model.go`

**依赖：** T19

**步骤：**
1. 装配真实 Linux sandbox、权限适配器、审批仓库、helper 路径和运行目录。
2. 从 Registry 生成六类工具 schema，并确保 schema 与执行分派一致。
3. 增加工具开始、工具结果、等待授权和预算耗尽标签。
4. 更新预算耗尽原因和旧 awaiting_tools 兼容文案。

**验证：** `go build ./... && go vet ./...` 退出 0；TUI 定向测试通过。

## T21: Go 真实沙箱场景套件

**文件：** `tests/e2e/`（Go 场景）、`Makefile`

**依赖：** T20

**步骤：**
1. 使用独立 run root、端口、HOME 和 fixture。
2. 验证正式工程只读、候选可写、越界三连、符号链接逃逸和宿主 sentinel 不可见。
3. 验证隔离不可用拒绝、命令超时进程组清理、预算耗尽和输出截断。
4. 验证候选冻结、diff 与 review 一致、accept 一次生效和 ask 授权后继续。

**验证：** 检查内存压力后运行 `make m04-e2e`，退出 0；真实环境不可创建 namespace 时只记录拒绝分支，不得宣称正例通过。

## T22: 普通任务与目标工作项 shell e2e

**文件：** `tests/e2e/m04_tools.sh`

**依赖：** T21

**步骤：**
1. 普通任务通过 TUI socket 提交调查、修改和命令任务。
2. 观察工具事件流、候选 ready、review 差异和受信 accept。
3. 比较接收前后正式工程 manifest，确认只改变一次。
4. 创建绑定 session 的目标工作项，执行同类工具操作并比较权限结果。
5. 确认目标接收后保持待独立复核。

**验证：** 检查内存压力后运行 `bash tests/e2e/m04_tools.sh`，退出 0。

## T23: S01 案例运行器重写

**文件：** `tests/cases/`（运行器与 S01 案例）

**依赖：** T22

**步骤：**
1. 以 `goalrun` 加载定义并创建目标。
2. 创建会话绑定和目标工作项运行归属。
3. 通过共享工具入口完成 S01 调查和修复，产生候选并生成 review。
4. 通过受信接收入口应用候选，断言正式工程仅改变一次。
5. 断言目标仍等待独立复核，并清理运行资源。

**验证：** 在可创建 Linux namespace 的环境运行 S01 案例，退出码 0。

## T24: 全量回归与独立提交验证

**文件：** 无新增文件；必要时只修复本里程碑引入的问题

**依赖：** T20、T22、T23

**步骤：**
1. 运行 `go build ./...`、`go vet ./...`、`go test ./...`。
2. 逐个运行 `run.sh`、`waiting_restart.sh`、`unsupported.sh`、`criteria_change.sh`、`dependency_change.sh`。
3. 运行 `make m03-e2e`、两个桥接目录的 `python3 -m unittest discover`、`make package` 和 `make test-package`。
4. 从本里程碑各提交建立临时 worktree，逐提交运行 `go build -buildvcs=false ./...`。
5. 保存每项命令的退出码和证据路径，失败项修复后重新执行。

**验证：** 所有命令退出 0；每个独立提交均可编译。

---

## 执行顺序

```text
批次 1（可并行）：T1、T6、T10、T11
批次 2（可并行）：T2、T3、T4（→T1）；T7、T8（→T6）；T9（→T6）
批次 3（串行链）：T5（→T2,T4）
批次 4（串行链）：T12（→T7,T8,T9,T10,T11）
批次 5（串行链）：T13（→T5）→ T14（→T1,T2,T3,T5,T13）
批次 6（串行链）：T15（→T9,T13,T14）→ T16 → T17
批次 7（串行链）：T18（→T12,T17）→ T19
批次 8（可并行但共享装配边界）：T20（→T19）
批次 9（重型，串行）：T21（→T20）
批次 10（重型，串行）：T22（→T21）
批次 11（重型，串行）：T23（→T22）
批次 12（全量回归，串行）：T24（→T20,T22,T23）
```

### 并行边界

- 批次 1、2 的任务没有共享实现文件，可并行；资源紧张时优先保留只读/单测任务，降低并发。
- T5 必须等待 T2 和 T4，因为写入/编辑依赖读取状态和 diff 契约。
- T12 必须等待三类 provider、执行器契约、预算事件和 sessionlog 终态全部定稿。
- T15–T17 都修改 `tool_executor.go`，必须串行，避免覆盖授权、路径和沙箱逻辑。
- T18、T19 都修改 `conversation/run.go`，必须串行；T20 的装配依赖 conversation 依赖契约完成。
- T21–T24 使用真实沙箱、服务、运行目录或全量构建，只能错峰执行，不能与其他重型任务并行。
