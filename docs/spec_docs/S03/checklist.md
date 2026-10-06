# S03 安全文件访问与候选验收事务 Checklist

> 每项通过运行代码或观察行为来验证。S03 的 Windows/macOS 主机行为不在本机验收范围，相关项目必须标记为阶段 5 待验，不能以目标平台编译代替真实 OS 证据。

## 实现完整性

- [x] **AC1 / F1：根目录边界** — 对正式根、候选根、manifest 和 ERC 报告运行绝对路径、`..`、根外符号链接/reparse point、目录替代文件和特殊文件 fixture；每次越界都被拒绝，根外文件无读取或修改（验证：`go test ./internal/platform/secfile ./internal/candidate -run 'Test.*(Unsafe|Path|Symlink|Reparse)' -count=1`）。
- [x] **AC2 / F2：统一安全读取** — 候选创建、manifest、review diff、snapshot materialize 和 ERC 报告读取都经过同一安全边界；注入链接替换、根目录替换或文件身份变化后操作失败且已有数据不变（验证：定向 candidate 测试 + 受影响包测试）。
- [x] **AC3 / F3：摘要与并发校验** — 在 acceptance 前修改、删除或替换正式/候选条目，验收返回摘要/版本冲突，不写 receipt，不将决策标记为 applied（验证：`go test ./internal/candidate ./internal/store -run 'Test.*(Stale|Drift|Concurrent|Acceptance)' -count=1`）。
- [x] **AC4 / F4：平台事务语义** — Linux 使用原子目录交换并保持现有结果；macOS/Windows 契约覆盖 `prepared`、中间 phase、回滚、跨卷和能力不足；跨卷或不可证明安全时拒绝且无无保护覆盖（验证：candidate transaction 测试 + 三平台目标编译；真实 OS 留阶段 5）。
- [x] **AC5 / F5：Rewind 事务** — 回退快照在链接、根目录替换、并发、跨卷和失败场景下遵守安全边界；成功得到完整目标快照，失败时正式工程和候选工程都完整可识别（验证：`go test ./internal/candidate ./internal/conversation ./internal/store -run 'Test.*Rewind' -count=1`）。
- [x] **AC6 / F6：崩溃恢复** — 在准备、保存旧根、安装新根、服务目录迁移、交换和 finalize 边界注入终止；重启后得到完整旧版本、完整新版本或 blocked，第二次恢复不改变结果（验证：故障注入与 recovery 测试）。
- [x] **AC7 / F7：拒绝可诊断** — 分别触发路径越界、链接拒绝、并发冲突、跨卷、能力不可用和 journal 不一致；日志、状态或错误能区分原因并指出对象/phase（验证：错误断言 + journal reason 查询）。

## 集成

- [x] **I1：secfile 平台边界** — `candidate`、`store` 和 conversation 不直接使用平台 syscall、链接跟随或目录交换原语，所有工程内容访问走 secfile/coordinator（验证：`bash scripts/check-platform.sh` 与源码扫描）。
- [x] **I2：Acceptance 调用链** — 决策校验 → prepared journal → coordinator apply → digest 校验 → swapped → finalize 顺序成立；重复调用只产生一个 receipt（验证：acceptance 单测与 M03 restart 测试）。
- [x] **I3：Rewind 调用链** — session ownership、生命周期和 digest guard 在 staging 前执行；coordinator 完成后再 finalize、清理 staging、失效旧 review 和写完成事件（验证：conversation rewind 测试和 M05 场景）。
- [x] **I4：启动恢复入口** — runtime 启动会查询 acceptance/rewind 未完成 journal，并调用同一 coordinator；恢复异常转为 blocked，不绕过安全校验（验证：runtime 重启测试 + store recovery 日志/phase）。
- [x] **I5：服务目录归属** — acceptance 交换或中断后 `.stable` 服务目录最终位于正式根；恢复期间不会把会话日志留在候选/rollback 根（验证：包含 `.stable` 的 acceptance 故障测试）。
- [x] **I6：数据库迁移兼容** — v11 journal 可迁移到 v12；新 phase、事务模式和 rollback 路径可读写；旧 receipt、candidate 状态和历史数据保持不变（验证：migration tests + `PRAGMA user_version` 查询）。

## 编译与测试

- [x] **C1：Linux 单元回归** — 所有 Go 单元测试通过，既有 manifest 摘要、原子交换、acceptance receipt、rewind digest 和恢复断言未放宽（验证：`make test`）。
- [x] **C2：平台门禁** — 业务包没有新增平台 syscall/unix/GOOS 直调或绕过 secfile 的私密/安全文件调用（验证：`make platform-check` 返回 0）。
- [x] **C3：目标平台编译** — Linux、Darwin、Windows 目标代码均编译通过；平台无关契约测试在 Linux 通过（验证：目标 `go build` 命令与定向契约测试）。
- [x] **C4：资源与生命周期限制** — manifest、ERC 报告、snapshot 的现有大小/数量上限、敏感信息过滤、文件类型限制和候选状态转换继续生效（验证：candidate/snapshot/permission 定向测试）。
- [x] **C5：规格链完整** — S03 的 `spec.md`、`plan.md`、`task.md`、`checklist.md` 可被 Git 看到且无未完成占位符；S00 C06/C07 记录状态和证据，C01–C05 历史结论未被改写（验证：`git ls-files docs/spec_docs/S03/`、占位标记扫描、矩阵 diff 审阅）。

## 端到端场景

- [x] **E1：Acceptance 并发变更与重启** — 候选完成 review 后修改正式工程，验收被拒且无 receipt；在 prepared/交换后模拟进程终止并重启，系统恢复到完整旧/新版本或 blocked，重复 reconcile 幂等（验证：`go test ./tests/e2e -run 'Test(M03|S03).*Accept' -count=1 -v`）。
- [x] **E2：Rewind 快照恢复与越界拒绝** — session 创建候选快照后发生候选变化，rewind 恢复完整目标快照；对根外链接、跨卷 staging 和中间 phase 注入失败，正式工程不变且 journal 可恢复或明确 blocked（验证：`go test ./tests/e2e -run 'Test(M05|S03).*Rewind' -count=1 -v`）。

## 阶段 5 待验

- [ ] **P1：Windows 真实文件系统行为** — 在 Windows 主机验证 reparse point、卷/文件身份、分阶段移动、崩溃恢复和访问边界；证据补回 C06/C07。
- [ ] **P2：macOS 真实文件系统行为** — 在 macOS 主机验证目录句柄、符号链接、同卷移动、崩溃恢复和访问边界；证据补回 C06/C07。
