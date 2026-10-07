# M09-A 协作事件与只读协调原型 Plan

> 状态：已批准（2026-10-07）。本文建立在已批准的 [spec.md](spec.md) 上，定义 M09-A 的 Go 架构、接口、数据流、文件边界和技术决策。四份规格文档批准前不开始实现。

## 架构概览

M09-A 在现有 session run 与工具调用框架上增添一个同步委派能力。用户的 `/delegate <任务>` 仍通过普通 session run 启动父 agent；父 agent 的新 `delegate_tasks` 工具一次接收一组子任务。工具将整批任务送入服务级 FIFO 资源池，等待所有任务进入终态，再以提交顺序把逐项结果返回父 agent。批次不脱离父 run，也不成为后台任务。

新增 `internal/agent` 协调层拥有服务级有界 worker 池、批次生命周期和只读子 run 调度。它通过依赖接口调用现有 provider 和执行框架，不接触 TUI 或持久化实现。每个子 run 独立持有取消上下文、预算计数及状态；任意时刻最多运行 3 个子任务，排队区最多容纳 32 个待处理任务。批次可提交任意数量的任务，但编码请求总量不超过 64 KiB；超过队列可接纳容量时，提交方等待 FIFO 空位并传播取消。

新增 `internal/execution` 适配层把 `delegate_tasks` 暴露为普通父 run 可用的工具，并构造受限子工具集。子 agent 复用父 run 的 provider/model、授权项目根、现有权限门和 sandbox 约束，只能使用只读的文件读取、搜索和目录列举能力。它们没有 MCP、网络工具、命令、写工具或委派工具。provider 请求仍经父 run 已配置的模型 provider 发出。

新增的协作生命周期事件作为父 `RunEvent` 写入现有 session event log，沿用已有序列、订阅、游标重放和 TUI 投影路径。事件只记录任务名、状态、阶段摘要和必要错误，不写入思考流或子 agent 原始 transcript。父 run 恢复时，系统先完成未终结子任务的 `interrupted` 事件，再将父 run 和尚未配对的委派工具调用结果一并终结；该过程幂等且不重跑任务。

架构职责覆盖：F1 由 TUI 命令入口启动普通 run；F2、F4、F5、F8、F9 由委派工具和协调器处理；F3、N1 由只读执行器和现有 permission/sandbox gate 保证；F6、F7、N3、N4 由事件日志、恢复逻辑和 TUI 投影保证；N5 通过复用现有会话、权限和候选流程且不添加候选写入实现；N6 由后续 task/checklist 中定义的本机验证覆盖。

## 核心数据结构与接口

### DelegationTask

```go
type DelegationTask struct {
    ID          string
    Name        string
    Instruction string
}
```

`ID` 在批次内唯一；`Name` 用于 TUI 和结果显示；`Instruction` 是子 agent 收到的显式任务。批次输入只含上述任务和协调所需的父 session/run 标识及只读上下文，不复制对话历史。任务数不设置单独上限，序列化后的整批输入不得超过 64 KiB。

### DelegationStatus

```go
type DelegationStatus string

const (
    DelegationQueued     DelegationStatus = "queued"
    DelegationRunning    DelegationStatus = "running"
    DelegationSucceeded  DelegationStatus = "succeeded"
    DelegationFailed     DelegationStatus = "failed"
    DelegationCanceled   DelegationStatus = "canceled"
    DelegationInterrupted DelegationStatus = "interrupted"
)
```

状态只能沿 `queued → running → terminal` 或 `queued → terminal` 前进。终态只写一次。队列容量或请求大小限制拒绝不产生伪造的子任务终态；工具调用返回明确的提交错误。

### DelegationEvent

```go
type DelegationEvent struct {
    BatchID   string
    TaskID    string
    TaskName  string
    Status    DelegationStatus
    Stage     string
    Summary   string
    Error     string
    UpdatedAt time.Time
}
```

事件由既有父 `RunEvent` 包装，增加批次/任务关联字段；`Stage` 和 `Summary` 只承载面向用户的非思考流进度。最终摘要最多 8 KiB；执行器累计工具输出沿用现行 50,000 字节上限。达到输出限制时需截断并在摘要中说明，不得静默丢失状态或错误。

### DelegationResult

```go
type DelegationResult struct {
    TaskID string
    Name   string
    Status DelegationStatus
    Summary string
    Error   string
}
```

结果按提交顺序返回，包含成功摘要或失败、取消、中断原因。混合结果保留成功项；全失败批次也返回每个子项的状态，不报告整批成功。

