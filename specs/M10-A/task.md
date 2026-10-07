# M10-A 本地 Print 与 Provider 选择 Tasks

## 文件清单

| 操作 | 文件 | 职责 |
|---|---|---|
| 修改 | `cmd/stable/main.go` | 注册 `--print` 和 `provider` 命令、更新 usage |
| 新建 | `cmd/stable/print.go` | print 参数/stdin、runtime、临时 session/run stream、输出和清理 |
| 新建 | `cmd/stable/print_test.go` | print 参数、输出、失败、中断和清理验证 |
| 新建 | `cmd/stable/provider.go` | provider list/show/use 命令 |
| 新建 | `cmd/stable/provider_test.go` | provider 查询、切换与 runtime 限制验证 |
| 新建 | `internal/appconfig/provider.go` | provider 来源解析及配置更新 |
| 新建 | `internal/appconfig/provider_test.go` | 来源、字段保留、私有权限和失败完整性验证 |
| 修改 | `internal/conversation/protocol.go` | ephemeral 创建字段和 `session_discard` 协议 |
| 修改 | `internal/conversation/session.go` | ephemeral session 创建接线 |
| 修改 | `internal/conversation/service.go` | `session_discard` 操作分派 |
| 修改 | `internal/conversation/authority.go` | 从 session 元信息生成只读 authority |
| 新建 | `internal/conversation/ephemeral.go` | 临时 session 销毁及服务运行态清理 |
| 新建 | `internal/conversation/ephemeral_test.go` | 生命周期与拒绝条件验证 |
| 修改 | `internal/sessionlog/events.go` | `SessionInfo.Ephemeral` |
| 修改 | `internal/sessionlog/sessions.go` | ephemeral 创建和列表过滤 |
| 新建 | `internal/sessionlog/ephemeral.go` | 验证标记后删除临时 session log |
| 新建 | `internal/sessionlog/ephemeral_test.go` | 列表过滤、删除校验验证 |
| 修改 | `internal/permission/model.go` | `Authority.ReadOnly` |
| 修改 | `internal/permission/policy.go` | 只读 authority 的硬拒绝 |
| 修改 | `internal/permission/policy_test.go` | 只读策略与 scope digest 回归 |
| 修改 | `internal/execution/tool_executor.go` | host 分派前拒绝非只读工具 |
| 新建 | `internal/execution/tool_executor_m10_test.go` | host/普通工具只读拒绝验证 |
| 新建 | `internal/store/ephemeral.go` | 清理 session 关联的审批与决定记录 |
| 新建 | `internal/store/ephemeral_test.go` | 清理范围与持久规则保留验证 |
| 修改 | `README.md` | print/provider 用法与只读边界 |

## T1：Provider 来源与配置更新

**文件：** `internal/appconfig/provider.go`、`internal/appconfig/provider_test.go`

**依赖：** 无

**步骤：**

1. 定义 `ConfigSource`、`ModelSelection` 和 `LoadWithModelSources`，逐字段识别 file/env/default 来源且不返回密钥值。
2. 实现 `UpdateModelSelection`，校验 provider、model 和 base URL；provider 变化时清除旧 `api_key`/`base_url`，同 provider 切换时保留密钥。
3. 只改 JSON 的 model 选择字段并保留其他字段；拒绝不安全的配置路径，使用同目录私有临时文件原子替换。
4. 覆盖缺少文件、无效 provider/base URL、环境变量覆盖、写入失败、旧字段保留和私有权限行为。

**验证：** `go test ./internal/appconfig -count=1` 通过；测试确认环境覆盖来源正确、凭据不出现在选择结果、其他配置字段保留，失败更新不改变原文件。

## T2：Session log ephemeral 元信息

**文件：** `internal/sessionlog/events.go`、`internal/sessionlog/sessions.go`、`internal/sessionlog/ephemeral.go`、`internal/sessionlog/ephemeral_test.go`

**依赖：** 无

**步骤：**

1. 为 `SessionInfo` 增加持久化 `Ephemeral` 字段；普通 `Create` 继续创建非临时 session，新增 `CreateEphemeral`。
2. 更新 `List`，从 session 创建事件读取元信息并过滤临时 session。
3. 实现 `DeleteEphemeral`：验证 ID、session 文件路径和创建元信息；只在 session 文件锁内删除带标记的临时 log。
4. 覆盖普通 session、临时 session、无效 ID、普通 session 删除拒绝和并发 append/delete 边界。

**验证：** `go test ./internal/sessionlog -count=1` 通过；临时 session 不出现在 List，普通 session 可重放且不能被临时删除入口移除。

## T3：Permission 只读 authority

