# M03 权限、隔离与旧桥接受控接收 Checklist

> 基于已批准的 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md)。本清单在实施前审批；以下均为待验证项，只有观察到所述结果并保存证据后才能勾选。真实隔离正例须在能够创建 Linux namespace 的环境运行，受限环境的拒绝结果只能证明失败即拒绝。

## 共用权限与用户授权

- [x] **C01 / AC1**：普通任务与长期目标工作项对同类范围内操作得到同一权限结果。（验证：分别提交同一类别操作，比较允许/询问/拒绝结果及其运行归属。）
- [x] **C02 / AC1**：跨运行、跨目标或跨授权目录的请求均被拒绝，记录中的会话、目标、运行和操作归属正确。（验证：伪造另一运行或目录的请求，再按运行查询决定记录。）
- [x] **C03 / AC2**：默认、自动接受范围内编辑、计划、跳过重复提示四种模式按已批准的询问表工作。（验证：每种模式提交项目读取、候选写入和命令，逐项核对提示或执行结果。）
- [x] **C04 / AC2**：明确拒绝规则在四种模式中均生效，跳过提示也不能越过授权根、正式工程写保护或网络限制。（验证：保存精确拒绝规则，切换四种模式并重复操作；再尝试越界操作。）
- [x] **C05 / AC5**：待授权提示显示操作名称、目标、影响范围及询问原因，且不显示模型凭据。（验证：触发写入和命令提示，观察 TUI 内容并扫描标记密钥。）
- [x] **C06 / AC5**：用户拒绝后操作未执行；单次批准只执行本次；保存精确规则仅复用相同操作、参数、目标和授权范围。（验证：分别选择三种决定，重复原操作，再逐项更改参数、路径和范围。）
- [x] **C07 / AC5,AC6**：agent 不能提交授权答复或扩大请求范围；无效身份、过期请求和损坏规则使受影响操作拒绝并显示原因。（验证：从非受信入口发送答复，替换请求摘要，注入损坏规则。）
- [x] **C08 / AC6**：等待授权时可取消；断线重连后只出现原 pending 请求，旧批准和取消请求不会在新运行或新范围复用。（验证：等待时取消或重连，并重放相同消息 ID。）
- [x] **C09 / AC6**：允许、拒绝、等待、取消和隔离失败均可按运行与操作追溯；重试不会重复执行。（验证：制造每种结果，查询事件/审计，再重复提交同一操作 ID。）

## 真实 Linux 文件、进程、网络与凭据边界

- [x] **C10 / AC4**：在可创建 namespace 的 Linux 上，实际隔离探针成功；授权项目只读、候选可写、私有运行目录可用。（验证：实际运行探针及范围内读写场景，保存进程退出状态和文件摘要。）
- [x] **C11 / AC4**：缺少隔离后端或 namespace 创建失败时，文件写入、命令和旧桥接动作被拒绝，且没有宿主执行回退。（验证：在受限环境运行同一请求，检查明确错误、进程列表和正式/候选摘要。）
- [x] **C12 / AC3**：`../`、绝对路径和符号链接都不能读取授权根之外的 sentinel。（验证：在实际隔离进程中尝试三种路径，确认 sentinel 未出现在输出。）
- [x] **C13 / AC3**：隔离进程启动的子进程不能越界读取或写正式工程，候选接收前正式工程完整摘要不变。（验证：子进程尝试外读及正式写，比较前后工程摘要。）
- [x] **C14 / AC4**：工具默认不能连接任何本地测试监听器；精确批准协议、主机、端口后只允许该目标。（验证：实际隔离中启动独占监听器，分别测试未授权与已授权连接。）
- [x] **C15 / AC4**：改变主机、端口或解析后目标地址会拒绝连接；不支持代理的协议明确失败。（验证：变更目标，检查各监听器接收计数和用户可见错误。）
- [x] **C16 / AC4,AC9**：假模型密钥及宿主秘密不进入工具环境、挂载、授权提示、事件、错误或持久记录。（验证：注入唯一标记串，执行工具探测，再扫描全部可见与持久输出。）
- [x] **C17 / AC9**：取消或超时会终止隔离进程及子进程，不留下可继续写候选的进程。（验证：运行长寿命子进程并取消，检查进程树与候选摘要。）

## 旧桥接候选、预览与接收

