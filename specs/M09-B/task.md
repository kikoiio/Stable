# M09-B fork skill 执行 Tasks

> 状态：已批准（2026-10-07）。任务依据已批准的 [spec.md](spec.md) 和 [plan.md](plan.md)。四份规格文档全部获批前不开始实现。

## 文件清单

| 操作 | 文件 | 职责 |
|---|---|---|
| 新建 | `internal/conversation/fork_skill.go` | Fork 调用输入/结果、上下文模式、协调器与执行器接口 |
| 新建 | `internal/conversation/fork_context.go` | session 可见对话快照、none/recent/full 模式与预算裁剪 |
| 新建 | `internal/conversation/fork_skill_run.go` | slash fork run 的创建、事件持久化、取消和终态 |
| 新建 | `internal/conversation/fork_skill_events.go` | 父 run 与独立 run 的协作事件 sink、脱敏和序列分配 |
| 新建 | `internal/conversation/fork_skill_recovery.go` | slash 和 `LoadSkill` 中断恢复、工具结果配对 |
| 修改 | `internal/conversation/skills.go` | fork/inline 分流、slash 和 tool 入口统一准备与激活审计 |
| 新建 | `internal/conversation/fork_skill_test.go` | 两种入口、模式、错误、结果与输入限制 |
| 新建 | `internal/conversation/fork_context_test.go` | 历史窗口、预算裁剪、思考流和跨 session 隔离 |
| 新建 | `internal/conversation/fork_skill_run_test.go` | 独立 run 的事件、游标、取消及终态 |
| 新建 | `internal/conversation/fork_skill_recovery_test.go` | interrupted 修复、工具结果配对及幂等性 |
| 修改 | `internal/conversation/run.go`、`protocol.go` | slash fork run 的启动/取消/订阅接线，沿用现有协议 |
| 修改 | `internal/execution/executor_factory.go`、`tool_executor.go` | 将父 run 身份和调用时上下文来源传入 SkillProvider，映射工具结果 |
| 修改 | `internal/execution/tool_executor_m07a_test.go` | SkillProvider 接口与 inline LoadSkill 回归 |
| 新建 | `internal/execution/tool_executor_fork_skill_test.go` | fork LoadSkill 的父 run 上下文、结果和错误传递 |
| 修改 | `internal/sessionlog/events.go`、`validate.go`、`projection.go` | SkillInvoked fork metadata 与 RunStarted/终态恢复投影 |
| 修改 | `internal/sessionlog/log.go` | 仅在新 metadata 需要专门验证或投影时扩展现有事件路径 |
| 新建 | `internal/sessionlog/fork_skill_test.go` | fork audit/run metadata 落盘、校验与回放 |
| 修改 | `internal/tui/model.go`、`transcript.go` | slash run 订阅/取消及 fork skill 状态与摘要展示 |
| 新建 | `internal/tui/fork_skill_test.go` | slash 和父 run 协作事件的显示与续读 |
| 修改 | `README.md` | 说明 fork skill 两种入口、上下文模式、只读限制及预算 |

> `internal/skills` 已解析 `mode: fork`、旧式 `context: fork` 和 `fork_context` 默认值。本项保留该解析契约；未知模式在执行入口校验并拒绝，不扩大 parser 的其他行为。

## T1：Fork 调用契约与模式校验

**文件：** `internal/conversation/fork_skill.go`、`fork_skill_test.go`

**依赖：** 无。

**步骤：**

1. 定义 slash/tool 入口、调用输入、结果和 `none`、`recent`、`full` 模式类型。
2. 定义统一的技能解析/准备和执行器接口，明确 session/run、技能来源、指令、上下文、provider/model、授权根与限额字段。
3. 校验未知 `fork_context` 和正文加渲染参数超过 64 KiB 时在提交资源池前失败。
4. 为 slash 结果与 tool 结果定义成功、失败、取消、中断映射；结果只含摘要和错误，不含 child transcript。

**验证：** `go test ./internal/conversation/... -run ForkSkillContract -count=1` 通过；覆盖三种合法模式、未知值、64 KiB 边界及超限、四类终态映射。

## T2：调用时上下文快照

**文件：** `internal/conversation/fork_context.go`、`fork_context_test.go`。

**依赖：** T1。

**步骤：**

1. 从指定 session 的 session log/可见消息投影构造调用时快照，不使用 run 创建时的初始 prompt 替代当前对话。
2. 实现 `none` 空上下文、`recent` 最近 5 轮可见对话、`full` 预算内完整可见历史。
3. 排除思考流、工具内部原始 transcript 和其它 session 的消息；按现有上下文预算裁剪 `full`。
4. 读取期间与 session 事件写入协调，保证不读取半条消息；工具入口可按 parent run 取得同一 session 的当前可见视图。

