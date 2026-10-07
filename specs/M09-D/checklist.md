# M09-D Agent 定义与后台任务 Checklist

> 状态：D 的 AC1–8 与完整功能场景已按测试逐项核对；SHA `e24331a0ac6d9745f9dc80fc8aed0142d47d2210` 的 Go build/unit 与 package 已通过，M09 Workspace Linux 已通过；E2E `e2e-core` 在 `dependency_change.sh` checker 版本恢复阶段失败，故 AC9 保留待验（2026-10-08）。

## 功能验收

- [x] **AC1 定义：** 内建/用户/项目覆盖正确，来源与只读能力可见；删除/修改重载生效；错误 YAML、未知字段、未知工具、非法名称、超长文件和符号链接明确拒绝，其它定义继续可用。（验证：临时目录 catalog 测试与 service list/reload。）
- [x] **AC2 分派：** 同步等待终态，后台入队成功即返回稳定 task/run ID；definition background 强制异步；未知角色、空指令、输入超限、queued 记录失败、queue full 均不会报告已接受。（验证：屏障 fake runner 与持久化失败注入。）
- [x] **AC3 权限：** child 收到正确角色正文/显式任务、provider/model/WorkRef/授权根/permission bounds；不含历史或兄弟结果；只读交集有效；越权与写入/命令/MCP/网络/递归委派均拒绝。（验证：fake provider 输入捕获及受控 executor 实际禁止操作。）
- [x] **AC4 生命周期：** 父正常结束、TUI 断线后继续；指定任务和父显式取消可停止关联 queued/running，另一个父 run 或 session 不受影响；查询显示实际终态。（验证：双父 run、双 session 屏障与断线 service fixture。）
- [x] **AC5 事件与通知：** 独立任务 run 和来源关联正确、游标单调、重复订阅去重；结果可交接下一父 run，destination 缺失时恢复交接；重复查询不重跑；完成通知不自动发起新 run。（验证：session replay、重连与 provider 调用计数。）
- [x] **AC6 预算：** 同步/后台/fork/hook 共用 3 workers/32 queue；8 轮、3 分钟、50,000 字节工具输出、8 KiB 摘要、64 KiB 输入上限生效；definition 与调用只能收窄；查询等待不超过 30 秒且不占 child worker。（验证：共享池竞争、预算边界与等待取消测试。）
- [x] **AC7 恢复与失败：** provider 错误、timeout、取消、入队/终态写盘失败有明确结果；首条 queued 前、queued、running，以及子项已终态而独立 run 尚未终结的间隙均恢复正确；连续两次恢复不重复终态、不调用 provider、不丢 tool result。（验证：持久 fixture、故障注入和两次恢复对比。）
- [x] **AC8 隐私和展示：** 列表、TUI 和日志不保存原始角色正文、credential、thinking 或 child 原始 transcript；摘要与错误脱敏截断，任务状态可见且不覆盖同时运行的父 run。（验证：带敏感标记的 fake 输出、sessionlog 投影和 TUI 视图对比。）
- [ ] **AC9 兼容：** M09-A/B/C、todo、普通 Session/Goal run、工具 hook 顺序、权限门、候选受控接收和独立目标验证回归通过。（验证：受影响包 fake 定向与完整 Go/云端集成。）

## 完整场景

- [x] **用户后台调查：** `/agents` 查看定义，`/agent explore <任务>` 启动，继续普通父 run；`/tasks` 查看状态和摘要，断开重连后按 cursor 恢复。（验证：fake-provider Linux service/client/TUI 集成，无外网。）
- [x] **父工具交接：** 父 Session/Goal 调用 `run_agent` 后台返回 ID，随后 `task_output` 查询或等待结果；tool call/result 配对，父继续汇总。（验证：fake 父 provider 脚本、run 和 session 回放。）
- [x] **队列与隔离取消：** 两个父 run 同时提交超过 worker 数的任务，取消其中一个；另一父的 queued/running 继续；超出 queue 上限的提交未接受。（验证：共享池屏障和终态计数。）
- [x] **角色变更：** 有任务运行时重载同名定义；当前任务使用原快照、新请求使用新定义，删除/无效定义不再可用。（验证：definition hash/input 捕获对比。）
- [x] **服务重启：** queued/running 任务恢复为 interrupted；已完成结果与通知仍能读到；多次恢复不产生新模型请求。（验证：持久日志 fixture 和 fake 启动计数。）
- [x] **受信边界：** 构造另一 session 的 ID、路径/工具越权和未知扩权字段；服务/模型工具明确拒绝，无正式文件/候选/目标事实变化。（验证：双 session fixture 及项目 manifest 对比。）

## 验证记录

