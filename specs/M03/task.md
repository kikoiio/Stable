# M03 权限、隔离与旧桥接受控接收 Tasks

> 依据已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。A 为共用权限与 Linux 隔离，B 为旧桥接候选与接收，V 为测试设施及端到端验证。每个编号是一个聚焦的 2–5 分钟工作单元；步骤中的测试与实现一同完成。四份文档全部批准后才执行代码任务。

## 文件清单

| 操作 | 文件 | 职责与所有权 |
| --- | --- | --- |
| 新建 | `internal/permission/{model,policy,rules,approval}.go` 及对应 `*_test.go` | A 权限负责人：上下文、硬边界、模式、精确规则和审批 |
| 新建 | `internal/sandbox/{linux,process,network,session}.go` 及对应 `*_test.go` | A 沙箱负责人：探针、隔离执行、网络代理和私有桌面 |
| 新建 | `internal/candidate/{workspace,review,accept}.go` 及对应 `*_test.go` | B 候选负责人：清单、副本、预览、原子接收和恢复 |
| 修改 | `internal/store/schema.sql`、`internal/store/sqlite.go` | 存储迁移负责人：先完成 A06–A07，再完成 B05；不得并行改同一文件 |
| 新建 | `internal/store/permission.go`、`internal/store/candidate.go` 及对应测试 | A/B 各自所有：条件状态转换、审计、候选与应用日志 |
| 修改 | `internal/execution/{coordinator,python_bridge}.go`、`workers/kicad/{bridge,schematic}.py`、`workers/computer/bridge.py` | B 桥接负责人：仅执行候选、净化进程环境、私有 GUI |
| 修改 | `internal/core/{types,activities}.go`、`internal/goalrun/create.go` | B 目标负责人：候选等待、正式版本、接收后独立复核 |
| 修改/新建 | `internal/conversation/{protocol,service,permission,review}.go`、`internal/tui/{model,permission_dialog,review_dialog}.go` | 会话与 TUI 负责人：A 协议和审批先行，B 审阅接收后续；共享文件串行 |
| 修改 | `cmd/stable/chatserve.go`、`cmd/agentworker/main.go`、`internal/runtime/supervisor.go` | 装配负责人：在 A/B 接口稳定后统一接线和恢复 |
| 新建/修改 | `tests/e2e/m03_*.sh`、`tests/e2e/lib.sh`、`tests/e2e/*.sh`、`tests/cases/run_case.sh`、`scripts/run_local.sh`、`Makefile` | V 验证负责人：隔离资源、真实 Linux 场景、回归入口 |

## A. 权限与隔离

