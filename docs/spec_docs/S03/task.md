# S03 安全文件访问与候选验收事务 Tasks

> 输入：[spec.md](spec.md)、[plan.md](plan.md)。每项任务完成后先执行本项验证，再进入依赖它的任务。

## 文件清单

| 操作 | 文件 | 职责 |
|---|---|---|
| 修改 | `internal/platform/secfile/secfile.go` | 扩展安全根目录、身份和事务原语契约 |
| 新建/修改 | `internal/platform/secfile/root.go`、`secfile_linux.go`、`secfile_darwin.go`、`secfile_windows.go`、`secfile_other.go` | 三平台根目录访问、链接/reparse point 检查、同卷与目录移动/交换 |
| 新建/修改 | `internal/platform/secfile/*_test.go` | 路径、身份、能力和平台契约测试 |
| 新建 | `internal/candidate/transaction.go` | acceptance/rewind 事务协调器、phase 和恢复状态 |
| 新建 | `internal/candidate/transaction_test.go` | 事务状态机、故障边界和幂等测试 |
| 修改 | `internal/candidate/workspace.go`、`review.go`、`snapshot.go`、`erc_checker.go` | 所有工程内容访问统一走安全根目录 |
| 修改 | `internal/candidate/accept.go`、`rewind.go` | 接入事务协调器，保留现有业务语义 |
| 修改 | `internal/candidate/*_test.go` | 根目录、链接、并发、跨卷和回归测试 |
| 修改 | `internal/store/schema.sql`、`sqlite.go` | journal phase/路径元数据与 v12 migration |
| 修改 | `internal/store/candidate.go`、`rewind.go` | acceptance/rewind journal、恢复与 finalize |
| 修改 | `internal/store/*_test.go` | migration、状态迁移、幂等恢复和阻断测试 |
| 修改 | `internal/conversation/snapshots.go` | rewind coordinator 集成 |
| 修改 | `tests/e2e/m03_accept_restart_test.go`、`m05_sessions_test.go` | 既有流程接入与回归 |
| 新建 | `tests/e2e/s03_secure_transaction_test.go` | S03 端到端安全边界和故障恢复场景 |
| 修改 | `docs/spec_docs/S00/platform-capability-matrix.md` | 更新 C06/C07 状态、限制和证据位置 |
| 新建 | `docs/spec_docs/S03/checklist.md` | 验收清单 |

## T1：定义 secfile 公共契约

**文件：** `internal/platform/secfile/secfile.go`、`root.go`

**依赖：** 无

**步骤：**

1. 定义 `Root`、根身份、条目身份、同卷错误和能力错误的公共语义。
2. 定义安全打开、根重新验证、同卷检查、原子交换和单次目录移动的最小接口。
3. 保持现有 `ErrUnsafePath`、`ErrDifferentDevice` 和私密文件接口兼容。
4. 为 transaction coordinator 规定 `atomic-exchange`、`journaled-move` 和 fail-closed 能力结果。

**验证：** `go test ./internal/platform/secfile` 编译通过；现有 secfile 测试不需修改即可运行。

## T2：保留并整理 Linux secfile 实现

**文件：** `internal/platform/secfile/secfile_linux.go`、Linux 测试

**依赖：** T1

**步骤：**

1. 将现有 `openat2`、`RESOLVE_BENEATH`、`RESOLVE_NO_SYMLINKS`、`O_NOFOLLOW` 和普通文件检查接入新契约。
2. 将 `RENAME_EXCHANGE`、同设备检查和根身份读取接入新接口。
3. 确保错误仍映射为安全路径、跨设备或底层 I/O 错误。
4. 增加同卷成功、跨卷拒绝、根目录符号链接拒绝的 Linux 定向测试。

**验证：** `go test ./internal/platform/secfile -run 'Test(Linux|Root|Exchange|SameVolume)' -count=1` 通过。

## T3：实现 macOS secfile 适配器

**文件：** `internal/platform/secfile/secfile_darwin.go`、Darwin 契约测试

**依赖：** T1

**步骤：**

1. 逐级打开根目录和相对路径组件，禁止跟随符号链接，并在最终句柄上检查普通文件和身份。
2. 实现根身份、同卷判定和不支持原子交换时的明确能力结果。
3. 实现同卷目录移动所需的底层原语，跨卷直接返回 `ErrDifferentDevice`。
4. 将纯路径、phase 能力和错误映射逻辑拆成 Linux 可运行的契约测试。