### DelegationLimits

```go
type DelegationLimits struct {
    Workers          int           // 默认 3
    QueueCapacity    int           // 默认 32
    MaxInputBytes    int           // 默认 64 KiB
    MaxToolRounds    int           // 每项默认 8 轮
    MaxDuration      time.Duration // 每项默认 3 分钟
    MaxSummaryBytes  int           // 每项默认 8 KiB
    MaxToolOutputBytes int         // 沿用现有 50,000 字节
}
```

有效时长不得超过父 run 的剩余时间；所有子任务同样受父 run 剩余授权与预算限制。资源池运行数与排队数均有硬上限，批量等待通过队列空位和 context 取消实现，不为任意大批次预先创建等量 worker goroutine。

### Delegator

```go
type Delegator interface {
    RunBatch(ctx context.Context, parent ParentRun, tasks []DelegationTask) ([]DelegationResult, error)
}
```

`RunBatch` 校验任务、父 run 类型和输入上限，为每项分配子 run ID 并提交至共享 FIFO 池；等待每项终态，按输入顺序返回结果。提交级错误通过 `error` 返回；单项执行错误写入对应 `DelegationResult`，不使其余任务丢失。该接口只接受 session run 父上下文。

### ChildRunner 与只读执行接口

```go
type ChildRunner interface {
    Run(ctx context.Context, input ChildRunInput) ChildRunResult
}

type ChildRunInput struct {
    ParentRunID string
    ChildRunID  string
    Task        DelegationTask
    ProjectRoot string
    Provider    llm.Provider
    Model       string
    Budget      DelegationLimits
}
```

具体实现沿用现有 agent runner/provider 抽象，建立精简系统上下文，只暴露 `read_file`、`glob`、`grep` 三类只读能力。项目根来自父 run 已授权根，文件操作仍通过现有权限门及 sandbox；本阶段不绕过或另造审批逻辑。子 runner 只返回最终结果和阶段事件，不向父 run 暴露原始对话或思考流。

### ProgressReporter 与事件载体

协调器依赖窄接口 `ProgressReporter.Publish(parentRunID, event)` 发布状态；runtime 将事件转换为父 run 事件并交由 conversation/sessionlog 持久化。错误通过现有 run 错误路径处理；发布事件失败不能伪造成功状态，应使父工具调用失败并保留可恢复的状态信息。

## 模块设计

### `internal/agent`

**职责：** 服务级 worker 池、FIFO 等待队列、批次和任务状态机、逐项取消、预算和输出限制、子 run 调用以及进度事件生成。

**对外接口：** `Delegator.RunBatch`；通过 `ChildRunner`、`ProgressReporter` 和配置型 `DelegationLimits` 注入 provider runner、事件写入与资源上限。

**依赖：** 标准库 context/time/sync、现有 `internal/llm` provider 与 agent runner 的最小抽象。不得依赖 TUI、具体 sessionlog 存储或新的外部服务。

**并发语义：** 一个 runtime/supervisor 持有一份全服务池；池容量默认 3 worker、最多 32 个排队项。多个父 run 共用池和 FIFO 次序。队列满时调用阻塞等待可用位置，同时监听父 context；父取消后移除或终结其尚未启动项。已运行项收到取消并进入 `canceled`。子任务完成后释放 worker 和队列容量。

### `internal/execution`

**职责：** 注册父 run 的 `delegate_tasks` schema 与执行器；将工具参数校验后调用 `Delegator`；构建仅含读、搜、列工具的 child executor。

**对外接口：** 现有 `ToolExecutor` / `ExecutorFactory` 的扩展点，以及委派工具的参数解析和结果序列化。

**依赖：** `internal/agent`、`internal/permission`、现有 sandbox、项目路径解析和 provider/runtime 配置。委派工具仅在 session WorkSession 中注册；其它 run 类型不可见也不可调用。子 executor 不注册 `delegate_tasks`，不注册 MCP 或网络工具。

**权限行为：** 所有子工具沿用同一权限门和授权根。若当前策略要求审批，继续走现有审批流程；拒绝、越权路径和工具错误作为子任务失败原因返回。

### `internal/runtime` 与 `cmd/stable`

**职责：** runtime/supervisor 创建一份共享 `Delegator` 和有界池，注入普通 session run 的 executor factory；CLI/TUI 命令解析 `/delegate <任务>` 并以普通用户 prompt 方式启动 run。

**依赖：** 现有会话服务、provider/model 配置和 run 管理。取消由现有父 run context 向 delegator 传播；不添加后台任务或新的会话入口协议。

