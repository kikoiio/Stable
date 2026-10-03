# M02 模型与流式对话 Tasks

## 文件清单

| 操作 | 文件 | 职责 |
| --- | --- | --- |
| 新建 | `internal/llm/provider.go` | provider 请求/事件、用量与 `Provider` 接口 |
| 新建 | `internal/llm/errors.go` | provider 错误分类、重试信息和凭据脱敏 |
| 新建 | `internal/llm/sse.go` | context 可取消的 SSE 解码器 |
| 新建 | `internal/llm/anthropic.go` | Anthropic streaming 适配 |
| 新建 | `internal/llm/openai.go` | OpenAI streaming 适配 |
| 新建 | `internal/llm/compatible.go` | OpenAI-compatible streaming 适配 |
| 新建 | `internal/llm/factory.go` | 从现有 appconfig 建立 streaming provider |
| 新建 | `internal/llm/*_test.go` | 三种协议 fixture、错误、截断、usage 与取消测试 |
| 新建 | `internal/agent/events.go` | M00 执行类型、事件类别和终态 |
| 新建 | `internal/agent/runner.go` | 共用 runner、provider 事件归一化和发布 |
| 新建 | `internal/agent/registry.go` | run 生命周期、取消和重复取消安全 |
| 新建 | `internal/agent/runner_test.go` | fake provider 下的顺序、重试、取消、终态测试 |
| 修改 | `internal/sessionlog/events.go` | 新增 run 事件类型和 payload |
| 修改 | `internal/sessionlog/log.go` | run payload 校验与事件追加/游标行为 |
| 修改 | `internal/sessionlog/log_test.go` | run 事件序号、校验和回放覆盖 |
| 修改 | `internal/conversation/protocol.go` | run start/subscribe/cancel 与游标协议 |
| 修改 | `internal/conversation/service.go` | runner 编排、归属检查、追加后广播和背压处理 |
| 修改 | `internal/conversation/session.go` | 将普通 chat 提交接入共用流式 run 入口 |
| 修改 | `internal/conversation/client.go` | 流式订阅、游标补读和取消客户端 |
| 修改 | `internal/conversation/protocol_test.go` | run 协议字段和无效归属校验 |
| 修改 | `internal/conversation/service_test.go` | 持久化、广播、并发归属和慢订阅者测试 |
| 修改 | `internal/conversation/session_test.go` | 普通 session chat 使用共用 runner 的回归测试 |
| 修改 | `internal/conversation/client_test.go` | 订阅、游标补读、取消和断连测试 |
| 修改 | `internal/core/activities.go` | goal work item 映射到统一 run client/outcome |
| 修改 | `internal/core/activities_test.go` | 失败/取消不验证目标及后续责任回归 |
| 修改 | `cmd/agentworker/main.go` | 注入 conversation Unix socket goal run client |
| 修改 | `cmd/agentworker/main_test.go` | worker 参数和 client 装配测试 |
| 修改 | `internal/runtime/supervisor.go` | streaming provider/runner 装配，向 worker 传递 socket 地址 |
| 修改 | `internal/appconfig/config.go` | 若必要，补齐 streaming 配置验证和安全摘要 |
| 修改 | `internal/appconfig/config_test.go` | provider 选择、base URL 与密钥诊断回归 |
| 修改 | `internal/tui/model.go` | run 事件状态、游标、当前 run 和取消命令 |
| 修改 | `internal/tui/transcript.go` | text/thinking/tool/usage/error/终态显示 |
| 修改 | `internal/tui/model_test.go` | 分流、实时增量、取消和终态模型测试 |
| 修改 | `internal/tui/transcript_test.go` | 思考分区、工具生命周期、用量与未知事件测试 |
| 新建 | `internal/conversation/run_integration_test.go` | fake provider 到持久 session transcript 的端到端测试 |

## T1: 定义 provider-neutral 消息与流事件

**文件：** `internal/llm/provider.go`
**依赖：** 无

