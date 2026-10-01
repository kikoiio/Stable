# V02 工程依赖变化后的证据失效 Tasks

> 依据已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。每项是一个聚焦工作单元，按依赖顺序执行；每项先运行写明的验证，再标记完成。实现按数个相邻任务分组提交，验收仍以 checklist 为准。

## 文件清单

| 操作 | 文件 | 职责 |
| --- | --- | --- |
| 新建 | `docs/spec_docs/V02/contracts/storage-v4.md`、`evidence-v2.md` | 数据契约与迁移语义 |
| 修改 | `internal/core/types.go`、`current_evidence.go`、`activities.go` 及对应测试 | 依赖类型、当前证据、定向复核 |
| 修改 | `internal/store/schema.sql`、`sqlite.go`、`sqlite_test.go` | v4 迁移、原子失效、令牌守卫 |
| 新建 | `workers/kicad/dependencies.py`、`test_dependencies.py` | 工程输入采集与测试 |
| 修改 | `workers/kicad/erc.py`、`bridge.py`、`test_erc.py` | 共用 KiCad 环境与只读入口 |
| 新建 | `internal/dependency/service.go`、`service_test.go` | Go 侧采集适配、刷新和唤醒 |
| 修改 | `internal/report/report.go`、`report_test.go` | 当前与历史证据投影 |
| 修改 | `cmd/agentctl/main.go`、`cmd/agentworker/main.go`、`cmd/agentworker/main_test.go` | 查询、导出、运行周期接入 |
| 新建 | `cmd/agentctl/main_test.go` | 查询返回前持久化与唤醒失败测试 |
| 修改 | `cmd/stable/oneshot.go`、`chatserve.go`、`main.go`、`chat_test.go` | 检查环境位置与对话服务装配及回归 |
| 修改 | `internal/conversation/service.go`、`session.go`、`service_test.go` | 对话 `/status` 刷新 |
| 新建 | `tests/e2e/dependency_change.sh` | 本地端到端演示 |
| 修改 | `Makefile` | 纳入端到端入口 |

## T01：写存储 v4 契约

**文件：** `docs/spec_docs/V02/contracts/storage-v4.md`。**依赖：** 无。
**步骤：**
1. 写明 `goals.dependency_revision`、`decisions.dependency_revision` 和 `goal_dependencies` 的类型、主键及旧值语义。
2. 定义迁移事务、旧版达标目标的待复核事件、定向失效与幂等事件 ID。
**验证：** 运行 `rg -n 'v4|dependency_revision|goal_dependencies|迁移|事件' docs/spec_docs/V02/contracts/storage-v4.md`，每项定义均可定位。

## T02：写证据 v2 契约

**文件：** `docs/spec_docs/V02/contracts/evidence-v2.md`。**依赖：** T01。
**步骤：**
1. 列出两类检查、来源状态、快照指纹、出处 v2 必填字段及 `unknown` 读法。
2. 写明当前证据条件、不可用快照、旧 v1 历史及路径变量只保留摘要的规则。
**验证：** 运行 `rg -n 'schema_version|fingerprint|unknown|absent_optional|missing_required|source_level' docs/spec_docs/V02/contracts/evidence-v2.md`，逐项对照 plan.md。

## T03：补核心依赖类型

**文件：** `internal/core/types.go`、`internal/core/activities.go`、`internal/core/types_test.go`。**依赖：** T02。
**步骤：**
1. 定义 `CheckFamily`、`DependencySource`、`DependencySnapshot`、`DependencyRefresh` 与采集、刷新接口，并给 `Activities` 增加暂不调用的刷新器字段。
2. 为目标、快照、决策、证据出处和复核令牌加 plan.md 所定字段；添加 JSON 往返测试，不改变现有筛选调用。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/core -run TestDependencyTypes`，类型往返与旧字段读取均通过。

## T04：增加 v2 当前证据规则

**文件：** `internal/core/current_evidence.go`、`internal/core/types_test.go`。**依赖：** T03。
**步骤：**
1. 增加接受目标快照的 v2 当前证据判定，比较类别、指纹、可用性、标准、原理图及来源等级；暂保留旧调用兼容入口供后续接线。
2. 测试同类匹配、别类变化、旧 v1、不可用输入、截图和模型陈述。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/core -run TestEvidenceCurrentDependencies`，各场景得到预期布尔值。

