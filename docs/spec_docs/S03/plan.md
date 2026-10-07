# S03 安全文件访问与候选验收事务 Plan

> 状态：已批准并完成（2026-10-07 系列收尾确认）。输入规格：[spec.md](spec.md)。

## 架构概览

1. **`internal/platform/secfile` 安全文件适配层** 统一根目录打开、相对路径校验、文件类型/身份检查、同卷检查和目录移动原语。Linux 保留 `openat2` 与 `RENAME_EXCHANGE`；macOS 使用逐级无跟随链接的目录句柄访问；Windows 使用句柄级 reparse point 检查和当前卷/文件身份检查。该层不依赖 candidate 或 store。
2. **`internal/candidate` 工作区与事务协调层** 让 manifest、review、snapshot、acceptance、rewind 都通过 secfile 的安全边界访问工程内容。新增事务协调器，统一校验正式/候选摘要、准备目录、驱动平台交换或分阶段移动，并在每个持久化 phase 变化前后执行一致性检查。
3. **`internal/store` 持久化 journal 与恢复层** 扩展 acceptance/rewind journal 的 phase 和路径元数据，增加数据库迁移。启动恢复根据 journal、根目录身份和摘要判断“旧版本、新版本或阻断”，调用 candidate 事务协调器完成剩余移动或清理；恢复保持幂等。
4. **调用方集成**：`internal/conversation` 的 rewind 流程和 `candidate.AcceptCandidate` 使用同一事务协调器；`runtime` 保持现有启动恢复入口，只增加 S03 journal 恢复。`SnapshotStore` 的 materialize、manifest 和 ERC checker 读取复用同一安全根目录契约。
5. **验证层**增加 secfile 平台契约测试、candidate 根目录/并发/跨卷测试、store migration 与故障恢复测试，并扩展现有 M03/M05 端到端场景。Linux 运行完整回归；Windows/macOS 做目标编译和可在 Linux 执行的纯逻辑契约测试。

## 核心数据结构与接口

### `secfile.Root`

表示一个经过验证的工程根目录，内部持有平台原生目录句柄或等价身份信息。

- `Path`：规范化后的根路径。
- `Identity`：卷/设备与根目录文件身份的不可变快照，具体表示由平台实现隐藏。
- `Open(rel)`：在根目录内打开相对路径，拒绝绝对路径、路径穿越、符号链接/reparse point 和非普通文件。
- `Stat(rel)`：在相同安全规则下读取条目类型、大小、模式和身份。
- `Revalidate()`：确认根目录仍是创建时的同一对象。
- `Close()`：释放平台句柄。

模块级接口：

```go
OpenRoot(path) (Root, error)
SameVolume(pathA, pathB string) error
ExchangeDirectories(pathA, pathB string) error
MoveDirectory(pathA, pathB string, replace bool) error
```

### `candidate.EntryStamp`

用于一次 manifest 或事务校验的运行时文件身份，不写入现有 manifest 摘要：

- 相对路径、文件类型、大小、模式；
- 平台文件身份和卷身份；
- 内容摘要。

现有 `ManifestEntry` 的 JSON 字段和摘要编码保持不变；`EntryStamp` 只用于检测并发替换和 TOCTOU。

### `candidate.DirectoryTransaction`

描述一次 acceptance 或 rewind 的文件系统事务：

- `ID`、`Kind`（acceptance/rewind）；
- `CurrentRoot`：当前正式或候选根；
- `IncomingRoot`：准备安装的新根；
- `RollbackRoot`：非原子平台用于保存旧根的同级路径；
- `ExpectedDigest`、`TargetDigest`；
- `ServiceRoot`：需要在 acceptance 后恢复的 `.stable` 服务目录（如存在）；
- `Mode`：`atomic-exchange` 或 `journaled-move`。

### `candidate.TransactionPhase`

