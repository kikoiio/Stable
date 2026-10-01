# V01 验收条件变化后的结论失效 Tasks

> 依据：已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。以下步骤在 `checklist.md` 也获批后执行。每项是约 2–5 分钟的聚焦工作单元；A/B/C 是独立 Claude CLI worktree，D 由主任务负责。每项先运行所列验证，再标记完成。

## 文件清单

| 操作 | 文件 | 职责/所有权 |
| --- | --- | --- |
| 修改 | `internal/core/types.go` | P0 建立共享类型；A 在交接后扩展存储接口 |
| 修改 | `internal/core/activities.go` | B：完整复核、版本守卫、决策衔接 |
| 新建 | `internal/core/current_evidence.go` | B：通用当前证据筛选 |
| 修改 | `internal/core/activities_test.go` | B：先复核、竞态、修复后复检 |
| 修改 | `internal/store/schema.sql`、`internal/store/sqlite.go` | A：迁移、事务和未处理事件 |
| 修改 | `internal/store/conversation_test.go`、`model_calls_test.go`、`sqlite_test.go` | A：确认、旧库与证据事务 |
| 修改 | `internal/execution/coordinator.go`、`coordinator_test.go` | B：旧版动作拒绝及测试 |
| 修改/新建 | `workers/kicad/bridge.py`、`erc.py`、`test_erc.py` | B：检查器版本、ERC 参数判定及测试 |
| 修改 | `internal/conversation/session.go`、`service_test.go` | C：确认调用及测试 |
| 修改 | `internal/goalrun/create.go`、`create_test.go` | C：工作流唤醒及测试 |
| 修改/新建 | `cmd/agentworker/main.go`、`main_test.go` | C：重送未处理事件及测试 |
| 修改 | `internal/report/report.go`、`report_test.go` | D：状态、历史、导出 |
| 修改 | `cmd/stable/chat.go`、`chat_test.go` | D：待复核终端提示 |
| 新建 | `tests/e2e/criteria_change.sh` | D：传感器板端到端场景 |
| 修改 | `Makefile` | D：接入端到端场景 |
| 新建 | `docs/spec_docs/V01/contracts/storage-v3.md`、`evidence-v1.md` | P0 起草，A 定稿版本化数据契约 |

## 准备与共享契约

### T01：记录当前基线

**文件：** 无源码改动。**依赖：** `checklist.md` 获批。**负责人：** 主任务。

**步骤：** 记录 `git rev-parse HEAD`、`git status --short`、`git diff --binary` 和未跟踪文件清单；逐项确认现有改动不属于 V01 新增。记录文档目录目前被 `/docs/` 忽略，后续提交 V01 文档时需单独纳入。

**验证：** 再运行 `git status --short`，与记录前完全相同；基线差异文件可读取。

### T02：加入共享事实类型

**文件：** `internal/core/types.go`。**依赖：** T01。**负责人：** 主任务。