## T05：加入 v4 新库结构

**文件：** `internal/store/schema.sql`、`internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T03。
**步骤：**
1. 新库结构加入依赖代次、决策代次和 `goal_dependencies` 表。
2. `Open` 将新库版本设为 4，已有库经独立迁移函数升级；保留 v1–v3 入口，并增加 `TestOpenV4` 核对新库版本。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store -run TestOpenV4`，新库打开且 `PRAGMA user_version` 为 4。

## T06：实现旧库 v4 迁移

**文件：** `internal/store/sqlite.go`、`internal/store/sqlite_test.go`。**依赖：** T05。
**步骤：**
1. 单事务补缺列和表；对旧版已达标目标写待复核、原因和 `migrate-v4-<goal>` 唤醒事件。
2. 增加 v2/v3 库升级、重复打开及旧证据不回填的测试。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store -run TestMigrateV4`，版本、旧行与事件数量正确。

## T07：读写目标依赖快照

**文件：** `internal/store/sqlite.go`、`internal/store/sqlite_test.go`。**依赖：** T05。
**步骤：**
1. 扩展目标扫描及 `GetGoalSnapshot`，读取依赖代次和按类别排序的快照。
2. 测试空快照与两个类别的 JSON 往返；旧证据仍按 v1 原样读出。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store -run TestGoalDependencySnapshot`，快照内容与排序一致。

## T08：建立初次采集基线

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`、`internal/core/types.go`。**依赖：** T07。
**步骤：**
1. 实现 `ReconcileDependencies` 初次快照写入，并加入 `StateStore` 接口；新建目标初次采集不写变化事件。
2. 测试目标代次、两个类别快照及事件数；使用旧库已待复核目标时保持其迁移事件。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store -run TestDependencyBaseline`，初始状态不被误判为变化。

## T09：原子定向失效

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T08。
**步骤：**
1. 快照变化时一个事务更新类别行、目标代次和待复核状态、代理状态、该类别证据失效原因及事件。
2. 测试 ERC 变化只失效 ERC 证据，连线证据 ID 和原结果保留。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store -run TestReconcileDependencyChange`，事务后目标、证据和事件一致。

## T10：不可用与重复变化

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T09。
**步骤：**
1. 对不可用快照保存原因并维持待复核；同指纹重复协调为无操作。
2. 测试必需输入缺失、恢复、重复查询以及内容恢复旧摘要后仍需新证据。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store -run TestReconcileDependencyIdempotence`，事件与代次数不重复。

## T11：扩展复核提交的代次核对

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T09。
**步骤：**
1. `CommitVerification` 在原有标准和原理图令牌外核对依赖代次；过期证据仅作为历史保存。
2. 加测试：检查期间协调新快照后提交旧令牌，目标保持待复核。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store -run TestCommitVerificationDependencyRace`，旧检查无法确认达标。

## T12：按全部当前证据汇总结论

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T04、T11。
**步骤：**
1. 提交事务在保存本轮证据后，按所有现行验收项查找当前 v2 证据；不单独信任本轮 `Passed`。
2. 测试仅重检 ERC 时复用连线证据，缺任一类别时不得达标。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store -run TestCommitVerificationReusesEvidence`，两种证据集合的状态正确。

## T13：守卫状态更新和旧动作

