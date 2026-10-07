# M09-C Hook Agent 执行 Checklist

> 状态：已批准（2026-10-07）。按已批准 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md) 验收；未完成项不得标记通过。

## 功能验收

- [x] **AC1 Agent action 路由：** `message` 优先，空时回退 `command`；两者都空时配置报告错误；command/prompt/http 仍使用原行为。
- [x] **AC2 Session/Goal 事件覆盖：** run_start、run_end、pre_tool_use、post_tool_use 都可触发 agent child；提示仅含 action 指令和当前事件字段，不含父历史。
- [x] **AC3 权限与继承：** fake provider 确认父 provider/model、WorkRef、授权根和 permission bounds 传递正确；child 只用 M09-A 只读工具集，拒绝写入、命令、MCP、网络和递归委派。
- [x] **AC4 静态拒绝：** `reject: true` 不由模型输出改变；pre_tool_use 在 permission gate 与工具之前同步完成；`async: true` 不绕过；`on_error: reject` 只在失败时拒绝。
- [x] **AC5 同步/异步结果：** 同步 action 等待终态；除 pre_tool_use 外，async 立即返回并在完成后 journal、更新事件、排入通知队列。
- [x] **AC6 预算和失败：** 3 workers、32 queue、8 rounds、3 分钟、50,000 字节工具输出、8 KiB 摘要和 64 KiB 输入有硬限制；队列满、超限、provider 错误、超时、取消和中断均有明确结果并遵循 `on_error`。
- [x] **AC7 事件和恢复：** queued/running/terminal 事件关联正确父 run 并可按游标续读；父取消停止当时已排队/运行 child；取消终态后的 run_end agent hook 执行一次有界任务；服务重启将未完成项标记 `interrupted` 且不重跑。
- [x] **AC8 隐私：** HookFired 记录 child run ID；任务结果走既有脱敏和长度限制；session log、TUI 和父 agent 均不暴露思考流或 child 原始 transcript。

## 兼容回归

- [x] M07-B command/prompt/http action、配置加载/热重载、静态拒绝、hook 顺序与通知回流回归通过。
- [x] M09-A `delegate_tasks` 批量 Session 委派和资源池行为回归通过；`RunBatch` 仍拒绝 Goal work。
- [x] M09-B slash 与 LoadSkill fork skill、上下文模式、取消和中断恢复回归通过。
- [x] Session/Goal 普通 run、工具调用事件配对和 TUI 游标续读回归通过。

## 验证记录

- [x] fake-provider/fake-delegator 定向用例不调用真实 provider 或外网，并与完整回归覆盖 AC1–AC8。
- [x] 本机只运行轻量定向验证；重型集成验证在 GitHub Actions 执行，并记录 workflow/run 链接、结论及失败项。若云端运行未完成或失败，明确记录未覆盖部分，不标记通过。
- [x] 最终 diff、格式化和文档状态检查完成；没有未审阅的生成文件或运行结果。

### 2026-10-07 本轮进度