**步骤：**
1. 定义 provider 请求、消息、stream event、usage 和 tool-call 增量结构。
2. 定义 `Provider.Stream(ctx, request)` 的事件与错误通道语义，明确关闭顺序。
3. 明确未报告的 usage 字段可缺省，真实零值仍可表达。

**验证：** `go test ./internal/llm` 编译通过；新增类型可由包内最小构造测试序列化/反序列化。

## T2: 统一 provider 错误分类和脱敏

**文件：** `internal/llm/errors.go`、`internal/llm/provider.go`
**依赖：** T1

**步骤：**
1. 定义 auth、rate_limit、network、context_limit、provider 五类错误。
2. 记录 HTTP 状态、Retry-After 和 retryable 属性，但输出安全的用户可见摘要。
3. 对可能包含 API key、Authorization header 或请求 URL 凭据的正文做脱敏。

**验证：** `go test ./internal/llm` 通过；表驱动测试断言分类正确且错误字符串不含 fixture 中的密钥。

## T3: 实现可取消的 SSE 解码器

**文件：** `internal/llm/sse.go`、`internal/llm/sse_test.go`
**依赖：** T1

**步骤：**
1. 支持 `data:` 多行合并、空行事件分隔、注释行及 `[DONE]` 结束标记。
2. 设置单事件最大字节数，格式错误和截断响应返回可诊断错误。
3. context 取消或 reader 关闭时停止解码，不遗留阻塞 goroutine。

**验证：** `go test ./internal/llm -run SSE` 通过；fixture 覆盖多行、空事件、超长、截断、DONE 和取消。

## T4: 实现 Anthropic 流式适配

**文件：** `internal/llm/anthropic.go`、`internal/llm/anthropic_test.go`
**依赖：** T1、T2、T3

**步骤：**
1. 构造 Anthropic messages streaming 请求，使用配置中的 model 与 key。
2. 将 content block 文本/思考、tool use 输入片段、message usage 与结束原因映射为标准事件。
3. 解析认证、限流、上下文超限和普通错误，保证响应体和请求头不会进入日志。

**验证：** `go test ./internal/llm -run Anthropic` 通过；httptest fixture 检查请求头/模型、事件顺序、usage、错误分类和 key 不泄漏。

## T5: 实现 OpenAI 流式适配

**文件：** `internal/llm/openai.go`、`internal/llm/openai_test.go`
**依赖：** T1、T2、T3

**步骤：**
1. 构造 OpenAI chat-completions streaming 请求，启用末尾 usage 报告。
2. 映射文本、reasoning/thinking（若 provider 提供）、tool-call ID/name/参数片段和结束原因。
3. 映射 prompt/completion/cached token usage，缺失字段保持 unavailable。

**验证：** `go test ./internal/llm -run OpenAI` 通过；fixture 覆盖分片 tool 参数、并行 choice 过滤、末尾 usage 和错误响应。

## T6: 实现 OpenAI-compatible 适配与 provider factory

**文件：** `internal/llm/compatible.go`、`internal/llm/factory.go`、`internal/llm/compatible_test.go`、`internal/llm/factory_test.go`
**依赖：** T1–T5

**步骤：**
1. 使用配置 base URL 构造兼容 endpoint，并复用 OpenAI-compatible SSE 解码。
2. 确保仅 compatible provider 接受自定义 base URL，key 不进入错误文本。
3. factory 覆盖 Anthropic、OpenAI、OpenAI-compatible 三种 provider 并拒绝未知名称。

**验证：** `go test ./internal/llm` 通过；factory 表驱动测试确认三种 provider 路由、model/base URL 配置和无效配置错误。

## T7: 完成三种 provider 协议 fixture 覆盖

**文件：** `internal/llm/*_test.go`
**依赖：** T3–T6

**步骤：**
1. 为正常结束、截断/格式错误、限流、网络中断和 context cancel 增加 fixture。
2. 覆盖文本、思考、工具调用片段及结束事件的有序解码。
3. 检查认证信息不会进入错误或测试日志输出。

**验证：** `go test ./internal/llm -count=1` 通过；测试不访问外网且不读取真实 API key。