**验证：** `go test ./internal/conversation/... -run ForkContext -count=1` 通过；fixture 断言三种模式边界、最近 5 轮、预算裁剪、session 隔离和思考流排除。

## T3：SkillGate fork/inline 分流

**文件：** `internal/conversation/skills.go`、`fork_skill.go`、`fork_skill_test.go`。

**依赖：** T1。

**步骤：**

1. 保留现有 inline skill 激活路径；fork skill 改为读取正文、按 M06 渲染参数并生成统一调用输入，不向父会话消息注入正文。
2. 对 slash 和 `LoadSkill` 共用正文读取、参数渲染、模式校验及 64 KiB 输入限制。
3. 记录包含来源、slash/tool 入口及 fork run 关联信息的 `skill_invoked` 元数据；日志不包含技能正文。
4. 正文读取失败、未知技能、非法模式和参数超限时返回清楚错误，不提交 child 任务。

**验证：** `go test ./internal/conversation/... -run 'SkillGate|ForkSkillActivation' -count=1` 通过；现有 inline 行为保持，fork 两入口记录审计且错误路径不创建子任务。

## T4：SkillInvoked 与 fork run 元数据

**文件：** `internal/sessionlog/events.go`、`validate.go`、`projection.go`、`log.go`（仅必要时）、`internal/sessionlog/fork_skill_test.go`。

**依赖：** T1。

**步骤：**

1. 为 `SkillInvoked` 增加向后兼容的 fork mode 和可选 fork run ID 字段；扩展允许的 slash/tool 审计关联语义。
2. 为独立 slash fork run 的持久化 `RunStarted` metadata 增加可识别的 fork skill 关联标记，使首条协作事件前崩溃也可恢复。
3. 保持旧 session log 记录可回放；校验新字段组合、技能名、入口和 run 关联一致性。
4. 仅在现有 log append/projection 路径需要专门分支时修改 `log.go`/`projection.go`，不增设存储格式或数据库。

**验证：** `go test ./internal/sessionlog/... -run 'ForkSkill|SkillInvoked|RunMetadata' -count=1` 通过；覆盖旧记录回放、新旧字段校验以及 run metadata 的持久化。

## T5：M09-A child runner 与事件 sink 适配

**文件：** `internal/conversation/fork_skill.go`、`fork_skill_events.go`、`internal/conversation/delegation.go`（必要时）、现有 `internal/agent/delegation.go` 与相关测试。

**依赖：** T1。

**步骤：**

1. 创建薄适配器，将一个 fork skill 调用映射为 M09-A 的单项 `DelegationTask`，复用共享 `PoolDelegator` 和只读 child runner。
2. 继承父 run/provider 配置、授权项目根和现有 permission/sandbox 行为；不创建第二个池或新增子工具。
3. 定义事件 sink 窄接口：tool 入口发布到活动父 run；slash 入口发布到独立 fork run 的持久化 sink。
4. 为任务名、阶段、摘要和错误复用既有脱敏与大小限制；结果按 M09-A 状态映射。

**验证：** `go test ./internal/conversation/... -run ForkSkillExecutor -count=1` 通过；fake child runner 验证资源池限额、provider/model 和授权根继承、事件落点区分及只读能力复用。

## T6：slash fork run 的事件生命周期

**文件：** `internal/conversation/fork_skill_run.go`、`fork_skill_events.go`、`internal/conversation/fork_skill_run_test.go`、`internal/conversation/run.go`。

**依赖：** T1、T4、T5。

**步骤：**

1. 为 slash 调用生成独立 run ID，持久化带 fork skill 关联 metadata 的 `RunStarted`，再开始 context build 和 child pool 提交。
2. 在独立 run 上维护单调 `RunSeq`，持久化并广播 queued/running/阶段/终态事件，使用现有 run event 类型、订阅和游标语义。
3. 持久化成功、失败、取消或中断终态与过滤后的摘要/错误；事件发布失败不得返回伪成功。
4. 在当前 fork run 执行期间支持 `run_cancel`，取消其排队或运行 child，且重复取消安全。

**验证：** `go test ./internal/conversation/... -run ForkSkillRun -count=1` 通过；回放验证 run ID、事件序号、游标续读、状态终结、取消传播和重复取消。

## T7：slash SkillGate 与协议/TUI 接线

**文件：** `internal/conversation/skills.go`、`protocol.go`、`run.go`、`internal/tui/model.go`、`internal/tui/fork_skill_test.go`。

**依赖：** T2、T3、T6。

**步骤：**

1. 在 slash skill invocation 中按 `mode: fork` / `context: fork` 路由到独立 fork run starter；inline 仍启动现有普通 run。
2. 在 fork child 完成前返回/订阅 fork run ID，使 TUI 可通过现有 `run_subscribe` 按游标追踪，而不是等待普通父 agent run 完成。
3. 将 `run_cancel` 分发到 fork starter 管理的 run；其它普通 run 仍走现有 runner。
4. 对运行中的 skill run 展示技能名、入口、排队/运行状态、阶段摘要与终态；不展示 thinking 或 child transcript。