| ID | 文件 | 依赖 | 步骤 | 验证 |
| --- | --- | --- | --- | --- |
| A01 | `internal/permission/model.go`、`model_test.go` | 无 | 定义 `Authority`、`Operation`、`Mode`、`PermissionDecision`；用规范字段生成稳定 `ScopeDigest` 与操作摘要。 | `go test ./internal/permission -run TestDigestStable`：同输入同摘要、范围变化摘要不同。 |
| A02 | `internal/permission/policy.go`、`policy_test.go` | A01 | 先校验运行归属、授权根和规范路径；拒绝跨根、符号链接及正式工程写入。 | `go test ./internal/permission -run TestHardBoundary`：全部越界请求为 deny。 |
| A03 | `internal/permission/policy.go`、`mode_test.go` | A02 | 实现四种模式的询问表；硬边界与明确拒绝始终优先，网络必须有精确授权。 | `go test ./internal/permission -run TestModeMatrix`：四模式及 bypass 越界结果符合 plan。 |
| A04 | `internal/permission/rules.go`、`rules_test.go` | A01,A02 | 精确匹配类别、名称、目标、参数和范围摘要；按 deny > ask > allow 决定，损坏规则拒绝。 | `go test ./internal/permission -run TestExactRules`：相邻路径或参数不命中。 |
| A05 | `internal/permission/approval.go`、`approval_test.go` | A01,A04 | 定义 pending、allowed_once、saved、denied、cancelled、expired 转换与一次性 token 的内存契约。 | `go test ./internal/permission -run TestApprovalTransition`：非法转换和二次消费失败。 |
| A06 | `internal/store/schema.sql`、`internal/store/permission_schema_test.go` | A01,A05 | 增加规则、审批与决定审计表，包含运行/操作/范围摘要、唯一键和状态约束。 | `go test ./internal/store -run TestPermissionSchema`：新库建表与约束有效。 |
| A07 | `internal/store/sqlite.go`、`internal/store/sqlite_test.go` | A06 | 将旧版 schema 迁移到权限表版本；旧数据和已有运行记录保留。 | `go test ./internal/store -run TestPermissionMigration`：旧版与新库均可打开。 |
| A08 | `internal/store/permission.go`、`permission_test.go` | A07 | 实现规则读取、审批 CAS、单次 token 原子消费和决定审计查询。 | `go test ./internal/store -run TestPermissionStore`：并发消费只有一次成功。 |
| A09 | `internal/permission/approval.go`、`approval_service_test.go` | A03,A04,A05,A08 | 实现 `Authorize`、`ResolveApproval`、`CancelApproval`；复核身份、状态、操作及范围摘要后落库。 | `go test ./internal/permission -run TestApprovalService`：拒绝、一次、保存、取消均有审计。 |
| A10 | `internal/conversation/permission.go`、`permission_test.go` | A01,A09 | 从持久会话、目标、工作项重建 `Authority`；客户端限制只可收窄，不可扩大。 | `go test ./internal/conversation -run TestAuthorityConstruction`：伪造根或工作项被拒绝。 |
| A11 | `internal/sandbox/process.go`、`process_test.go` | A01 | 定义 `SandboxProfile`、`SandboxResult`、`SandboxManager`；校验根目录、argv、资源上限及隔离错误。 | `go test ./internal/sandbox -run TestProfileValidation`：缺少候选根或不可信参数失败。 |
| A12 | `internal/sandbox/linux.go`、`linux_test.go` | A11 | 构造 bubblewrap argv：最小只读运行时、授权项目只读、候选独占可写、私有 HOME/TMP/XDG、PID/网络隔离、清空继承环境。 | `go test ./internal/sandbox -run TestBubblewrapArgs`：无宿主整根挂载、无正式可写挂载。 |
| A13 | `internal/sandbox/linux.go`、`probe_test.go` | A12 | `Probe` 实际运行等价最小进程，检查候选写、项目只读、私有路径和默认断网；namespace 失败视为不可用。 | `go test ./internal/sandbox -run TestProbe`：可用宿主通过，受限宿主明确拒绝。 |
| A14 | `internal/sandbox/process.go`、`run_test.go` | A12,A13 | 实现 `RunIsolated` 的 argv 执行、超时、输出截断和取消；启动或探测失败绝不回退宿主执行。 | `go test ./internal/sandbox -run TestRunIsolated`：失败时宿主 sentinel 不变。 |
| A15 | `internal/sandbox/network.go`、`network_test.go` | A11 | 定义协议/主机/端口精确 `NetworkGrant`，校验解析地址与授权目标。 | `go test ./internal/sandbox -run TestNetworkGrant`：更换主机、端口或解析结果均拒绝。 |
| A16 | `internal/sandbox/network.go`、`proxy_test.go` | A12,A15 | 用受信 Unix 套接字提供 HTTP CONNECT/SOCKS5 转发和会话取消，隔离进程仍处独立网络空间。 | `go test ./internal/sandbox -run TestNetworkProxy`：仅获批本地测试目标收到连接。 |
| A17 | `internal/sandbox/session.go`、`session_test.go` | A11,A12,A13 | 实现隔离会话 ID、代次、候选绑定和进程组启动/停止；重启使旧代次失效。 | `go test ./internal/sandbox -run TestSessionGeneration`：旧句柄不可复用。 |
| A18 | `internal/sandbox/session.go`、`desktop_test.go` | A17 | 在会话内监督私有 Xvfb/KiCad 生命周期；冻结前停止写者，崩溃时清理整个会话。 | `go test ./internal/sandbox -run TestDesktopLifecycle`：停止后无残留写进程。 |
| A19 | `internal/conversation/protocol.go`、`permission_protocol_test.go` | A05 | 增加审批推送、答复、取消和补读消息；字段包含操作、目标、范围、原因但不含答复凭据。 | `go test ./internal/conversation -run TestApprovalProtocol`：无效决定/扩大范围拒绝。 |
| A20 | `internal/conversation/permission.go`、`internal/conversation/service.go`、`permission_integration_test.go` | A08,A09,A10,A19 | 持久 pending 后推送；本地用户入口复核答复并按游标补读，取消后不能执行。 | `go test ./internal/conversation -run TestApprovalReconnect`：重连只见原请求且只执行一次。 |
| A21 | `internal/tui/permission_dialog.go`、`permission_dialog_test.go` | A19 | 显示操作、目标、范围和原因；提供拒绝、批准一次、保存精确规则选项。 | `go test ./internal/tui -run TestPermissionDialog`：三个选项生成正确消息。 |
| A22 | `internal/tui/model.go`、`permission_model_test.go` | A20,A21 | 将弹层接入状态模型；断线重连恢复 pending，完成后清除旧提示。 | `go test ./internal/tui -run TestPermissionReconnect`：旧提示不能再次批准。 |