### `internal/conversation` 与 `internal/sessionlog`

**职责：** 将子任务进度/终态写入父 run 事件流，保持现有游标和订阅语义；启动恢复时识别未终结的委派状态、修复子状态和未闭合工具调用，再恢复会话服务。

**恢复约束：** 对每个尚未终态的子任务，幂等追加 `interrupted`。若父 run 有未闭合的 `delegate_tasks` `EventToolCall`，同时追加对应的 `EventToolResult`，内容明确表示服务重启中断；父 run 标记 `interrupted`。恢复不重新调度子任务。既有事件日志写入/投影接口是唯一持久化路径，不新增数据库或客户端协议。

### `internal/tui`

**职责：** 渲染按任务聚合的 `queued`、`running`、阶段摘要和终态；按现有 `run_subscribe` 的序列游标消费或补取事件；不展示思考流。

**命令体验：** `/delegate` 缺任务时显示用法，不建立 run；有效输入转发给普通父 run，后续拆分及委派由父 agent 决定。现有 run 的展示和输入行为不改变。

## 模块交互与数据流

1. 用户输入 `/delegate <任务>`；TUI 将去除命令前缀的任务作为普通 prompt，调用现有会话 run 启动接口。空参数只显示用法。
2. 父 agent 按现有工具循环分析任务；需要并行调查时调用 `delegate_tasks`，提交一组任务。现有 ToolExecutor 先记录 `EventToolCall`，再进入执行器。
3. `internal/execution` 校验父 run 是 WorkSession、参数格式、每任务唯一 ID 和整批输入 ≤64 KiB；创建子运行输入时仅复制任务、授权项目根、provider/model 引用和限额，不复制会话 transcript。
4. `Delegator` 先持久化每项 `queued` 事件，再按全服务 FIFO 池接纳任务。池最多运行 3 项、最多保留 32 个排队项。调用方受队列满背压；取消能中断排队等待。
5. worker 发出 `running` 事件，构造只读 child executor，并使用独立 child run ID 运行现有 agent loop。每项限制 8 轮工具调用、3 分钟（且不超过父剩余时长）、50,000 字节工具输出和 8 KiB 最终摘要。
6. child runner 通过 reporter 发布 `running` 阶段摘要；协调器将最终成功、失败或取消事件提交至父 run 事件日志，并生成按原输入顺序排列的 `DelegationResult`。状态事件只含面向用户的阶段信息，不写思考流。
7. `ToolExecutor` 将整个批次的逐项结果写为配对 `EventToolResult`，父 agent 读取后自行综合并继续普通 run。
8. 用户断线后，TUI 使用现有游标重新订阅父 run 事件，重放协作事件以恢复列表和状态。
9. 服务重启时，在对外接受新 session run 前恢复检查委派事件及未闭合工具调用。恢复将排队/运行中的子项标记 `interrupted`，将父 run 标记 `interrupted` 并补写工具结果配对。所有写入以 run/batch/task 终态作为幂等依据；不重跑。
10. 用户取消父 run 时，取消 context 传播到所有运行中 child；排队任务进入 `canceled`，完成事件和可用结果落盘，委派工具完成其逐项结果返回路径。

## 文件组织

```text
internal/agent/
├── delegation.go                  — DelegationTask/Result/Limits、Delegator、RunBatch
├── delegation_test.go             — FIFO、有界 worker、背压、取消、部分失败
├── events.go                      — 协作状态和进度事件
├── executor.go                    — ChildRunner/只读子 run 执行与预算限制
└── runner.go                      — 复用现有 agent loop 的 child runner 适配

internal/execution/
├── delegation_tools.go             — delegate_tasks schema、参数和结果映射
├── executor_factory.go             — 注入 Delegator 并创建受限 child executor
├── tool_executor.go                — 父 tool call/result 配对及委派执行集成
└── delegation_tools_test.go        — WorkSession 限定、工具边界、失败映射

internal/runtime/
└── supervisor.go                   — 组装全服务共享的 Delegator/worker pool

cmd/stable/
├── chatserve.go                    — /delegate 命令解析及普通 run 启动
└── chatserve_test.go               — 命令缺参和正常 run 行为

internal/sessionlog/
├── events.go                       — 父 RunEvent 的协作字段/类型
├── log.go                          — 现有持久化写入路径扩展
├── validate.go                     — 协作事件状态与字段合法性
└── projection.go                   — 如当前恢复投影需要，投影子任务状态

internal/conversation/
├── service.go                      — runtime reporter 接线和恢复入口
├── run.go                          — 父 run 事件写入/订阅接线
└── delegation_recovery_test.go     — interrupted 修复与恢复幂等

internal/tui/
├── model.go                        — /delegate 输入路由（如命令解析位于此处）
├── transcript.go                   — 协作事件投影与文本渲染
└── delegation_test.go              — 游标续读和状态展示

README.md                           — 说明 /delegate 的同步只读行为和限制
specs/M09-A/
├── spec.md
├── plan.md
├── task.md
└── checklist.md
```