## T8: 定义共用执行请求、事件与终态

**文件：** `internal/agent/events.go`
**依赖：** T1

**步骤：**
1. 定义 WorkRef、ExecutionRequest、ExecutionEvent 与 EventKind。
2. 保留 M00 基线、允许范围、资源与权限边界字段；M02 只传递、不扩大或执行这些约束。
3. 定义 UsageInfo、ToolCall、RunOutcome 和 completed/cancelled/failed/awaiting_tools 终态。

**验证：** `go test ./internal/agent` 编译通过；表驱动测试覆盖普通/目标 WorkRef 校验与四种终态编码。

## T9: 实现 runner 的正常流与事件序号

**文件：** `internal/agent/runner.go`、`internal/agent/registry.go`
**依赖：** T2、T8

**步骤：**
1. 注入 provider 与事件发布端，校验 runID、sessionID 和 WorkRef。
2. 为单次 run 建立 context、RunHandle 与 registry 项。
3. 按 provider 顺序映射事件，分配从 1 开始的 RunSeq，并保留累计文本。

**验证：** `go test ./internal/agent -run 'Start|Sequence|WorkRef'` 通过；fake provider 事件序列逐项比对输出 ID、RunID 和 RunSeq。

## T10: 实现 runner 取消、重试和互斥终态

**文件：** `internal/agent/runner.go`、`internal/agent/registry.go`
**依赖：** T9

**步骤：**
1. `Cancel(runID)` 调用运行 context cancel，重复调用不 panic、不生成第二个终态。
2. 对 retryable rate/network 错误执行有界重试，尊重 Retry-After 和总等待上限；auth/参数错误不重试。
3. 错误或取消前已产生文本继续保留；收到未执行 tool-call 时以 awaiting_tools 结束。

**验证：** `go test ./internal/agent -run 'Cancel|Retry|Terminal|AwaitingTools'` 通过；断言每条路径恰有一个终态且 provider 收到取消。

## T11: 验证 runner 并发和故障行为

**文件：** `internal/agent/runner_test.go`
**依赖：** T9、T10

**步骤：**
1. 用 fake provider 覆盖多 run 并发、provider 错误和通道提前关闭。
2. 校验不同 run 的事件序号独立递增且内容不交叉。
3. 校验慢消费、重复取消及服务端发布错误能够安全结束或显式失败。

**验证：** `go test -race ./internal/agent -count=1` 通过。

## T12: 扩展 sessionlog run 事件 schema

**文件：** `internal/sessionlog/events.go`、`internal/sessionlog/log.go`
**依赖：** T8

**步骤：**
1. 增加 run_started、各类 delta、usage、retry、error 和 terminal 类型及强类型 payload。
2. 校验 runID、sessionID、RunSeq、事件类别和 terminal 状态字段。
3. 保持现有 session event Seq 全局递增并返回每次 append 的持久游标；新增按 AfterSeq 读取后续事件的 API。

**验证：** `go test ./internal/sessionlog` 通过；既有事件仍可 append/replay，新事件缺字段或非法终态会被拒绝。

## T13: 验证 sessionlog 顺序、回放与旧数据读取

**文件：** `internal/sessionlog/log_test.go`
**依赖：** T12

**步骤：**
1. 连续追加跨多个 run 的事件，检查 session Seq 连续且 RunSeq 在各 run 内单调。
2. 从游标回放并检查 run/session 归属、partial 内容和最终状态。
3. 使用现有旧版 fixture 确认历史 session transcript 仍可读取。

**验证：** `go test ./internal/sessionlog -count=1` 通过；断言回放结果与追加顺序一致。

## T14: 扩展 conversation run 协议

**文件：** `internal/conversation/protocol.go`、`internal/conversation/protocol_test.go`
**依赖：** T8、T12

**步骤：**
1. 增加 run_start、run_subscribe、run_cancel 请求字段及 run_event、run_outcome、resync 响应。
2. 支持 AfterSeq 游标、run/session 过滤和目标 WorkRef 字段。
3. 拒绝无效操作、缺少归属 ID、跨 session run 订阅及未知 JSON 字段。

