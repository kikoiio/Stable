# M09-B fork skill 执行 Checklist

> 状态：实施与验收通过（2026-10-07）。所有 provider 场景使用 fake，不依赖真实模型或外网。

## 实现完整性

- [x] **AC1 slash 入口：** 输入有效 fork skill slash 命令后，当前 session 创建独立 fork run，用户能看到排队、运行进度及终态，且可通过 run ID 和游标续读。（验证：fake-provider e2e 发起 slash 命令，检查 `RunStarted`、queued/running/terminal 事件和结果回放；检查 inline skill 仍走原普通 run 路径。）
- [x] **AC2 `LoadSkill` 入口：** 父 agent 调用 fork skill 后得到状态、摘要或清楚错误作为工具结果，并能继续生成最终回复。（验证：fake 父 agent e2e 检查 `EventToolCall`/`EventToolResult` 配对、结果可见且父 run 继续完成。）
- [x] **AC3 上下文模式：** `none` 不包含历史，`recent` 恰取最近 5 轮可见对话，`full` 受模型上下文预算限制；未知模式不会启动子 agent。（验证：fake provider 捕获输入，与 session fixture 逐项对比三种模式、thinking 排除及未知模式下 provider 调用数为零。）
- [x] **AC4 只读边界：** 子 agent 只能读取、搜索和列出授权根内内容；越权路径、写入、命令、MCP、外部网络和递归委派不可用或被拒绝。（验证：检查 child 工具集并实际尝试越权路径和禁止工具，确认文件内容与目录状态未改变。）
- [x] **AC5 终态与部分失败：** 成功返回摘要；失败、取消和中断保留明确原因；一个 child 失败时其它结果仍返回；slash 用户及 tool 父 agent 能读取相应结果。（验证：fake child runner 覆盖混合成功/失败和四类终态，检查事件回放和 tool result。）
- [x] **AC6 预算与取消：** 每项最多 8 轮、3 分钟、50,000 字节工具输出和 8 KiB 摘要，输入最多 64 KiB；取消父 run 会停止其 `LoadSkill` 子项。（验证：受控 fake runner/provider 在各边界触发，检查拒绝、截断说明、子项终态和无残留活动任务。）
- [x] **AC7 重启与续读：** 未完成 fork run/子项在重启恢复后标为 `interrupted`，不自动重跑；未闭合工具调用有结果配对；重复恢复幂等。（验证：分别模拟首条协作事件前、queued、running 和工具调用期间重启，执行两次恢复并核对事件计数、终态和工具配对。）
- [x] **AC8 审计与回归：** 两入口都产生来源清楚的 `skill_invoked`；日志不包含技能正文、思考流或 child 原始 transcript；inline、普通 run 和 M09-A 委派行为不变。（验证：回放并检查 session log/TUI 输出字段，运行 inline、普通 run 和 M09-A 定向回归。）
- [x] **AC9 Linux 验证：** fake-provider Linux e2e 覆盖 slash 与 `LoadSkill` 两入口、父结果交接、上下文隔离、只读限制、取消和重启恢复。（验证：在 Linux 本机执行完整 e2e，无真实 provider 凭证和外网调用。）

## 集成

- [x] Slash fork run 的开始、协作事件、终态和取消走现有 session run 持久化、订阅及游标路径。（验证：断开后使用 `run_subscribe` 的上一游标重连，逐条观察缺失事件按序补齐。）
- [x] `LoadSkill` 协作事件与调用它的父 run 共享事件序列，结果成为父工具结果。（验证：回放父 run 事件，检查单调序号、子项事件与 tool call/result 关联。）
- [x] 两种入口使用同一 M09-A 只读 runner、资源池、provider/model、授权根及预算限制。（验证：fake provider/runner 检查配置继承，多个父 run 并发时活动数不超过 3、排队数不超过 32。）
- [x] 子项进度只展示任务名、入口、排队/运行状态、阶段摘要和终态。（验证：TUI 事件投影检查各字段可见；注入 thinking/raw transcript 字段，确认没有显示或持久化。）
- [x] SkillGate 的 fork 路径不会把技能正文写入父消息；inline 路径仍按既有方式工作。（验证：对比两种调用后的 session message 和 tool result 记录。）
- [x] 父 run 取消只影响其关联子项，不会取消共享池中其它父 run 的任务。（验证：两个父 run 的同步 fake runner 中取消一个，观察另一个继续运行并成功结束。）

