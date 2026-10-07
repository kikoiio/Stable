# M10-A 本地 Print 与 Provider 选择 Checklist

> 依据已批准的 `spec.md`、`plan.md` 与 `task.md`。实施时逐项勾选，并将验证命令、结果或失败原因记入对应项。

## T1 Provider 来源与配置更新

- [x] `ConfigSource`、`ModelSelection` 和 `LoadWithModelSources` 返回 provider/model/base URL 的有效值与来源，不暴露凭据。
- [x] `UpdateModelSelection` 校验 provider、model、base URL；provider 改变时清除旧 key/base URL，同 provider 时保留凭据。
- [x] 更新仅触及模型选择字段，保留配置文件其他字段；配置权限为私有。
- [x] 覆盖缺少配置文件的私有创建、无效 JSON/model 与写入目标失败时原内容不变。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T2 Session log ephemeral 元信息

- [x] 普通 session 默认非临时；`CreateEphemeral` 持久化临时标记。
- [x] `List` 过滤临时 session，普通 session 的重放行为保持可用。
- [x] `DeleteEphemeral` 只删除经校验的临时 session 文件。
- [x] 覆盖普通 session 删除拒绝。
- [x] 覆盖并发 append/delete 序列化与空/路径穿越 ID 拒绝。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T3 Permission 只读 authority

- [x] `Authority.ReadOnly` 在只读 authority 上对所有非 `OpRead` 操作实施硬拒绝，优先于精确 allow。
- [x] `OpRead` 继续遵循已有规则；需要交互审批的读操作在 print 中拒绝，不等待 TUI；`ReadOnly` 不改变 scope digest。
- [x] 覆盖 `OpWrite`、`OpCommand`、`OpMCPTool`、`OpNetwork`、`OpLegacy`。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T4 临时 session 权限记录清理

- [x] session 关联的审批请求与权限决定在同一个 SQLite transaction 中清理。
- [x] 空 session ID 被拒绝；全局 `permission_rules` 与其他 session 记录保留。
- [x] 覆盖无匹配记录及通过 SQLite trigger 注入语句失败后的事务回滚。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T5 创建 ephemeral session 并过滤列表

- [x] `session_create` 支持 ephemeral 标记，普通创建协议与行为兼容。
- [x] 临时 session 标记写入 session log，并从 session 列表投影中隐藏。
- [x] 临时 session 可按 ID 重放并通过 fake provider run。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T6 受限 session_discard

- [x] discard 校验 project root、session ID 和 ephemeral 标记；普通 session 与活动 run 均被拒绝。
- [x] 成功时清理 session 权限记录、JSONL transcript、临时 run 目录和 session 级运行态。
- [x] 清理失败返回错误；成功响应在清理完成后发送。
- [x] 验证：受影响包与全量 Go 测试通过；结果：fake provider 临时会话集成测试及 `go test -p 2 ./...`。

## T7 只读 authority 派生

- [x] `BuildAuthority` 从可信 session 元信息为 ephemeral session 设置 `ReadOnly`。
- [x] 普通 session authority 不受影响；客户端 request/bounds 不能自行扩大或覆盖该标记。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T8 Executor 只读硬门

- [x] executor 只允许 `read_file`、`glob`、`grep` 等现有 `OpRead` 工具。
- [x] 非只读 host、普通、MCP、task、write、command 等工具在任何副作用前拒绝。
- [x] 拒绝路径不触发 hooks 或 host action；普通 session 流程不变。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T9 Provider CLI

- [x] `provider list` 展示 OpenAI、Anthropic、openai-compatible、Gemini 及能力说明。
- [x] `provider show` 展示有效 provider/model/base URL 和字段来源，不展示密钥。
- [x] `provider use <provider> --model <model> [--base-url URL]` 更新配置并报告仍生效的环境变量覆盖。
- [x] 覆盖 provider use 的运行时拒绝、停止时持久更新、保留其他配置、环境变量优先及密钥不泄漏（runtime 状态由测试替身提供）。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T10 Print CLI 与 run stream

