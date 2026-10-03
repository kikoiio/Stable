# M02 模型与流式对话 Checklist

> 按实测结果更新；保留未覆盖的检查项。

## Provider 与配置

- [x] **AC1 / F1：三种流式 provider 配置可用。** 分别以 Anthropic、OpenAI、OpenAI-compatible 配置构造 client，并用本地协议 fixture 收到有效流事件。（验证：`go test ./internal/llm -count=1`；fixture 断言三个 provider 路由和事件输出。）
- [x] **AC1 / N5：现有 Gemini 结构化目标决策不回归。** 使用既有 fake/provider 测试执行结构化决策路径。（验证：`go test ./internal/decision ./internal/core -count=1`）
- [x] **AC9 / N1：凭据不会出现在错误、日志、session transcript 或 TUI。** 使用带唯一标记的假 API key 触发认证错误并提交包含该标记的输入；检查所有输出与持久文件均已脱敏。（验证：对应 appconfig/provider/conversation 测试断言 transcript、日志和错误文本均不含标记。）
- [x] **AC11 / N6：自动化验证不依赖外网或真实 API key。** 不配置真实 key，所有 provider host 使用 localhost fake server 运行测试。（验证：`go test ./internal/llm ./internal/agent ./internal/conversation` 通过；provider fixture 只绑定 localhost。）

## 共用执行与事件顺序

- [x] **AC2 / F2：普通任务和目标工作项进入同一 runner。** 分别提交 session 与 goal WorkRef，确认两者都经过同一个 Runner 实例，事件保留各自 WorkRef 归属。（验证：conversation/agent 集成测试记录 runner 调用及 WorkRef。）
- [x] **AC2：运行事件不改变目标事实。** 对目标工作项返回 completed、failed、cancelled 与 awaiting_tools，检查模型事件未直接将目标置为 verified，且由 Stable 控制流安排后续责任。（验证：`go test ./internal/core -count=1`；新增断言覆盖四种 outcome。）
- [x] **AC3 / F3：provider 事件转换后顺序稳定。** 用 fixture 发送文本、思考、tool-call、usage 和结束事件，检查事件 ID 唯一、RunSeq 单调且不同 run 的编号独立。（验证：`go test ./internal/llm ./internal/agent -count=1`；断言检查各类事件顺序及 run 间隔离。）
- [x] **AC3 / N2：session cursor 与 run 序号可分别恢复。** 交错追加两个 run 的事件，按 AfterSeq 回放后确认 session Seq 连续、RunSeq 各自连续、内容归属不串。（验证：`go test ./internal/sessionlog ./internal/conversation -count=1`；回放 fixture 断言游标与两种序号。）
- [x] **AC3 / F9：流式文本和终态可重载。** 对正常完成、provider 错误和取消分别重载 session transcript，确认完整/部分文本及最终状态与持久事件相同。（验证：conversation run integration 测试检查 Replay 输出。）
- [x] **AC3 / F4：多 run 实时显示互不串流。** 并发推送两个 session/run 的不同文本块，TUI 只将事件追加到相应 transcript。（验证：`go test ./internal/tui -count=1`；模型测试断言逐块更新和归属过滤。）
- [x] **AC4 / F4：思考与最终回答分区呈现。** 输入交错的 thinking 与 text delta，检查思考只进入独立区域，不进入可提交 assistant 文本。（验证：`go test ./internal/tui -count=1`；transcript 断言区域分离。）
- [x] **AC4 / F7：tool-call 完整呈现但不执行。** 发送名称、ID、分片参数与完成事件，检查 UI 显示生命周期，run 以 awaiting_tools 结束且没有工具执行器调用。（验证：`go test ./internal/agent ./internal/tui -count=1`；fake 执行器调用计数为零。）
- [x] **AC5 / F8：usage 真实值与不可用值区分。** 检查 provider 报告的 input/output/cache token 原样显示；未报告字段显示 unavailable，不显示成 0。（验证：`go test ./internal/llm ./internal/tui -count=1`；fixture 和 transcript 分别断言归一化与呈现。）