**文件：** `internal/permission/model.go`、`internal/permission/policy.go`、`internal/permission/policy_test.go`

**依赖：** 无

**步骤：**

1. 为 `Authority` 增加 `ReadOnly` 字段。
2. 在 `Policy.Decide` 的精确 allow 匹配前拒绝 `ReadOnly` authority 上所有非 `OpRead` 操作。
3. 将 `ReadOnly` 排除在 scope digest 外，确保既有只读规则继续按原 scope 匹配。
4. 增加 `OpWrite`、`OpCommand`、`OpMCPTool`、`OpNetwork`、`OpLegacy` 即使有精确 allow 也拒绝的用例；确认 `OpRead` 仍按现有规则判断。

**验证：** `go test ./internal/permission -count=1` 通过；非只读操作无法被 exact allow 放行，ReadOnly 标记不改变既有只读 scope digest。

## T4：临时 session 权限记录清理

**文件：** `internal/store/ephemeral.go`、`internal/store/ephemeral_test.go`

**依赖：** 无

**步骤：**

1. 新增按 session ID 删除审批请求与权限决定的 store 方法。
2. 在单个 SQLite transaction 中执行两类记录清理。
3. 不删除 `permission_rules`，不接受空 session ID。
4. 覆盖有记录、无记录、事务失败和持久精确规则保留。

**验证：** `go test ./internal/store -run 'Ephemeral|Permission' -count=1` 通过；目标 session 临时记录删除，其他 session 与全局权限规则仍在。

## T5：创建 ephemeral session 并过滤列表

**文件：** `internal/conversation/protocol.go`、`internal/conversation/session.go`、`internal/conversation/ephemeral_test.go`

**依赖：** T2

**步骤：**

1. `ClientMsg` 增加 `Ephemeral` 字段；保留现有 session_create 的校验与响应格式。
2. `session_create` 将 ephemeral 标记传给 `sessionlog.CreateEphemeral`；普通调用仍创建持久 session。
3. 通过现有 session_list/list 投影确认临时项不会显示。
4. 覆盖普通创建兼容、临时创建标记持久化和列表过滤。

**验证：** `go test ./internal/conversation -run 'SessionCreate|EphemeralList' -count=1` 通过；普通 session 仍可列出，临时 session 不可列出但可由 ID 重放。

## T6：受限 session_discard

**文件：** `internal/conversation/protocol.go`、`internal/conversation/service.go`、`internal/conversation/ephemeral.go`、`internal/conversation/ephemeral_test.go`、`internal/store/ephemeral.go`

**依赖：** T2、T4、T5

**步骤：**

1. 注册 `session_discard`，校验 project root 和 session ID；拒绝删除普通 session。
2. service 检查该 session 没有活动 run，并重读 session log 确认 ephemeral 标记。
3. 依次清理 session 审批/决定记录、ephemeral JSONL 和 session 级 plan/question/notice 等运行态。
4. 任一清理失败时返回明确错误；成功后发送 discard 完成响应。
5. 覆盖活动 run 拒绝、普通 session 拒绝、成功清理和存储失败。

**验证：** `go test ./internal/conversation -run 'SessionDiscard|Ephemeral' -count=1` 通过；成功后 session log 与临时权限记录均不存在，普通 session 数据保持不变。

## T7：只读 authority 派生

**文件：** `internal/conversation/authority.go`、`internal/conversation/authority_test.go`

**依赖：** T2、T3

**步骤：**

1. `BuildAuthority` 读取并校验 session 创建事件元信息。
2. 对 ephemeral session 设置服务端 `Authority.ReadOnly=true`；普通 session 保持 false。
3. 不从客户端 `ExecutionRequest` 或 `PermissionBounds` 信任 ReadOnly 值；service 仍以重建后的 authority 覆盖 client bounds。
4. 增加 ephemeral/普通 session authority 断言。

**验证：** `go test ./internal/conversation -run 'BuildAuthority|EphemeralAuthority' -count=1` 通过；ephemeral 标记能派生只读 authority，客户端值不能扩大权限。

## T8：Executor 只读硬门

**文件：** `internal/execution/tool_executor.go`、`internal/execution/tool_executor_m10_test.go`

**依赖：** T3

**步骤：**

1. 增加与现有 `OpRead` 分类一致的只读工具识别，仅允许 `read_file`、`glob`、`grep`。
2. 在 host 工具分派前检查可信 authority；只读 run 的其他工具调用返回明确拒绝错误，不执行 hook host 动作、MCP、task、write、command 或其他副作用。
3. 保持普通 session 的工具执行流程不变。
4. 覆盖 host 工具、普通工具、MCP 工具拒绝，以及三个只读工具可继续执行。