**步骤：** 增加 `GoalPendingReverification`、`EvidenceProvenance`、`VerificationToken`、`VerificationResult`，并给 `Evidence`、`Decision` 增加计划中规定的可空版本字段；暂不扩展 `StateStore`，避免未实现的存储方法破坏编译。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/core` 通过；现有 JSON 字段仍可读取。

### T03：起草两份数据契约

**文件：** `docs/spec_docs/V01/contracts/storage-v3.md`、`evidence-v1.md`。**依赖：** T02。**负责人：** 主任务。

**步骤：** 写明 SQLite v3 新列、迁移和事件恢复规则；写明证据 v1 的必填字段、`unknown` 读法、当前证据条件和示例记录。每份控制在一页左右，并与 `plan.md` 字段一致。

**验证：** `rg -n '版本|迁移|unknown|criteria_revision|checker_version' docs/spec_docs/V01/contracts` 能找到相应定义；所有字段语义已写完整。

### T04：建立不改动主工作树的快照

**文件：** 无源码改动。**依赖：** T03。**负责人：** 主任务。

**步骤：** 使用 `git stash create` 生成包含已跟踪工作树内容的 Git 快照提交；另存 T01 列出的未跟踪代码文件和四份 V01 文档清单。快照提交只作为 worktree 基点，不更新 `master`、索引或当前文件。

**验证：** 快照 SHA 可由 `git cat-file -t <SHA>` 识别为 `commit`；`git status --short` 与 T01 基线相比只包含 T02/T03 的预期新增。

### T05：创建三个隔离 worktree

**文件：** 无源码改动。**依赖：** T04。**负责人：** 主任务。

**步骤：** 先检查本聊天已附着的 worktree；若无可复用项，用托管 worktree 工具从 T04 的快照 SHA 分别创建 A、B、C。把 T01 的未跟踪代码和 V01 文档复制到每个 worktree，核对内容；记录三个绝对路径。

**验证：** `git worktree list` 显示主树和 A/B/C；每个 worktree 的快照 SHA 相同，关键基线文件摘要与主树一致，三个工作目录互不相同。

### T06：下发并行任务边界

**文件：** 无源码改动。**依赖：** T05。**负责人：** 主任务。

**步骤：** 在 A、B worktree 分别启动 Claude CLI，提供已批准的四文档、各自负责文件和任务及禁止改动其他实现线文件的说明；要求每完成一组任务报告实际验证结果。C 待 T12 确认事务接口可用时启动。主任务保留 D 文件写入权。跨 worktree 的依赖由主任务按共同快照提取并同步已完成的文件差异，同步内容不纳入接收线的提交。

**验证：** 两个 Claude CLI 会话均报告其 worktree 路径、负责文件和首个任务；`git status --short` 显示它们尚未写入对方负责的文件。

## A：存储与迁移

### T07：扩展数据库结构

**文件：** `internal/store/schema.sql`、`sqlite.go`。**依赖：** T06。**负责人：** A。

**步骤：** 给证据添加可空标准版本、出处 JSON、失效原因，给决策添加可空标准版本；把新库版本设为 3，并让旧库迁移在事务中逐列安全添加。保留旧 `result` 值。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store -run 'Test.*Migrat'` 通过；新库 `PRAGMA user_version` 为 3。

### T08：迁移旧达标目标

**文件：** `internal/store/sqlite.go`、`model_calls_test.go`。**依赖：** T07。**负责人：** A。

**步骤：** 旧证据的新增字段读作未知；把缺少可判定当前证据的旧 `verified` 目标置为 `pending_reverification`，写明原因并插入一次可重送事件；重复打开数据库不得重复生成事件。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store -run 'Test.*Migrat'` 通过，旧证据行数不变且目标待复核。

### T09：读写证据出处

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T07。**负责人：** A。

**步骤：** 更新 `RecordEvidence` 与快照读取以保存/解析版本和出处；旧行返回空版本及未知出处，不把空值补成当前版本。原始结果和失效原因分别保存。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store -run 'Test.*Evidence'` 通过；新证据字段往返一致，旧行仍可读。

### T10：保存决策版本并守卫动作预留

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T07、T02。**负责人：** A。

**步骤：** 在决策读写中加入可空标准版本；`ReserveAction` 事务核对决策版本与当前目标版本，不匹配时拒绝新动作预留，旧 `nil` 决策也拒绝。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store -run 'Test.*Decision|Test.*Action'` 通过；过期决策不产生新 `prepared` 动作。

### T11：原子确认现有目标标准

**文件：** `internal/store/sqlite.go`、`conversation_test.go`。**依赖：** T09。**负责人：** A。

**步骤：** 实现 `ConfirmGoalCriteria`：读取待确认提案并校验标准，在一个事务内确认提案、增加目标标准版本、置待复核、给旧证据写失效原因、插入标准变更事件。无效或重复确认均不改目标。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store -run 'Test.*Confirm'` 通过；确认后的快照同时显示新版标准、待复核和事件。

### T12：固定确认事务的返回契约

**文件：** `internal/store/sqlite.go`、`conversation_test.go`、`internal/core/types.go`。**依赖：** T11。**负责人：** A。

