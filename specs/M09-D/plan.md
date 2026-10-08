# M09-D Agent 定义与后台任务 Plan

> 状态：四份文档已获用户批准；实现与验收完成（2026-10-08）。AC1–9 和完整场景的逐项证据见 [checklist](checklist.md)。

## 架构与职责

新增 `internal/agentcatalog` 负责定义的读取、严格 frontmatter 校验、内建角色、来源覆盖及快照。它不启动模型或写目标事实。服务侧新增 `BackgroundAgentCoordinator`，将角色正文和调用者指令编码为一项只读任务，构造独立 run 与受信作用域，持久化接受记录并提交共享池。

共享池增加可观察的异步提交句柄；现有 `RunTask`/`RunBatch` 继续保持原调用语义。后台状态、查询、取消、通知与恢复归 conversation/sessionlog 所有；execution 将父 agent 工具调用映射到该服务，TUI 使用 session/run 游标显示任务并提供 slash 命令。

## 核心接口

```go
// internal/agentcatalog
type Definition struct {
    Name, Description, Model, Instruction, Source string
    Tools, DisallowedTools []string
    MaxTurns int
    Background bool
}
type Snapshot struct { Definitions []Definition; Rejections []string }
type Catalog interface {
    Snapshot() Snapshot
    Reload() Snapshot
    Resolve(name string) (Definition, bool)
}

// internal/agent
type TaskHandle struct {
    BatchID, TaskID string
    Results <-chan DelegationResult
    Cancel context.CancelFunc
}
type TaskSubmitter interface {
    SubmitTask(context.Context, ParentRun, DelegationTask) (*TaskHandle, error)
}

// execution calls a narrow service interface; it must not import conversation.
type AgentTaskRequest struct {
    AgentName, Instruction, Model string
    Background bool
    Timeout time.Duration
}
type AgentTaskSnapshot struct {
    ID, RunID, OriginRunID, SessionID, AgentName, Name string
    Status DelegationStatus
    Summary, Error string
}
type AgentTaskService interface {
    Run(context.Context, ParentRun, AgentTaskRequest) (AgentTaskSnapshot, error)
    Output(context.Context, ParentRun, string, time.Duration) (AgentTaskSnapshot, error)
    Stop(context.Context, ParentRun, string) (AgentTaskSnapshot, error)
}
```

字段约束及 JSON 键在实现前由协议任务统一定稿；job/task 的外部 ID 为服务生成的不透明 ID。任务 ID 不取用户路径或名称。Definition 和公开目录列表分开投影，列表不返回 Instruction。

## 共享池提交与资源控制

- `SubmitTask` 复用父作用域、任务输入校验和已有 FIFO 队列；采用非阻塞入队，满时返回明确错误。不为尚未接受的请求启动 goroutine。
- 接受与服务关闭互斥：queued 事件持久化成功且任务进入池后才能返回 handle；queued 记录失败不启动 runner。只启动现有 3 workers，句柄使用单个容量为 1 的终态结果通道。
- `RunTask` 在句柄上同步等待，保持现有 queue-full 结果映射；`RunBatch` 保持 Session-only、提交顺序和现有背压语义。异步 API 不改变 M09-A/B/C 的错误结果。
- 每项限制由共享池归一化；definition maxTurns 和 invocation timeout 只能收窄。为 parent 作用域增加可选单项收窄预算，由池取现有上限与请求值中的较小值。
- 同步工具调用仍跟随父 context/deadline；后台接受后使用 service lifetime 和自己的单项 deadline，同时通过 OriginRunID 注册父显式取消。父正常终态不会取消它。

## 定义发现

用户目录由 runtime 的既有 config-root 路径传入，项目目录从受信授权根派生，不由 tool args 指定。重载构建新的完整 map，再原子替换；加载路径以 secfile/root 校验，拒绝符号链接、特殊文件、超长数据和未知字段。名称和工具列表均规范化、去重；稳定排序供列表和工具描述消费。解析失败保留拒绝原因并继续其它项。

已接受任务保存不可变 Definition 快照，重载不改变正在执行的正文、模型或工具边界；未启动请求使用最新快照。角色的公开元数据与内建 general-purpose 描述明确写出当前只读范围。

## 持久事件与执行流

1. 服务从调用者 ParentRun 构造受信 WorkRef、授权根与 permission bounds；请求内无 session/root/credential 字段。
2. 解析角色，取有效只读工具交集，拼接角色正文和显式任务；检查完整 JSON 编码后的 64 KiB 上限。
3. 创建独立 run ID、task ID；保留 OriginRunID，用独立 run ID 重写 authority.run_id，保留 session/goal/workitem/root/permission 其余字段。
4. session log `RunStarted` 增加可选 `AgentTaskID`、`AgentName`、`OriginRunID` 标识及受信 `OriginCallID` 工具调用关联。它保存任务标签与来源，不保存 definition 正文。独立 run 使用与 origin 相同的 WorkRef，以便所属 Goal 的权限保持一致，但不修改目标事实。
5. coordinator 在 active map 中先注册取消 handle，再入队；注册与父取消共享锁/取消代次，避免取消期间新加入旧父任务漏取消。提交失败将独立 run 终结为 failed 并向调用方返回未接受错误。
6. queued/running/progress/terminal 沿已有 DelegationEvent 记录到独立 run；父 tool result 返回引用 ID 或同步结果。所有公开文本先脱敏并截断。
7. terminal 在事件锁下只记录一次，并追加独立 run outcome；记录成功后广播、移除 active handle并产生 session 完成通知。通知投影以 task ID + terminal seq 去重。
8. 下一次普通父 run 在构造上下文时读取所属 session 尚未交接的 terminal 摘要，每轮至多注入 20 条、编码摘要总计 64 KiB，余项留待后续 run；记录 destination run ID 的通知引用；服务恢复时对不存在 run_started 的 destination 不视作已交接。客户端通知通过 cursor 消费，不依赖只在内存中的队列。

