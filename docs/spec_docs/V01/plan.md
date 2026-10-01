# V01 验收条件变化后的结论失效 Plan

> 依据：已批准的 [spec.md](spec.md)。目标实施周期为 4–5 个工作日。此文只设计 V01；检查规则、项目设置和外部数据变化的自动失效留给后续小愿景。

## 架构概览

**事实存储**是当前结论的唯一来源。确认现有目标的新标准时，在一次 SQLite 事务内确认提案、增加标准版本、把目标置为 `pending_reverification`、记录旧证据的失效原因，并写入持久唤醒事件。确认响应在事务提交之后返回。旧证据和原始检查结果保留。

**验证与版本守卫**从目标标准版本和产物摘要组成一次复核令牌。独立检查先于模型修复决策执行；检查结束时连同证据提交，存储只允许令牌仍匹配的结果改变当前结论。模型决策也记录标准版本，执行协调器拒绝基于旧版本的待执行动作。

**工作流唤醒**使用现有 Temporal 工作流和事件表。运行中的工作流接收标准变更信号；已结束的工作流由同一唤醒入口启动新一轮。事件先持久化；worker 启动时补送尚未处理的事件，包括已投递但未完成处理的事件。

**状态与导出**使用同一套当前证据判定：只有标准版本、产物摘要、验收项与来源都适用的检查证据可支持达标。历史证据可查看，旧报告与当前交付报告分开。

## 核心数据结构与接口

### `core.GoalStatus`

增加 `GoalPendingReverification = "pending_reverification"`。标准确认事务写入此状态和包含新版本的 `Reason`；新版独立检查通过后为 `verified`，检查不通过并进入现有修复流程时为 `active`，无法继续时由现有流程转为 `needs_human`。终端把新状态显示为“待复核”。

### `core.EvidenceProvenance` 与 `core.Evidence`

```go
type EvidenceProvenance struct {
    SchemaVersion    int    `json:"schema_version"` // V01 为 1
    Claim            string `json:"claim"`
    Coverage         string `json:"coverage"`
    CheckerID        string `json:"checker_id"`
    CheckerVersion   string `json:"checker_version"`
    SourceLevel      string `json:"source_level"` // tool_check | observation | unknown
    InvalidationRule string `json:"invalidation_rule"`
}

type Evidence struct {
    // 保留现有 ID、GoalID、CriterionID、ArtifactID、Kind、Result、ReportPath、CreatedAt。
    CriteriaRevision *int                `json:"criteria_revision"` // nil 表示旧记录未知
    Provenance       *EvidenceProvenance `json:"provenance"`        // nil 表示旧记录未知
    InvalidatedReason string             `json:"invalidated_reason"`
}
```

`Result` 保留检查当时的原始 `pass`/`fail`；失效原因另存，不把新证据的原始结果改写成 `stale`。既有 `stale` 行保持原值，因其原始结果已不可恢复。新验收证据必须有非空出处和标准版本；截图可作为 `observation` 保存，但不得进入验收证据集合。读取旧行时，状态/导出把缺失出处明确显示为 `unknown`。

### `core.VerificationToken` 与 `core.VerificationResult`

```go
type VerificationToken struct {
    GoalID           string
    CriteriaRevision int
    ArtifactID       string
}

type VerificationResult struct {
    Evidence []Evidence
    Passed   bool
    Unmet    []string
}

func (a *Activities) VerifyGoal(ctx context.Context, goalID string) (result VerificationResult, current bool, err error)
```

一次完整复核使用同一令牌。检查报告文件名包含标准版本和唯一检查 ID，避免不同版本在同一产物摘要下覆盖报告。

### `core.Decision`

增加 `CriteriaRevision *int`。新决策必须记录读取时的版本；旧记录为 `nil`，未执行动作无法证明适用于当前标准，应拒绝执行。已经完成的动作不回滚，其结果由新版检查重新评价。

### 存储及唤醒接口

```go
type CriteriaConfirmation struct {
    Proposal core.CriteriaProposal
    Goal     core.Goal
    Event    core.Event
}

func (s *Store) ConfirmGoalCriteria(ctx context.Context, proposalID string) (CriteriaConfirmation, error)
func (s *Store) CommitVerification(ctx context.Context, token core.VerificationToken, result core.VerificationResult) (current bool, err error)
func (s *Store) UpdateStatusForToken(ctx context.Context, token core.VerificationToken, status core.GoalStatus, reason string) (current bool, err error)
func WakeGoal(ctx context.Context, temporalAddress, goalID, eventID string) error
func EvidenceCurrent(goal core.Goal, artifactID string, evidence core.Evidence) bool
```