**验证：** `go test ./internal/conversation/... ./internal/tui/... -run 'ForkSkill|SkillInvoke' -count=1` 通过；slash fork 能被订阅、续读和取消，inline slash 行为回归通过。

## T8：LoadSkill 父 run 上下文接线

**文件：** `internal/execution/executor_factory.go`、`tool_executor.go`、`tool_executor_m07a_test.go`、`tool_executor_fork_skill_test.go`。

**依赖：** T1、T2、T3、T5。

**步骤：**

1. 将 `SkillProvider.LoadSkill` 的调用参数扩展为包含父 session/run ID 与调用时可见上下文来源；避免只传 session/name/args 而丢失运行身份。
2. 在 `executeLoadSkill` 中将已知的 WorkSession run 与授权信息交给 provider；不向 provider 传递无关 session 或其它任务数据。
3. fork provider 执行子 agent 并返回终态、摘要或错误作为工具结果；inline provider 仍返回既有技能正文结果。
4. 保持 tool call/result 配对、脱敏、错误路径和现有 fake provider 测试适配。

**验证：** `go test ./internal/execution/... -run 'LoadSkill|ForkSkill' -count=1` 通过；断言父 run ID/context 来源传递正确，fork 结果返回为 tool result，inline 仍保持原行为。

## T9：父 run 取消与失败传播

**文件：** `internal/conversation/fork_skill.go`、`fork_skill_events.go`、`internal/execution/tool_executor_fork_skill_test.go`、`internal/conversation/fork_skill_test.go`。

**依赖：** T5、T8。

**步骤：**

1. 将 `LoadSkill` 子项 context 绑定父 run 生命周期，父取消/失败后停止该子项。
2. 子项失败、中断和取消作为明确 tool result 返回；不把单项失败变成伪成功摘要。
3. 保留共享池里其它父 run 的排队/运行任务，不因一个父 run 取消误停全局池。
4. slash run 的取消继续由 T6 管理，不把 slash 独立任务错误地绑定到无关父 run。

**验证：** `go test ./internal/conversation/... ./internal/execution/... -run 'ForkSkill.*Cancel|LoadSkill.*Cancel' -count=1` 通过；两个父 run 并行时取消其中一个，仅终结其子项。

## T10：fork run 与 LoadSkill 中断恢复

**文件：** `internal/conversation/fork_skill_recovery.go`、`service.go`、`run.go`、`internal/conversation/fork_skill_recovery_test.go`、`internal/sessionlog/projection.go`。

**依赖：** T4、T6、T8、T9。

**步骤：**

1. 在服务接受新 run 前扫描持久化 run metadata、协作事件和工具调用/结果配对，找出未终结 slash fork run 与 `LoadSkill` child。
2. 为未终结子项写入一次 `interrupted` 终态，不重新入池、不重跑。
3. 为 slash run 写入 interrupted terminal；为中断的父 run 配对补写明确的 `EventToolResult` 并终结父 run。
4. 通过 event sequence 和 terminal 检查确保连续两次恢复幂等，并保留已有成功结果。

**验证：** `go test ./internal/conversation/... -run 'ForkSkillRecovery|InterruptedToolPair' -count=1` 通过；覆盖首条协作事件前、queued、running、tool call 中途崩溃和连续两次恢复。

## T11：上下文/安全/预算集成覆盖

**文件：** `internal/conversation/fork_skill_test.go`、`fork_context_test.go`、`internal/execution/tool_executor_fork_skill_test.go`、必要的 `internal/sessionlog/fork_skill_test.go`。

**依赖：** T2、T3、T5、T7、T8。

**步骤：**

1. 使用 fake provider 检查 `none`、最近 5 轮 `recent` 和预算内 `full` 的真实输入。
2. 断言 child 只得到 M09-A 的只读 read/search/list 工具和同一授权根，拒绝写、命令、MCP、网络和递归委派。
3. 覆盖 provider/model 继承、64 KiB 输入、8 轮、3 分钟、50,000 字节工具输出及 8 KiB 摘要限制。
4. 检查 session log 与 TUI 不包含思考流或 child 原始 transcript，slash/tool 来源均可审计。

**验证：** `go test ./internal/conversation/... ./internal/execution/... ./internal/sessionlog/... -run 'ForkSkill|ForkContext' -count=1` 通过；所有 provider 调用使用 fake，不访问真实模型或外网。

## T12：端到端集成与现有行为回归

**文件：** `internal/conversation`、`internal/execution`、`internal/tui` 的集成测试及现有 M07-A/M09-A 回归测试。

**依赖：** T7、T8、T9、T10、T11。