- [x] `stable --print [文本]` 支持参数输入与 stdin 至 EOF；Gemini 在启动 run 前明确拒绝。
- [x] runtime 未运行时自动启动；从当前工作目录创建临时 session 并复用现有 runner。
- [x] stdout 仅含回答文本；错误、审批/提问/计划审批请求和运行失败写 stderr 并非零退出。
- [x] 只读工具可按既有规则执行；非只读工具导致 run 取消、明确失败且不产生 candidate。
- [x] SIGINT/SIGTERM 会取消 run、等待终态并尝试 discard；成功、失败、中断路径均尝试清理。
- [x] 清理失败可观察；凭据在流式回答、错误和 transcript 中脱敏。
- [x] CLI 与真实 conversation service、fake provider 的 stdin smoke 覆盖 runtime 自动启动分支、纯回答 stdout、凭据脱敏和临时 transcript 清理；runtime 的 supervisor 由测试替身代替。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T11 CLI 分派与帮助文档

- [x] `--print` 与 `provider` 正确分派；无参数 TUI 和既有子命令行为保持兼容。
- [x] usage 和 README 记录输入/stdin、输出通道、provider 命令、Gemini 限制及只读边界。
- [x] 覆盖 provider list/show、print 输入解析和只读工具路由。
- [x] 验证：受影响包与全量 Go 测试通过；结果：`go test -p 2 ./...`。

## T12 集成回归与验收

- [x] 受影响包的定向测试均通过；结果：`go test -p 2 ./internal/appconfig ./internal/sessionlog ./internal/permission ./internal/store ./internal/conversation ./internal/execution ./cmd/stable`。
- [x] fake provider 服务集成覆盖成功 run、当前工作目录绑定与临时 transcript 清理。
- [x] CLI fake-provider smoke 覆盖 provider 失败清理、SIGINT 取消清理、写工具拒绝和 ask_user 不挂起；测试通过 runtime control/up 替身连接真实 conversation service，不启动 supervisor/Temporal 子进程。
- [x] provider use 覆盖 runtime 停止/运行状态、配置保留、环境变量优先级及密钥不泄漏（通过 runtime-control 测试替身，不启动 supervisor）。
- [x] Gemini print 在 runtime 启动前返回明确不支持错误；CLI 错误由主入口以非零退出。
- [x] 无需真实 API key 或外网；测试使用 fake provider、临时配置与隔离目录。
- [x] 检查整机内存余量后，独占执行全量验证 `go test -p 2 ./...`；结果通过。

## 最终验收

- [x] AC1：参数/stdin 输入、自动启动分支与当前目录绑定由 CLI smoke 和服务集成测试覆盖。
- [x] AC2：复用 runner；`OpRead` 权限与非 `OpRead` 硬拒绝由 authority/executor 测试覆盖，CLI 写工具拒绝、取消和 ask_user 不挂起由 fake-provider smoke 覆盖。
- [x] AC3：纯回答 stdout、凭据脱敏和失败返回由 CLI smoke 覆盖；主入口将错误输出 stderr 并以非零状态退出。
- [x] AC4：成功、provider 失败和中断清理由 CLI smoke 覆盖；临时 session 不入列表，清理失败会合并进返回错误。
- [x] AC5：provider list/show 完整且不泄漏凭据；Gemini 仅目标决策。
- [x] AC6：provider use 在 runtime 关闭时更新字段并保留其他设置；运行时拒绝；环境变量优先。
- [x] AC7：Gemini print 明确失败，不启动流式 run。
- [x] AC8：fake provider 与临时私有配置完成验证，不依赖真实密钥或外网。
- [x] `go test -p 2 ./...` 通过；为避开系统 `/tmp` 配额，验证时将 `TMPDIR` 与 `GOTMPDIR` 指向短路径磁盘目录。
- [x] README 与实际 CLI 行为一致。