统一事务状态：`prepared`、`old_saved`、`target_installed`、`swapped`、`finalized`、`blocked`。Linux 原子交换可以从 `prepared` 直接进入 `swapped`；分阶段平台按顺序记录中间 phase。任何非终态都允许转为 `blocked`，终态不可回退。

### `candidate.TransactionJournal`

供 acceptance/rewind 两条流程共享的最小持久化契约：

```go
type TransactionJournal interface {
    Advance(ctx context.Context, id, from, to, reason string) error
}
```

候选事务协调器接口：

```go
type TransactionCoordinator interface {
    Apply(ctx context.Context, tx DirectoryTransaction, journal TransactionJournal) error
    Inspect(ctx context.Context, tx DirectoryTransaction, phase TransactionPhase) (RecoveryState, error)
    Recover(ctx context.Context, tx DirectoryTransaction, phase TransactionPhase, journal TransactionJournal) error
    Cleanup(ctx context.Context, tx DirectoryTransaction) error
}
```

`RecoveryState` 只表示可观察结果：完整旧版本、完整新版本、可继续的中间状态或阻断；不暴露平台句柄。

### `store.AcceptanceRecovery` / `store.RewindJournal`

保留现有字段和业务含义，增加事务模式、rollback/backup 路径和当前 phase。数据库迁移扩展两个 journal 的 phase 约束和索引；旧版本记录默认解释为 `atomic-exchange`，确保 S02 已有数据可恢复。

## 模块设计

### `internal/platform/secfile`

**职责：** 提供跨平台安全根目录、文件身份校验、相对路径访问、同卷检查和目录移动/交换原语；平台差异只留在 build-tag 实现。

**对外接口：** `OpenRoot`、`SecureOpen`、`OpenNoFollow`、`SameVolume`、`ExchangeDirectories`、`MoveDirectory` 及根目录身份查询。

**依赖：** Go 文件 API；Linux 使用现有 `x/sys/unix`，macOS 使用 Unix 目录句柄/无跟随链接调用，Windows 使用系统文件句柄与 reparse point 检查。不得依赖 `candidate`、`store` 或业务状态。

### `internal/candidate/workspace.go` 与新增路径校验模块

**职责：** 生成 manifest、复制候选文件、读取 manifest 条目、创建/冻结候选；所有工程内容访问从 `secfile.Root` 取得文件句柄，并在遍历前后重新验证根身份。

**对外接口：** 保留 `BuildManifest`、`CreateCandidate`、`FreezeCandidate`、`CleanRelative` 等现有入口；新增内部的根快照和条目校验辅助函数。

**依赖：** `secfile`、摘要计算、现有配额/生命周期规则。

### `internal/candidate/review.go`、`snapshot.go`、`erc_checker.go`

**职责：** 让 diff、快照 blob、manifest materialize、ERC 报告读取复用同一安全根目录和文件身份校验；保留现有摘要、敏感信息过滤和大小上限。

**对外接口：** 现有 review/snapshot/checker API 保持兼容。

**依赖：** `workspace` 安全访问、`secfile`、sandbox checker（不改变阶段 4 范围）。

### 新增 `internal/candidate/transaction.go`

**职责：** 编排 acceptance/rewind 的目录事务；校验 expected/target 摘要和根身份，按平台选择原子交换或 journal 分阶段移动，处理 `.stable` 服务目录恢复，并通过 `TransactionJournal` 推进 phase。

**对外接口：** 实现 `TransactionCoordinator`；`AcceptCandidate`、`SwapWithStaging` 改为调用它。

**依赖：** `secfile`、`TransactionJournal`、manifest/recovery 状态。

### `internal/store/candidate.go`、`rewind.go`、`sqlite.go`、`schema.sql`

**职责：** 保存事务模式、rollback 路径和中间 phase；执行 v12 schema migration；启动时读取未完成事务并调用 coordinator，完成、回滚或阻断。

**对外接口：** 保留现有 acceptance/rewind store 方法；扩展 recovery 查询和 phase 转换，确保旧 journal 默认按 Linux 原子交换解释。

