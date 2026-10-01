# V02 工程依赖变化后的证据失效 Plan

> 依据：已批准的 [spec.md](spec.md)。目标实施周期约 5 个工作日；只覆盖当前传感器板的 ERC 与连线检查。若任务拆解表明超出 5 天，先按用户可验收行为拆小，再提交任务文档审批。

## 架构概览

### 依赖采集

KiCad 适配器在与 ERC 相同的目标专属配置环境中，采集每个检查类别实际选用的工程输入和检查器版本。项目 ERC 设置、项目符号库表和库文件、KiCad 全局符号库表与路径变量归入 `kicad.erc`；固定传感器连线检查器的身份与版本归入 `sensor.connection`。采集结果以内容摘要标识，不使用文件修改时间。采集器区分可选输入不存在、必需输入缺失、内容损坏及版本不可得。

### 变化协调

所有用户可见的状态查询与导出，以及每次目标运行，先经过共用的依赖刷新入口。该入口采集新快照后调用存储事务：若某类别快照变化，事务保存新快照、推进目标依赖代次、仅使该类别证据失效、将目标置为待复核并插入一个持久唤醒事件。事务提交后尝试投递事件；失败时保持未处理状态，沿用 V01 的事件补送。刷新再读取最新目标快照供状态或导出使用。相同快照重复刷新不写事件。

### 定向复核与投影

核心检查流程按验收项寻找当前证据，只运行缺证据的类别。检查开始与结束时刷新依赖；复核令牌绑定标准版本、原理图摘要和目标依赖代次。提交事务保存本轮证据，再把新证据与仍有效的历史证据合并判断所有验收项。版本不匹配的结果仅保留为历史。状态与导出使用同一当前证据规则，并展示证据的输入快照、检查器版本及失效原因。

### 迁移

存储格式升至 v4，证据出处升至 v2。V01 证据原样保留，缺少依赖快照显示为 `unknown`，不能证明当前达标。迁移中对旧版已达标目标写入待复核状态和幂等唤醒事件；运行时首次刷新建立当前依赖基线，然后重新检查。

## 核心数据结构与接口

### 检查类别与输入

```go
type CheckFamily string // "kicad.erc" | "sensor.connection"

type DependencySource struct {
    Kind     string // project_erc | project_symbol_table | symbol_library | global_symbol_table | path_variable
    Identity string // 可辨认的输入名称或路径；路径变量只显示名称
    Digest   string // 输入内容或变量值的 SHA-256；未知时为空
    State    string // present | absent_optional | missing_required | unreadable | malformed
    Reason   string // 非 present 状态的可读原因
}

type DependencySnapshot struct {
    SchemaVersion  int // 1
    Family         CheckFamily
    Sources        []DependencySource // 按 Kind、Identity 排序
    CheckerID      string
    CheckerVersion string
    Fingerprint    string // 由类别、来源状态与摘要、检查器身份和版本生成
    Available      bool
    Reason         string // 不可用时的可读原因
}
```

`absent_optional` 是稳定、可验证的状态，不等同于错误。必需库缺失、配置解析失败或检查器版本无法取得时 `Available=false`，仍生成可持久化的快照；不会产生通过证据。项目设置只取影响 ERC 的配置内容；库表按整张表比较，库文件按被表选用且属于当前原理图所用库的文件比较，不解析到单个符号。路径变量只采集解析所需的键和值摘要，不把原值写入状态或导出。

### 目标、证据和令牌

```go
type Goal struct {
    // 既有字段省略
    DependencyRevision int64 `json:"dependency_revision"`
}

type GoalSnapshot struct {
    // 既有字段省略
    Dependencies []DependencySnapshot `json:"dependencies"`
}

type EvidenceProvenance struct {
    // v1 字段沿用；新证据 SchemaVersion=2
    Family     CheckFamily         `json:"family"`
    Dependency *DependencySnapshot `json:"dependency,omitempty"`
}

type VerificationToken struct {
    GoalID             string
    CriteriaRevision   int
    ArtifactID         string
    DependencyRevision int64
}

type Decision struct {
    // 既有字段省略
    DependencyRevision *int64 `json:"dependency_revision,omitempty"`
}
```

每条 v2 验收证据冻结检查时的依赖快照；v1 行不补写。`EvidenceCurrent(snapshot, actualArtifactID, evidence)` 仅接受原结果通过、未失效、标准与原理图匹配、v2 出处完整且来源为独立工具检查、对应类别快照 `Available=true` 且指纹相同的证据。当前检查器版本已包含在指纹里。模型文本与截图继续被排除。