**验证：** `GOOS=darwin GOARCH=amd64 go build ./internal/platform/secfile` 通过；`go test ./internal/platform/secfile -run 'Test(Darwin|Path|Capability)'` 通过。

## T4：实现 Windows secfile 适配器

**文件：** `internal/platform/secfile/secfile_windows.go`、Windows 契约测试

**依赖：** T1

**步骤：**

1. 使用系统文件句柄检查 reparse point、普通文件类型、卷身份和文件身份。
2. 实现 Windows 根目录打开、相对路径访问和同卷目录移动能力。
3. 原子交换不可用时返回 journaled-move 能力，不得伪装成原子交换。
4. 将路径分类、能力选择、reparse point 错误映射拆成 Linux 可运行的契约测试。

**验证：** `GOOS=windows GOARCH=amd64 go build ./internal/platform/secfile` 通过；`go test ./internal/platform/secfile -run 'Test(Windows|Path|Capability)'` 通过。

## T5：统一 candidate 工作区安全访问

**文件：** `internal/candidate/workspace.go`、`workspace_test.go`

**依赖：** T1、T2、T3、T4

**步骤：**

1. 让 `BuildManifest` 通过 `secfile.Root` 遍历和打开条目，并在遍历前后重新验证根身份。
2. 让 `CreateCandidate`、复制文件和 `FreezeCandidate` 复用安全根、同卷和私密目录接口。
3. 保留 `ManifestEntry` 字段、摘要编码、`.stable` 排除规则和候选生命周期。
4. 对绝对路径、`..`、符号链接、特殊文件、根目录替换和条目替换返回明确拒绝。

**验证：** `go test ./internal/candidate -run 'Test(BuildManifest|CreateCandidate|FreezeCandidate|Unsafe|Symlink)' -count=1` 通过；manifest 摘要回归值不变。

## T6：接入 review、snapshot 和 ERC 安全读取

**文件：** `internal/candidate/review.go`、`snapshot.go`、`erc_checker.go` 及对应测试

**依赖：** T5

**步骤：**

1. 用统一安全读取替换 diff 的正式/候选条目读取。
2. 让 snapshot blob 写入、manifest materialize 和恢复读取校验根身份与普通文件类型。
3. 让 ERC 报告读取使用受控根目录和现有大小上限，不允许报告路径越界。
4. 保留敏感信息过滤、摘要和 checker finding 语义。

**验证：** `go test ./internal/candidate -run 'Test(Diff|Snapshot|ERC|Report|Credential)' -count=1` 通过；链接和根替换 fixture 均被拒绝。

## T7：实现 candidate 事务协调器

**文件：** 新建 `internal/candidate/transaction.go`、`transaction_test.go`

**依赖：** T2、T3、T4、T5

**步骤：**

1. 定义 `DirectoryTransaction`、phase、`RecoveryState` 和 `TransactionCoordinator`。
2. 实现 Linux 原子交换路径，并在交换前后校验根身份和 expected/target digest。
3. 实现 macOS/Windows journaled-move：保存旧根、安装新根、恢复旧根到候选/staging，每步通过 journal 回调推进 phase。
4. 将跨卷、路径替换、能力不可用和恢复状态异常映射为 fail-closed 错误。
5. 将 `.stable` 服务目录恢复纳入 acceptance transaction 的完成/恢复逻辑。

**验证：** `go test ./internal/candidate -run 'Test(Transaction|Exchange|Recovery|CrossVolume)' -count=1` 通过；Linux 原子交换回归结果不变。

## T8：扩展 journal schema 与 v12 migration

**文件：** `internal/store/schema.sql`、`internal/store/sqlite.go`、migration 测试

**依赖：** T7

**步骤：**

1. 为 acceptance/rewind journal 增加事务模式、rollback/backup 路径和中间 phase。
2. 将 phase CHECK 约束扩展为 `prepared`、`old_saved`、`target_installed`、`swapped`、`finalized`、`blocked`。
3. 编写 v12 migration，重建受 CHECK 约束影响的表并原样迁移旧记录。
4. 将旧记录默认标记为 `atomic-exchange`，不改变既有 journal 内容和 receipt。
5. 更新 schema 新建库路径、索引和 user_version。

