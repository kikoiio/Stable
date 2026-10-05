# S01 平台边界抽取(阶段 1)Checklist

> 状态:已批准(2026-10-06),开发未启动。每一项通过运行代码或观察行为验证,先有证据再下结论。

## 实现完整性
- [ ] 六域包均含 中立入口 + linux 实现 + other stub(验证:文件盘点 + GOOS=linux/windows/darwin 三向编译探针)
- [ ] sandbox 迁移后测试随迁且通过(验证:`go test ./internal/platform/sandbox/`)

## 需求对照(spec AC1–AC9 逐条)
- [ ] AC1 六域包落地且接口使业务包无需平台 API;grep 门禁(N2 检查工具)跑通且零违例
- [ ] AC2 业务包 `LinuxManager{}` 直引 grep = 0;组合根构造点集中在 cmd/*,全部经注入
- [ ] AC3 `GOOS=windows go build ./...` 与 `GOOS=darwin go build ./...` 均退出码 0;compile-inventory.md 每项有处置记录
- [ ] AC4 store:Linux 测试全过;windows/darwin 编译通过;非 Linux Open 返回明确 unsupported(探针+代码审阅)
- [ ] AC5 布局推导单轨:chatserveHelperPath 删除,grep 无第二套推导;`.exe`/dev-install/libexec/share 处理集中在 paths
- [ ] AC6 含平台 API 的 .go 文件均持有 build tag 或位于 platform,无 tag 清单为空
- [ ] AC7 `make test` 全过;S00 能力表 26 条可运行验证重放全过;S00 能力表完成 N4 位置同步
- [ ] AC8 `git ls-files docs/spec_docs/S01/` 可见五份文档;spec_docs 其他文件忽略状态不变
- [ ] AC9 端到端场景 1 通过

## 行为保全(搬移而非重写)
- [ ] lock 重试循环与 status 幂等探测逻辑逐行未变(验证:代码审阅 + supervisor 测试)
- [ ] sandbox proxy 拒绝已存在路径 + SameFile 清理语义未变(验证:network 测试)
- [ ] 候选事务语义:`go test ./internal/store/ -run TestReconcile` 全过(交换前/后/冲突/rewind 各用例)
- [ ] conversation IPC 协议测试通过(`go test ./internal/conversation/ -run TestSessionProtocolClientLifecycle`)
- [ ] candidate 安全根目录与原子交换测试全过(`go test ./internal/candidate/... -count=1`)

## 编译与测试
- [ ] `make test` 全过
- [ ] `make platform-check` 零违例(含 grep 门禁与双 GOOS 编译)

## 端到端场景
- [ ] 场景 1(Linux 回归):构建→up→候选验收关键路径重放,行为与迁移前一致(e2e 断言:验收 receipt、formal 翻转、无残留进程)
- [ ] 场景 2(跨平台开发者视角):`GOOS=darwin go build ./...` 直接成功;非 Linux 仅在触发 unsupported 能力时得到明确错误(store Open、sandbox Probe),无 panic
- [ ] 场景 3(门禁可持续):临时引入一处违例(业务包写 `unix.Open`)→ `make platform-check` 必须报出 → 删除后复验归零