目标共用一个单调递增的 `DependencyRevision` 保护正在进行的检查和动作；各类别独立指纹决定哪些既有证据可以保留。依赖变化后即使内容又恢复为旧摘要，已写的失效原因也不能被清除，必须得到新的检查证据。

SQLite v4 为 `goals` 增加非空 `dependency_revision`（默认 0），为 `decisions` 增加可空 `dependency_revision`（旧决策为未知），新增 `goal_dependencies(goal_id, family, snapshot_json)`，主键为目标与检查类别。证据表不增列：v2 快照保存在既有 `provenance` JSON 中。`GetGoalSnapshot` 读取当前各类别快照；同一事务更新这些行、目标与代理状态、受影响的证据以及事件。

### 协调接口

```go
type DependencyCollector interface {
    Collect(context.Context, Goal) ([]DependencySnapshot, error)
}

type DependencyRefresher interface {
    Refresh(context.Context, string) (DependencyRefresh, error)
}

type DependencyRefresh struct {
    ChangedFamilies    []CheckFamily
    DependencyRevision int64
    Event              *Event // 新变化时的持久事件；无变化时为 nil
    Snapshot           GoalSnapshot
}

// StateStore 增加：
ReconcileDependencies(context.Context, string, []DependencySnapshot) (DependencyRefresh, error)
```

`Collect` 的普通输入故障写入不可用快照；只有采集器本身无法运行等系统错误才返回 `error`。`ReconcileDependencies` 在一个事务中比较并保存所有类别；新目标首次采集建立基线。已有证据但没有依赖版本的目标由迁移转待复核。变化事件 ID 由目标 ID 与新依赖代次确定，因此重复刷新幂等。`Refresh` 在事务后投递事件，投递失败仍返回最新持久状态与未处理事件，供补送程序重试。

既有 `CommitVerification(token, result)`、`UpdateStatusForToken(token, ...)` 和动作预留核对依赖代次。`CommitVerification` 不以本轮的 `result.Passed` 单独决定目标达标，而是保存本轮证据后，按全部当前验收项重新计算；本轮只检查了 ERC 时可复用当前连线证据。令牌过期时本轮证据标明失效原因并返回 `current=false`。

## 模块设计

### KiCad 依赖采集模块

**职责：** 提供与 `kicad.run_erc` 一致的配置环境，解析和摘要 ERC 相关项目设置、项目/全局符号库表、路径变量及所选库文件；取得真实 KiCad CLI 版本与连线检查器版本。库表发生任何内容变化可保守使 ERC 失效，库文件以库为单位，不追踪单个符号。可选表缺失形成稳定来源状态；必需输入缺失或损坏形成不可用快照。

**对外接口：** KiCad bridge 的 `kicad.describe_dependencies` 只读操作，返回两个 `DependencySnapshot` 及可用性原因。

**依赖：** 目标原理图与项目目录、ERC 实际使用的目标专属 KiCad 配置、KiCad CLI。不得读取或扫描未被检查环境选用的宿主配置。

### 依赖协调模块

**职责：** `Refresh` 统一执行采集、事务协调、事件投递与最新快照读取。状态查询、导出、对话 `/status` 和目标运行共用此路径，避免各入口出现不同的当前性判断。

**对外接口：** `DependencyRefresher.Refresh(ctx, goalID)`；唤醒函数以回调注入，调用现有 `WakeGoal`。

**依赖：** `DependencyCollector`、`StateStore`、唤醒回调；不依赖报告模块。

### 持久状态模块

**职责：** v4 迁移；按检查类别保存当前快照；在单个事务内推进依赖代次、定向失效、设置待复核、插入事件；提交复核时聚合全部当前证据；对旧决策与旧令牌拒绝动作和状态更新。

**对外接口：** `ReconcileDependencies`、扩展语义的 `CommitVerification`、`UpdateStatusForToken`、`ReserveAction`、`GetGoalSnapshot`。

**依赖：** SQLite 与 `core` 数据类型；不读取工程文件，不调用模型或 KiCad。

### 核心复核模块

**职责：** 运行前刷新；按当前证据缺口选择 ERC 或连线检查；检查后再次刷新，阻止期间变化的输入产生当前结论；不可用输入时保持待复核并说明原因。新决策绑定依赖代次。

**对外接口：** 既有 `Activities.EvaluateGoal`、`VerifyGoal`，由注入的 `DependencyRefresher` 完成刷新。

**依赖：** `StateStore`、KiCad capability、依赖刷新接口、既有独立检查与动作策略。

### 状态与导出模块

**职责：** 对刷新后的快照统一投影当前证据、缺口和历史；在导出中只把当前 ERC 报告放入交付位置，历史报告保留并标记原因。展示新证据的来源名称与摘要、检查器版本；旧字段显示 `unknown`。