**步骤：** 让 `CriteriaConfirmation` 返回已确认提案、目标和唯一事件；与 B/C 核对调用约定。暂不扩展 `StateStore`，直到两个新存储方法均已实现。此时 A 对 `core/types.go` 独占写入。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store ./internal/core` 编译并通过；重复确认不会返回第二个事件。

### T13：提交完整复核结果

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T09、T12。**负责人：** A。

**步骤：** 实现 `CommitVerification`：同一事务保存本轮全部证据，比较标准版本与产物摘要；匹配且全部通过才写 `verified`，匹配但失败写 `active` 和未满足项；不匹配给本轮证据写明失效原因，只保留历史证据并返回 `current=false`。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store -run 'Test.*Verification'` 通过；过期结果不改变当前状态。

### T14：保护其他状态变化

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T12。**负责人：** A。

**步骤：** 实现 `UpdateStatusForToken`，比较标准版本与摘要后才更新状态；产物摘要变更时若目标仍待复核，则保持待复核并更新原因。两个新存储方法都可编译后，在 `StateStore` 增加计划中的接口签名。覆盖旧模型返回等待、求助或错误的并发场景。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store -run 'Test.*Token|Test.*Artifact'` 通过；旧令牌不能覆盖待复核。

### T15：读取全部未处理事件

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T11。**负责人：** A。

**步骤：** 增加读取 `pending` 和 `signaled` 两种未处理事件的查询；保持原有事件 ID 去重，已 `processed` 事件不再返回。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store -run 'Test.*Event'` 通过，三个状态的返回集合符合预期。

### T16：完成 A 线存储验证与契约

**文件：** `internal/store/conversation_test.go`、`model_calls_test.go`、`sqlite_test.go`、两份 `contracts/*.md`。**依赖：** T08–T15。**负责人：** A。

**步骤：** 补齐崩溃边界、重复确认和旧证据测试；核对契约文档与真实列/字段，提交只属于 A 的差异并报告提交 SHA。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store` 通过；契约字段均有明确语义，提交仅包含 A 所有文件。

## B：验证与动作守卫

### T17：取得真实 ERC 检查器版本

**文件：** `workers/kicad/erc.py`、`bridge.py`、`test_erc.py`。**依赖：** T06。**负责人：** B。

**步骤：** ERC 运行时取得实际 `kicad-cli` 版本，并在能力结果中连同报告违规数返回检查器身份；版本命令失败时返回不可验证结果，不填猜测版本。

**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_*.py'`；测试用可控的 `kicad-cli` 响应核对 `checker_version` 与实际版本一致，版本命令失败时不返回通过。

### T18：按当前 ERC 阈值判定

**文件：** `workers/kicad/erc.py`、`bridge.py`、`test_erc.py`。**依赖：** T17。**负责人：** B。

**步骤：** 从请求接收当前标准的 `max_violations`，按独立 JSON 报告的违规数判定；缺失阈值沿用现有验证规则中的默认值 0，错误报告不可通过，同一报告可供多个 ERC 标准项生成各自结果。

**验证：** 对违规数 0、等于阈值、大于阈值的夹具/测试输入运行 Python 测试，结果依次为通过、通过、失败。

### T19：标识连线检查器

**文件：** `workers/kicad/bridge.py`。**依赖：** T06。**负责人：** B。

**步骤：** 为 `inspect_design` 返回固定且随检查语义升级的连线检查器 ID/版本；观察结果继续来自真实设计文件，截图或模型文本不进入检查结果。

**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_*.py'`；`inspect_design` 结果含连线检查器版本。

### T20：实现通用当前证据筛选

**文件：** `internal/core/current_evidence.go`、`activities_test.go`。**依赖：** T02。**负责人：** B。

**步骤：** 实现 `EvidenceCurrent`，要求通过、未失效、标准版本和摘要匹配、出处完整、来源为 `tool_check`；旧证据及截图均返回否。不得加入 KiCad 专有分支。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/core -run 'Test.*CurrentEvidence'` 通过。

### T21：按令牌收集 ERC 证据

**文件：** `internal/core/activities.go`、`activities_test.go`。**依赖：** T13、T17、T18。**负责人：** B。

**步骤：** 主任务先将 A 线 T13/T14 已完成的接口差异同步到 B worktree，且不纳入 B 提交。`VerifyGoal` 读取标准版本和摘要，使用包含版本与唯一 ID 的报告路径运行 ERC；根据当前阈值为每个 ERC 验收项产生带出处的证据，不在检查中途直接置 `verified`。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/core -run 'Test.*Verify'` 通过；两次不同版本的报告路径不同。