## B. 旧桥接候选、审阅与接收

| ID | 文件 | 依赖 | 步骤 | 验证 |
| --- | --- | --- | --- | --- |
| B01 | `internal/candidate/workspace.go`、`workspace_test.go` | 无 | 定义 `Candidate` 与 `ManifestEntry`；规范相对路径并拒绝穿越。 | `go test ./internal/candidate -run TestManifestPath`：越界路径失败。 |
| B02 | `internal/candidate/workspace.go`、`manifest_test.go` | B01 | 枚举正式工程全部文件、权限和摘要；拒绝符号链接与特殊文件，生成稳定清单摘要。 | `go test ./internal/candidate -run TestManifest`：链接拒绝，顺序不影响摘要。 |
| B03 | `internal/candidate/workspace.go`、`copy_test.go` | B02 | 在正式工程同文件系统建候选副本；桌面缓存和报告另放私有运行目录。 | `go test ./internal/candidate -run TestCreateCandidate`：正式摘要不变，副本完整。 |
| B04 | `internal/candidate/workspace.go`、`freeze_test.go` | B03 | 通过写者停止回调冻结候选并重算清单；先以 fake 写者验证晚写入阻塞，真实 GUI 在 B19 接入。 | `go test ./internal/candidate -run TestFreezeCandidate`：晚写入不能进入预览。 |
| B05 | `internal/store/schema.sql`、`internal/store/sqlite.go`、`migration_test.go` | A07,B01 | 在 A 迁移之后增加候选、预览、决定、回执、应用日志和动作等待状态；升级旧库。 | `go test ./internal/store -run TestCandidateMigration`：旧库升级且旧动作保留。 |
| B06 | `internal/store/candidate.go`、`candidate_test.go` | B05 | 实现候选创建、条件状态转换和重复 ID 幂等读取。 | `go test ./internal/store -run TestCandidateTransition`：非法跃迁拒绝。 |
| B07 | `internal/candidate/review.go`、`review_test.go` | B02,B04 | 比对全部新增、修改、删除文件，生成文本精确 diff；无法列明实际影响则阻塞。 | `go test ./internal/candidate -run TestReviewDiff`：完整变更清单可重算。 |
| B08 | `internal/candidate/review.go`、`finding_test.go` | B07 | 增加 `Finding` 和可替换检查器；记录 pass/fail/unavailable、版本、原因及影响文件。 | `go test ./internal/candidate -run TestReviewFindings`：不可用检查不被误记通过。 |
| B09 | `internal/candidate/review.go`、`internal/store/candidate.go`、`preview_test.go` | B06,B08 | 固化正式/候选清单、差异和发现为 `PreviewDigest` 并持久化；版本变化使旧预览失效。 | `go test ./internal/candidate -run TestPreviewDigest`：摘要稳定且变化可见。 |
| B10 | `internal/candidate/accept.go`、`accept_guard_test.go` | B09,A09 | 复核用户、候选、预览、正式版本及 normal/force 覆盖项；冲突要求新预览。 | `go test ./internal/candidate -run TestAcceptGuards`：旧预览和未逐项确认被拒。 |
| B11 | `internal/candidate/accept.go`、`exchange_test.go` | B10 | 封装同文件系统 `renameat2(RENAME_EXCHANGE)` 原子目录交换；跨设备或不支持时拒绝。 | `go test ./internal/candidate -run TestExchangeProjectDir`：正式目录只呈现旧版或新版。 |
| B12 | `internal/candidate/accept.go`、`internal/store/candidate.go`、`accept_idempotent_test.go` | B11 | CAS 写 prepared 应用日志，交换目录，写 swapped/finalized 和唯一 `Receipt`；重复决定返回原回执。 | `go test ./internal/candidate -run TestAcceptIdempotent`：接收计数始终为一。 |
| B13 | `internal/candidate/accept.go`、`reconcile_test.go` | B12 | 启动恢复比对旧/新清单：旧版重试、新版补账、第三版阻塞。 | `go test ./internal/candidate -run TestReconcileAcceptance`：三状态均不误报已验证。 |
| B14 | `internal/store/candidate.go`、`goal_transition_test.go` | B12 | 接收记账事务更新正式摘要、失效旧证据、写 pending_reverification 与唯一唤醒事件。 | `go test ./internal/store -run TestAcceptedGoalPendingReverification`：重复事务只唤醒一次。 |
| B15 | `internal/goalrun/create.go`、`internal/core/types.go`、`project_layout_test.go` | B03,B14 | 新目标区分正式工程、候选与运行目录；检查报告永远写运行目录。 | `go test ./internal/goalrun ./internal/core -run TestProjectLayout`：正式工程无报告文件。 |
| B16 | `internal/execution/coordinator.go`、`coordinator_test.go` | B06,B09,B15 | 旧动作先建候选，桥接只收到候选路径；完成后进入 awaiting_accept，不能记 applied/verified。 | `go test ./internal/execution -run TestRepairOnlyCandidate`：接收前正式摘要不变。 |
| B17 | `internal/execution/python_bridge.go`、`python_bridge_test.go` | A14,B16 | 旧 Python 桥接改走 `RunIsolated`，显式净化环境；探针/启动错误直接返回。 | `go test ./internal/execution -run TestBridgeSandboxFailure`：无宿主回退执行。 |
| B18 | `workers/kicad/bridge.py`、`workers/kicad/schematic.py`、`workers/kicad/test_candidate.py` | B16,B17 | 只接受候选路径和期望摘要；修复回执与候选摘要吻合，不接受正式根。 | `python3 -m unittest discover -s workers/kicad`：正式 sentinel 不变。 |
| B19 | `workers/computer/bridge.py`、`workers/computer/test_candidate.py` | A18,B03,B17 | GUI 动作绑定候选 ID/会话代次；停止后旧窗口身份失效，不重放旧动作。 | `python3 -m unittest discover -s workers/computer`：旧代次请求失败。 |
| B20 | `internal/core/activities.go`、`activities_test.go` | B14,B16 | awaiting_accept 时目标等待；接收唤醒后仅由独立 `VerifyGoal` 对当前正式/条件版本产生新证据。 | `go test ./internal/core -run TestAcceptNeedsReverification`：强制接收也不能直接 verified。 |
| B21 | `internal/conversation/protocol.go`、`review_protocol_test.go` | A19,B09,B10 | 增加 review_get/accept/force/cancel 消息和字段校验；客户端不能指定更宽根或伪造归属。 | `go test ./internal/conversation -run TestReviewProtocol`：无效摘要/覆盖项拒绝。 |
| B22 | `internal/conversation/review.go`、`internal/conversation/service.go`、`review_service_test.go` | A20,B12,B14,B21 | 受信入口核对用户与候选归属，持久化决定；冲突返回新预览，广播接收状态。 | `go test ./internal/conversation -run TestReviewService`：跨会话接收失败。 |
| B23 | `internal/tui/review_dialog.go`、`internal/tui/model.go`、`review_dialog_test.go` | A22,B21,B22 | 展示完整差异、检查和冲突；普通/逐项强制确认、取消与重连恢复。 | `go test ./internal/tui -run TestReviewDialog`：覆盖项缺失无法提交。 |
| B24 | `internal/runtime/supervisor.go`、`supervisor_test.go` | B13,B20 | 启动先调用 `ReconcileAcceptances`，未对账完不派发新动作。 | `go test ./internal/runtime -run TestReconcileBeforeDispatch`：恢复顺序正确。 |
| B25 | `cmd/agentworker/main.go`、`main_test.go` | A14,B17,B20,B24 | 在 worker 入口装配权限、沙箱、候选和旧桥接，隔离不可用时拒绝动作。 | `go test ./cmd/agentworker -run TestWorkerAssembly`：无宿主回退。 |
| B26 | `cmd/stable/chatserve.go`、`chatserve_test.go` | A20,A22,B22,B23,B25 | 在受信服务入口装配审批、预览、接收、审计与启动恢复。 | `go test ./cmd/stable -run TestChatserveAssembly`：仅受信入口可提交决定。 |

