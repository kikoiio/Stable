# M09-B fork skill 执行 Plan

> 状态：已批准（2026-10-07）。本文建立在已批准的 [spec.md](spec.md) 上。四份规格文档全部获批前不开始实现。

## 架构概览

M09-B 复用 M09-A 的只读子 agent、服务级 FIFO 资源池和协作事件，不改变通用 `delegate_tasks` 语义。两种 fork skill 入口都创建独立 child run：斜杠入口创建当前 session 下可订阅的独立 run；父 agent 的 `LoadSkill` 入口创建关联父 run 的 child run，并将逐项结果返回为工具结果。inline skill 继续使用原有激活路径。

`SkillGate` 识别 fork skill、读取技能正文、按 M06 规则渲染参数并记录激活元数据。`ForkContextBuilder` 按 `fork_context` 生成隔离的可见历史快照。`ForkSkillCoordinator` 将技能指令、快照和父 run 的 provider/model、授权根与预算交给 M09-A `PoolDelegator`；子 agent 只获得 M09-A 的只读工具集。

斜杠入口由 `ForkSkillRunStarter` 管理独立 run 的开始、协作事件、取消和终态；事件写入 session run 日志并通过现有订阅/游标路径展示。`LoadSkill` 入口由 `ToolExecutor` 把父 run 身份、授权与上下文快照来源传给技能 provider，协作事件写入父 run 的事件序列，执行结果配对为 `EventToolResult`。

架构职责：F1/F2/F7 由 `SkillGate`、两种入口适配器与调用审计承接；F3/N3 由上下文快照构造与过滤承接；F4/N1/N2 由 M09-A 子 agent、权限门和共享池承接；F5/N4 由事件适配、TUI 和恢复流程承接；F6 复用父 run 取消及 M09-A 中断恢复机制。inline skill 与其它 M09-A 能力不变。

## 核心数据结构与接口

### `ForkSkillInvocation`

```go
type ForkSkillInvocation struct {
    SessionID       string
    RunID           string // slash fork run；LoadSkill 时为 parent run ID
    ParentRunID     string // LoadSkill 时设置
    SkillName       string
    SkillSource     string
    Entry           SkillEntry // slash 或 tool
    Instruction     string     // 正文与按 M06 渲染的参数
    ContextMode     ForkContextMode
    ContextMessages []Message
    Provider        llm.Provider
    Model           string
    ProjectRoot     string
    Limits          agent.DelegationLimits
}
```

这是协调层的内部输入。只传当前调用所需的任务、可见上下文快照和授权信息，不保存思考流或完整 child transcript。技能正文与参数合计不得超过 64 KiB；字段中凭证不序列化到日志。

### `ForkContextMode` 与 `ForkContextBuilder`

```go
type ForkContextMode string

const (
    ForkContextNone   ForkContextMode = "none"
    ForkContextRecent ForkContextMode = "recent"
    ForkContextFull   ForkContextMode = "full"
)

type ForkContextBuilder interface {
    Build(ctx context.Context, sessionID, parentRunID string, mode ForkContextMode) ([]Message, error)
}
```

`none` 生成空历史；`recent` 选取最后 5 轮可见对话；`full` 返回现有上下文预算允许的完整可见历史。slash 从 session log 重建调用时的 session 对话，`LoadSkill` 使用调用父 run 时的可见视图。未知模式返回错误，不进入资源池。

### `ForkSkillResult`

```go
type ForkSkillResult struct {
    ChildRunID string
    SkillName  string
    Entry      SkillEntry
    Status     agent.DelegationStatus
    Summary    string
    Error      string
}
```

结果不包含 child transcript；摘要最多 8 KiB。slash 适配器将结果映射为 run 终态和可续读结果；`LoadSkill` 适配器将其格式化为父工具结果。失败、中断、取消保留明确原因。

### `ForkSkillExecutor` 与运行入口