文件为设计目标，任务拆解时会根据实际现有接口细化创建/修改清单。若 repo 中 sessionlog 包或路径名称与现有组织不同，在不改变本 plan 接口和行为的前提下调整到既有 session event log 所属包。

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 用户入口 | `/delegate <任务>` 映射为普通 WorkSession prompt | 父 agent 负责拆解，沿用既有 run 生命周期、预算、取消和权限行为。 |
| 父 agent 工具入口 | 单个 `delegate_tasks` 批量同步工具，仅对 WorkSession 开放 | 一次调用并行提交并收齐结果，避免增加独立控制 API 或后台任务状态机。 |
| 执行方式 | 子 agent 运行现有 agent loop，独立 child run ID | 复用 provider/tool loop 机制，隔离每项预算、取消及事件关联。 |
| 子工具权限 | 只注册 `read_file`、`glob`、`grep`，沿用父授权根、permission gate 与 sandbox | 能力严格收窄，并保留既有审批语义；不引入新权限审批机制。 |
| 上下文继承 | 只传显式子任务和必要项目只读上下文 | 防止完整对话、秘密或兄弟结果无必要扩散。 |
| 模型选择 | 复用父 run 的 provider/model | 保持部署配置、provider 费用路径和行为一致；provider 网络通信是执行模型所必需。 |
| 资源池 | 服务级 FIFO；默认 3 个活动 worker、32 个队列项；批次任务数无硬性计数上限，整批输入 ≤64 KiB | 让不同父 run 共用资源上限，避免超大并行；队列满时阻塞背压而非无限堆积。 |
| 单项上限 | 最多 8 轮工具调用、3 分钟、50,000 字节工具输出、8 KiB 摘要；时长受父剩余预算进一步收紧 | 同时约束模型迭代、时间和记录大小。 |
| 事件持久化 | 协作事件作为父 RunEvent 写入既有 session event log | 复用序列、游标订阅、断线重放和现有脱敏策略，不另加数据库或协议。 |
| TUI 展示 | 仅展示任务名、排队/运行状态、阶段摘要及终态 | 提供协作可见性，不暴露思考流。 |
| 失败语义 | 单项终态和原因逐项保留，结果按提交顺序返回 | 混合成功不丢失；调用方可以准确综合失败项。 |
| 父取消 | 取消所有活跃子项，并将排队项终结为 canceled | 子任务生命周期受父 run 所有，避免孤儿任务。 |
| 服务重启 | 不自动重跑；幂等追加子项 interrupted、父 run interrupted 和未闭合委派工具的 EventToolResult | 让恢复后的模型上下文保持工具调用配对，不把未知完成状态误作成功。 |
| 审批 | 子只读工具需要审批时复用当前 permission flow | 保持用户既有授权和审批控制的一致性。 |
| 本轮范围 | 不接入 M07 fork skill/hook agent、后台任务和工作树写入 | 这些需要各自的入口、恢复或受控写入设计，超出 M09-A 已批准范围。 |

## Plan 自检

- **Spec 覆盖：** F1–F9 在架构概览中逐项标明归属；N1–N6 分别由只读执行边界、资源池、日志过滤、状态机与恢复、无候选写入的集成边界以及后续本机验证承接。
- **接口完整性：** `Delegator`、`ChildRunner`、输入/结果/事件/预算结构覆盖提交、执行、进度、取消和返回；运行时、持久化与 TUI 通过依赖接口接线。
- **依赖清晰：** TUI → conversation/runtime → execution → agent coordinator/child runner → provider 与受限工具；事件 reporter 从 agent 层回到父 run event log。agent 协调层不反向依赖 TUI 或具体持久化实现。
- **矛盾检查：** 同步批次、父取消、重启不重跑、只读边界与 spec 一致；并发 3、队列 32、8 轮、3 分钟、64 KiB 输入、8 KiB 摘要及 50,000 字节工具输出均为本阶段批准值。