**文件：** `internal/store/sqlite.go`、`sqlite_test.go`。**依赖：** T11。
**步骤：**
1. 决策记录读写可空依赖代次；`UpdateStatusForToken` 与 `ReserveAction` 拒绝未知或过期代次。
2. 加测试，确认旧决策不能预留动作、旧状态更新不能覆盖待复核。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store -run TestDependencyRevisionGuards`，受拒绝操作不修改目标或产物。

## T14：核对存储契约与并发测试

**文件：** `internal/store/sqlite_test.go`、`docs/spec_docs/V02/contracts/storage-v4.md`。**依赖：** T06、T10、T12、T13。
**步骤：**
1. 测试两次变化、事务失败回滚、重复事件及重启后的快照读取。
2. 对照实际 SQL 更新契约中的字段、事件和未知值语义。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/store`，全部通过，且契约与数据库字段一致。

## T15：统一 KiCad 检查环境

**文件：** `workers/kicad/erc.py`、`workers/kicad/dependencies.py`、`workers/kicad/test_erc.py`。**依赖：** T02。
**步骤：**
1. 把当前 ERC 的目标专属配置环境构造供依赖采集复用；保持 ERC 命令参数不变。
2. 用现有模拟 KiCad 测试确认 ERC 的配置路径、版本和报告行为未变。
**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_erc.py'`，原有 ERC 测试通过。

## T16：采集项目 ERC 设置

**文件：** `workers/kicad/dependencies.py`、`test_dependencies.py`。**依赖：** T15。
**步骤：**
1. 从与原理图同名的项目配置中解析 ERC 相关设置并对内容取摘要，忽略仅修改时间的变化。
2. 测试 ERC 设置变化、无关文件时间变化、配置损坏与必需项目文件缺失。
**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_dependencies.py' -k TestProjectERCSettings`，摘要和不可用原因正确。

## T17：采集项目与全局符号库表

**文件：** `workers/kicad/dependencies.py`、`test_dependencies.py`。**依赖：** T16。
**步骤：**
1. 读取项目表和检查环境使用的全局表；项目表可选，缺失时记 `absent_optional`。
2. 解析表映射及路径变量引用；表内容变化即改变 ERC 类快照，损坏时标 `malformed`。
**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_dependencies.py' -k TestSymbolTables`，项目表缺失、修改及损坏均按约定分类。

## T18：采集路径变量

**文件：** `workers/kicad/dependencies.py`、`test_dependencies.py`。**依赖：** T17。
**步骤：**
1. 对表解析实际需要的 KiCad 路径变量记录变量名和值摘要，并使用与 ERC 相同的环境解析路径。
2. 测试变量值改变、缺失和无关环境变量改变；输出不得包含变量原值。
**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_dependencies.py' -k TestPathVariables`，只有相关变量改变指纹。

## T19：采集选用的符号库文件

**文件：** `workers/kicad/dependencies.py`、`test_dependencies.py`。**依赖：** T17、T18。
**步骤：**
1. 按当前原理图所用的库昵称定位选用的库文件，以整个库文件内容为粒度取摘要。
2. 测试项目库内容变化、必需库缺失及未选用的系统库变化；不解析单个符号。
**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_dependencies.py' -k TestSelectedLibraries`，所选库变化触发 ERC 指纹变化。

## T20：加入检查器版本和稳定指纹

**文件：** `workers/kicad/dependencies.py`、`test_dependencies.py`。**依赖：** T16、T17、T18、T19。
**步骤：**
1. ERC 快照加入真实 `kicad-cli version`；连线快照加入现有固定检查器版本，按排序后的来源状态与摘要生成指纹。
2. 测试版本变化、版本不可得、同内容不同修改时间及同时间不同内容。
**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_dependencies.py' -k TestDependencyFingerprint`，两类指纹和可用性正确。

## T21：暴露只读采集入口

**文件：** `workers/kicad/bridge.py`、`test_dependencies.py`。**依赖：** T20。
**步骤：**
1. 新增 `kicad.describe_dependencies`，沿用协议版本、目标路径授权与目标根目录检查。
2. 返回两类完整快照；故障作为不可用快照返回，非授权路径仍被阻止。
**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_dependencies.py' -k TestDependencyBridge`，JSON 协议与拒绝路径正确。