## V. 验证设施与端到端场景

| ID | 文件 | 依赖 | 步骤 | 验证 |
| --- | --- | --- | --- | --- |
| V01 | `tests/e2e/lib.sh` | 无 | 分配独立 run_root、端口、goal/run ID、HOME/TMP 和清理回调；避免固定共享资源。 | 运行两个 helper 实例：路径/端口/ID 不同，退出无遗留进程。 |
| V02 | `scripts/run_local.sh`、`tests/e2e/lib.sh` | V01 | 支持预构建二进制目录，默认单次调用行为保留。 | 预构建一次后两个 run_root 可启动，原脚本单跑通过。 |
| V03 | `tests/e2e/m03_fixture.sh` | V01 | 从现有 S01 工程复制正式/候选到同一文件系统，准备外部 sentinel 与假密钥；清理不碰仓库 fixture。 | fixture 自检：正式 SHA 等于源件，清理后源件仍在。 |
| V04 | `tests/e2e/run.sh` | V02 | 改为使用独立 run_root、端口和 ID，保留原恢复断言。 | `bash tests/e2e/run.sh`：PASS。 |
| V05 | `tests/e2e/waiting_restart.sh` | V02 | 改为使用独立资源，保留等待重启断言。 | `bash tests/e2e/waiting_restart.sh`：PASS。 |
| V06 | `tests/e2e/parallel.sh` | V02 | 改为使用独立资源，保留原并行断言。 | `bash tests/e2e/parallel.sh`：PASS。 |
| V07 | `tests/e2e/unsupported.sh` | V02 | 改为使用独立资源，保留不支持操作的断言。 | `bash tests/e2e/unsupported.sh`：PASS。 |
| V08 | `tests/e2e/criteria_change.sh` | V02 | 改为使用独立资源，保留条件版本变化断言。 | `bash tests/e2e/criteria_change.sh`：PASS。 |
| V09 | `tests/e2e/dependency_change.sh` | V02 | 改为使用独立资源，保留依赖变化断言。 | `bash tests/e2e/dependency_change.sh`：PASS。 |
| V10 | `tests/e2e/conversation.sh` | V02 | 改为使用独立资源，保留会话断言。 | `bash tests/e2e/conversation.sh`：PASS。 |
| V11 | `tests/cases/run_case.sh` | V02 | 将 case runner 的固定端口和 ID 改为 helper 分配。 | `bash tests/cases/run_case.sh S01_missing_wire`：PASS。 |
| V12 | `internal/permission/mode_test.go`、`rules_test.go` | A03,A04 | 添加四模式与明确拒绝优先的交叉用例。 | `go test ./internal/permission -run 'TestModeMatrix|TestExactRules'`：AC1/AC2 通过。 |
| V13 | `internal/conversation/permission_integration_test.go` | A09,A20 | 添加跨运行、取消及重连后一次性批准失效用例。 | `go test ./internal/conversation -run TestApprovalReconnect`：AC5/AC6 通过。 |
| V14 | `tests/e2e/m03_sandbox_files.sh` | V03,A13,A14 | 建立真实隔离文件场景，尝试 `../` 读取外部 sentinel 与候选内写入。 | 可建 namespace 的 Linux 上运行脚本：外部不可读，候选写入成功。 |
| V15 | `tests/e2e/m03_sandbox_files.sh` | V14 | 在同场景添加符号链接越界读取。 | 运行脚本：链接越界失败，sentinel 不变。 |
| V16 | `tests/e2e/m03_sandbox_files.sh` | V15 | 在同场景添加子进程越界和正式工程写入尝试。 | 运行脚本：子进程失败，接收前正式 SHA 不变。 |
| V17 | `tests/e2e/m03_sandbox_network.sh` | V01,A13,A16 | 用独占本地监听器验证默认断网与获批目标可达。 | 可建 namespace 的 Linux 上运行脚本：仅获批监听器收到连接。 |
| V18 | `tests/e2e/m03_sandbox_network.sh` | V17 | 添加改主机、端口、解析结果后的拒绝断言。 | 运行脚本：三个未授权目标均未收到连接。 |
| V19 | `tests/e2e/m03_sandbox_secrets.sh` | V03,A13,A14,A20 | 注入假模型密钥与宿主秘密，扫描工具环境、挂载、事件及错误。 | 可建 namespace 的 Linux 上运行脚本：输出与持久记录无标记串。 |
| V20 | `internal/candidate/review_test.go`、`finding_test.go` | B08,V03 | 用 fake 桥接覆盖 S01/S02/U01/U04 的新增、修改、删除及检查失败。 | `go test ./internal/candidate -run 'TestReviewDiff|TestReviewFindings'`：预览完整。 |
| V21 | `internal/candidate/accept_guard_test.go` | B10,V20 | 添加普通/强制接收、逐项覆盖与正式版本变化用例。 | `go test ./internal/candidate -run TestAcceptGuards`：旧预览拒绝。 |
| V22 | `internal/candidate/reconcile_test.go` | B13,B14 | 在 prepared 后注入中断并重开存储。 | `go test ./internal/candidate -run TestReconcilePrepared`：旧版可重试且仅一回执。 |
| V23 | `internal/candidate/reconcile_test.go` | V22 | 在 swapped 后注入中断并重开存储。 | `go test ./internal/candidate -run TestReconcileSwapped`：新版补账且仅一复核责任。 |
| V24 | `internal/candidate/reconcile_test.go` | V23 | 在 finalized 后重复提交并重开存储。 | `go test ./internal/candidate -run TestReconcileFinalized`：同一回执、不重复应用。 |
| V25 | `tests/e2e/m03_kicad_candidate.sh` | V03,V16,V18,V19,B18,B20,B26 | 在真实隔离中启动 S01 KiCad 修复并获取候选。 | 可建 namespace 的 Linux 上运行脚本：产生候选 ID。 |
| V26 | `tests/e2e/m03_kicad_candidate.sh` | V25 | 获取完整差异与发现，断言接收前正式 SHA 不变。 | 运行脚本：预览含全部变更，正式 SHA 不变。 |
| V27 | `tests/e2e/m03_kicad_candidate.sh` | V26 | 通过受信入口普通接收，验证单一回执与正式版本变化。 | 运行脚本：一次接收、一次版本变化。 |
| V28 | `tests/e2e/m03_kicad_candidate.sh` | V27 | 触发独立 ERC，确认当前版本与条件版本的目标结论。 | 运行脚本：仅新证据通过后状态为 verified。 |
| V29 | `tests/e2e/m03_computer_session.sh` | V03,V16,A18,B19,B26 | 在真实隔离中启动私有 Xvfb/KiCad，检查代次与进程归属。 | 可建 namespace 的 Linux 上运行脚本：全部进程受会话监督。 |
| V30 | `tests/e2e/m03_computer_session.sh` | V29 | 终止会话并恢复新代次，检查旧窗口失效及正式 SHA。 | 运行脚本：无残留进程，旧代次拒绝，正式 SHA 不变。 |
| V31 | `tests/e2e/m03_force_accept.sh` | V03,V16,V21,B22,B26 | 制造检查 fail/unavailable，断言普通接收被拒。 | 可建 namespace 的 Linux 上运行脚本：正式 SHA 不变。 |
| V32 | `tests/e2e/m03_force_accept.sh` | V31 | 对已展示限制逐项强制确认并读取回执及目标状态。 | 运行脚本：正式版本改变但仍 pending_reverification。 |
| V33 | `tests/e2e/m03_accept_restart.sh` | V01,V03,V24,V28,B26 | 候选冻结前中断、重启并确认原候选状态可判定。 | 可建 namespace 的 Linux 上运行脚本：无未受控写者。 |
| V34 | `tests/e2e/m03_accept_restart.sh` | V33 | 预览后正式版本变化，再重启并尝试旧决定。 | 运行脚本：旧预览失效，必须重新审阅。 |
| V35 | `tests/e2e/m03_accept_restart.sh` | V34 | 在目录交换前后分别注入中断并重试接收。 | 运行脚本：正式版只旧或新，回执唯一。 |
| V36 | `tests/e2e/m03_accept_restart.sh` | V35 | 在独立复核前中断、重启并读取目标状态。 | 运行脚本：恢复后仍待复核，绝不误标 verified。 |
| V37 | `Makefile`、`tests/e2e/m03_suite.sh` | V04–V36 | 汇总受影响包和 AC1–AC9 场景；各场景隔离端口、目录、display，安装类测试串行。 | `go test ./...` 与 M03 suite 全部通过，并保存真实 Linux 结果。 |

