# M03 权限、隔离与旧桥接受控接收 Plan

> 基于已批准的 [spec.md](spec.md)。M03-A 交付通用权限与 Linux 隔离；M03-B 在该边界内迁移旧 KiCad/电脑桥接，并交付候选审阅、用户接收和独立复核衔接。M04 的通用工具循环只复用本阶段接口，不在 M03 执行。

## 架构概览

受信控制面从持久会话、目标和运行事实构造不可由客户端扩大的授权上下文。`internal/permission` 根据硬边界、权限模式及精确规则决定允许、拒绝或等待用户；待审批请求和用户决定先持久化，再经现有会话服务推送到 TUI。普通任务、目标工作项和旧桥接共用此入口。

`internal/sandbox` 使用实际启动探针确认 Linux 隔离能力。子进程只看见必要系统运行文件、只读项目输入、可写候选工程和私有运行目录；继承环境被清除，网络默认断开。需要授权网络时，子进程经受信白名单代理访问被批准的目标。电脑桥接由持续受监督的隔离会话持有私有 Xvfb/KiCad，不能让一次性 Python 父进程退出后留下未受控 GUI。

`internal/candidate` 为旧桥接准备工程副本和正式版本清单，桥接只修改副本。桥接完成后冻结候选、重新枚举全部变更并执行适用检查；预览及发现持久化。受信接收再次检查候选、预览和正式工程版本，以原子目录交换应用已展示的工程文件组，并以持久应用记录跨越文件系统和 SQLite 的提交边界。接收后目标进入待复核状态，由现有独立检查产生新证据。

两子项的汇合门槛为真实隔离探测与旧桥接候选场景通过。M03-A 的权限规则、协议与沙箱可分别开发；M03-B 的工程副本和预览逻辑可先使用 fake 桥接开发，但真实桥接执行必须等待 M03-A。

## 核心数据结构与接口

### 授权上下文与操作

`Authority` 由受信服务生成并冻结：`RunID`、`WorkRef`、会话/目标归属、授权项目根、候选根、基线版本、权限模式、资源限制、网络范围和 `ScopeDigest`。普通任务从持久会话确定项目根；目标工作项还须匹配目标授权根、工作项及条件版本。客户端请求中的 `AllowedScope`、`PermissionBounds` 仅可缩小服务端确定的范围。

`Operation` 表示稳定操作 ID、类别（read/write/command/network/legacy）、名称、目标的规范化相对路径、参数摘要和可选网络目标。绝对路径与符号链接须在受信侧解析并检查，隔离挂载仍是最终边界。`ScopeDigest` 与操作摘要共同确定本次询问的精确身份。

`PermissionDecision` 为 `allow`、`deny` 或 `ask`，包含原因、授权上下文摘要及可选审批 ID。`ApprovalRequest` 持久保存运行、操作、展示内容、创建时间和 pending/allowed_once/saved/denied/cancelled/expired 状态。`ExactRule` 保存效果、操作类别、名称、目标、参数摘要、授权根摘要和网络目标；规则损坏时拒绝受影响操作，deny > ask > allow。

`Mode` 保留 default、acceptEdits、plan、bypassPermissions 的用户可见语义。default 对范围内读取自动允许，写入和命令询问；acceptEdits 对范围内候选写入自动允许、命令仍询问；plan 对范围内读取自动允许，写入和命令仍询问，计划文件专用行为留给 M06；bypassPermissions 跳过一般询问。所有模式均先受硬边界和明确拒绝规则约束，网络仍须精确授权。

`PermissionService` 对外提供：

```go
Authorize(ctx context.Context, authority Authority, operation Operation) (PermissionDecision, error)
ResolveApproval(ctx context.Context, approvalID string, choice ApprovalChoice, principal UserPrincipal) (PermissionDecision, error)
CancelApproval(ctx context.Context, approvalID string, principal UserPrincipal) error
```

`ResolveApproval` 复核请求仍为 pending、操作与范围摘要未变化、答复来自受信用户入口。单次允许以一次性 token 消费；保存规则的匹配条件不得比所展示操作更宽。agent 运行事件与桥接输入均不包含答复凭据。

### 隔离配置与网络

`SandboxProfile` 包含只读系统运行挂载、只读授权项目输入、唯一可写候选工程、私有 HOME/TMP/XDG 目录、净化环境、运行上限及获批网络目标。`SandboxResult` 包含退出状态、受限输出、超时和隔离错误；不回传秘密环境。`SandboxSession` 包含会话 ID、代次、候选 ID、受监督进程句柄与状态，不把宿主 DISPLAY 或 X11 套接字提供给 agent。

`SandboxManager` 对外提供：

```go
Probe(ctx context.Context, profile SandboxProfile) error
RunIsolated(ctx context.Context, profile SandboxProfile, argv []string, stdin io.Reader) (SandboxResult, error)
StartIsolatedSession(ctx context.Context, profile SandboxProfile) (SandboxSession, error)
StopIsolatedSession(ctx context.Context, sessionID string) error
```