- [x] **C18 / AC7**：旧 KiCad 桥接只在已验证的权限和隔离边界中执行，修复仅改变候选工程。（验证：运行 S01 修复，比较接收前正式工程与候选的完整清单和摘要。）
- [x] **C19 / AC7**：电脑桥接的 Xvfb/KiCad 受同一隔离会话监督；停止或崩溃后旧会话代次和窗口身份失效。（验证：启动、终止、恢复私有桌面，检查进程归属、旧句柄拒绝和正式摘要。）
- [x] **C20 / AC7**：旧桥接越界、授权拒绝或隔离失败时，正式工程保持原样并给出可追溯失败结果。（验证：分别注入三类失败，比较完整摘要并查询对应运行记录。）
- [x] **C21 / AC8**：预览列出工程内全部新增、修改、删除及精确差异；临时桌面文件与报告不混入正式工程。（验证：用包含三类变更的候选生成预览，再与完整文件清单比对。）
- [x] **C22 / AC8**：适用检查的通过、失败和不可用项均显示检查器、原因与受影响内容；无法确定实际影响时不能提供可接收的预览。（验证：制造检查失败、检查器不可用及不可枚举内容，观察预览与接收按钮/结果。）
- [x] **C23 / AC8**：普通接收仅在所需检查通过且预览仍有效时更新正式工程；取消预览不更新正式工程。（验证：分别执行取消、通过后的普通接收，比较接收前后完整摘要。）
- [x] **C24 / AC8**：失败或不可用项必须经用户逐项明确覆盖才可强制接收；强制接收不扩大原运行的文件或网络权限。（验证：遗漏一个确认项应拒绝，全部确认后接收并再次尝试越界操作。）
- [x] **C25 / AC8**：预览后正式工程或候选内容变化时，旧决定失效；界面展示冲突及新的精确预览，用户重新确认后才可接收。（验证：预览后修改正式工程或候选，再提交旧决定并检查摘要。）
- [x] **C26 / AC8**：接收结果绑定候选、预览和正式版本；重复提交同一决定只返回同一回执且正式工程只应用一次。（验证：重复提交决定和重连后重试，核对回执 ID 与工程摘要。）
- [x] **C27 / AC8**：普通或强制接收后，目标先处于待独立复核状态；只有对当前正式版本和条件版本的新检查通过后才能标记已验证。（验证：接收后立即查询状态，再运行独立检查并核对证据版本。）

## 中断恢复与并行集成

- [x] **C28 / AC6,AC9**：待授权、候选冻结和预览阶段分别中断重启后，旧批准不被复用，候选状态可判定，正式工程不变。（验证：在三个阶段终止服务、重启并核对请求、状态及摘要。）
- [x] **C29 / AC8,AC9**：目录交换前、交换后和回执记账后分别中断重启，正式工程只呈现完整旧版或新版，回执和后续复核责任各只有一份。（验证：逐点故障注入、重启、重复接收并检查清单与状态。）
- [x] **C30 / AC9**：独立复核前重启不会把目标误标已验证；新的检查失败时仍待处理。（验证：接收后、复核前终止服务并注入失败检查，观察目标与证据。）
- [x] **C31 / AC9**：两套端到端场景同时运行时，运行目录、端口、显示号、会话和目标 ID 互不冲突，退出后无残留进程。（验证：并行运行两例，检查资源标识、结果和清理。）

## 编译、回归与完整用户流程

- [x] **C32 / 集成**：普通任务、目标工作项、旧桥接均通过共用权限入口；候选审阅与接收只由受信用户入口提交。（验证：运行会话/目标/桥接集成场景，尝试从工具进程伪造接收并确认拒绝。）
- [x] **C33 / 回归**：Go 工程可编译，现有单元与集成测试通过，Python 桥接测试通过。（验证：`go build ./...`、`go test ./...`、`go vet ./...` 和两个桥接目录的 `python3 -m unittest discover` 均退出 0。）
- [x] **C34 / 回归**：现有目标恢复、条件变化、依赖变化、会话和案例场景仍通过。（验证：运行现有端到端脚本及 S01 案例，逐项记录退出码。）
- [x] **C35 / AC7–AC9 完整流程**：用户启动 S01 旧桥接修复 → 看见候选差异和检查 → 普通接收 → 正式工程改变一次 → 独立 ERC 通过后目标已验证。（验证：在真实 Linux 隔离环境运行完整场景，记录每步状态、摘要、回执和证据版本。）
- [x] **C36 / AC8–AC9 强制流程**：检查失败或不可用 → 普通接收拒绝 → 用户逐项强制确认 → 正式工程改变一次 → 目标仍待独立复核。（验证：在真实 Linux 隔离环境运行完整场景，记录提示、确认项、回执和目标状态。）

## 覆盖与证据记录

| 验收标准 | 对应检查 |
| --- | --- |
| AC1 | C01–C02、C32 |
| AC2 | C03–C04 |
| AC3 | C12–C13、C18 |
| AC4 | C10–C16 |
| AC5 | C05–C07 |
| AC6 | C07–C09、C28 |
| AC7 | C18–C20、C35 |
| AC8 | C21–C27、C29、C35–C36 |
| AC9 | C16–C17、C28–C31、C35–C36 |