**步骤：**

1. 通过 slash 入口启动 fork skill，验证独立 run 事件及最终结果可续读。
2. 通过 fake 父 agent 调用 `LoadSkill`，验证结果回到父 tool call/result，父 agent 可继续处理。
3. 覆盖部分失败、取消、中断恢复、断线游标续读、未知上下文模式和正文读取错误。
4. 回归 inline skill、普通 run、M09-A `delegate_tasks`、现有权限审批及 session event projection。

**验证：** `go test ./internal/conversation/... ./internal/execution/... ./internal/sessionlog/... ./internal/tui/... -run 'ForkSkill|Skill|Delegation' -count=1` 通过；端到端场景不依赖真实 provider/网络，项目文件无变化。

## T13：文档与完整验证

**文件：** `README.md`；验证期间不新增实现文件。

**依赖：** T12。

**步骤：**

1. 说明 slash 和 `LoadSkill` 两种 fork 入口、`fork_context` 三种模式、默认值和最近 5 轮定义。
2. 说明只读能力、模型 provider 请求例外、资源池和单项预算、取消及重启中断行为。
3. 完成 checklist 后，先执行受影响包的定向验证，再安排一次受控全量 build/test；根据 `MemAvailable`、换页速率和 memory PSI 调整并行度。
4. 记录真实验证命令和结果；出现资源压力时降低并行度或分批，不重复盲目运行失败命令。

**验证：** `go test ./...` 与 `go build ./...` 退出码均为 0；fake-provider Linux 场景覆盖两种入口、只读边界、上下文隔离、取消和重启恢复。验证未实际完成的项目需明确标记，不得记为通过。

## 执行顺序

```text
并行批次 A：T1 契约 ──┬──→ T2 上下文快照 ──┬──→ T3 SkillGate ──┬──→ T5 M09-A 适配 ──┬──→ T6 slash run ──┐
                     └──→ T4 sessionlog ───┘                   │                     ├──→ T8 LoadSkill ─┤
                                                                │                     │                │
                                                                └─────────────────────┴──→ T7 TUI/协议 │
                                                                                                       ↓
并行批次 B：T9 取消传播（依赖 T5/T8） ──→ T10 恢复（依赖 T4/T6/T8/T9） ──┐
                                                                         ├──→ T11 集成覆盖 ──→ T12 回归 ──→ T13 文档/全量验证
T2/T3/T5/T7/T8 ──→ T11 上下文与安全覆盖 ──────────────────────────────────┘
```

**可执行并行批次：**

1. **批次 A：** T1 与 T4 可并行；分别编辑 conversation 契约文件和 sessionlog 文件。T1 完成后 T2/T3 可按依赖继续；T4 独立进行。
2. **批次 B：** T2 与 T4 可并行；T3 等 T1，T5 等 T1。T2/T3 分别修改 `fork_context.go` 与 `skills.go`，不共享写入文件。
3. **批次 C：** T5、T6、T8 的依赖按 DAG 执行；T6 和 T8 可并行，但分别由 slash run 与 execution tool owner 修改各自文件。T7 等 slash 生命周期接口确定后进行。
4. **批次 D：** T9 取消传播可与 T6 的独立事件验证并行，避免同时编辑同一 conversation 文件；T10 必须等 T4/T6/T8/T9 汇合。
5. **批次 E：** T11、T12 按各自依赖汇合；T13 的全量验证单独安排一次受控资源批次，不并发启动多套完整测试/build。

并行任务不得同时编辑相同文件。`internal/conversation/run.go`、`skills.go`、`service.go` 如由多个任务触及，按上表先完成接口 owner 任务，再依赖该提交修改；协调文件只由一个任务负责合并。任何全量测试/build 开始前检查 `free -h`、`vmstat 1 5`、`/proc/pressure/memory`，并复用已有服务实例。

## Task 自检

- **Plan 覆盖：** 六个设计部分均有实现任务：接口/上下文（T1–T2）、SkillGate（T3）、审计/事件（T4–T6）、slash/TUI（T7）、LoadSkill/取消（T8–T9）、恢复（T10）、集成回归及文档验证（T11–T13）。
- **依赖链：** DAG 无环；任务入口依赖明确，slash 与 tool 两条路径在集成覆盖汇合。
- **验证完整性：** 每项均有可执行的定向验证命令及可观察断言；受影响集成与全量验证安排在汇合后。
- **粒度与并行：** 可独立的上下文、sessionlog 和执行入口工作分开；共享 conversation 文件指定先行 owner 和汇合点。资源重型验证串行受控。
- **类型一致性：** `ForkSkillInvocation`、`ForkContextMode/Builder`、`ForkSkillResult`、`ForkSkillExecutor`、`ForkSkillRunStarter` 与已批准 Plan 一致；M09-A 的 pool 和事件类型复用。