## T22：完成 KiCad 采集回归

**文件：** `workers/kicad/test_dependencies.py`、`test_erc.py`。**依赖：** T21。
**步骤：**
1. 覆盖项目配置、项目库、全局表与路径变量的组合变化，以及检查期间输入变化。
2. 运行全部 KiCad 单元测试并修复采集入口与 ERC 环境不一致。
**验证：** 运行 `python3 -m unittest discover -s workers/kicad -p 'test_*.py'`，全部通过。

## T23：实现 Go 侧采集适配

**文件：** `internal/dependency/service.go`、`service_test.go`。**依赖：** T03、T21。
**步骤：**
1. 用现有 Python bridge 调用 `kicad.describe_dependencies`，严格解码两类快照并校验类别、指纹、版本字段。
2. 让 bridge 脚本位置可从调用方的检查环境装配；测试缺一类、重复类别与非法 JSON。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/dependency -run TestCollectorAdapter`，只接受完整快照。

## T24：实现刷新与持久事件投递

**文件：** `internal/dependency/service.go`、`service_test.go`。**依赖：** T09、T23。
**步骤：**
1. `Refresh` 读取目标、采集快照、调用 `ReconcileDependencies`、尝试 `WakeGoal` 回调，再读取最新快照。
2. 测试唤醒失败时待复核已持久化、事件可补送，同快照不会再唤醒。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/dependency -run TestRefreshWake`，持久状态与返回快照一致。

## T25：覆盖刷新竞态

**文件：** `internal/dependency/service_test.go`。**依赖：** T24。
**步骤：**
1. 并发执行两次相同刷新与一次新快照刷新，检查代次与事件数。
2. 测试不可用快照仍返回待复核，采集器系统错误不伪造通过。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/dependency -run TestRefreshConcurrency`，无冲突结论或重复事件。

## T26：把刷新器接入 worker

**文件：** `cmd/agentworker/main.go`、`cmd/agentworker/main_test.go`。**依赖：** T24。
**步骤：**
1. 在 worker 创建与 KiCad bridge 同源的依赖采集器和刷新器，向 `Activities` 注入。
2. 保留现有未处理事件补送；测试 worker 启动装配与旧事件仍能补送。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./cmd/agentworker`，启动与补送测试通过。

## T27：运行前刷新依赖

**文件：** `internal/core/activities.go`、`activities_test.go`。**依赖：** T04、T24、T26。
**步骤：**
1. `EvaluateGoal` 在观察与模型决策前调用刷新器；不可用类别保持待复核并显示原因。
2. 测试已达标目标被发现变化后不会继续使用旧有效证据进入决策。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/core -run TestEvaluateRefreshesDependencies`，先刷新后观察。

## T28：选择缺失检查类别

**文件：** `internal/core/activities.go`、`activities_test.go`、`current_evidence.go`。**依赖：** T12、T27。
**步骤：**
1. `VerifyGoal` 根据快照对每个现行验收项查找 v2 当前证据；只检查缺失的 ERC 或连线类别。
2. 将核心调用方切到快照版当前证据判定，暂保留报告模块要用的旧兼容入口；测试只失效 ERC 时不执行连线检查。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/core -run TestVerifyOnlyMissingFamily`，检查调用次数与保留证据 ID 正确。

## T29：ERC 证据绑定快照