**依赖：** SQLite、`candidate` coordinator。恢复只依据持久化数据与磁盘可观察状态，不猜测进程是否曾经运行。

### `internal/conversation/snapshots.go` 与 runtime 启动恢复

**职责：** rewind 同步路径使用统一 coordinator；保留 session ownership、运行中拒绝、事件记录和 review 失效行为。runtime 继续从既有入口触发 acceptance/rewind reconcile。

**依赖：** `candidate`、`store`、`sessionlog`。

### 测试与文档模块

**职责：** secfile 平台契约、candidate 安全边界、事务 phase/migration、故障恢复和 M03/M05 端到端覆盖；同步 S00 C06/C07 状态与证据，落盘 S03 四份文档。

**依赖：** 现有 Go 测试、目标平台编译命令、Linux fixture。

## 模块交互

### Review / manifest 流程

1. 调用方把可信正式根或候选根交给 `candidate`。
2. `candidate` 通过 `secfile.OpenRoot` 获取根身份；遍历只产生安全的相对路径。
3. 每个条目经 `Root.Open` 和 `Root.Stat` 读取，记录现有 `ManifestEntry` 摘要，同时保存运行时 `EntryStamp`。
4. 遍历结束后重新验证根身份；根或条目发生替换则返回并发/安全错误。
5. review、snapshot 和 ERC checker 复用同一读取路径，不再各自实现链接和根目录判断。

### Acceptance 流程

1. `AcceptCandidate` 校验 review、决策摘要、当前正式/候选 manifest 和生命周期。
2. `store.SaveAcceptanceDecision` 在 SQLite 中写入 `prepared` journal，并保存事务模式和 rollback 路径。
3. `TransactionCoordinator` 重新打开两个根，检查身份、同卷条件和 expected digest。
4. Linux 调用原子目录交换；macOS/Windows 依次保存旧根、安装新根、恢复旧根到候选位置，每一步成功后推进对应 phase。
5. 事务完成后恢复 `.stable` 服务目录，重新生成正式 manifest，确认 target digest。
6. journal 进入 `swapped`，随后由 `FinalizeAcceptance` 在同一数据库事务中写入 receipt、候选状态、目标状态和事件。
7. 任一步骤失败都进入 `blocked` 或保留未完成 journal，调用方得到可诊断错误。

### Rewind 流程

1. conversation 校验 session ownership、候选状态、无活动 run、无其他未完成事务和当前 candidate digest。
2. `SnapshotStore` 将验证过的快照 materialize 到候选根同卷的 staging 目录。
3. 写入 `RewindJournal(prepared)`，调用 coordinator 交换候选根与 staging；非原子平台同时维护 rollback 路径。
4. 验证 target digest 后将 journal 置为 `swapped`，`FinalizeRewind` 更新候选摘要并关闭 journal，最后清理旧 staging。
5. 失效旧 review，并按现有顺序写入 rewind session 事件。

### 启动恢复流程

1. runtime 打开 store 后查询 `prepared`、`old_saved`、`target_installed`、`swapped` journal。
2. store 将每条记录和磁盘路径交给 coordinator `Inspect/Recover`。
3. coordinator 只根据根身份、路径存在性和 expected/target digest 判断：旧版本仍完整则清理 staging/rollback 并 finalized；新版本已完整则补齐服务目录和数据库 finalize；中间状态可继续则完成剩余移动并校验摘要；两边都不匹配或身份异常则 blocked，不覆盖任何工程。
4. 恢复再次运行得到相同结果，不重复 receipt、状态更新或文件移动。

## 文件组织

