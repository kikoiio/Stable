# M09-A 协作事件与只读协调原型 Tasks

> 状态：已批准（2026-10-07）。任务基于已批准的 `spec.md` 与 `plan.md`。此清单描述后续实现工作；四份规格文档全部获批之前不开始实现。

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `internal/agent/delegation.go` | 委派任务、状态、结果、限额、协调器接口和批次校验 |
| 新建 | `internal/agent/delegation_test.go` | 状态机、FIFO 池、背压、部分失败与取消验证 |
| 新建 | `internal/agent/events.go` | 协作进度事件和 reporter/runner 适配接口 |
| 新建 | `internal/agent/executor.go`、`runner.go` | 有界 worker 协调和受限 child run runner |
| 修改 | `internal/agent/runner.go`、`executor.go` | 若当前 agent loop 需要导出窄接口，接入 child runner 所需的最小扩展 |
| 新建 | `internal/execution/delegation_tools.go` | `delegate_tasks` schema、参数校验、父工具结果格式化 |
| 修改 | `internal/execution/executor_factory.go`、`tool_executor.go`、`tools_schema.go` | 委派工具注册、执行和 child executor 构造 |
| 新建 | `internal/execution/delegation_tools_test.go` | 工具可见性、权限边界和逐项结果映射 |
| 修改 | `internal/sessionlog/events.go`、`validate.go`、`projection.go` | 协作事件类型、字段校验与恢复投影 |
| 修改 | `internal/sessionlog/log.go` | 仅当现有 append/replay 路径需要为新事件增加专门处理时修改 |
| 新建 | `internal/sessionlog/delegation_test.go` | 协作事件落盘、回放和状态合法性 |
| 修改 | `internal/conversation/service.go`、`run.go` | reporter 接线、run 取消传播、启动恢复修复 |
| 新建 | `internal/conversation/delegation_recovery_test.go` | 中断恢复、工具调用结果配对、幂等验证 |
| 修改 | `internal/runtime/supervisor.go` | 构造并共享全服务委派池，接入 session run executor |
| 修改 | `cmd/stable/chatserve.go` | `/delegate` 命令解析和普通父 run 启动 |
| 修改 | `internal/tui/model.go`、`transcript.go` | `/delegate` 输入路由和协作事件展示 |
| 新建 | `internal/tui/delegation_test.go` | 进度呈现、状态投影与游标续读 |
| 修改 | `README.md` | 描述入口、同步行为、只读能力和资源限制 |

> 任务实施时先沿用 `internal/sessionlog` 现有事件模型及 `run_subscribe` 协议。若当前代码的调用点名称有差异，只调整接线位置，不新增第二套存储或客户端协议。

## T1: 委派契约与输入校验

**文件：** `internal/agent/delegation.go`、`internal/agent/delegation_test.go`

**依赖：** 无。

**步骤：**

1. 定义 `DelegationTask`、`DelegationStatus`、`DelegationResult`、`DelegationLimits` 和 `Delegator` 接口。
2. 实现批次校验：任务 ID 非空且批次内唯一，名称和指令非空，父 run 必须为 session WorkSession，编码输入不超过 64 KiB。
3. 定义状态迁移规则，确保只有 `queued → running → terminal` 或 `queued → terminal`，终态不可重复改变。
4. 将默认限额设为 3 worker、32 个队列项、8 轮、3 分钟、8 KiB 摘要和现有 50,000 字节工具输出上限。

**验证：** `go test ./internal/agent/ -run DelegationContract -count=1` 退出码为 0；覆盖重复/缺失 ID、空任务、64 KiB 边界、错误 run 类型及非法状态迁移。

## T2: 有界 FIFO 协调池

**文件：** `internal/agent/delegation.go`、`delegation_test.go`

**依赖：** T1。

**步骤：**

1. 实现服务级共享 FIFO worker 池，活动 worker 默认 3，待处理队列容量 32。
2. 在入队容量用尽时阻塞提交者，监听 context 取消并释放队列槽位；不可按批次大小创建 goroutine。
3. 按输入顺序保留结果位置；子项失败不停止同批其它子项。
4. 父 context 取消时取消正在执行的任务并将尚未运行项设为 `canceled`；每项释放 worker/队列资源且只终结一次。

**验证：** `go test ./internal/agent/ -run 'Delegation(Pool|Backpressure|Cancel|PartialFailure)' -count=1` 退出码为 0；使用同步 fake runner 断言并发不超过 3、FIFO 入队、队列背压可解除、部分失败仍返回成功项、取消后无活跃项。

## T3: Child runner 与只读工具集

**文件：** `internal/agent/executor.go`、`runner.go`、必要的现有 `runner.go`/`executor.go` 扩展及对应测试。

**依赖：** T1。

**步骤：**