## 端到端场景

- [x] **Slash 独立执行与续读：** 用户运行 `/技能名 <参数>`；观察独立 run ID、queued/running 阶段、过滤后的最终摘要和 terminal，之后按游标能重放相同结果。（验证：fake-provider Linux e2e 加 session log replay。）
- [x] **父 agent 加载 fork skill：** 父 agent 调用 `LoadSkill` 并提供参数；观察只读 child 执行，结果回到配对的工具结果，父 agent 能基于摘要继续回复。（验证：fake-parent e2e 检查权限边界、事件序列、tool result 和 parent terminal。）
- [x] **上下文与隐私：** 分别选择 none/recent/full；观察 provider 收到规定历史范围且不含 thinking、其它 session 或 child transcript。（验证：fake provider 输入捕获与持久化日志/TUI 输出对照。）
- [x] **部分失败和取消：** 同一批或不同父 run 中有子项失败、一个父 run 被取消；观察失败原因保留，成功结果不丢失，其他父 run 继续。（验证：屏障式 fake runner 检查每项终态和无残留任务。）
- [x] **服务重启中断：** 在 slash child 与 `LoadSkill` 执行中模拟进程停止，再运行启动恢复；观察 interrupted 事件、工具结果配对及游标续读，任务不重跑。（验证：持久日志 fixture 执行两次恢复，比较恢复前后 child 启动计数和事件序列。）

## 构建与测试

- [x] Fork skill、sessionlog、execution、conversation、TUI 和 M09-A 相关定向测试通过。（验证：按 task.md 执行受影响包定向测试，命令退出码为 0。）
- [x] Linux fake-provider 集成场景通过，不访问真实 provider 或外网，且授权项目文件无变更。（验证：执行端到端用例并核对 fake provider 调用记录和临时目录差异。）
- [x] 全量 Go 测试通过。（验证：资源评估后执行一次 `go test ./...`，退出码为 0。）
- [x] CLI/worker 构建与 release package acceptance 通过。（验证：Go workflow 的 `go build ./cmd/...` 和 `make test-package` 均退出码为 0。）
- [x] 仓库未配置 lint workflow 或 lint 工具，标记为不适用。

## 验收记录

- Go workflow：[run 37573428965](https://github.com/kikoiio/Stable/actions/runs/37573428965)；依赖校验、CLI/worker 构建、`go test ./...` 与 `make test-package` 均通过。
- E2E workflow：[run 37573428928](https://github.com/kikoiio/Stable/actions/runs/37573428928)；unit、platform boundary、M03、M04、sessions、cases 与核心 `make e2e` 均通过。
- provider 场景由 fake 实现；无真实模型凭证或外网请求。重型验证在 GitHub Actions Linux runner 执行。
- 本地 `git diff --check` 通过。仓库没有配置 lint workflow 或 lint 工具。

## 验收覆盖自检

| Spec 验收标准 | Checklist 覆盖 |
|---|---|
| AC1 slash 入口 | 实现完整性 AC1；slash 端到端 |
| AC2 LoadSkill 入口 | 实现完整性 AC2；父 agent 端到端 |
| AC3 上下文模式 | 实现完整性 AC3；上下文与隐私端到端 |
| AC4 权限与隔离 | 实现完整性 AC4；共享只读 runner 集成 |
| AC5 结果与失败 | 实现完整性 AC5；部分失败和取消端到端 |
| AC6 预算与取消 | 实现完整性 AC6；取消端到端 |
| AC7 重启与续读 | 实现完整性 AC7；重启中断端到端 |
| AC8 审计与回归 | 实现完整性 AC8；SkillGate 集成 |
| AC9 Linux 验证 | 实现完整性 AC9；所有 fake-provider e2e 场景 |