### T22：完成连线证据和整轮提交

**文件：** `internal/core/activities.go`、`activities_test.go`。**依赖：** T13、T19、T21。**负责人：** B。

**步骤：** 为每个当前连线验收项产生对应版本证据；汇总全部当前验收项的通过与未满足列表，调用 `CommitVerification` 一次提交。提交返回过期时不报告新版达标。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/core -run 'Test.*Criteria|Test.*Verify'` 通过；缺任一验收项均不达标。

### T23：待复核先检查再决策

**文件：** `internal/core/activities.go`、`activities_test.go`。**依赖：** T22。**负责人：** B。

**步骤：** `EvaluateGoal` 遇 `pending_reverification` 时先调用 `VerifyGoal`；通过直接结束本轮，未通过才进入现有模型决策和修复流程；版本过期时重新读取目标并等待/处理最新事件。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/core -run 'Test.*Pending|Test.*Evaluate'` 通过；已通过的新标准不会额外调用模型。

### T24：模型返回后守卫状态

**文件：** `internal/core/activities.go`、`activities_test.go`。**依赖：** T14、T23。**负责人：** B。

**步骤：** 决策记录写入标准版本；模型返回后再核对令牌，过期的动作/等待/求助/错误不改变新版目标；状态变化统一走 `UpdateStatusForToken`。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/core -run 'Test.*CriteriaRace|Test.*StaleDecision'` 通过。

### T25：阻止旧版动作执行

**文件：** `internal/execution/coordinator.go`、`coordinator_test.go`。**依赖：** T10。**负责人：** B。

**步骤：** 主任务先将 A 线 T10 已完成的存储差异同步到 B worktree，且不纳入 B 提交。`ExecuteOrReconcile` 在预留和真正执行前核对决策标准版本；版本过期或未知时把动作标记为阻塞，保持产物逐字节不变；已完成动作只供新版复核参考。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/execution -run 'Test.*Criteria|Test.*Stale'` 通过，并比较执行前后文件摘要。

### T26：完成 B 线验证

**文件：** `internal/core/activities_test.go`、`internal/execution/coordinator_test.go`、`workers/kicad/` 已负责文件。**依赖：** T17–T25。**负责人：** B。

**步骤：** 补齐“检查中再次改标准”的受控暂停测试，以及修复后重新取令牌的测试；提交只属于 B 的差异并报告提交 SHA。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/core ./internal/execution` 与 KiCad Python 测试通过，旧版结果没有把新版目标置为达标。

## C：会话与唤醒

### T27：唤醒运行中或已结束的目标

**文件：** `internal/goalrun/create.go`、`create_test.go`。**依赖：** T12。**负责人：** C。

**步骤：** 主任务先把 A 线 T12 的确认事务接口差异同步到 C worktree，且不纳入 C 提交。实现 `WakeGoal`，用现有 Temporal 任务队列及目标 ID 执行 `SignalWithStartWorkflow`；运行中只发信号，已结束则启动新一轮并发信号，事件 ID 保持不变。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/goalrun -run 'Test.*Wake'` 通过；两种工作流状态各得到一次事件。

### T28：重送未处理事件

**文件：** `cmd/agentworker/main.go`、`main_test.go`。**依赖：** T15、T27。**负责人：** C。

**步骤：** 主任务先将 A 线 T15 的事件查询差异同步到 C worktree，且不纳入 C 提交。worker 启动时读取 `pending` 和 `signaled` 事件并经 `WakeGoal` 补送；投递成功再保留/更新事件状态，失败只记录并留待再次恢复。把重送循环抽成可注入唤醒函数的单元，使两种事件状态可独立测试。

**验证：** `GOPROXY=off GOSUMDB=off go test ./cmd/agentworker -run 'Test.*Replay'` 通过；模拟唤醒器收到 `pending` 和 `signaled`，不收到 `processed`，失败事件保留供重试。

### T29：会话确认改用原子事务