**验证：** `go test ./internal/execution -run 'ReadOnly' -count=1` 通过；拒绝用例没有创建 candidate、执行 host action 或触发 MCP 调用。

## T9：Provider CLI

**文件：** `cmd/stable/provider.go`、`cmd/stable/provider_test.go`

**依赖：** T1

**步骤：**

1. 实现 `provider list`，展示 OpenAI、Anthropic、openai-compatible、Gemini 的能力说明。
2. 实现 `provider show`，展示有效 provider/model/base URL 和逐字段来源，不展示 key。
3. 实现 `provider use <provider> --model <model> [--base-url URL]`，调用 appconfig 更新接口。
4. 使用 runtime status 检查；runtime 运行时拒绝更新并提示 `stable down`。
5. 输出配置保存结果和仍生效的环境变量覆盖。

**验证：** `go test ./cmd/stable -run 'Provider' -count=1` 通过；list/show 不泄漏密钥，use 更新预期字段且运行中的 runtime 不导致配置变化。

## T10：Print CLI 与 run stream

**文件：** `cmd/stable/print.go`、`cmd/stable/print_test.go`

**依赖：** T1、T5、T6、T7、T8

**步骤：**

1. 实现参数文本与 stdin 至 EOF 输入；加载配置并提前拒绝不支持流式的 Gemini provider。
2. 检查/启动 runtime，使用当前工作目录创建 ephemeral session。
3. 构造 `WorkSession` run 并通过现有 conversation stream 启动；只将文本增量写 stdout。
4. 对非只读工具执行事件发送 `run_cancel`；对审批、提问、计划审批和 provider/run 错误写 stderr 并非零退出。
5. 处理 SIGINT/SIGTERM：取消活动 run、等待终态，再调用 session_discard；所有退出路径都执行清理。
6. 覆盖输出通道、provider 错误、运行失败、只读拒绝、交互事件、中断取消和清理失败。

**验证：** `go test ./cmd/stable -run 'Print' -count=1` 通过；stdout 只有回答文本，错误走 stderr，所有终态均尝试清理临时 session。

## T11：CLI 分派与帮助文档

**文件：** `cmd/stable/main.go`、`README.md`

**依赖：** T9、T10

**步骤：**

1. 在 main 分派 `--print` 与 `provider`，保持无参数 TUI 和现有子命令行为不变。
2. 更新 usage 与 README，记录参数/stdin、输出通道、provider 命令、Gemini 限制和只读边界。
3. 覆盖 help 输出、命令路由和未知 provider 子命令错误。

**验证：** `go test ./cmd/stable -run 'Print|Provider|Usage' -count=1` 通过；`stable help` 和 README 展示的语法一致。

## T12：集成回归与 spec 验收

**文件：** 受影响包的现有测试文件；必要时补充各任务指定测试文件

**依赖：** T1–T11

**步骤：**

1. 运行各任务指定的包级定向测试。
2. 使用 fake provider 串接临时 session 创建、只读 run、文本输出、非只读拒绝、session_discard 和记录清理。
3. 验证 provider use 在 runtime 停止/运行两种状态下的写入结果及环境变量覆盖。
4. 执行项目全量 Go 测试；根据 checklist 记录每项证据并修复失败。

**验证：** `go test ./...` 通过；fake provider 场景覆盖 print 成功/失败与 provider 配置流程，不使用真实 key 或外网。

## 执行顺序

```text
批次 1（最多 4 项；包和文件互不重叠）
T1 ─┐
T2 ─┤
T3 ─┼─────────────────────────────┐
T4 ─┘                             │
                                  ▼
批次 2（接口基础完成后）
T5(T2) ──┬── T6(T4,T5) ──┐
T7(T2,T3) ───────────────┤
T8(T3) ──────────────────┼── T10(T1,T5,T6,T7,T8) ──┐
T9(T1) ──────────────────┘                          │
                                                    ▼
批次 3
T11(T9,T10) ── T12(全部任务汇合)
```

T5/T6/T7 会修改 conversation 包的不同文件；不得同时修改相同文件。T10 依赖只读 authority、executor guard 和 session discard 契约全部定稿。集成与全量 Go 测试安排在所有功能任务汇合后执行。

## 执行资源约束

- 每轮并行最多启动文件与依赖互不冲突的任务；批次 1 的单测按本机 `MemAvailable`、后续 `vmstat` 采样和 memory PSI 调整并发。
- 全量 `go test ./...` 是重型汇合验证，先检查整机与当前 cgroup 的可用内存；不与其他重型操作并发。
- 所有测试使用 fake provider、临时配置和隔离的临时目录；不启动重复 runtime，不连接外部 API。