`ConfirmGoalCriteria` 只处理已有目标的提案，校验失败或重复确认均不改目标；新建目标仍走现有创建路径。`CommitVerification` 始终保存可追溯的检查记录；令牌过期时将该记录标为历史且不改变当前状态，返回 `current=false`。令牌有效时，只有全部当前标准通过才写入 `verified`。`EvidenceCurrent` 是模型上下文与报告共同使用的通用筛选器，不包含 KiCad 专有判断。

`core.StateStore` 增加 `CommitVerification` 与 `UpdateStatusForToken` 对应签名；会话使用存储实现的 `ConfirmGoalCriteria`。`EvidenceCurrent` 只接受未失效、版本和摘要匹配、出处完整且来源等级为 `tool_check` 的通过证据。未通过的当前复核把目标转为 `active` 并记录未满足项，然后才进入现有修复决策。旧版模型返回的等待、求助或错误也只能通过 `UpdateStatusForToken` 尝试改状态，令牌过期时不得覆盖新版待复核状态。

## 模块设计

### 存储与数据契约

**职责：** 迁移数据库；保存标准版本、证据出处、失效原因和决策版本；执行确认与复核提交事务。

**对外接口：** `ConfirmGoalCriteria`、`CommitVerification`，以及扩展后的快照读取和决策记录。

**依赖：** `core` 中的事实类型，不依赖会话、报告或 Temporal。

数据库以事务升至 `PRAGMA user_version = 3`。`evidence` 增加可空的标准版本、出处 JSON 和失效原因；`decisions` 增加可空的标准版本。旧证据保留原始行，空字段按未知解释，不推断其检查器版本。迁移把缺少可判定当前证据的旧 `verified` 目标转为待复核并插入可补送的事件。读取未处理事件时包括 `pending` 与 `signaled`，由事件 ID 去重。契约分别记录在 `contracts/storage-v3.md` 和 `contracts/evidence-v1.md`。

### 目标活动与当前证据

**职责：** 从当前标准和产物取得令牌，先运行完整独立检查，再决定是否调用现有修复能力；在模型上下文中只提供当前证据。模型调用返回后重查令牌，旧版结果不再驱动动作或状态变更。

**对外接口：** 保留 `EvaluateGoal`，将 `VerifyGoal` 调整为上面的完整结果签名，增加通用的 `EvidenceCurrent`；验证结束调用 `CommitVerification`。

**依赖：** `StateStore`、`ArtifactStore`、现有能力接口和执行协调器。标准版本过期时重新读取最新目标，不沿用旧版结果。产物摘要变化时若目标处于待复核状态，保持待复核并更新原因。现有 KiCad 检查逻辑不向通用证据筛选器扩散。

### 执行协调器

**职责：** 执行前比较决策的标准版本与当前目标；阻止旧版准备中或结果未知的动作再次写入。动作预留时在存储事务中再核对一次版本；已开始的动作可留下历史结果，但不能确认新版目标。

**对外接口：** 保留 `ExecuteOrReconcile`，内部增加标准版本守卫。

**依赖：** 存储、产物摘要、现有策略与能力调用接口。

### 会话与唤醒

**职责：** 确认已有目标的提案后调用单个存储事务，生成确认消息并唤醒目标；worker 启动时重送未处理事件。

**对外接口：** 会话保留 `/confirm`；`WakeGoal` 封装 Temporal 的 `SignalWithStartWorkflow`，使用目标 ID、现有任务队列和事件 ID；worker 从存储读取全部未处理事件并重送。

**依赖：** 会话依赖存储和唤醒入口；唤醒入口依赖 Temporal 客户端，不更改事实存储。

### KiCad 检查适配器

**职责：** 在现有 ERC 与连线检查结果中提供准确的检查器身份和版本。ERC 记录实际 `kicad-cli` 版本；连线检查使用随代码维护的语义版本。ERC 的 `max_violations` 参数按检查报告中的违规数判定；同一轮多个 ERC 验收项可共用一次报告，但每项分别产生证据。