**验证：** `go test ./internal/conversation -run Protocol` 通过；协议 fixture 对有效请求成功解码、对越权/无效请求明确报错。

## T15: 实现 conversation run 编排与持久优先分发

**文件：** `internal/conversation/service.go`、`internal/conversation/session.go`、`internal/conversation/protocol.go`
**依赖：** T9、T12、T14

**步骤：**
1. 将 runner 和 sessionlog publisher 注入 service；run_start 校验 WorkRef 并分配 runID。
2. 将普通 TUI chat 提交转换为 `WorkRef{Kind: session}` 并进入该 run_start 路径；goal worker 请求同一入口。
3. 先追加 run_started 和每个 ExecutionEvent，再将带 session cursor 的事件分发给匹配订阅者。
4. registry 将 runID 绑定到 session/work；终态只写入一次，错误/取消后保留 partial 事件。

**验证：** `go test ./internal/conversation -run 'RunStart|Persist|Ownership'` 通过；fake publisher 记录顺序证明每次广播前已成功 append。

## T16: 实现 run 订阅、游标补读、取消和背压

**文件：** `internal/conversation/service.go`、`internal/conversation/client.go`
**依赖：** T15

**步骤：**
1. run_subscribe 根据 session AfterSeq 回放遗漏事件，并只推送所订阅的 run/session。
2. run_cancel 校验调用者归属后调用 runner.Cancel；goal client 可等待 run_outcome。
3. 队列溢出时发 resync/cursor 或关闭订阅；不静默丢事件、不阻塞 runner。

**验证：** `go test ./internal/conversation -run 'Subscribe|Replay|Cancel|Backpressure'` 通过；慢客户端能够观察到补读游标或显式断开。

## T17: 覆盖 conversation 协议并发与断连场景

**文件：** `internal/conversation/service_test.go`、`internal/conversation/client_test.go`
**依赖：** T15、T16

**步骤：**
1. 并发启动 session/goal 两类 run，验证客户端仅收到授权关联事件。
2. 断开后按 AfterSeq 重连，验证无静默丢失、错序或重复终态。
3. 覆盖 cancel 与 completion/error 竞争时终态唯一。

**验证：** `go test -race ./internal/conversation -count=1` 通过。

## T18: 接入 appconfig 与 runtime provider 装配

**文件：** `internal/appconfig/config.go`、`internal/appconfig/config_test.go`、`internal/runtime/supervisor.go`
**依赖：** T6、T15

**步骤：**
1. streaming provider factory 使用既有 provider/model/base URL/key 配置，不引入第二套密钥字段。
2. 只在配置摘要中输出非秘密字段，保证错误/日志不包含 API key。
3. supervisor 在 conversation service 注入单一 runner，并向 agentworker 传递现有 chat socket 地址。

**验证：** `go test ./internal/appconfig ./internal/runtime` 通过；测试配置摘要与启动日志 fixture 不包含 key。

## T19: 将目标工作项接到共享 run

**文件：** `internal/core/activities.go`、`internal/core/activities_test.go`、`cmd/agentworker/main.go`、`cmd/agentworker/main_test.go`
**依赖：** T16、T18

**步骤：**
1. 定义并注入 `GoalRunClient`，将目标/工作项 ID、session、意图和允许边界映射为 ExecutionRequest。
2. 经本机 conversation socket start/subscribe/cancel，不在 worker 进程创建 runner。
3. 将 completed/failed/cancelled/awaiting_tools 结果返回 Stable 活动；目标验证与下一责任仍由现有控制流维护。

**验证：** `go test ./internal/core ./cmd/agentworker` 通过；测试证明取消/失败不会使目标变为 verified，并检查 worker 使用注入 client。

## T20: 在 TUI 模型接收并关联流事件

**文件：** `internal/tui/model.go`、`internal/tui/model_test.go`
**依赖：** T14、T16