普通 run 的模型历史只增加经过验证的 summary/error 通知，不增加 child 原始 transcript。goal 路径仅接收与其 WorkRef 匹配的任务通知；不把其他目标或普通 session 任务的结果混入目标上下文。

## 查询、取消与恢复

- `task_list` 只按请求已验证的 session 查询；支持 after cursor、limit，limit 默认 20、上限 100。
- `task_output` 先校验 task.session 与当前 run/session 一致；默认不等，block wait 取调用 context 与 30 秒较小值。等待使用完成通道，不占 child pool worker。终态从持久日志投影。
- `task_stop` 幂等：queued/running 调用 cancel；已 terminal 返回已有结果；跨 session 返回无访问权限。取消状态直到实际 child 退出后持久化，不能提前声称资源已释放。
- 父取消回调取 OriginRunID 关联 handle，取消当前集合；父 completed/failed 正常结束及 socket 断开不会隐式停止已接受任务。service.Close 只取消自身管理的任务。
- 启动恢复识别 agent-task 的 RunStarted 标识，即使尚无首条 queued 事件也终结为 interrupted。未 terminal 的 child 先补 interrupted，再补独立 run terminal；已 terminal child 但 run outcome 缺失时按其真实终态闭合，不改写为成功或重新调用 provider。恢复复用现有事件幂等键和合法状态校验。
- 保留 completed/failed/canceled/interrupted 结果；活动 map 移除后依旧可从 session log 查询，不把所有历史任务留在 RAM。

## 协议、工具与 TUI

conversation protocol 增加 `agent_list`、`agent_reload`、`agent_task_start`、`agent_task_list`、`agent_task_get`、`agent_task_cancel` 操作和对应 typed payload。`ClientMsg` 增加 AgentName、TaskID、Background、WaitMS、TimeoutMS 等受限字段；重复利用 Limit/AfterSeq。客户端不直接提交 ParentRun 或 permission bounds。

execution 工具 `run_agent` 取角色、instruction、background、model、timeout；`task_output` 取 task_id、block、wait_ms；`task_stop` 取 task_id。由 runtime 单次注入 AgentTaskService；只在普通父工具集提供，不加入 read-only child schemas。所有调用仍进入现有 pre/post hooks、权限与 tool result 配对路径。

TUI `/agents` 列目录，`/agents reload` 展示变更与拒绝；`/agent` 返回独立后台 run 并订阅进度；`/tasks` 分页显示本 session，get/stop 操作不改变会话或目标归属。普通父 run 的订阅和任务独立 run 订阅使用已有 session 游标派送，不用全局 ActiveRunID 覆盖掉仍运行的父 run。

## 文件边界与技术决策

| 所有者 | 文件 | 内容 |
|---|---|---|
| 定义模块 | `internal/agentcatalog/{definition,catalog}.go` 与测试 | 严格加载、快照、内建和来源覆盖 |
| 共享池 | `internal/agent/delegation.go`、测试 | 提交句柄、取消/关闭竞争、预算收窄 |
| sessionlog | `events.go`、`validate.go`、`log.go`、投影与测试 | 来源字段、任务/通知投影、恢复合法性 |
| conversation | `agent_tasks.go`、`agent_task_recovery.go`、`agent_catalog.go` 与测试 | lifecycle、查询、取消、通知和恢复 |
| integration 主负责人 | `protocol.go`、`service.go`、`run.go`、`delegation_recovery.go` | 协议与既有运行/恢复接入 |
| execution | `agent_task_tools.go`、`executor_factory.go`、`tool_executor.go`、schemas 与测试 | 父工具适配和 child 不可递归 |
| runtime | `supervisor.go` 与测试 | catalog/service 注入与关闭 |
| TUI/CLI | `internal/tui`、`internal/conversation/client.go`、`cmd/stable` | slash、列表、独立任务订阅与恢复 |

源端自定义 permissionMode 的扩权、继承 thinking、原始 transcript 保存和无限内存任务表不照搬；按 spec 明确范围适配。SQLite 或 session event log 的新读写不引入第二事实源：公开状态以持久 session/run 事件为准。

## 验证策略

使用 fake provider 输入捕获、可取消屏障 runner、临时定义目录、队列满 fixture 和持久化失败注入；不接入真实模型或真实用户目录。跨模块测试验证后台接受、原父结束、状态续读、通知交接、父/独立取消、重启首事件前/queued/running/terminal-gap 恢复及 A/B/C 共池兼容。Go/E2E/package 重型回归在 GitHub Actions；记录确切代码 SHA 和每个 job 状态。