**文件：** `internal/core/activities.go`、`activities_test.go`。**依赖：** T20、T28。
**步骤：**
1. ERC 独立检查产生 v2 出处，冻结本轮 `kicad.erc` 快照，并保留原主张、覆盖范围和报告。
2. 测试两个 ERC 验收项共用报告且各证据指向同一检查时快照。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/core -run TestERCEvidenceDependencySnapshot`，证据字段完整。

## T30：连线证据绑定快照

**文件：** `internal/core/activities.go`、`activities_test.go`。**依赖：** T20、T28。
**步骤：**
1. 连线检查产生 v2 出处，冻结 `sensor.connection` 快照和检查器版本。
2. 测试仅连线检查器版本改变时 ERC 证据仍为当前，连线检查重新执行。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/core -run TestConnectionEvidenceDependencySnapshot`，两类证据独立。

## T31：检查后刷新和过期处理

**文件：** `internal/core/activities.go`、`activities_test.go`。**依赖：** T29、T30。
**步骤：**
1. 检查结束、提交前再次刷新依赖；若代次变化，仅保存历史结果并让新事件推动下一轮。
2. 决策记录注入依赖代次；测试检查期间变更与旧动作被拒绝。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/core -run TestDependencyChangesDuringVerification`，旧通过不使目标达标。

## T32：复核核心回归

**文件：** `internal/core/activities_test.go`。**依赖：** T27、T28、T29、T30、T31。
**步骤：**
1. 运行标准变更、原理图变更和依赖变更组合测试，确认 V01 完整复核仍可用。
2. 修复测试替身以返回两类快照，确认无新模型调用即可完成可通过的定向复核。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/core`，全部通过。

## T33：状态展示当前与历史依赖

**文件：** `internal/report/report.go`、`report_test.go`。**依赖：** T04、T07、T28。
**步骤：**
1. 正规化证据展示原理图摘要、标准版本、类别快照、检查器版本、覆盖范围、当前标记和失效原因；v1 缺失项显示 `unknown`，报告调用方切到快照版当前证据判定后移除旧兼容入口。
2. 状态只接受 v2 当前证据；测试项目设置变化后仅 ERC 条目未验证。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/report -run TestDependencyStatusProjection`，字段与缺口正确。

## T34：导出当前报告与历史

**文件：** `internal/report/report.go`、`report_test.go`。**依赖：** T33。
**步骤：**
1. 只有当前 ERC 证据的报告进入交付位置；失效报告及快照出处进入历史清单。
2. 测试待复核时旧 ERC 报告不出现在当前交付，连线历史不丢失。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/report`，全部通过。

## T35：接入 agentctl 查询与导出

**文件：** `cmd/agentctl/main.go`、`main_test.go`。**依赖：** T24、T34。
**步骤：**
1. `status` 与 `export` 都在读取快照前调用共用 `Refresh`，使用命令传入的运行/检查环境定位 bridge。
2. 测试已结束目标第一次查询时返回持久待复核；Temporal 不可达时仍返回待复核并保留事件。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./cmd/agentctl ./internal/dependency`，并用测试 CLI 读取变更后状态为待复核。

## T36：接入 stable 命令装配

**文件：** `cmd/stable/oneshot.go`、`chatserve.go`、`main.go`。**依赖：** T35。
**步骤：**
1. 让 `stable goal status/export` 向 agentctl 传入与运行时一致的检查环境位置。
2. 内嵌与独立对话服务都注入同一刷新器，不建立第二套依赖规则。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./cmd/stable`，命令及服务装配测试通过。

## T37：对话状态查询先刷新