**对外接口：** 现有能力结果的 `postcondition` 增加检查器信息与违规数；没有可核对的版本或报告时检查不可作为通过证据。

**依赖：** 已安装的 KiCad 命令和现有传感器板检查代码。

### 状态、导出与终端

**职责：** 统一检查全部当前验收项的证据覆盖；状态和导出以规范化证据视图明确显示待复核、未满足项、历史失效原因及未知出处。当前 ERC 报告仅在其证据适用于当前标准和产物时复制为交付报告；可取得的旧报告归档到导出历史目录。

**对外接口：** 保留 `BuildStatus`、`Export` 和现有 CLI 命令；扩展输出字段而不删除既有字段。

**依赖：** 只读目标快照、产物文件和报告文件。截图与模型陈述不参与达标判定。

## 模块交互

```text
用户 /confirm
  → 会话读取提案
  → 存储 ConfirmGoalCriteria：校验 + 确认 + 版本增加 + 待复核 + 历史失效 + 事件（单事务）
  → 会话尝试 WakeGoal 投递事件或为已结束目标启动新一轮；持久事务已提交后返回确认
  → 工作流读取最新标准与产物，先独立复核
  → 存储 CommitVerification：保存证据，并比较令牌后更新当前结论
       ├─ 版本已过期：仅留历史；处理最新事件并重新复核
       ├─ 全部通过：目标 verified
       └─ 未通过：现有模型决策 → 标准版本守卫 → 现有修复 → 重新取令牌并复核
  → 状态与导出按当前版本和产物投影，历史证据单独展示
```

`ConfirmGoalCriteria` 提交早于成功响应；唤醒晚于持久事件写入。进程在两者之间终止时，事件仍可在 worker 重启后补送。检查进行中修改标准时，旧检查仍可完成并留下历史记录，但其提交不能改变新版目标状态；旧模型输出也不能覆盖待复核状态。若旧决策尚未执行，协调器在写入产物前拒绝它。重复投递按事件 ID 去重，不重复确认提案或复用旧版结论。

## 文件组织

| 操作 | 文件 | 职责 |
| --- | --- | --- |
| 修改 | `internal/core/types.go` | 新状态、证据出处、版本令牌和决策版本 |
| 修改 | `internal/core/activities.go` | 先复核后决策、带令牌提交、过期结果处理 |
| 新建 | `internal/core/current_evidence.go` | 通用当前证据筛选 |
| 修改 | `internal/store/schema.sql`、`internal/store/sqlite.go` | v3 迁移、确认与复核事务、快照读写 |
| 修改 | `internal/conversation/session.go` | 已有目标确认路径改用单事务 |
| 修改 | `internal/goalrun/create.go`、`cmd/agentworker/main.go` | 唤醒已结束工作流与待处理事件补送 |
| 修改 | `internal/execution/coordinator.go` | 旧标准决策的执行守卫 |
| 修改 | `workers/kicad/bridge.py`、`workers/kicad/erc.py` | 检查器版本与 ERC 违规数 |
| 修改 | `internal/report/report.go`、`cmd/stable/chat.go` | 当前结论、历史证据、导出和待复核提示 |
| 新建 | `docs/spec_docs/V01/contracts/storage-v3.md`、`docs/spec_docs/V01/contracts/evidence-v1.md` | 一页式版本化数据契约 |
| 修改 | `internal/store/conversation_test.go`、`internal/store/model_calls_test.go`、`internal/store/sqlite_test.go` | 提案确认、旧库迁移、事件与证据事务 |
| 修改 | `internal/core/activities_test.go`、`internal/execution/coordinator_test.go` | 复核竞态与旧版动作守卫 |
| 修改 | `internal/conversation/service_test.go`、`internal/goalrun/create_test.go` | 确认与已完成目标的唤醒 |
| 修改 | `internal/report/report_test.go`、`cmd/stable/chat_test.go` | 全标准覆盖、历史证据与待复核提示 |
| 新建/修改 | `tests/e2e/criteria_change.sh`、`Makefile` | 传感器板标准变更端到端演示及测试入口 |

## 并行实施与集成

V01 保持一份面向用户行为的 spec。模块边界和数据契约在本 plan 中固定；`task.md` 按文件所有权拆为可并行的实现线，并在 `checklist.md` 中统一验收端到端行为。四份文档全部获批后才启动实现。

