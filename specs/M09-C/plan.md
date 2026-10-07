# M09-C Hook Agent 执行 Plan

> 状态：已批准（2026-10-07）。本文建立在已批准的 [spec.md](spec.md) 上。`task.md` 与 `checklist.md` 获批前不开始实现。

## 架构概览

保留 `HookGate` 作为 hook 事件入口。`agent` action 交给新增的 `HookAgentCoordinator`，由它组装提示、事件字段和父 run 执行范围，并提交到 M09-A 的 `PoolDelegator`。子项事件、取消和终态归属触发它的 Session/Goal run；完成结果回到 HookGate，沿用现有 journal、脱敏、截断和通知回流。

pre_tool_use 使用同步路径；其它事件按 `async` 设置执行。已有 `command`、`prompt`、`http` action 保持当前执行路径。

## 核心数据结构与接口

### `HookAgentInvocation`

```go
type HookAgentInvocation struct {
    Parent      agent.ParentRun
    HookID      string
    Instruction string
    Event       hooks.Context
    Timeout     time.Duration
}
```

`Parent` 携带 run/work、provider/model、项目根、权限边界及 child executor factory；`Instruction` 使用 action `message`，为空时回退 `command`；`Event` 只包含当前 hook 的事件名、工具名、参数、文件路径和消息，不包含对话历史。调用参数与编码后的事件上下文合计受 64 KiB 限制。

### `HookAgentExecutor`

```go
type HookAgentExecutor interface {
    Execute(context.Context, HookAgentInvocation) (agent.DelegationResult, error)
}
```

执行器负责创建一项 `agent.DelegationTask` 并提交共享池。结果沿用 `agent.DelegationResult`，因此复用 queued/running/succeeded/failed/canceled/interrupted 终态与摘要上限。

### 共享池单项接口

```go
type Delegator interface {
    RunBatch(context.Context, ParentRun, []DelegationTask) ([]DelegationResult, error)
    RunTask(context.Context, ParentRun, DelegationTask) (DelegationResult, error)
}
```

`RunTask` 支持 Session 与 Goal WorkRef；现有 `RunBatch` 仍保持 M09-A 的 Session-only 约束。两者复用同一服务级 FIFO worker/queue、child runner、进度 reporter、取消链和单项预算，不增加第二套池。

`DelegationEvent` 携带所属 session ID，使服务在 run_end 子项继续运行、父 run 已退出活动表后仍能把协作事件写回原父 run 的持久事件流。

### 父 run 范围传递

run_start/run_end 由 conversation 直接提供 run 范围。工具前后 hook 通过扩展 `execution.HookRunner` 调用信息传入当前 `agent.ParentRun` 所需字段，以保留父 run ID、WorkRef、provider/model、项目授权根和权限边界。调用链只传执行所需范围，不传完整对话快照。

## 模块设计

### `internal/agent`

扩展 `PoolDelegator` 的单项提交能力。`RunTask` 校验父 run 必需字段、Session/Goal WorkRef、任务输入和预算，并在同一 FIFO 资源池排队。内部 worker 仍调用现有只读 child runner，沿用 M09-A 事件发布及取消路径。批量 `RunBatch` 的入口、验证和行为保持不变。

### `internal/conversation`

`HookGate` 识别 `agent` action 并委派给 `HookAgentCoordinator`；其它 action 继续走 `hooks.FireOne`。coordinator 负责 message/command 回退、输入编码与限额、timeout 计算、结果映射、关联父 run、跟踪异步任务及取消。HookGate 继续负责 hook journal、结果脱敏/截断和通知队列。

对于非 pre_tool_use 的 async action，服务维护按父 run 关联的 task handle，避免由无管理的裸 goroutine 执行。run_end 在父 run 到达终态后触发；若父 run 因取消进入终态，其 run_end agent hook 仍启动一次有界任务，使用 service lifetime 管理。父取消会停止取消发生时已排队或运行的子项；服务关闭/重启会将未完成子项中断并记录，不重跑。

### `internal/execution`

扩展 `HookRunner` 接口及 `ToolExecutor` 调用点，将当前 run/work 范围随 pre_tool_use/post_tool_use 一并传递。pre_tool_use 保持在 permission gate 与实际工具执行之前，等待 hook 结果；静态 `reject` 或 `on_error: reject` 仍由既有逻辑决定是否拦截。

### `internal/hooks`

保留 YAML 字段和 `agent` 必填提示校验。运行时通过 conversation 注入的 agent executor 分派，不让 hooks 包依赖 agent/conversation；直接执行其它 action 的原有代码和配置语义不变。Agent action 的 timeout 由 coordinator 使用：0 表示 3 分钟，正值不超过 3 分钟上限。

### `internal/sessionlog` 与 `internal/tui`

协作 queued/running/terminal 状态复用带所属 session ID 的 `DelegationEvent` 并写入父 run 流。`HookFired` 增加 child run ID 与超时标志，校验 child ID 长度。TUI 复用 delegation 渲染，显示 hook ID、任务状态、阶段摘要与终态；事件按现有 run ID/cursor 订阅和恢复，不展示思考流或 child transcript。

## 模块交互与数据流