- [x] fake 场景使用临时目录，未读取真实用户 definitions 或调用真实 provider/外网。
- [ ] 格式化、diff和协议文档检查通过，无未审阅生成文件。
- [x] 重型构建、Go 全量、E2E 与 package acceptance 在已授权 GitHub Actions 执行；精确 SHA、workflow/run/job 记录如下。所有列出的 job 均成功。
- [x] README 记载只读角色、后台生命周期、查看/取消入口与重启不重跑；M09 总范围状态已更新，E/F 仍单独验收。

- 代码提交 `ccc3fbc7a606f29c07f3d2156fea6736f892d641`：GitHub Actions [Go run 37590568076](https://github.com/kikoiio/Stable/actions/runs/37590568076) 的 `build-and-test`（依赖校验、构建、`go test ./...`）与 `test-package` 均通过。
- 同一 SHA 的 GitHub Actions [E2E run 37590568082](https://github.com/kikoiio/Stable/actions/runs/37590568082)：`unit`、`cases`、`e2e-m03`、`e2e-m04`、`e2e-sessions` 与 `e2e-core`（`make e2e`）全部通过。

## 逐项证据映射（2026-10-07）

以下测试均来自当前仓库，已纳入 `75594bd` 的 Go 全量通过记录；本轮改变的 E/F 与共享执行器仍须新 SHA 组合复验，不据此提前勾选 AC9。

| 条目 | 可复现证据 | 核对结果 |
|---|---|---|
| AC1 | `agentcatalog.TestCatalogPrecedenceReloadAndImmutableViews`、`TestParseRejectsInvalidDefinitionsWithoutLeakingValues`、`TestInvalidEntriesAndSymlinksAreRejected`；`conversation.TestAgentCatalogSocketInventoryReloadIsPrivateAndSessionOwned` | 覆盖、删除/重载、错误定义隔离、文件/根链接拒绝、公开来源与只读字段；全部用临时目录。 |
| AC2 | `conversation.TestAgentTaskSynchronousWaitAndInputRejection`、`TestAgentTaskRoleBudgetModelToolsAndAudit`、`TestAgentTaskQueuedPersistenceFailureHasNoAcceptedReceiptOrChild`、`TestAgentTaskFullQueueRejectsAtServiceWithoutProviderCall` | 同步屏障、definition 强制后台、稳定独立 ID；拒绝路径不产生已接受句柄或模型调用。 |
| AC3 | `conversation.TestAgentTaskFullParentToolChainThroughSocketSessionAndGoal`、`TestAgentTaskRoleBudgetModelToolsAndAudit`、`TestAgentTaskGoalAuthorityMatchesWorkAndRejectsMismatch`；`execution.TestReadOnlyExecutorRejectsWritesAndCommands` | 实际 Session/Goal 父工具链、child authority/正文/任务/模型与三只读工具；捕获的请求没有父历史；实际 executor 拒绝写/命令/MCP/网络/递归且正式文件保持原样。 |
| AC4 | `conversation.TestAgentTaskSocketDisconnectAndParentCompletionKeepBackgroundAlive`、`TestAgentTaskExplicitParentCancelAndSessionOwnership`；`agent.TestPoolDelegatorCancelOneParentLetsOtherQueuedRunContinue`、`TestPoolDelegatorCancelsQueuedAndRunningWork` | 断开请求及父正常 terminal 后任务继续；显式取消只停止对应父关联 queued/running，跨 session 拒绝。 |
| AC5 | `conversation.TestAgentTaskFullParentToolChainThroughSocketSessionAndGoal`、`TestAgentTaskNotificationsRecoverMissingDestinationAndFilterWork`、`TestAgentTaskListCursorOrdersUpdatesAndServiceCloseCancels`；`tui.TestAgentTerminalUpdatesDeduplicateAcrossBothStreams` | 独立 task/run 来源、单调 cursor、两个后续父 run 仅交接一次摘要、缺失 destination 恢复；完成通知没有自动发起 run。 |
| AC6 | `agent.TestChildBudgetDefaultDurationIsBounded`、`TestPoolDelegatorTaskBudgetCanOnlyNarrow`、`TestPoolDelegatorIsSharedAcrossParentRuns`、`TestCappedExecutorStopsAtAggregateOutputLimit`、`TestStreamingChildRunnerCapsSummaryWhileStreaming`；`conversation.TestAgentTaskSocketDisconnectAndParentCompletionKeepBackgroundAlive`、`TestAgentCatalogSocketInventoryReloadIsPrivateAndSessionOwned` | 默认 3/32/8轮/3分钟/50,000输出/8KiB摘要/64KiB输入有边界断言；同一 service 的 AgentTasks、fork、hook 均使用 `deps.Delegator`。Output 等待在 service 收窄为30秒，协议拒绝超限，取消等待不取消 child、不占模型 pool。 |
| AC7 | `conversation.TestAgentTaskRecoveryAllCrashGapsIsIdempotent`、`TestAgentTaskRecoveryPairsParentToolCallWithoutRerunning`、`TestAgentTaskRecoveryAssociatesEachPendingParentCallExactly`、`TestAgentTaskTerminalPersistenceFailureReturnsErrorAndRecovers`；`agent.TestPoolDelegatorBoundsConcurrencyAndReturnsPartialResults` | 首queued前/queued/running/各终态间隙恢复两次日志长度不变；终态写盘故障可恢复，父tool配对不丢失，provider错误/超时/取消明确。 |
| AC8 | `conversation.TestAgentTaskTerminalRedactsCredentialsAndCapsPublicText`、`TestAgentTaskRoleBudgetModelToolsAndAudit`；`agent.TestStreamingChildRunnerSanitizesTrimmedAndBudgetClippedRoleEcho`；`tui.TestAgentTaskTranscriptNeverDisplaysRawChildTextOrThinking`、`TestAgentOneShotResultsDoNotEndParentAndStopWaitsForExit` | role正文/credential/thinking/child原始文本不进入公开事实或视图；摘要错误截断，独立更新保持父run/cursor。 |
| 用户后台调查 | service 真实 Linux IPC fixture `TestAgentTaskSocketDisconnectAndParentCompletionKeepBackgroundAlive`；TUI IPC fixture `TestAgentCommandsUseServerOwnedScopeAndPreserveParent`、`TestAgentSessionSubscriptionResumesOwnCursor`、`TestAgentRestoreRebuildsDurableTaskAndKeepsParentCursor` | slash发窄请求、后台继续、独立订阅cursor重连与持久重建分别有 fixture；无真实 provider/外网。 |
| 父工具交接 | `TestAgentTaskFullParentToolChainThroughSocketSessionAndGoal` | fake父provider在真实socket依次 run_agent→task_output→继续总结；验证call/result来源与唯一child run，覆盖Session和Goal。 |
| 队列与隔离取消 | `TestAgentTaskFullQueueRejectsAtServiceWithoutProviderCall` + `TestPoolDelegatorCancelOneParentLetsOtherQueuedRunContinue` | worker/queue屏障，拒绝溢出，取消一个父后另一个父排队任务继续。 |
| 角色变更 | `conversation.TestAgentTaskReloadPreservesAcceptedDefinitionSnapshot` + `agentcatalog.TestCatalogPrecedenceReloadAndImmutableViews` | 当前已接受任务保留原定义，新请求采用新正文；删除/无效定义回退或不可用。 |
| 服务重启 | `TestAgentTaskRecoveryAllCrashGapsIsIdempotent` + `TestAgentTaskNotificationsRecoverMissingDestinationAndFilterWork` | unfinished恢复interrupted，已终态摘要仍可读，重复恢复不新增请求/事实。 |
| 受信边界 | `TestAgentTaskExplicitParentCancelAndSessionOwnership` + `TestAgentTaskGoalAuthorityMatchesWorkAndRejectsMismatch` + `execution.TestReadOnlyExecutorRejectsWritesAndCommands` + `TestAgentCatalogSocketInventoryReloadIsPrivateAndSessionOwned` | 另一session/错误Goal authority/客户端根/越权工具均拒绝；正式sentinel和候选不存在性有断言。 |

- 最近已完成云端基线：代码 SHA `75594bd2c4279c115c18fb5f22c741f868dcfcb5` 的 [Go/package run 37644076609](https://github.com/kikoiio/Stable/actions/runs/37644076609) 与 [E2E run 37644076736](https://github.com/kikoiio/Stable/actions/runs/37644076736) 所有 job 成功。
- 最新组合代码：SHA `e24331a0ac6d9745f9dc80fc8aed0142d47d2210` 的 [Go run 37651671848](https://github.com/kikoiio/Stable/actions/runs/37651671848) 中 `build-and-test` 与 `test-package` 成功；[E2E run 37651671760](https://github.com/kikoiio/Stable/actions/runs/37651671760) 除 `e2e-core` 外所有 jobs 成功，`e2e-core` 在 `dependency_change.sh` checker 版本恢复阶段超时，agent run 状态为 failed；[M09 Workspace Linux run 37651671803](https://github.com/kikoiio/Stable/actions/runs/37651671803) 成功。AC9 待复验。
- AC9 在新 E/F 组合代码的 Go/package/E2E 与 sandbox/quota 结果完成后再勾选；以上基线不代替新提交验证。