- 修复服务重启时异步 `run_end` Hook Agent 子项可能遗留在 queued/running 的恢复缺口；恢复按 run/batch/task 追加唯一 `interrupted` 事件，包括父 run 已 terminal 的情况。
- 修复异步 Hook Agent 生命周期：任务由 HookGate 的 service context 管理，正常父 run 结束时继续运行，父 run 以 cancelled 终结或服务关闭时取消关联 child。
- 通过：`go test ./internal/conversation -run 'TestRecover(InterruptedRunEndHookChildAfterTerminal|DelegationClosesOpenToolCallAndIsIdempotent)' -count=1`。
- 通过：`go test ./internal/conversation -run 'HookAgent|RecoverInterruptedRunEndHookChildAfterTerminal|RecoverDelegationClosesOpenToolCallAndIsIdempotent' -count=1`，覆盖异步任务跨正常结束、父取消、恢复幂等。
- 通过：`go test ./internal/conversation -run HookAgent -count=1`，fake delegator 覆盖 Session/Goal 的四类 lifecycle event 和 WorkRef 传递。
- 通过：`go test -p 2 ./internal/agent ./internal/hooks ./internal/conversation ./internal/execution ./internal/sessionlog ./internal/tui -run 'HookAgent|RunTask|Delegation|HookEvents|HookFired|ForkSkill|ForkContext|ReadOnlyExecutor|Validate|Fire' -count=1`。
- 相关模块整包测试未全部完成：一次运行因工作区 `TMPDIR` 过长导致 Unix socket bind 失败；改回系统临时目录后，conversation/execution 测试二进制链接遇到 disk quota exceeded。未将该次运行计为通过。
- GitHub Actions [E2E run 37578219968](https://github.com/kikoiio/Stable/actions/runs/37578219968) 在远端分支提交 `47bf2de` 运行。`unit`、`cases`、`e2e-m03`、`e2e-m04`、`e2e-sessions` 通过；`e2e-core` 的 `make e2e` 失败，脚本报告 `candidate did not become ready`，期间 chat client 多次报告 socket `FileNotFoundError`。此失败不在 M09-C Hook Agent 测试路径。
- GitHub Actions [Go run 37580835794](https://github.com/kikoiio/Stable/actions/runs/37580835794) 在提交 `6fcc08fbd0ce7b2e823709922e4d893aa247efa1` 运行；`build-and-test`（依赖校验、构建、`go test ./...`）与 `test-package` 全部通过。
- GitHub Actions [E2E run 37580835641](https://github.com/kikoiio/Stable/actions/runs/37580835641) 在同一提交运行；`unit`、`cases`、`e2e-sessions`、`e2e-m03`、`e2e-m04`、`e2e-core`（`make e2e`）全部通过。
- checklist-only 提交 `8c86a3a` 的 [Go run 37581684595](https://github.com/kikoiio/Stable/actions/runs/37581684595) 全部通过；同提交的 [E2E run 37581684642](https://github.com/kikoiio/Stable/actions/runs/37581684642) 中 `unit`、`cases`、`e2e-sessions`、`e2e-m03`、`e2e-m04` 通过，`e2e-core` 失败。
- 失败 artifact 的 `run.sh` 数据显示 crash-recovery marker 已生成；随后 chat client 遇到 socket `FileNotFoundError`，状态/SQLite 操作报告 `database is locked`，`make e2e` 未完成该场景后续验收。与同一代码提交上成功的 E2E run 相比，这是恢复路径的间歇性失败；当时先保持云端重型验证项未勾选，修正后再复测。
- 修正恢复 E2E 的竞争窗口：marker 出现后立即离开 chat 轮询；crash 窗口中的 SQLite 通知遇到 `database is locked` 时最多重试 10 秒，其它错误仍直接失败。
- GitHub Actions [Go run 37583604272](https://github.com/kikoiio/Stable/actions/runs/37583604272) 在提交 `8efb8a6375a7f582a599b3f5d500038660f8e637` 运行；`build-and-test`、`test-package` 全部通过。
- GitHub Actions [E2E run 37583604300](https://github.com/kikoiio/Stable/actions/runs/37583604300) 在同一提交运行；`unit`、`cases`、`e2e-sessions`、`e2e-m03`、`e2e-m04`、`e2e-core` 全部通过，包含修正后的 crash/recovery 场景。
- 截至 `8efb8a6` 时，Go 与 E2E workflow 均通过、云端重型验证已完成，但 AC1–AC8 尚未逐项核实，因此当时整体未标记通过。
- 旧 E2E run `37578219968` 属于未包含本轮改动的提交 `47bf2de`，曾有 `e2e-core` 失败；这是早期验证记录，后续修复与成功 run 见下方。
- 新增 AC 缺口用例：`TestHookAgentMessageTakesPriorityAndEmptyActionIsRejected`、`TestHookAgentPreToolUseWaitsEvenWhenConfiguredAsync`、`TestHookAgentParentCarriesRunAuthorityAndReadOnlyTools`、`TestHookAgentJournalRedactsBoundsAndAssociatesChildRun`；`TestChildBudgetDefaultDurationIsBounded` 现在校验全部默认预算，`TestStreamingChildRunnerUsesOnlyAssignedTaskAndReadTools` 断言 thinking delta 不进入 child 摘要。
- 本机 `GOMAXPROCS=2 go test -p 2 ./internal/conversation ./internal/hooks -run 'HookAgent|Validate|LoadFiles' -count=1` 未能链接 conversation 测试二进制（`disk quota exceeded`）；`internal/hooks` 定向测试通过。未将本机 conversation 检查记为通过。
- GitHub Actions [Go run 37585087842](https://github.com/kikoiio/Stable/actions/runs/37585087842) 在提交 `c621de8284058fa8a09c1ec5b5e91c71661466c6` 运行；`build-and-test`（`go test ./...`）与 `test-package` 全部通过。
- GitHub Actions [E2E run 37585087864](https://github.com/kikoiio/Stable/actions/runs/37585087864) 在同一提交运行；`unit`、`cases`、`e2e-sessions`、`e2e-m03`、`e2e-m04`、`e2e-core` 全部通过。
- 截至提交 `c621de8`，AC1–AC8、M07-B/M09-A/M09-B 兼容回归及 fake-provider/fake-delegator 验证均有测试证据，全部验收项通过。