## 执行顺序与最大并行批次

1. **契约与测试资源**：A01、B01、V01 可并行；A01 完成后 A11、A02 可与 B02、V02/V03 并行。A06→A07→B05 是单一 schema 迁移链；A08 与 B06 改不同 store 文件，可并行。
2. **独立实现线**：权限 A02→A03/A04→A05→A09→A10/A19→A20，A19→A21，A20/A21→A22；沙箱 A11→A12/A15，A12→A13→A14/A17，A15→A16，A17→A18；候选 B01→B02→B03→B04→B07→B08→B09→B10→B11→B12→B13/B14。无跨线依赖的节点同时执行，候选预览先用 fake 桥接。
3. **共享文件汇合**：`schema.sql/sqlite.go` 按 A06/A07→B05；`conversation/protocol.go` 按 A19→B21；`conversation/service.go` 按 A20→B22；`tui/model.go` 按 A22→B23；`sandbox/session.go` 按 A17→A18 后由 B19 使用接口；装配文件分别由 B24→B25→B26 单一负责人顺序修改。候选 `workspace.go`、`review.go`、`accept.go` 内部也按表中依赖串行。
4. **安全门**：A13 的真实 Linux 探针和 V14–V19 的越界、断网、秘密测试通过后，才可运行 B17–B19 的真实旧桥接及 V25–V36。受限 CI/工具沙箱中的探针拒绝是失败即拒绝证据，不能代替可建 namespace 宿主上的正例。B17 的代码与 fake 测试可提前准备；真实执行待安全门。
5. **并行验证与汇合**：V04–V11 可在各脚本独立资源下并行；V12/V13、V20–V24 可与上述实现线并行。安全门后 V25、V29、V31 三条脚本线在不同 run_root、端口、私有 display 下并行；V33 等 V28 与 V24；最终 V37 汇总。任何两个任务改同一文件或使用同一 SQLite 库、端口、display、fixture 时串行或先隔离资源。

所有单项验证通过后才标完成；并行汇合点运行相应包的集成测试。不得把探针失败静默记为通过，也不得在审批完成前执行实现任务。