**文件：** `internal/conversation/service.go`、`session.go`、`service_test.go`。**依赖：** T24、T36。
**步骤：**
1. 对话 `/status` 列目标前逐个刷新；任一目标采集失败时返回可解释错误，不能输出旧达标。
2. 保留状态广播行为，不在无用户操作时密集扫描库文件。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/conversation -run TestStatusRefreshesDependencies`，返回的目标状态为最新持久值。

## T38：对话与命令入口回归

**文件：** `internal/conversation/service_test.go`、`cmd/stable/chat_test.go`。**依赖：** T37。
**步骤：**
1. 测试多目标 `/status` 仅改变受影响目标；确认标准修改路径仍产生原有 V01 事件。
2. 运行对话和 CLI 全量测试，修复装配参数回退。
**验证：** 运行 `GOPROXY=off GOSUMDB=off go test ./internal/conversation ./cmd/stable`，全部通过。

## T39：建立端到端测试环境

**文件：** `tests/e2e/dependency_change.sh`。**依赖：** T35、T36。
**步骤：**
1. 沿用 V01 端到端脚本的本地 Temporal、模拟模型、开发版安装布局及清理方式。
2. 建立同时有 ERC 与连线标准的已达标目标，记录原理图摘要和两类证据 ID。
**验证：** 运行 `bash tests/e2e/dependency_change.sh --baseline-only`，输出基线目标和证据 ID，退出码为 0。

## T40：端到端覆盖项目设置和项目库

**文件：** `tests/e2e/dependency_change.sh`。**依赖：** T22、T32、T39。
**步骤：**
1. 在原理图与标准不变时修改项目 ERC 设置，立即查询并导出，断言 ERC 历史、连线当前、待复核和自动复核。
2. 分别修改项目库表与一个所选库文件，断言同样的定向失效；恢复后仍需新 ERC 证据。
**验证：** 运行 `bash tests/e2e/dependency_change.sh --project-cases`，每个子场景输出 PASS。

## T41：端到端覆盖全局配置与版本

**文件：** `tests/e2e/dependency_change.sh`。**依赖：** T40。
**步骤：**
1. 修改目标专属检查环境中的全局符号库表和所用路径变量，断言 ERC 失效而连线证据保留。
2. 受控替换检查器版本输出，分别断言 ERC 与连线类别的定向失效。
**验证：** 运行 `bash tests/e2e/dependency_change.sh --global-and-version`，四类变化均输出 PASS。

## T42：端到端覆盖竞态与不可用

**文件：** `tests/e2e/dependency_change.sh`。**依赖：** T40、T41。
**步骤：**
1. 暂停一次检查，期间再改依赖，放行旧通过后断言不能达标并会再次复核。
2. 删除或损坏必需输入，检查状态/导出的原因；重启后重复查询不增事件，恢复输入后重新取得证据。
**验证：** 运行 `bash tests/e2e/dependency_change.sh --race-and-unavailable`，竞态和恢复场景均输出 PASS。

## T43：完整回归与交付核对

**文件：** `Makefile`、`docs/spec_docs/V02/contracts/storage-v4.md`、`evidence-v2.md`。**依赖：** T14、T22、T25、T32、T34、T38、T42。
**步骤：**
1. 将 V02 端到端脚本纳入 `make e2e`，复核两份契约与实际字段；只把本轮文档和代码纳入提交。
2. 运行 Go、KiCad Python、V01 与 V02 端到端回归，并保存命令输出供 checklist 验收。
**验证：** `GOPROXY=off GOSUMDB=off go test ./...`、`python3 -m unittest discover -s workers/kicad -p 'test_*.py'`、`bash tests/e2e/criteria_change.sh`、`bash tests/e2e/dependency_change.sh` 均退出 0；`git diff --check` 无空白错误。

## 执行顺序

```text
契约与类型：T01 → T02 → T03 → T04
存储：      T03 → T05 → T06/T07 → T08 → T09 → T10/T11 → T12/T13 → T14
KiCad：     T02 → T15 → T16 → T17 → T18/T19 → T20 → T21 → T22
协调：      T03 + T09 + T21 → T23 → T24 → T25 → T26
核心：      T04 + T12 + T24 + T26 → T27 → T28 → T29/T30 → T31 → T32
展示入口：  T04 + T07 + T28 → T33 → T34 → T35 → T36 → T37 → T38
端到端：    T22 + T32 + T35 + T36 → T39 → T40 → T41 → T42 → T43
```

T06/T07、T10/T11、T18/T19、T29/T30 是在各自依赖满足后可独立推进的分支；同一文件若同时触及，先完成一项并验证，再开始另一项。