调用方传递 argv 数组，不拼接 shell 命令。`Probe` 实际启动与运行配置等价的最小进程，验证只读项目、候选写入、宿主私有路径不可读和默认网络不可达；程序存在但命名空间创建失败视为不可用。`NetworkGrant` 使用用户批准的协议、主机与端口；受信代理只转发这些目标，解析后的目标变化须重新核对授权。隔离内的本地代理进程通过专用 Unix 套接字与受信代理通信，对支持代理的命令提供 HTTP CONNECT/SOCKS5 入口；子进程仍保留独立网络空间，不直接共享宿主网络。不支持代理的协议返回明确失败。

### 候选、预览与接收

`Candidate` 保存候选 ID、工作项/动作归属、正式工程根和候选根、基线文件清单摘要、候选文件清单摘要、条件/依赖版本及 prepared/running/ready/reviewed/accepted/rejected/blocked 状态。文件清单覆盖工程文件的新增、修改和删除；临时桌面配置、日志和截图位于独立运行目录，不混入正式工程。候选冻结前必须停止会继续写入候选的 GUI 进程。

`Finding` 记录检查项目、结果（pass/fail/unavailable）、受影响文件、原因和检查器版本。`Review` 保存候选与当前正式版本、全部精确差异、检查发现、越界与授权发现、实际应用后的文件清单及 `PreviewDigest`。无法列出实际影响时不得提供可确认预览。

`AcceptanceDecision` 保存用户身份、候选 ID、候选内容摘要、预览摘要、正式版本、normal/force 方式及逐项确认的覆盖项。正式版本冲突先重建预览，旧决定失效；强制接收不扩大 agent 已运行的权限。`Receipt` 保存决定 ID、应用后正式版本与接收时间。`ApplyJournal` 保存 prepared/swapped/finalized 状态及交换前后两个工程清单摘要，用于崩溃后判定尚未应用或已交换待记账。

`CandidateController` 对外提供：

```go
CreateCandidate(ctx context.Context, action LegacyAction) (Candidate, error)
FreezeCandidate(ctx context.Context, candidateID string) (Candidate, error)
ReviewCandidate(ctx context.Context, candidateID string) (Review, error)
AcceptCandidate(ctx context.Context, decision AcceptanceDecision) (Receipt, error)
ReconcileAcceptances(ctx context.Context) error
```

`AcceptCandidate` 仅供受信控制面调用。它检查决定、预览、候选文件和当前正式工程，再记录应用意图并使用 Linux `renameat2(RENAME_EXCHANGE)` 原子交换同一文件系统中的正式工程目录；不支持原子交换时拒绝接收。若应用中断，`ReconcileAcceptances` 对比正式清单与日志：匹配旧版则重试，匹配新版则补全回执和待复核责任，其余情况阻塞并要求人工处理；任何不确定状态不能标为已验证。

## 模块设计

### 权限与审批（M03-A，F1/F2/F5/F6）

`internal/permission` 只接受服务端构造的 `Authority`。先检查工作归属、路径/网络硬边界和系统可用性，再应用规则和模式。审批请求由 `internal/store` 持久保存，`internal/conversation` 将其推送到所属会话并允许按游标补读。TUI 显示操作、目标、影响范围、原因和单次/保存规则/拒绝选项；会话服务用本地用户入口身份及未暴露给 sandbox 的答复凭据核对决定。取消和断线后保留 pending 的可恢复状态。

### Linux 隔离与私有桌面（M03-A，F3/F4/F7）

`internal/sandbox` 用 bubblewrap 的 argv 接口建立只读运行时挂载、项目输入与单独可写候选挂载，清除宿主环境，隔离 PID、挂载和网络，约束时限与输出。任何探测或启动失败都向上返回明确错误，不回退宿主执行。网络代理仅提供获批目标的出站 TCP 连接，默认不存在；模型 provider 在 sandbox 外运行。

电脑桥接使用长期运行的隔离会话管理器：同一私有桌面中的 Xvfb、KiCad 与桥接请求处理进程随会话一起受监督。重启时旧句柄和代次失效，先观察并重建，禁止复用旧窗口身份或直接重放动作。KiCad 的一次性检查和修复可调用 `RunIsolated`。旧 Python 桥接不得继承 `os.Environ`；证据路径在受信侧重新核验。

### 旧桥接候选与接收（M03-B，F7/F8）

`internal/candidate` 基于正式项目创建同一文件系统中的候选工程，记录完整基线。`internal/execution` 将旧动作的桥接目标改为候选路径，不再把正式路径传给修复桥接；动作先进入 candidate_ready/awaiting_accept，不能把候选修改记为 applied。受信侧冻结候选后运行 KiCad/ERC 等适用检查，形成 `Review` 并让目标等待用户。

`internal/conversation` 增加候选读取、普通/强制接收及取消请求；`internal/tui` 显示全部差异、失败/不可用项、越界、授权与版本冲突，并逐项收集覆盖确认。接收端重新比对正式版本；冲突时返回新预览要求重新确认。接收完成后，`internal/store` 写回正式版本、失效旧证据、标记 pending_reverification 并写入唤醒事件；`internal/core` 只在随后的独立检查对确切版本和条件版本通过后写 verified。

## 模块交互与状态流