```go
type ForkSkillExecutor interface {
    Execute(ctx context.Context, invocation ForkSkillInvocation) (ForkSkillResult, error)
}

type ForkSkillRunStarter interface {
    Start(ctx context.Context, sessionID, skillName, args string) (*RunHandle, error)
}
```

执行器提交一个 `agent.DelegationTask` 给现有 `agent.Delegator`，使用共享 FIFO 池、每项最多 8 轮/3 分钟/50,000 字节工具输出/8 KiB 摘要及 64 KiB 输入限制。Start 为 slash 建立独立 run；tool 入口由 `ForkSkillProvider` 调用同一执行器并绑定父 run。

### 事件与审计扩展

协作状态继续使用 M09-A `agent.DelegationEvent`，包含 batch/task ID、名称、状态、阶段摘要及更新时间。事件适配器为 slash fork run 提供独立持久化/广播 sink，为 `LoadSkill` 继续使用父 run sink。`SkillInvoked` 增加可选 fork 模式及关联 RunID 元数据；日志记录脱敏后的参数，不记录技能正文或思考流。

## 模块设计

### `internal/conversation` 技能路由与上下文

`SkillGate` 保留 inline 路径，并为 fork skill 增加正文加载、参数渲染、`fork_context` 解析、输入上限验证和 `skill_invoked` 记录。它向 slash starter 与 `LoadSkill` provider 提供统一的内部解析结果。`ForkContextBuilder` 从对应 session 的事件投影中构造调用时可见消息，排除思考流和其他 session 内容；读取时与 session 事件写入协调，避免半条消息进入快照。

### `internal/agent` 只读子任务执行

复用 M09-A `PoolDelegator`、child runner 和资源池，不创建第二个 worker pool。Fork skill 正文和参数作为一个显式任务指令；上下文模式生成的历史仅用于模型输入。工具注册、provider/model 继承、授权根、审批流程、预算限制与取消均沿用 M09-A。fork 子 agent 不注册递归委派、MCP、网络、命令或写入工具。

### `internal/execution` 工具入口

扩展 `SkillProvider`/`executeLoadSkill` 的调用上下文，使 provider 可取得父 session/run 标识和调用时可见上下文来源。fork 技能由 fork provider 返回子 agent 的状态与摘要；inline skill 仍返回原有激活内容。父工具调用照常记录 `EventToolCall`，结果写入配对 `EventToolResult`。

### `internal/conversation` slash 独立 run 与事件持久化

`ForkSkillRunStarter` 为 slash 调用分配 run ID 和 session 序列，写入 `RunStarted`，并把子项协作事件适配到此 run 的 `RunEvent` 流，最终写入成功、失败、取消或中断终态。与现有 `StreamingRunner.PublishDelegation` 仅接受活动父 run 的行为分开：引入窄事件 sink 接口，使活动父 run 继续走现有发布器，独立 fork run 走 conversation 持久化 sink。重启恢复通过持久化 fork run 元数据识别尚未终结的 run，即使崩溃发生在第一条协作事件之前也能补记 `interrupted`；恢复不重跑。

### `internal/tui` 展示与续读

斜杠 run 沿用 `run_subscribe` 和游标续读，展示任务名、入口、排队/运行状态、阶段摘要与终态。`LoadSkill` 的协作事件在父 run 流中展示摘要状态，最终内容由父 agent 处理。两者都不展示思考流或 child 原始 transcript。

## 模块交互与数据流

### 斜杠命令入口

1. `Conversation` 收到 `/技能名 <参数>` 后，由 `SkillGate` 识别 fork skill，读取正文、按 M06 渲染参数并验证模式和 64 KiB 输入限制。未知模式或校验失败时报告错误，不启动子 agent。
2. `ForkSkillRunStarter` 为调用创建当前 session 的独立 run，持久化 `RunStarted` 和含来源、技能名及 run ID 的 `skill_invoked` 元数据，然后请求 `ForkContextBuilder` 构造快照。
3. `ForkSkillExecutor` 将指令、快照、父配置的 provider/model 和授权根交给 M09-A 共享池。队列和协作事件经 run 事件适配器写入此 fork run 的序列；TUI 按 run ID 与游标订阅状态。
4. 子项结束后，starter 持久化终态及过滤后的摘要或错误。服务重启时，未完成的 fork run 标记为 `interrupted`，不重跑；之后可按原 run ID 续读事件与结果。