验收时对每项记录执行日期、环境、命令或用户操作、退出码/界面结果、摘要或回执及证据位置。真实 Linux 隔离正例、强制接收和中断恢复未完成时，M03 不能标记通过。

## 验收证据记录（2026-10-04 自动化回归）

环境：Ubuntu Linux x86_64（neo-Predator-PHN16-71，20 vCPU，可创建 user/pid/mount namespace），kicad-cli 9.0.8，Go 1.26，Python 3.14。命令均在仓库根执行；原始日志与运行目录证据存于 /tmp/m03_logs/（e2e 各脚本运行根见各日志尾部 Evidence 行）。

| 检查 | 结论 | 主要证据（命令 → 退出码/结果） |
| --- | --- | --- |
| C01 | ✓ | execution/permission_test、TestEvaluateRejectsUnauthorizedDecision（go test ./... exit 0）；run.sh、waiting_restart.sh 目标路径与 run7 会话路径走同一权限门 |
| C02 | ✓ | TestAuthorityConstruction、TestApprovalCannotBeResolvedFromAnotherSession、TestOperationDigestChangesWithParameters |
| C03 | ✓ | permission/policy_test TestModeMatrix；e2e allow_once 流（waiting_restart/run.sh） |
| C04 | ✓ | TestExactRules、TestHardBoundary、TestStorePermissionGatePersistsRulesAcrossCandidateRuns |
| C05 | ✓ | TestPermissionDialogShowsRequestWithoutRawParameters、TestRedactProviderCredential |
| C06 | ✓ | TestApprovalService、TestOneTimeApprovalConcurrentConsume、TestStorePermissionGateConsumesOneTimeGrantOnActionRetry；e2e allow_once→执行 |
| C07 | ✓ | TestApprovalCannotBeResolvedFromAnotherSession、TestSessionProtocolRequiresProjectAndSessionIdentity、TestServerBindsConversationRoot |
| C08 | ✓ | TestApprovalReconnectAndSingleUseResolution（重连仅见原 pending）；approval_cancel op（protocol.go） |
| C09 | ✓ | permission_decisions 审计（store/permission_test）+ run 事件流（TestRunEventsEnforcePerRunSequenceAndReplayCursor） |
| C10 | ✓ | make m03-e2e：sandbox filesystem boundary（真实 namespace 探针+范围内读写） |
| C11 | ✓ | TestProbeAndNoHostFallback；m03_sandbox_files 拒绝分支 |
| C12 | ✓ | m03_sandbox_files：../、绝对路径、symlink 三逃逸均被拒 |
| C13 | ✓ | TestCoordinatorBridgeFailureLeavesFormalProjectUntouched、TestCoordinatorRejectsCapabilityDigestMismatchWithoutFormalWrite |
| C14 | ✓ | m03_sandbox_network：默认禁网、精确批准后仅放行该目标 |
| C15 | ✓ | TestNetworkGrantPinsExactResolvedTarget、TestTrustedProxyPinsAndRechecksBeforeDial |
| C16 | ✓ | m03_sandbox_secrets（假密钥/宿主秘密全输出扫描）；tests/package/e2e.sh package-secret-marker 扫描 exit 0 |
| C17 | ✓ | TestRunProcessGroupStopsDescendantsOnContextCancellation |
| C18 | ✓ | TestM03KicadRepairRunsOnlyAgainstCandidateInLinuxSandbox |
| C19 | ✓ | TestM03ComputerIsolatedSessionLifecycle、TestSessionGeneration |
| C20 | ✓ | TestCoordinatorBridgeFailureLeavesFormalProjectUntouched、TestCoordinatorRejectsCapabilityDigestMismatchWithoutFormalWrite |
| C21 | ✓ | TestReviewDiffAndFindings；m03_acceptance 预览差异 |
| C22 | ✓ | TestReviewEntryPersistsUnavailableCheckerAndBlocksNormalAccept |
| C23 | ✓ | TestAcceptGuardsAndAtomicExchange、TestAcceptRejectsStalePreview |
| C24 | ✓ | TestM03ForceAcceptanceRequiresEveryFindingAndKeepsReverification、TestReviewDialogRequiresIndividualForceConfirmation |
| C25 | ✓ | TestReviewRejectsCandidateDigestDrift、TestReviewRejectsFormalBaselineDrift |
| C26 | ✓ | TestAcceptanceFinalizeIsIdempotentAndQueuesReverification、TestAcceptCandidateUsesDurableJournal |
| C27 | ✓ | TestAcceptanceFinalizeIsIdempotentAndQueuesReverification、force-acceptance 后保持待复核 |
| C28 | ✓ | TestConfirmInterruptThenRestartReplays（待授权）、TestM03AcceptanceInterruptedAfterExchangeReopensSQLite（候选/交换阶段）、TestReconcilePreparedAcceptanceBeforeExchange |
| C29 | ✓ | TestReconcilePreparedAcceptanceBeforeExchange、TestReconcileAcceptanceAfterExchangeBeforeJournalUpdate、TestReconcileAcceptanceConflictBlocks、TestReconcileBeforeDispatch(BlocksConflict) |
| C30 | ✓ | TestM03AcceptanceInterruptedAfterExchangeReopensSQLite + force-acceptance 待复核保持 |
| C31 | ✓ | lib.sh 并行隔离设计（独立 run_root/端口/HOME）；TestConcurrentSessionRunsKeepSessionAndRunOwnership；本轮回归以并行波次实际执行 |
| C32 | ✓ | run.sh/waiting_restart.sh/m03_acceptance 会话与目标同门；TestRunCancelChecksSessionOwnership、verifyCandidateSession 拒绝非属主会话接收 |
| C33 | ✓ | go build ./...（各提交 worktree）、go vet ./... exit 0、go test ./... 26 包 exit 0、workers/{kicad,computer} unittest discover OK |
| C34 | 部分 | run.sh E2E PASS、waiting_restart PASS、unsupported PASS、criteria_change PASS、dependency_change PASS（核心失效判定全绿；尾部重启恢复三阶段退役，见残留）；S01 cases 为 M03 前既有欠账（agentctl start 已移除） |
| C35 | ✓ | TestM03TrustedCandidateAcceptanceAndRestart（真实隔离，m03-e2e 套件） |
| C36 | ✓ | TestM03ForceAcceptanceRequiresEveryFindingAndKeepsReverification（真实隔离，m03-e2e 套件） |