```text
普通任务/目标工作项 → 受信端建立 Authority → 权限判定 → 允许/拒绝/持久审批
旧动作 → 建候选工程 → 权限判定与真实隔离 → 桥接修改候选 → 冻结与检查
候选就绪 → 持久 Review → TUI 展示 → 用户决定 → 重新核对 → 应用日志与目录交换
接收回执 → 待复核事件 → 独立检查正式工程 → 已验证或继续待复核
```

审批状态：pending → allowed_once / saved / denied / cancelled / expired；允许一次的 token 只能消费一次。候选状态：prepared → running → ready → reviewed → accepted / rejected / blocked。接收应用记录：prepared → swapped → finalized。所有状态转换使用受信存储的条件更新；旧回执的重试返回同一回执，不重复应用。

M03-A 的安全判定与真实隔离先完成；M03-B 的候选拷贝、清单、预览及 UI 可用 fake 桥接分别开发。真实旧桥接必须在安全门通过后接线。M04 通用工具只调用已验证的权限与沙箱接口，产生的候选复用 M03-B 接收流程。

## 文件组织

| 文件或目录 | 职责 |
| --- | --- |
| `internal/permission/{model,policy,rules,approval}.go` | 授权上下文、模式/规则、精确审批及审计 |
| `internal/sandbox/{linux,process,session,network}.go` | 实际探测、隔离执行、私有桌面与受信网络代理 |
| `internal/candidate/{workspace,review,accept}.go` | 工程清单、候选冻结、检查预览、原子接收与恢复 |
| `internal/store/schema.sql`、`permission.go`、`candidate.go` | 权限规则、审批、候选、预览、决定、回执和应用日志 |
| `internal/execution/{coordinator,python_bridge}.go` | 旧动作从正式直写切换到隔离候选与持久等待 |
| `internal/core/{types,activities}.go`、`internal/goalrun/create.go` | 候选/目标状态与新工程目录布局、复核唤醒 |
| `internal/conversation/{protocol,service,permission,review}.go` | 受信用户入口、审批和接收协议、断线补读 |
| `internal/tui/{model,permission_dialog,review_dialog}.go` | 权限请求、完整预览、逐项覆盖确认和结果展示 |
| `cmd/stable/chatserve.go`、`cmd/agentworker/main.go`、`internal/runtime/supervisor.go` | 控制面及 worker 装配、启动恢复扫描 |
| `workers/kicad/bridge.py`、`workers/computer/bridge.py` | 候选路径与持续私有桌面会话适配 |

测试按职责放在对应包；真实 Linux 边界、桥接候选接收及重启恢复场景放在 `tests/e2e/` 和 `tests/cases/`。每个场景使用独立运行目录、端口和私有显示号，避免并行冲突。

## 技术决策

| 决策点 | 选择 | 理由 |
| --- | --- | --- |
| 运行授权来源 | 由服务端从持久会话、目标和工作项重建；客户端字段只可缩小范围 | 避免伪造宽松 `ExecutionRequest` |
| Linux 隔离后端 | bubblewrap 实际启动探针；最小白名单挂载、净化环境、私有 PID/网络/临时目录 | 程序存在不代表可用，整根只读挂载会泄漏宿主数据 |
| 权限顺序 | 硬边界优先，规则 deny > ask > allow，最后应用模式 | 保留 mewcode 的操作习惯而不让 bypass 逃出隔离 |
| 网络 | 独立网络空间 + 受信目标白名单代理；未授权不提供代理 | 精确授权不等于开放整个宿主网络 |
| 电脑会话 | 长寿命隔离监督与代次失效；候选冻结前停止可写 GUI | 一次性包装无法约束后续 Xvfb/KiCad 进程 |
| 候选范围 | 新目标使用可版本化的正式工程目录；完整工程清单与候选同文件系统 | 多文件差异可精确预览和原子交换；旧数据格式无需兼容 |
| 接收事务 | 先持久记录用户决定和应用意图，再原子交换工程目录，最后记账与唤醒；启动时对账 | SQLite 与文件系统无法同事务提交，日志保证幂等恢复 |
| 验证结论 | 接收后先 pending_reverification，再运行现有独立检查 | 运行成功和接收成功都不等于目标达标 |
| Linux 验收 | fake 快速测试与可创建命名空间的真实 Linux 作业分开；隔离失败用例仍在受限环境验证 | 当前工具沙箱的 namespace 限制不能代表目标宿主能力 |

## 需求覆盖与实施门槛

| 需求 | 设计归属 |
| --- | --- |
| F1、F2、F5、F6 | `Authority`、`PermissionService`、精确规则、持久审批及 TUI |
| F3、F4 | `SandboxManager`、最小挂载、私有环境和受信网络代理 |
| F7 | 候选工程、旧桥接适配和持续隔离桌面 |
| F8 | `CandidateController`、预览/决定/回执、应用日志与独立复核 |

M03-A 的真实隔离未通过前，不接入任何旧桥接修改、命令或网络执行。M03-B 的预览和接收未通过前，不把候选修改应用到正式工程。四份 M03 文档及各自审批完成之前，不编写本计划中的实现代码。