### 父 agent 的 `LoadSkill` 入口

1. `ToolExecutor` 调用 `LoadSkill` 时，将父 session、父 run、授权根和当前可见对话快照来源传给 `ForkSkillProvider`。读取、渲染和校验步骤与斜杠入口共用。
2. `ForkSkillExecutor` 将单项任务提交给同一 M09-A 共享池。协作事件关联到父 run，并使用父 run 的事件序列、取消、权限及审批流程。
3. 子项结束后，provider 将状态、摘要或错误包装为工具结果交回父 agent；不向父消息内联技能正文，也不向父 agent 交付 child 原始 transcript。
4. 若服务在工具调用期间重启，恢复流程将子项标记为 `interrupted`，并为未配对的工具调用写入明确中断结果，保证事件序列完整且不重复执行。

### 上下文与事件处理

- `ForkContextBuilder` 在调用时从对应 session 的可见对话构造快照：`none` 为空，`recent` 取最近 5 轮，`full` 取预算内完整可见历史。构造期间协调 session 事件读写，避免读取未完成消息；三种模式均排除思考流，且不读其他 session。
- M09-A runner 继续负责只读工具执行、资源池排队与单项限额。事件适配器区分“父 run 内子项事件”和“斜杠独立 run 事件”的落点，复用同一协作事件类型、脱敏、大小限制与终态规则。
- TUI 展示技能名、入口、排队/运行状态、阶段摘要及终态，不展示思考流。斜杠入口可查看独立 run 的事件流；`LoadSkill` 的最终结果由父 agent 汇总。

## 文件组织

按现有目录扩展 fork skill 路径；M09-A 的通用委派逻辑继续复用。实现任务拆解时再确认具体文件边界。

```text
internal/conversation/
├── skills.go                       — fork/inline 分流、正文与参数解析、激活审计
├── fork_skill.go                   — ForkSkillCoordinator、ForkContextBuilder、Slash Run starter
├── fork_skill_events.go            — 父 run 与独立 fork run 的事件 sink 适配
├── fork_skill_recovery.go          — 未终结 fork run 与工具调用的中断修复
├── fork_skill_test.go              — 两种入口、上下文模式、校验与部分失败
└── fork_skill_recovery_test.go     — 中断修复、工具结果配对与幂等恢复

internal/execution/
├── executor_factory.go             — fork skill provider 与父 run 上下文接线
├── tool_executor.go                 — LoadSkill 调用上下文和结果映射
└── tool_executor_fork_skill_test.go — 工具入口、父 run 结果及回归

internal/agent/
├── delegation.go                    — 复用既有 PoolDelegator、任务与预算接口
└── delegation_runner.go              — 复用只读 child runner，不新增独立池

internal/sessionlog/
├── events.go                         — SkillInvoked 的 fork 模式和关联 run 元数据
├── validate.go                       — 扩展 fork 激活事件校验
├── projection.go                     — 如独立 run 恢复所需，投影 fork 元数据
└── fork_skill_test.go                — 新增审计字段与投影验证

internal/tui/
├── model.go                          — slash fork run 的启动及订阅接线
├── transcript.go                     — fork skill 状态和摘要展示
└── fork_skill_test.go                — 两种入口进度与终态显示

specs/M09-B/
├── spec.md
├── plan.md
├── task.md
└── checklist.md
```

新增文件只承载 fork skill 专属编排、事件适配与恢复；子 agent 执行、资源上限和只读工具集复用 M09-A。若实现时发现现有 run 事件或恢复接口可直接承载某项职责，可合并到现有文件，但保持上述职责边界及验收行为。


## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 执行入口 | 斜杠命令创建 session 下的独立 fork run；`LoadSkill` 创建关联父 run 的 child run | 复用现有 run 生命周期，同时满足用户查看和父 agent 汇总两种用法。 |
| fork 执行器 | 两个入口共用 `ForkSkillExecutor`，提交单项任务到 M09-A `PoolDelegator` | 复用只读工具集、权限门、provider/model、资源池和预算，不引入第二套子 agent 执行框架。 |
| 事件发布 | 用窄事件 sink 抽象事件落点：`LoadSkill` 发到活动父 run；slash 发到持久化的独立 fork run | M09-A 当前 reporter 面向活动父 run；slash run 需要自己的序列、持久化和游标订阅。分开落点可保留共用事件格式与状态语义。 |
| 斜杠 run 恢复识别 | 在 `RunStarted` 或同一持久化 run 元数据中标记 fork skill 关联信息，并记录 `skill_invoked` 的 run ID | 即使进程在首条协作事件前退出，恢复流程也能找到未终结 fork run 并标记 `interrupted`。 |
| `LoadSkill` 调用上下文 | 扩展 provider 接口，传递父 run 标识及调用时可见对话快照来源；不使用 executor 创建时的初始 request 消息代替当前视图 | 工具调用发生时父对话已继续推进；按 session log 取调用时快照才能实现 `recent/full` 语义。 |
| 上下文模式 | `none` 默认；`recent` 从可见 session 历史取最近 5 轮；`full` 受当前模型上下文预算裁剪 | 与用户确认的窗口与预算要求一致，同时让无上下文执行保持最小暴露。 |
| 资源与并发 | 复用 M09-A：全服务最多 3 个运行项、32 个排队项；每项最多 8 轮、3 分钟、50,000 字节工具输出、8 KiB 摘要，输入最多 64 KiB | 统一资源管理和硬限制，避免 fork skill 绕过已批准的子 agent 限额。 |
| 持久化格式 | 协作状态作为 run 事件保存；`SkillInvoked` 记录入口、技能来源和可选 fork run 关联；不持久化思考流或 child transcript | 复用现有 session log、脱敏和按游标恢复能力，并保持记录最小化。 |
| 服务重启 | 未终结的 slash fork run 标为 `interrupted`；父 run 中未完成的 `LoadSkill` 同时补齐子项终态和工具结果；都不自动重跑 | 保证 run 事件和工具调用结果配对，且不把不确定完成状态误当成功。 |
| `fork_context` 错误 | 仅接受 `none`、`recent`、`full`；未知值在排队前报错 | 避免配置拼写错误导致意外扩大上下文或产生无上下文调用。 |
| inline 兼容 | fork 与 inline 在 `SkillGate` 分流；inline 仍沿现有正文注入和工具结果路径 | 将新执行生命周期限制在 fork 技能，不改变已存在的 inline skill 行为。 |
| 交互展示 | TUI 展示技能名、入口、排队/运行状态、阶段摘要和终态，不展示思考流或原始 transcript | 提供用户所需的可观察性，不暴露内部模型推理内容。 |
| 其他 M09 能力 | 本项不实现 hook agent、后台恢复执行或工作树写入 | 保持 M09-B 范围独立，避免引入尚未规格化的生命周期和写入行为。 |

## Plan 自检

- **Spec 覆盖：** F1–F7 已分别落到入口路由、统一执行器、上下文构造、只读执行、事件与 TUI、取消/恢复和审计扩展。N1–N5 分别由共享权限门、M09-A 有界资源、事件脱敏、run 事件与幂等恢复、inline 兼容路径承接。
- **接口完整性：** 执行输入、上下文构建、结果结构、两类事件落点均已定义；`LoadSkill` 父 run 与 slash 独立 run 的调用关系及恢复责任有明确描述。
- **依赖清晰：** agent/execution 协调层通过窄事件 sink 依赖 conversation/sessionlog；TUI 订阅持久化事件，不反向参与执行。新增恢复标记放入既有 run 元数据或事件。
- **矛盾检查：** 两个入口、三种上下文模式、资源预算、只读边界、隐私、取消与重启不重跑行为符合已批准 Spec。