1. 定义 `ChildRunInput`、`ChildRunResult` 和 `ChildRunner`，字段只包含父/子 run ID、一个显式任务、授权项目根、父 provider/model 和限额。
2. 通过现有 agent loop/provider 创建子 run，不传完整对话 transcript 或兄弟任务信息。
3. 子 executor 只注册 `read_file`、`glob`、`grep`；不注册命令、写操作、MCP、网络和委派工具。
4. 每项应用最多 8 轮和 3 分钟限制，时长不得超过父 run 剩余时长；累计工具输出最多 50,000 字节，摘要最多 8 KiB，截断时保留状态和错误说明。
5. 复用父授权根、已有 permission gate 和 sandbox。权限需要审批时沿用现有审批路径。

**验证：** `go test ./internal/agent/ -run ChildRun -count=1` 与 `go test ./internal/execution/ -run ReadOnlyExecutor -count=1` 退出码均为 0；provider fake 断言 provider/model 与父 run 一致、上下文不含 transcript，工具清单仅有读搜列能力，越权路径和写/命令/MCP/网络/递归委派不可调用。

## T4: 协作事件与 sessionlog 回放

**文件：** `internal/agent/events.go`、`internal/sessionlog/events.go`、`validate.go`、`projection.go`、`log.go`（如确有需要）及 `internal/sessionlog/delegation_test.go`。

**依赖：** T1。

**步骤：**

1. 定义含 batch/task ID、任务名、状态、阶段摘要、错误和更新时间的协作事件载体，并以父 `RunEvent` 持久化。
2. 校验必填关联字段、允许的状态、摘要长度和终态唯一性；事件中不得保存思考流或原始 child transcript。
3. 将任务事件接入现有 session event append/replay/project 路径，沿用现有事件序列与游标。
4. 对事件摘要及错误应用现有敏感信息过滤和大小限制。

**验证：** `go test ./internal/sessionlog/ -run Delegation -count=1` 退出码为 0；追加后回放顺序一致，非法字段/过长摘要/第二终态被拒绝，事件数据不含原始 transcript。

## T5: 批量委派工具及工具执行集成

**文件：** `internal/execution/delegation_tools.go`、`executor_factory.go`、`tool_executor.go`、`tools_schema.go`、`delegation_tools_test.go`。

**依赖：** T1、T2、T3。

**步骤：**

1. 定义 `delegate_tasks` 的工具 schema，参数包括批次任务名称、唯一 ID 和明确指令。
2. 只在 session WorkSession 中向父 agent 暴露 schema；其他 run 类型调用时返回拒绝结果。
3. 解析并校验工具参数，再调用共享 `Delegator.RunBatch`；返回输入顺序一致的逐项 JSON 结果，包括状态、摘要和错误原因。
4. 接入现有 ToolExecutor 的 tool call/result 日志、脱敏和权限门；提交级错误作为工具错误返回，子项错误保留在该项结果中。
5. 给 child executor 单独构造只读 schema，保证 `delegate_tasks` 不递归注册。

**验证：** `go test ./internal/execution/ -run Delegation -count=1` 退出码为 0；断言仅 WorkSession 能看到工具、批次结果顺序稳定、混合成功/失败和全失败均如实返回、提交错误有说明、调用与结果事件配对。

## T6: Runtime 与父 run 生命周期接线

**文件：** `internal/runtime/supervisor.go`、`cmd/stable/chatserve.go`、必要的 `internal/conversation/service.go` 接线。

**依赖：** T2、T3、T5。

**步骤：**

1. 在 runtime/supervisor 生命周期中只创建一份委派协调器和共享 worker 池，并注入现有 session run executor factory。
2. 为每个父 run 提供其 provider/model、授权项目根和剩余预算视图，不能从全局配置扩大权限。
3. 将父 run context 传入批次；父 run 正常结束、取消或失败后不遗留该批次的运行子项。
4. 关闭服务时阻止新任务入队并取消活动项；此阶段关闭行为不得自动重跑任务。

**验证：** `go test ./internal/runtime/... -run Delegation -count=1` 和 `go test ./cmd/stable/... -run Delegation -count=1` 退出码均为 0；测试确认两个父 session 共用同一并发上限，父 run 取消能传到活动 child。

## T7: `/delegate` 普通 run 命令入口

**文件：** `cmd/stable/chatserve.go`、`internal/tui/model.go`、相应命令测试。

**依赖：** 无；需在集成时与 T6 汇合。

**步骤：**

1. 识别 `/delegate <任务>`，去除命令前缀后通过既有普通 session prompt/run 入口提交。
2. 缺少或全为空白任务时显示 `/delegate <任务>` 用法提示，不创建 run。
3. 不在 TUI 端拆分子任务，留给父 agent 按需调用 `delegate_tasks`。
4. 确认新命令不会改变现有普通 prompt、命令或 run 行为。

**验证：** `go test ./cmd/stable/... -run DelegateCommand -count=1` 与 `go test ./internal/tui/... -run DelegateCommand -count=1` 退出码均为 0；断言有效命令创建普通 run，空命令没有新增 run。

## T8: TUI 协作状态与游标恢复

**文件：** `internal/tui/model.go`、`transcript.go`、`delegation_test.go`。

**依赖：** T4、T7。

**步骤：**