**验证：** `go test ./internal/store -run 'Test(.*Migration|OpenV|Schema)' -count=1` 通过；从 v11 数据库重开后 user_version 为 12，旧 journal 可查询。

## T9：接入 acceptance 事务

**文件：** `internal/candidate/accept.go`、`internal/store/candidate.go` 相关 acceptance 调用测试

**依赖：** T7、T8

**步骤：**

1. 保留现有 review/decision/finding 校验，将文件系统交换改为 coordinator `Apply`。
2. 在 prepared journal 中保存事务模式和 rollback 路径，再执行文件系统变更。
3. 保留 `.stable` 迁移、target digest 校验、receipt 幂等、候选状态和目标重新验证语义。
4. 将中间 phase、阻断原因和恢复所需路径写入 store。

**验证：** `go test ./internal/candidate ./internal/store -run 'Test(Accept|Acceptance)' -count=1` 通过；重复 acceptance 只产生一个 receipt。

## T10：接入 rewind 事务

**文件：** `internal/candidate/rewind.go`、`internal/conversation/snapshots.go`、对应测试

**依赖：** T7、T8

**步骤：**

1. 保留 session ownership、候选状态、活动 run、未完成事务和 digest guard。
2. 让 staging 创建、快照 materialize 和候选交换使用 coordinator。
3. 保存 rollback 路径和中间 phase，成功后验证 target digest，再 finalize、清理旧 staging 和失效旧 review。
4. 保留 pending/failed/completed session 事件顺序。

**验证：** `go test ./internal/candidate ./internal/conversation -run 'Test(Rewind|Snapshot)' -count=1` 通过；正式工程在 rewind 前后保持不变。

## T11：实现 acceptance 启动恢复

**文件：** `internal/store/candidate.go`、acceptance 恢复测试

**依赖：** T8、T9

**步骤：**

1. 扩展 pending 查询，包含所有未完成 phase 和事务元数据。
2. 按根身份、路径存在性和 old/new digest 调用 coordinator `Inspect/Recover`。
3. 覆盖旧版本、新版本、`old_saved`、`target_installed`、服务目录未恢复和不一致阻断场景。
4. 保留 receipt、候选状态、目标状态和事件的 SQLite 幂等 finalize。

**验证：** `go test ./internal/store -run 'TestReconcileAcceptance|Test.*Acceptance.*Recovery' -count=1` 通过；连续两次 reconcile 不重复移动或写 receipt。

## T12：实现 rewind 启动恢复

**文件：** `internal/store/rewind.go`、rewind 恢复测试

**依赖：** T8、T10

**步骤：**

1. 扩展 unfinished rewind 查询和 phase 转换。
2. 对 prepared、old_saved、target_installed 和 swapped 分别判断旧版本、新版本、中间状态和阻断。
3. 可继续时完成剩余移动；旧版本时清理 staging/rollback；不一致时 blocked 且不覆盖工程。
4. 保留候选状态、target digest、review 失效和旧测试的 finalize 语义。

**验证：** `go test ./internal/store -run 'TestReconcileRewind|Test.*Rewind.*Recovery' -count=1` 通过；重复恢复保持同一 digest 和 phase。

## T13：补齐安全边界与并发测试

**文件：** `internal/platform/secfile/*_test.go`、`internal/candidate/*_test.go`

**依赖：** T5、T6、T7

**步骤：**

1. 增加绝对路径、路径穿越、根外链接、reparse point 逻辑、目录替代文件和特殊文件 fixture。
2. 增加 manifest/review/ERC 读取过程中的文件替换、删除和根替换测试。
3. 增加跨卷拒绝和能力 fail-closed 测试，不依赖真实 Windows/macOS 主机。
4. 验证现有大小、数量、敏感信息过滤和生命周期限制仍生效。

**验证：** `go test ./internal/platform/secfile ./internal/candidate -count=1` 通过；所有越界和并发场景均返回拒绝且数据未变。

## T14：补齐事务故障注入测试

**文件：** `internal/candidate/transaction_test.go`、`internal/store/candidate_test.go`、`rewind_test.go`

**依赖：** T9、T10、T11、T12

**步骤：**