**文件：** `internal/conversation/session.go`、`service_test.go`。**依赖：** T12、T27。**负责人：** C。

**步骤：** 已有目标的 `/confirm` 调用 `ConfirmGoalCriteria`，提交后生成确认消息并调用 `WakeGoal`；投递失败时仍保留事务内事件。新目标确认路径维持原行为。

**验证：** 在可监听本机 loopback 的环境运行 `GOPROXY=off GOSUMDB=off go test ./internal/conversation -run 'Test.*Confirm'`；确认回复出现时状态已待复核。

### T30：完成 C 线恢复验证

**文件：** `internal/conversation/service_test.go`、`internal/goalrun/create_test.go`。**依赖：** T27–T29。**负责人：** C。

**步骤：** 覆盖已完成工作流重开、重复信号、确认后立即中断再启动；提交只属于 C 的差异并报告提交 SHA。

**验证：** 在允许本机 loopback 的环境运行相关 Go 包测试；每个提案只增加一次版本，未处理事件最终得到处理。

## D：状态、导出与端到端场景

### T31：按全部当前标准计算状态

**文件：** `internal/report/report.go`、`report_test.go`。**依赖：** T20。**负责人：** 主任务。

**步骤：** 主任务先将 B 线 T20 的当前证据筛选差异同步到主工作树；`BuildStatus` 对当前目标的每个验收项寻找 `EvidenceCurrent` 认可的证据；ERC 还重新核对报告违规数与当前阈值。状态非 `verified`、证据缺项或产物摘要不符时 `Verified=false` 并列出原因。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/report -run 'Test.*Status|Test.*Criterion'` 通过；只有 ERC、缺连线证据时不达标。

### T32：区分当前与历史导出

**文件：** `internal/report/report.go`、`report_test.go`。**依赖：** T31。**负责人：** 主任务。

**步骤：** 在状态/导出中提供规范化证据视图，旧空字段明确为 `unknown`；只复制当前 ERC 为交付报告，可读取的旧报告放在历史目录，保留原始结果与失效原因。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/report -run 'Test.*Export|Test.*History'` 通过；待复核导出无当前 ERC 报告。

### T33：显示待复核提示

**文件：** `cmd/stable/chat.go`、`chat_test.go`。**依赖：** T02。**负责人：** 主任务。

**步骤：** 终端接收目标更新时将 `pending_reverification` 显示为“待复核”，同时显示标准版本和原因；保留其他状态原有输出。

**验证：** `GOPROXY=off GOSUMDB=off go test ./cmd/stable -run 'Test.*Chat|Test.*Pending'` 通过。

### T34：完成 D 线状态和导出验证

**文件：** `internal/report/report_test.go`、`cmd/stable/chat_test.go`。**依赖：** T31–T33。**负责人：** 主任务。

**步骤：** 覆盖旧证据、截图、检查器版本未知、标准版本变化和报告路径仍存在等边界；只记录并提交属于 D 的 V01 差异，不把 T01 的已有改动混入提交。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/report ./cmd/stable` 通过；提交差异仅含 D 的新增变更。

### T35：编写传感器板端到端场景

**文件：** `tests/e2e/criteria_change.sh`。**依赖：** T29、T31。**负责人：** 主任务。

**步骤：** 用独立运行目录和模拟模型创建并验证目标；确认新版标准后暂缓检查，核对待复核状态与导出，再允许自动复核；追加一次检查中改标准的受控暂停场景，最后核对新版证据和旧证据历史。此任务完成场景脚本，真实运行留给集成后的 T41。

**验证：** `bash -n tests/e2e/criteria_change.sh` 通过；脚本逐段检查待复核、完整复核、竞态、历史导出，所有命令和夹具路径均存在。

### T36：接入统一测试入口

**文件：** `Makefile`。**依赖：** T35。**负责人：** 主任务。

**步骤：** 把新脚本加入 `make e2e`，更新目标说明，不改变其他脚本的运行顺序与含义。

**验证：** `make -n e2e` 输出包含 `tests/e2e/criteria_change.sh`，且原有 e2e 命令仍按原顺序列出；真实运行留给 T41。

## 集成与交付

### T37：审阅三条实现线的差异

**文件：** A/B/C worktree 的提交。**依赖：** T16、T26、T30、T34。**负责人：** 主任务。

**步骤：** 对每条线比较其提交相对 T04 快照的文件清单和差异；共享给 B/C 的 A 线未提交同步差异必须从接收线提交中排除。检查是否越过文件所有权、是否覆盖已有改动、是否与 `plan.md` 接口一致。发现偏差先让对应线修正。

**验证：** 三条线各有提交 SHA、测试记录和只含本线文件的差异；主树的 T01 既有文件内容仍可核对。

### T38：集成存储和验证线

**文件：** 主工作树内 A/B 负责文件。**依赖：** T37。**负责人：** 主任务。

**步骤：** 按共同快照基点提取 A、B 各自负责文件的 V01 差异并应用到主工作树；不要把快照祖先里的既有改动当成新提交合并。解决接口编译冲突，不覆盖 T01 记录的旧改动；核对 `StateStore`、证据格式和动作守卫一致。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/store ./internal/core ./internal/execution` 通过，T01 旧改动仍在。