## 取消、错误与恢复

- [x] **AC6 / F5 / N3：取消传到 provider 并产生唯一 cancelled 终态。** run 流运行中重复发起取消，验证 provider request context 被取消、SSE 连接关闭、partial 文本保留且只写入一个 terminal。（验证：`go test -race ./internal/agent ./internal/conversation -count=1`；取消集成断言重复取消安全、连接关闭且只有一个 terminal。）
- [x] **AC6：goal 取消或取消/完成竞争不等于验证成功。** 在 goal run 取消及 completion 竞争场景中检查状态仍由 Stable 持久控制流决定，不能由 runner 的文本/终态直接 verified。（验证：`go test ./internal/core -count=1`；目标状态断言覆盖取消与竞争。）
- [x] **AC7 / F6：五类 provider 错误可区分。** 通过本地 HTTP fixture 分别制造认证、限流、网络、上下文超限和普通错误，检查标准错误类别与可见提示一致。（验证：`go test ./internal/llm ./internal/agent -count=1`；错误表驱动测试断言五类输出。）
- [x] **AC7 / N4：重试次数与等待受限。** 限流 fixture 返回 Retry-After，网络 fixture 短暂失败；检查重试事件可见、等待不超上限，认证/参数错误不重试，先前文本不丢。（验证：`go test ./internal/agent ./internal/llm -count=1`；fake clock/fixture 断言重试次数、等待和 partial 内容。）
- [x] **N2：慢消费者不会静默丢失事件。** 填满订阅队列，检查服务发送 resync/cursor 信号或断开该订阅；按游标恢复后可取回遗漏事件。（验证：`go test ./internal/conversation -count=1`；backpressure 测试检查 resync/disconnect 和游标补读。）
- [x] **F4：未知事件安全忽略。** 向服务和 TUI 注入未来类型事件，确认当前运行继续，已知消息保持不变且不发生 panic。（验证：`go test ./internal/conversation ./internal/tui -count=1`；未知事件 fixture 后续已知事件仍可见。）

## 集成与回归

- [x] **AC8：并发会话事件不互相覆盖。** 并发运行 session 与 goal stream 并重载两个 transcript，检查每条 event 的 RunID、SessionID、Seq 和持久内容都匹配。（验证：`go test -race ./internal/conversation -count=1`；并发集成断言逐条核对归属与游标。）
- [x] **AC10：fake fixture 覆盖规范列出的流场景。** 确认正常完成、tool-call、partial、malformed/truncated、usage、rate-limit、cancel 与 session reload 各有自动化断言。（验证：`go test ./internal/llm ./internal/agent ./internal/sessionlog ./internal/conversation -count=1`；各场景均有断言。）
- [x] **AC10 / M01 回归：完整 Go 测试通过。**（验证：`go test ./...`。）
- [x] **AC11：Linux 首发构建与静态检查通过。**（验证：Linux 上运行 `go vet ./...`、`go mod verify` 和 `git diff --check`。）

## 端到端场景

- [x] **普通任务流：提交 → 增量呈现 → 重载。** 在本地 fake provider 下从 TUI session 提交一条消息，观察逐块回答、独立思考区、用量和 completed 状态；关闭并重载 session 后 transcript 与终态保持一致。（验证：`go test ./internal/conversation ./internal/tui -count=1`；端到端 fixture 检查 Replay，TUI model 测试检查各增量。）
- [x] **目标任务取消流：goal activity → 共享 run → cancel。** 用 fake provider 启动目标工作项，确认 TUI 与 goal activity 关联同一 RunID；取消后保留 partial、关闭流、goal 未 verified 且 Stable 仍保留复核责任。（验证：`go test ./internal/core ./internal/conversation -count=1`；目标取消集成断言同一 RunID、partial 和未验证状态。）