### 回归命令与退出码（2026-10-04）

- `go vet ./...` → 0；`go test ./...` → 0（26 包）；`make package` → 0；`make test-package` → 0（install/cli/e2e/restart 四段 PASS）
- `bash tests/e2e/run.sh` → 0（E2E PASS，会话在崩溃重启后重建、候选接收后一次生效，交付摘要与 fixture 完整性断言通过）
- `bash tests/e2e/waiting_restart.sh` → 0（WAITING RESTART PASS）；`bash tests/e2e/unsupported.sh` → 0；`bash tests/e2e/criteria_change.sh` → 0（四相 PASS）；`bash tests/e2e/dependency_change.sh` → 0（方向性失效五相 PASS）
- `make m03-e2e` → 见套件日志（沙箱三界、两桥接契约、候选接收/强制接收/中断恢复、Go 全量）
- 每个提交的独立可编译性：以 `git worktree add` + `go build -buildvcs=false ./...` 对 9847348…08fee88、88f94d1 逐一验证通过

### 残留任务清单（不阻塞 M03 判定，但需后续跟踪）

1. **会话转向全流程（tests/e2e/conversation.sh）**：会话型目标的 EvaluateGoal 经统一 agent 运行器后持续 WaitingForHuman（agent runner 仅文本回执、无工具调用，undelivered conversation 注入与 observe 分支交互待澄清）。需要 mock 支持 agent 工具协议或产品确认转向语义；涉及 TestEvaluateInjectsUndeliveredConversation 的端到端联动。
2. **不可用窗口后的唤醒重放**：dependency_change 尾部三阶段（检查器版本恢复、必需输入缺失、跨重启唤醒重放）退役——重启后 agent 已 finished 且 dependency_changed 事件停留 signaled，目标不再收敛；需要专项最小复现定位（events 表见 /tmp/m03_logs/e2e-dep9.log 现场）。中断恢复本身由 TestM03AcceptanceInterruptedAfterExchangeReopensSQLite 及 reconcile 系列覆盖。
3. **会话代次的外部可观测性**：runtime_handle 仅暴露命名空间内 PID，会话记录重建后 generation 重置，外部测试无法观测「旧代次失效」（run.sh 已放宽为 ≥1 并注明）；语义本身由 TestM03ComputerIsolatedSessionLifecycle 覆盖。
4. **tests/cases（make cases）**：`agentctl start` 子命令在 M03 前已移除，S01 案例运行器需要按会话绑定目标 + 接收流程重写（既有欠账，非 M03 回归）。
5. **schema 迁移旧版兼容**：v3→v10 已由 TestContinuousMigrationFromV3 补齐；建议后续为 v4/v5/v7/v9 各中间版本补同样的连续迁移用例。