### T39：集成会话线并保留文档

**文件：** 主工作树内 C 负责文件及 V01 四文档/契约。**依赖：** T38。**负责人：** 主任务。

**步骤：** 应用 C 的 V01 增量，解决唤醒调用差异；将被 `/docs/` 忽略的 V01 文档明确加入交付提交，不纳入其他忽略文件。

**验证：** `GOPROXY=off GOSUMDB=off go test ./internal/conversation ./internal/goalrun` 在允许 loopback 的环境通过；`git diff --cached --name-only` 只列出预期 V01 文档及代码。

### T40：运行全量编译与单元测试

**文件：** 全仓。**依赖：** T39。**负责人：** 主任务。

**步骤：** 在允许本机 loopback 的环境运行 `GOPROXY=off GOSUMDB=off go test ./...`；检查 Python 适配器测试与格式检查；失败时只修具体失败项并重跑。

**验证：** Go 全量测试和 KiCad Python 测试均通过，测试输出保存供验收报告引用。

### T41：运行标准变更端到端流程

**文件：** `tests/e2e/criteria_change.sh`、必要的直接相关修正。**依赖：** T40。**负责人：** 主任务。

**步骤：** 运行新脚本和现有传感器板用例；核对已完成目标重开、旧版检查竞态、导出、重启事件补送；修复失败后重跑对应场景。

**验证：** 新脚本、相关现有 e2e 和场景用例均输出 PASS；没有旧版证据被计入当前达标。

### T42：提交 V01 增量并准备验收

**文件：** V01 代码、测试、四文档与契约。**依赖：** T41。**负责人：** 主任务。

**步骤：** 分组提交已验证的 V01 增量；逐项核对暂存区，不把 T01 基线中原有的未提交改动作为 V01 改动提交。记录提交 SHA、未纳入的原有改动和测试证据，再进入 `checklist.md` 逐项验收。

**验证：** `git show --stat` 只显示 V01 新增内容；`git status --short` 中原有改动仍可与 T01 对照；验收所需命令与输出已记录。

## 执行顺序

```text
T01 → T02 → T03 → T04 → T05 → T06
                              ├→ A: T07…T16 ────────────┐
                              ├→ B: T17…T20 → T21…T26 ──┤
                              └→ D: T31…T34 ────────────┤
                                    A 的 T12 → C: T27…T30 ┤
                                                      T35 → T36
                                           全部交付 → T37…T42
```

B 的 T21–T24 依赖 A 的 T13/T14；B 的 T25 依赖 A 的 T10；D 的 T31 依赖 B 的 T20。每条线在依赖未到位时只执行无依赖任务。

## 验证环境说明

当前沙箱运行 Go 包时，本机监听被限制，`internal/conversation` 的 `httptest` 基线测试因此无法在受限网络环境启动；需要允许 loopback 的本机环境做该包与端到端验证。其余 Go 定向包可使用 `GOPROXY=off GOSUMDB=off` 离线运行。环境限制须与真实测试失败分别记录，不能用“应当通过”替代实际结果。