**对外接口：** 既有 `BuildStatus`、`Export`；调用方负责先 `Refresh` 再读取最新快照。

**依赖：** `core` 当前证据规则、工程原理图与报告文件；不修改依赖状态。

## 模块交互

1. `stable goal status`、`stable goal export` 或对话 `/status` 调用 `Refresh(goalID)`；运行中的 `EvaluateGoal` 也在观察前调用它。
2. 采集器返回两类快照；存储事务逐类比较。若无变化，不写入；若有变化，只标记对应类别旧证据并产生一个新代次事件，目标及代理状态改为待复核。若某输入不可用，原因写入目标和快照。
3. 事务提交后协调器用已有 `WakeGoal` 投递事件。失败不撤销持久状态；worker 重启或事件补送会继续投递。调用方重新读取快照并返回状态或导出。
4. 工作流读取全部当前证据，只检查缺失类别。每个检查产出的 v2 证据绑定检查时快照；检查结束后再次 `Refresh`，再用令牌提交。存储层核对三个版本维度，合并保留的证据，对所有当前验收项得出结论。
5. 检查期间的新变化使代次增加；旧令牌检查结果仅入历史，不会覆盖待复核。新事件驱动下一轮。动作预留也拒绝旧代次决策。

## 文件组织

```text
docs/spec_docs/V02/
├── spec.md
├── plan.md
└── contracts/
    ├── storage-v4.md       # 迁移、代次、事务与事件契约
    └── evidence-v2.md      # 依赖快照及新旧证据解释
workers/kicad/
├── dependencies.py         # 两类检查的依赖采集与摘要
├── bridge.py               # describe_dependencies 只读入口
├── erc.py                  # 与采集器共用 KiCad 环境
└── test_dependencies.py    # 内容变化、缺失及版本测试
internal/dependency/
├── service.go              # Refresh 与持久事件投递
└── service_test.go
internal/core/
├── types.go                # 依赖类型、代次和接口
├── current_evidence.go     # v2 当前证据筛选
├── activities.go           # 定向复核和检查后刷新
└── activities_test.go
internal/store/
├── schema.sql              # 新库 v4 结构
├── sqlite.go               # v4 迁移和原子协调
└── sqlite_test.go
internal/report/
├── report.go               # 出处、缺口与历史投影
└── report_test.go
cmd/agentctl/main.go        # status/export 接入 Refresh
cmd/agentworker/main.go     # 运行周期注入 Refresher
cmd/stable/oneshot.go       # 向 agentctl 传递检查环境位置
cmd/stable/chatserve.go     # 对话服务注入 Refresher
cmd/stable/main.go          # 内嵌对话服务注入 Refresher
internal/conversation/
├── service.go              # 刷新依赖的装配
└── session.go              # /status 在返回前刷新
tests/e2e/dependency_change.sh
Makefile                    # 纳入端到端测试入口
```

## 技术决策

| 决策点 | 选择 | 理由 |
| --- | --- | --- |
| 失效粒度 | 检查类别快照 | 满足 ERC 与连线证据分离；先按设计原则的检查器、产物、条件族建边，避免过早实现符号级图谱 |
| 输入识别 | ERC 相关项目设置、完整库表、选用的库文件、相关路径变量的内容摘要 | 同修改时间也能发现变化；库表无关项变化可保守复核，不扫描全部系统库 |
| 版本守卫 | 全目标单调依赖代次，加每类独立快照摘要 | 前者保护并发检查与动作，后者保留无关证据 |
| 数据契约 | SQLite v4、证据出处 v2 | 不改写 V01 历史，旧证据缺少依赖信息时明确为未知 |
| 通知 | 事务内写幂等事件，提交后投递 | 查询返回前已有持久待复核状态；投递失败可补送 |
| 不可用输入 | 保存不可用快照、保持待复核 | 无法核对依赖时不借用旧通过证据 |
| KiCad 全局配置 | 以目标专属检查环境中实际使用的全局表和路径变量为准 | 复核与采集看到相同输入，不扫描任意宿主配置 |
| 复核汇总 | 只运行缺失类别，提交事务核对所有验收项 | 实现定向复核，避免部分结果误判为全目标达标 |

## Spec 覆盖核对

| 需求 | 设计归属 |
| --- | --- |
| F1 | KiCad 依赖采集、`DependencySnapshot` |
| F2 | 依赖协调、存储原子事务、状态/导出/运行入口 |
| F3 | 按类别保存快照、定向失效、`EvidenceCurrent` |
| F4 | 核心定向复核、扩展令牌、提交时汇总所有验收项 |
| F5 | 不可用快照、待复核原因、当前证据过滤 |
| F6 | v2 证据出处、状态与导出投影、历史报告 |