1. **事件收集：** run_start/run_end 从 `Conversation` 取得父 run 范围；pre/post_tool 从执行器接收当前 run/work 范围及 hook 事件字段。HookGate 按配置条件筛选，并保留既有 hook 顺序。
2. **任务构造：** 命中 agent action 后，coordinator 选择 `message` 或回退 `command`，编码提示和当前事件字段；输入超过 64 KiB 或 timeout 非法时不提交子项，并将失败交回 hook 错误处理。
3. **池化执行：** coordinator 调用共享池 `RunTask`。池分配 child run ID 并在父 run 事件流中发布 queued/running/terminal 协作状态。child runner 使用继承的 provider/model、WorkRef、授权根、permission bounds 与 M09-A 只读工具集。
4. **同步和异步：** 同步 action 等待 child 终态；非 pre_tool_use 的 async action 将受服务生命周期跟踪并立即返回。pre_tool_use 即使 `async: true` 也等待结束，再进入静态拒绝逻辑和 permission gate。run_end child 使用 service lifetime，避免父 run 已终态时继承已取消的上下文。
5. **结果处理：** 成功摘要作为 hook 输出；失败、超时、队列已满、取消和中断映射为明确错误。结果脱敏与截断后写入 HookFired，并将非空输出加入现有通知队列；`reject` 仅由配置决定，模型输出不产生 allow/deny。
6. **恢复与展示：** 父 run 的事件订阅按游标展示协作状态。服务重启将未终结 child 标为 `interrupted`，不自动重跑；HookFired 保留 child run ID 与最终原因。

## 文件组织

```text
internal/agent/
├── delegation.go                    — RunTask 单项接口与共享池执行
└── delegation_test.go               — Session/Goal、预算、队列和取消

internal/conversation/
├── hooks.go                          — HookGate agent 分派、父 run 范围、通知/journal
├── hook_agent.go                     — invocation 构造、协调、异步 task 跟踪
└── hook_agent_test.go                — action 输入、失败映射、Session/Goal 与生命周期

internal/execution/
├── executor_factory.go               — 扩展 HookRunner 调用范围
├── tool_executor.go                  — pre/post_tool 传递父 run/work 上下文
└── tool_executor_test.go             — hook 顺序与旧 action 回归

internal/hooks/
├── hooks.go                          — agent action 校验与 executor 分派
└── hooks_test.go                     — 提示回退及其它 action 兼容

internal/sessionlog/
├── events.go                         — HookFired 子 run 关联字段
├── validate.go                       — 新字段校验
└── hook_test.go                      — 持久化、投影与大小约束

internal/tui/
├── transcript.go                     — hook 子项阶段与终态展示
└── hook_agent_test.go                — 父 run 事件恢复和显示

specs/M09-C/
├── spec.md
├── plan.md
├── task.md
└── checklist.md
```

文件边界按职责分配；实现时如果现有文件已承载对应逻辑，可以合并，但不得改变批准的接口行为和验收范围。

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 执行器注入 | HookGate 使用 `HookAgentCoordinator`；hooks 包不依赖 agent/conversation | 保持配置/动作模型与生命周期编排分层，避免包循环。 |
| Session/Goal | 新增单项 `RunTask` 支持两类 WorkRef；批量 `RunBatch` 保持 Session-only | 满足 M09-C 事件覆盖，同时不改变 M09-A 通用委派入口。 |
| child 身份 | 每个 hook agent 调用一个 child run；协作事件在所属父 run 流，HookFired 记录 child run ID | 复用现有事件游标和 TUI 进度展现，并可追溯 hook 结果。 |
| 上下文 | 只传提示、当前事件字段和执行范围，不传父会话历史 | 满足最小上下文边界，避免 hook action 扩大历史可见范围。 |
| 权限 | 复用 M09-A 只读工具、授权根、permission gate 和 provider/model | 子项能力与父 run 权限一致，不增加网络/写入/命令权限。 |
| timeout | agent action 0 使用 3 分钟；正值取配置值和 3 分钟中的较小值；其它 action 不变 | 遵循 M09-A 单项硬上限，同时允许配置更短超时。 |
| async | 非 pre_tool_use 异步跟踪；pre_tool_use 即使标 async 也同步完成 | 拦截必须先于 permission gate，避免异步绕过静态拒绝或错误拒绝。 |
| 父 run 取消 | 取消时停止已排队/运行 child；随后触发的 run_end agent hook 继续执行一次有界任务 | 保留 M07 run_end 对取消终态的触发语义；run_end 已是新的结束事件。 |
| 服务重启 | 未终结 child 标记 `interrupted`，不自动重跑 | 与 M09-A/B 恢复策略一致，避免不确定任务重复执行。 |
| 持久化 | 复用 DelegationEvent 父 run 流；HookFired 增加 child run ID | 状态可按游标恢复，结果可从 hook 审计定位至 child。 |
| 资源 | 3 worker、32 queue、8 rounds、3 分钟、50,000 字节工具输出、8 KiB 摘要、64 KiB 输入 | 与已批准 M09-A/B 资源上限保持一致。 |
| 验证 | fake provider 覆盖 Session/Goal、权限、拒绝顺序、预算、取消和恢复；重型集成测试使用已授权 GitHub Actions | 避免真实 provider/外网依赖，并按用户资源偏好安排重型验证。 |

## Plan 自检

- **Spec 覆盖：** F1–F7 对应 action 输入、执行范围、同步/异步、拒绝语义、预算、生命周期、审计；N1–N5 对应权限、资源、恢复、留存和兼容。AC1–AC8 均在模块职责或验证路径中覆盖。
- **接口完整性：** Hook invocation、执行器、单项共享池路径和父 run 范围传递均已定义；Session/Goal 的数据流及父取消/run_end 语义明确。
- **依赖清晰：** hooks 负责配置与分派，conversation 负责编排，agent 负责共享池执行，execution 只传递父 run 范围，sessionlog/TUI 承载可恢复状态。
- **矛盾检查：** `pre_tool_use` 强制同步；reject 为静态；父 run 普通取消与 post-terminal run_end 的生命周期分别定义；服务重启中断且不重跑；共享池和单项预算与 M09-A/B 一致。