```text
internal/platform/secfile/
├── secfile.go             — 统一错误、根目录和平台原语接口
├── root.go                — Root、RootIdentity、相对路径访问协调
├── secfile_linux.go       — 保留 openat2、RENAME_EXCHANGE、设备检查
├── secfile_darwin.go      — 目录句柄、O_NOFOLLOW 和同卷实现
├── secfile_windows.go     — 文件句柄、reparse point、卷/文件身份实现
├── secfile_other.go       — 非目标平台继续 fail-closed
└── *_test.go              — 路径/身份/能力契约测试

internal/candidate/
├── transaction.go         — DirectoryTransaction、phase、Apply/Recover
├── transaction_test.go    — 平台模式与故障状态机测试
├── workspace.go           — manifest/候选创建接入 Root
├── review.go              — diff 与条目读取接入 Root
├── snapshot.go            — snapshot blob/materialize 接入 Root
├── accept.go              — acceptance 使用 coordinator
├── rewind.go              — staging/swap 使用 coordinator
└── *_test.go              — 越界、链接、并发、跨卷回归

internal/store/
├── schema.sql             — journal phase/元数据约束
├── sqlite.go              — v12 journal migration
├── candidate.go           — acceptance recovery 与 phase 扩展
├── rewind.go              — rewind recovery 与 phase 扩展
└── *_test.go              — migration、幂等恢复、阻断测试

internal/conversation/snapshots.go
                           — rewind coordinator 集成

tests/e2e/
├── m03_accept_restart_test.go
├── m05_sessions_test.go
└── s03_secure_transaction_test.go
                           — 端到端故障/边界场景

docs/spec_docs/S00/platform-capability-matrix.md
                           — 更新 C06/C07 状态和证据
docs/spec_docs/S03/
├── spec.md
├── plan.md
├── task.md
└── checklist.md
                           — S03 规格链文档
.gitignore                 — S03 文档白名单
```

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 安全访问边界 | 扩展现有 `internal/platform/secfile`，由 `candidate` 只使用 Root/安全打开接口 | 保持 S02 的平台适配边界，避免业务包重新出现 syscall、链接或平台判断 |
| Linux 实现 | 保留 `openat2`、`O_NOFOLLOW`、设备检查和 `RENAME_EXCHANGE` | 满足 Linux 回归基线，避免改变已验证的原子交换语义 |
| macOS 文件访问 | 逐级目录句柄打开并禁止跟随链接，最终句柄再做普通文件与身份校验 | macOS 没有 Linux `openat2`，但可以保留根目录约束和 TOCTOU 防护 |
| Windows 文件访问 | 使用系统文件句柄检查 reparse point、卷身份和文件身份；无法证明安全时返回明确错误 | Windows `os.File`/`os.Chmod` 不能单独表达所需边界，必须依赖句柄级信息 |
| 非 Linux 目录事务 | 三步同卷移动：保存旧根、安装新根、把旧根放回候选/staging 位置；每步持久化 phase | 支持崩溃恢复，避免无 journal 的逐文件覆盖；跨卷直接拒绝 |
| Journal 状态 | 为 acceptance/rewind 增加 `old_saved`、`target_installed` 等中间 phase、事务模式和 rollback 路径 | 仅凭 `prepared/swapped` 无法判断非原子移动发生在哪一步 |
| 数据库迁移 | 新增 v12 migration，重建带 CHECK 约束的 journal 表并保留旧记录 | SQLite 不能直接修改既有 CHECK 约束；旧记录按 atomic-exchange 兼容解释 |
| 恢复判定 | 只依据 journal、根身份、路径存在性和 expected/target 摘要；不猜测历史执行结果 | 使恢复可审计、可重复，异常时安全阻断 |
| Manifest 兼容 | 保持现有 `ManifestEntry` 字段和摘要编码；文件身份只存运行时 `EntryStamp` | 不使已有候选、review 和 snapshot 摘要失效 |
| `.stable` 服务目录 | 将服务目录从正式根转移到最终正式根的动作纳入事务恢复 | 防止 acceptance 中断后会话日志停留在错误根目录 |
| 验收边界 | 目标平台编译 + Linux 契约测试；macOS/Windows 真实行为不属本系列范围 | 与 S03 已批准的验证范围一致，不冒充未执行的 OS 证据 |