当前仓库只有主工作树，且其中有未提交改动；`claude` CLI 已安装。实施前先记录当前 `HEAD`、已跟踪差异和相关未跟踪文件，生成**不改变主分支和当前工作树**的一致基线快照。三个隔离 worktree 均从该快照创建，必要的未跟踪文件逐一复制并核对内容。现有未提交改动不作为 V01 的新增改动提交。

| 阶段/实现线 | 文件所有权 | 启动条件 | 交付接口 |
| --- | --- | --- | --- |
| P0 契约基线（主任务） | `internal/core/types.go` 的新增事实类型与状态；两份数据契约初稿 | 四文档获批 | 可编译的共享类型、固定字段与返回语义 |
| A 存储与迁移（Claude CLI 1） | `internal/store/`、数据契约定稿；P0 交接后独占 `internal/core/types.go` 的存储接口扩展 | P0 | `ConfirmGoalCriteria`、`CommitVerification`、`UpdateStatusForToken` 及迁移测试 |
| B 验证与动作守卫（Claude CLI 2） | `internal/core/activities.go`、`current_evidence.go`、`internal/execution/`、`workers/kicad/` | P0；先做适配器与守卫，活动接入待 A 接口完成 | 令牌复核、当前证据筛选和旧决策拒绝 |
| C 会话与唤醒（Claude CLI 3） | `internal/conversation/session.go`、`internal/goalrun/`、`cmd/agentworker/main.go` | A 的确认接口可用 | 原子确认的调用方、已结束工作流唤醒及事件补送 |
| D 状态与验收集成（主任务） | `internal/report/`、`cmd/stable/chat.go`、`tests/e2e/criteria_change.sh`、`Makefile` | P0；集成验证等 A–C 完成 | 当前与历史输出、端到端演示 |

每个文件同一时刻只有一条实现线负责写入；`core/types.go` 在 P0 完成后明确交接给 A。A、B 的无依赖部分与 D 可先并行；B 的活动接入和 C 的确认调用在 A 接口落地后继续，避免在不同 worktree 里分别猜测事务返回值。各线只提交其所有文件的 V01 增量。主任务审阅每条增量相对共同基线的差异，将其依次应用到保留未提交改动的主工作树，解决接口偏差，然后运行全量测试和端到端验收。跨模块验收由主任务负责，不能用各线局部通过代替。

## 技术决策

| 决策点 | 选择 | 理由 |
| --- | --- | --- |
| 标准变更的失效粒度 | 每次确认整轮失效并完整复核 | V01 优先证明结论正确；逐项复用留给后续依赖细化 |
| 数据格式 | SQLite v3 增量迁移 + 证据出处格式 v1 | 保持现有单机存储，可检查、可备份，并保留旧数据 |
| 旧版已验证目标 | 缺少可判定当前证据时转待复核并排队 | 不能把未知出处推断为可信新版证据 |
| 确认与复核的并发 | 单事务确认；令牌按标准版本与产物摘要条件提交 | 避免“新版标准配旧版达标结论”和检查中改标准的竞态 |
| 唤醒已结束目标 | Temporal `SignalWithStartWorkflow` + 持久事件去重 | 复用现有工作流，无需另建调度器；已结束目标可重新运行 |
| ERC 判定 | 使用当前标准参数和独立报告中的违规数 | 避免仅凭旧的无违规报告或固定阈值宣称达标 |
| 检查来源 | KiCad 实际版本 + 连线检查器语义版本；未知不算通过 | 可追溯且不伪造来源，满足首版证据契约 |
| 修复范围 | 只复用当前修复能力，并在执行前检查决策版本 | 满足 V01 演示，不提前建设新执行器或新工具 |

## Spec 覆盖核对

| 需求 | 设计归属 |
| --- | --- |
| F1 | `ConfirmGoalCriteria` 的校验、提案状态与版本事务 |
| F2 | 确认事务中的 `pending_reverification`、旧证据失效原因与同步状态投影 |
| F3 | `EvidenceProvenance`、KiCad 检查适配器、旧行未知标记及数据契约 |
| F4 | `WakeGoal`、工作流先复核、现有修复与复检 |
| F5 | `VerificationToken` 条件提交、`Decision.CriteriaRevision` 执行守卫 |
| F6 | `EvidenceCurrent`、全部验收项覆盖检查、当前与历史报告分离 |