**步骤：**
1. 将 runID、sessionID、AfterSeq、状态和 partial 内容加入界面状态。
2. 仅处理当前订阅的 run/session，按 RunSeq 检查连续性并对未知事件忽略。
3. 接收文本、思考、tool-call、usage、retry、error 与 terminal 更新。

**验证：** `go test ./internal/tui -run 'Run|Stream|Unknown|Session'` 通过；模型测试验证实时追加且交错 run 不串流。

## T21: 呈现思考、工具、用量与运行终态

**文件：** `internal/tui/transcript.go`、`internal/tui/transcript_test.go`
**依赖：** T20

**步骤：**
1. 将思考内容显示在独立区域，不合并进可提交的 assistant 文本。
2. 显示 tool-call 名称、ID、参数片段和完成状态，不触发执行。
3. 展示已报告 token 字段；不可用字段显示 unavailable；显示重试、错误、取消和 awaiting_tools。

**验证：** `go test ./internal/tui -run 'Thinking|Tool|Usage|Terminal'` 通过；视图输出断言思考与回答分区且未报告 usage 不显示为 0。

## T22: 接入取消与 TUI 断线补读

**文件：** `internal/tui/model.go`、`internal/tui/model_test.go`、`internal/conversation/client.go`
**依赖：** T16、T20

**步骤：**
1. 在运行中通过现有 TUI 按键/命令发起 run_cancel。
2. 断线重连时使用最后确认的 session cursor 订阅或刷新 transcript。
3. 对取消响应等待唯一 terminal，保留已展示 partial 文本。

**验证：** `go test ./internal/tui ./internal/conversation -run 'Cancel|Reconnect|Cursor'` 通过；取消后 UI 状态为 cancelled 且 partial 内容仍在。

## T23: 验证 TUI 流式行为与旧界面回归

**文件：** `internal/tui/model_test.go`、`internal/tui/transcript_test.go`
**依赖：** T20–T22

**步骤：**
1. 覆盖文本逐块更新、thinking 分区、tool-call、usage unavailable、错误和取消状态。
2. 覆盖 unknown event 安全忽略和多个 session/run 隔离。
3. 运行 M01 已有布局、导航、输入和滚动测试确认流式状态没有回归。

**验证：** `go test ./internal/tui -count=1` 通过。

## T24: 完成端到端流式持久化与重载场景

**文件：** `internal/conversation/run_integration_test.go`
**依赖：** T7、T13、T17、T19、T23

**步骤：**
1. 用 fake provider 经 conversation run_start 输出文本、思考、工具调用和 usage。
2. 验证每项先持久化再广播，TUI client 只接收当前 session/run 的事件。
3. 重载 transcript，检查 partial/terminal 状态一致；另测取消与 provider 错误路径。

**验证：** `go test ./internal/conversation -run 'TestRun.*Integration' -count=1` 通过；测试不需要 API key 或外网。

## T25: 全量回归与 Linux 验收构建

**文件：** 本任务涉及的 M02 代码与测试文件
**依赖：** T1–T24

**步骤：**
1. 运行全量 Go 测试，修复 M02 引入的回归。
2. 验证既有 Gemini structured decision、session 持久化、M01 TUI 测试仍通过。
3. 检查改动格式、module 校验和 Linux 构建。

**验证：** `go test ./...`、`go vet ./...`、`go mod verify`、`git diff --check` 均通过；全部测试仅使用 fake provider/本地 fixture。

## 执行顺序

```text
T1 → T2 → T3 → (T4, T5) → T6 → T7
T1 → T8 → T9 → T10 → T11
       ├→ T12 → T13
       └→ T14 → T15 → T16 → T17
T6 + T15 → T18
T16 + T18 → T19
T14 + T16 → T20 → T21
T16 + T20 → T22
T20 + T21 + T22 → T23
T7 + T13 + T17 + T19 + T23 → T24 → T25
```

同一依赖层中的独立 provider 适配器可分别实现；conversation 持久化与 TUI 消费需在其协议和事件契约完成后执行。任何任务都不得绕过 M00 的统一 runner 或让 agent/tool-call 修改 Stable 目标验证事实。