1. 按 batch/task ID 聚合父 run 的协作事件，并展示任务名、排队/运行状态、非思考流阶段摘要和终态。
2. 继续使用现有 `run_subscribe` 和序列游标；断线后补取缺失事件，按序更新任务状态。
3. 隐藏思考流和 child 原始 transcript；错误与摘要只展示事件中已过滤内容。
4. 适配成功、失败、取消、中断四类终态，并确保终态后不被迟到进度覆盖。

**验证：** `go test ./internal/tui/... -run Delegation -count=1` 退出码为 0；模型事件序列测试覆盖 queued→running→terminal、游标续读、重复事件、终态后迟到事件和思考流不可见。

## T9: 服务启动恢复与幂等修复

**文件：** `internal/conversation/service.go`、`run.go`、`internal/sessionlog/projection.go`、`internal/conversation/delegation_recovery_test.go`。

**依赖：** T4、T5、T6。

**步骤：**

1. 在启动接收新 session run 前扫描父 run 事件日志中的未终结 queued/running 子任务与未闭合 `delegate_tasks` 工具调用。
2. 为每个未完成子项追加唯一 `interrupted` 终态，不重新排队或执行。
3. 将对应父 run 终结为 `interrupted`，并给尚无结果的委派工具调用追加明确的中断 `EventToolResult`，保持 call/result 配对。
4. 以已有事件序列和终态检查保证重复恢复幂等；不重复追加终态、不覆盖已有结果。

**验证：** `go test ./internal/conversation/... -run DelegationRecovery -count=1` 与 `go test ./internal/sessionlog/... -run ToolCallPairing -count=1` 退出码均为 0；分别模拟 queued、running、工具调用中途重启及连续运行两次恢复。

## T10: 端到端整合与边界回归

**文件：** 覆盖 `cmd/stable`、`internal/runtime`、`internal/conversation`、`internal/execution` 和 `internal/tui` 的集成测试文件。

**依赖：** T6、T7、T8、T9。

**步骤：**

1. 使用 fake provider 和临时授权项目根运行完整 `/delegate` 父 run；父 agent 调用批量委派工具，至少两个 child 并行只读搜索并将逐项结果交还父 agent。
2. 增加混合成功/失败、资源池排队、父取消、越权读、禁止工具、服务中断恢复和断线游标续读场景。
3. 确认 TUI 无思考流，sessionlog 有序记录任务生命周期且 tool call/result 完整配对。
4. 回归普通 session run、现有 permission gate、候选接收和目标验证，确认只读 child 结果不产生候选或目标状态变更。

**验证：** `go test ./internal/agent/... ./internal/execution/... ./internal/conversation/... ./internal/runtime/... ./internal/tui/... -run 'Delegation|ReadOnlyExecutor' -count=1` 与相关集成测试命令退出码为 0；完整父 run 场景使用 fake provider，不依赖真实模型费用或外网。

## T11: 使用说明与完整验证

**文件：** `README.md`；验证任务不修改文件。

**依赖：** T10。

**步骤：**

1. 说明 `/delegate <任务>` 会启动普通父 run，子任务由父 agent 通过工具批量委派。
2. 说明只读工具范围、provider 请求例外、队列/预算默认值、同步等待、取消与重启中断行为。
3. 按 checklist 执行 Linux 定向测试、构建及回归；先确认与改动相关的测试，再安排全量验证。
4. 记录每项真实命令、退出码和观察到的行为；失败先定位和修复后再复验。

**验证：** `go test ./...` 退出码为 0，`go build ./...` 退出码为 0；手动或自动 TUI 端到端场景能观察到 queued/running/terminal 和父 agent 汇总结果。验证证据记录到最终验收报告，不因命令启动成功即标记通过。

## 执行顺序

```text
T1 委派契约 ──→ T2 有界池 ──┬──→ T5 批量工具 ──→ T6 Runtime 接线 ──┬──→ T9 恢复幂等 ──┐
      └────────→ T3 只读 child runner ─┘                            │                    │
T4 协作事件 ────────────────────────────────────────────────────────┘                    ├──→ T10 整合 ──→ T11 文档与完整验证
T7 命令入口 ─────────────────────────────────→ T8 TUI 协作状态 ────────────────────────┘
```

**可执行并行批次：**

1. 批次 A：T1、T4、T7 可并行；三者拥有独立文件。
2. 批次 B：T2、T3 可并行；两者都依赖 T1，分别负责 `delegation.go` 与 child runner 文件。避免并行编辑同一文件。
3. 批次 C：T5 依赖 T1、T2、T3；T6 依赖 T2、T3、T5；T8 依赖 T4、T7。T8 可与 T5/T6 并行。
4. 批次 D：T9 依赖 T4、T5、T6；T10 等待 T8 和 T9 汇合。
5. 批次 E：T11 等待 T10 完成；全量验证安排在单一受控批次，避免重复启动重型任务。

并行时不得同时编辑同一文件。T1/T2/T3 如需修改 `internal/agent/runner.go` 或 `executor.go`，先由 T1 确定窄接口并集中调整，再让其余任务消费该接口。T6、T8、T9 可独立开发测试，但只有三者汇合后的 T10 才能验证真实端到端生命周期。