1. 在准备、保存旧根、安装新根、恢复旧根、服务目录迁移和 finalize 前后注入终止/错误。
2. 对每个 phase 验证恢复结果只能是完整旧版本、完整新版本或 blocked。
3. 验证错误恢复、数据库 finalize 和清理路径可重复执行。
4. 验证正式工程不会出现半写入状态，且 rollback/staging 不越出授权父目录。

**验证：** `go test ./internal/candidate ./internal/store -run 'Test.*(Fault|Crash|Recovery|Idempot)' -count=1` 通过。

## T15：扩展端到端场景

**文件：** `tests/e2e/m03_accept_restart_test.go`、`m05_sessions_test.go`、`s03_secure_transaction_test.go`

**依赖：** T11、T12、T13、T14

**步骤：**

1. 保留 M03 acceptance/restart 和 M05 snapshot/rewind 既有场景，改用统一事务入口。
2. 新增完整链路：候选生成 → review → 安全拒绝 fixture → acceptance 或 rewind → 重启恢复 → 状态/摘要检查。
3. 新增跨卷、并发变更、根替换和 journal 中间 phase 的端到端场景。
4. 记录真实 OS 未覆盖项，避免在 Linux 测试结果中声明 Windows/macOS 行为已验证。

**验证：** `go test ./tests/e2e -run 'Test(M03|M05|S03)' -count=1 -v` 通过。

## T16：更新能力矩阵与规格链文档

**文件：** `docs/spec_docs/S00/platform-capability-matrix.md`、`docs/spec_docs/S03/checklist.md`

**依赖：** T13、T14、T15

**步骤：**

1. 只更新 S00 中 C06/C07 对应安全文件访问和候选事务行，写明支持、降级、待真实 OS 验证状态和证据位置。
2. 保留 C01–C05 的 S02 结论以及历史证据，不重写无关行。
3. 根据 spec AC1–AC11、plan 集成点和端到端场景编写可观察 checklist。
4. 明确阶段 5 的 macOS/Windows 真实 OS 待验项。

**验证：** `git diff --check` 通过；`git ls-files docs/spec_docs/S03/` 显示四份规格链文件，矩阵 diff 仅包含 C06/C07 相关内容。

## T17：运行集成门禁并完成交付准备

**文件：** 受影响的所有 Go 文件、`Makefile` 相关验证脚本、S03 文档

**依赖：** T2、T3、T4、T13、T14、T15、T16

**步骤：**

1. 运行受影响包的完整 Go 测试并修复回归。
2. 运行 Linux 完整测试、platform-check 和 S03 端到端测试。
3. 分别执行 Linux、Darwin、Windows 目标编译，确认目标平台没有编译期依赖泄漏。
4. 对 checklist 每项记录实际证据和未完成的真实 OS 待验项。
5. 检查工作树仅包含 S03 允许文件，并按仓库约定提交每个逻辑任务组。

**验证：** `make test`、`make platform-check`、目标平台 `go build` 和 S03 定向 e2e 均返回 0；验收报告逐项引用命令输出或测试结果。

## 执行顺序

```text
T1
├── T2（Linux secfile）
├── T3（Darwin secfile）
└── T4（Windows secfile）

T2/T3/T4 → T5（workspace） → T6（review/snapshot/ERC）
T2/T3/T4/T5 → T7（事务协调器） → T8（schema/migration）

T7/T8 ─┬→ T9（acceptance） → T11（acceptance recovery） ─┐
       └→ T10（rewind）    → T12（rewind recovery）     ├→ T14
T5/T6/T7 → T13（安全边界测试）                           │
T11/T12/T13/T14 → T15（端到端） → T16（矩阵/checklist） → T17
```

### 最大并行批次

- **批次 1：** T2、T3、T4。三项只修改各自平台文件，验证资源轻量，可并行。
- **批次 2：** T5 完成后执行 T6；T7 可在 T6 之前并行准备，但提交前必须汇合 T5 的安全访问契约。
- **批次 3：** T9 与 T10 可并行，分别修改 acceptance 与 rewind 调用方；T11 与 T12 分别修改对应 store 文件，也可并行。
- **批次 4：** T13 可与 T11/T12 并行；T14 必须等待两条恢复链完成。
- **批次 5：** T15、T16、T17 按顺序执行；集成测试和目标编译错峰运行，避免同时占用高内存。

共享 `transaction.go`、`schema.sql` 或同一测试 fixture 的任务必须先完成契约任务再并行，避免覆盖修改。
