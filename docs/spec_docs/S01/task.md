# S01 平台边界抽取(阶段 1)Tasks

> 状态:已完成（2026-10-07 系列收尾确认）。依据已批准的 spec.md 与 plan.md(docs/spec_docs/S01/)。

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `internal/platform/ipc/`、`lock/`、`proc/`、`secfile/`、`paths/`(各含中立+linux+other 文件) | 平台域包 |
| 迁移 | `internal/sandbox/` → `internal/platform/sandbox/` | 沙箱域(含 New()/stub) |
| 删除 | `internal/runtime/paths.go`、`internal/appconfig/owner_unix.go`、`chatserveHelperPath` | 双轨/旧文件收敛 |
| 新建 | `internal/store/driver_linux.go`、`driver_other.go`;`internal/appconfig/owner_linux.go`、`owner_other.go` | 平台 stub |
| 修改 | `internal/runtime/{supervisor,sessions}.go`、`internal/conversation/service.go`、`internal/candidate/{workspace,accept,erc_checker}.go`、`internal/dependency/service.go`、`cmd/stable/{main,chatserve}.go`、`cmd/agentworker/main.go` | 调用点切换与注入 |
| 新建 | `scripts/check-platform.sh`、Makefile `platform-check` 目标 | grep 门禁 + GOOS 双编译 |
| 新建 | `docs/spec_docs/S01/compile-inventory.md` | 编译问题清单 |
| 修改 | `docs/spec_docs/S00/platform-capability-matrix.md` | N4 位置同步 |

## T1: platform/ipc 包

**文件:** 新建 `internal/platform/ipc/ipc.go`、`ipc_linux.go`、`ipc_other.go`
**依赖:** 无
**步骤:** 按 plan 定义 ListenPrivate(path, removeStale)(listen 后 chmod 0600,顺序保持现状)与 DialPrivate(path, timeout);linux 实现自 supervisor/conversation 现有内联代码提炼;other stub 返回明确 unsupported。
**验证:** `GOOS=linux go build ./internal/platform/ipc/`、`GOOS=windows go build ./internal/platform/ipc/` 通过(包自足)。

## T2: platform/lock 包

**文件:** 新建 `internal/platform/lock/lock.go`、`lock_linux.go`、`lock_other.go`
**依赖:** 无
**步骤:** Guard 接口(Release)与 TryAcquire(path):0600 建文件 + Flock LOCK_EX|LOCK_NB;other stub 返回 unsupported。
**验证:** 双 GOOS 编译通过;行为验证随 T8 supervisor 回归。

## T3: platform/proc 包

**文件:** 新建 `internal/platform/proc/proc.go`、`proc_linux.go`、`proc_other.go`
**依赖:** 无
**步骤:** Alive(cmd)(Signal(0)+/proc stat 非 Z)、StopProcess(cmd, grace)(SIGTERM→wait→Kill)、StopGroup(pgid, termGrace, killGrace)、ConfigureChild(cmd)(Setpgid+Pdeathsig)、Cmdline(pid)(NUL→空格);全部原样迁入现有实现;other stub 返回 unsupported/false。
**验证:** 双 GOOS 编译通过;行为验证随 T8/T6 回归(含 TestRunProcessGroupStopsDescendantsOnContextCancellation)。

## T4: platform/secfile 包

**文件:** 新建 `internal/platform/secfile/secfile.go`、`secfile_linux.go`、`secfile_other.go`
**依赖:** 无
**步骤:** SecureOpen(root, rel)(openat2 RESOLVE_BENEATH|NO_SYMLINKS+O_NOFOLLOW+fstat)、OpenNoFollow(path)、Exchange(dirA, dirB)(Lstat+同设备号+RENAME_EXCHANGE),自 candidate 原样迁入;other stub 返回 unsupported。
**验证:** 双 GOOS 编译通过;行为验证随 T7 candidate 既有测试回归。

## T5: platform/paths 包与全仓迁移

**文件:** 新建 `internal/platform/paths/paths.go`、`paths_linux.go`(exeSuffix="")、`paths_other.go`(exeSuffix=".exe");删除 `internal/runtime/paths.go`;修改全部 Paths/Resolve 引用(runtime、cmd/stable、tui 等按 grep 实际结果)
**依赖:** T1–T4
**步骤:** Paths 结构与 Resolve/Prepare 原样迁入;新增 HelperBinary(name)(Libexec/name+exeSuffix)与 HelperBinaryResolved(name)(libexec→dev-install/libexec→同级,stat 探测);全仓 import 与类型引用更新。
**验证:** `make test` 全过;`go build ./...` 通过。(影响面最广,独占批次执行)

## T6: sandbox 域迁移与中立化

**文件:** 迁移 `internal/sandbox/*` → `internal/platform/sandbox/*`;修改全仓 import(执行、runtime、dependency、cmd、tests)
**依赖:** T5
**步骤:** process.go/network.go 原样;linux 系文件带 //go:build linux 迁入;session.go 中立化(Setpgid/Pdeathsig→proc.ConfigureChild,syscall.Kill→proc.StopGroup,LinuxManager 直引→newLinuxManager());新增 New()(linux→LinuxManager,!linux→stub)与 other.go;包内 socket 建立改用 ipc.ListenPrivate;network 同设备号语义不变。
**验证:** `make test` 全过(sandbox/candidate/execution 测试随迁);`GOOS=darwin go build ./internal/platform/sandbox/` 通过。

## T7: candidate 改调 secfile

**文件:** 修改 `internal/candidate/workspace.go`、`accept.go`、`erc_checker.go`
**依赖:** T6
**步骤:** secureOpen→secfile.SecureOpen;ExchangeProjectDir 的 Lstat+设备号+Renameat2→secfile.Exchange;erc 报告 unix.Open→secfile.OpenNoFollow;CleanRelative/BuildManifest/CreateCandidate 纯逻辑不动。
**验证:** `go test ./internal/candidate/... -count=1` 全过(含穿越/符号链接/原子交换用例)。

## T8: runtime 调用点切换

**文件:** 修改 `internal/runtime/supervisor.go`、`sessions.go`
**依赖:** T5,T6
**步骤:** Flock→lock.TryAcquire(20s 循环与 status 探测原样);socket→ipc.ListenPrivate(p.Socket, true);processAlive/stopProcess→proc.Alive/proc.StopProcess(cmd, 5*time.Second);temporal/agentworker spawn→p.HelperBinary;Up() 的 p.Root+"/bin/stable"→filepath.Join(p.Bin, "stable")(进清单);sessions.go /proc 读取→proc.Cmdline。
**验证:** `go test ./internal/runtime/... -count=1` 全过。

## T9: conversation 与 dependency

**文件:** 修改 `internal/conversation/service.go`、`internal/dependency/service.go`
**依赖:** T6
**步骤:** service.go serve socket→ipc.ListenPrivate(path, true)(chmod 失败关 listener 语义保留);NewKiCadRefresher 增加 sbx sandbox.SandboxManager 参数,内部构造点移除。
**验证:** `go test ./internal/conversation/ -count=1` 全过。

## T10: store 驱动拆分

**文件:** 新建 `internal/store/driver_linux.go`、`driver_other.go`;修改 `sqlite.go`
**依赖:** 无(可与 T5–T9 并行)
**步骤:** driver_linux.go 空导入 mattn/go-sqlite3 并置 driverAvailable=true;driver_other.go 置 false;Open() 开头不可用即返回 `store: sqlite is unsupported on this platform`。
**验证:** `go test ./internal/store/ -count=1` 全过;`GOOS=windows go build ./internal/store/` 通过。

## T11: appconfig owner 拆分

**文件:** 新建 `internal/appconfig/owner_linux.go`、`owner_other.go`;删除 `owner_unix.go`;修改 `config.go` 如需
**依赖:** 无(可与 T5–T9 并行)
**步骤:** ownedByCurrentUser 迁 owner_linux.go(linux tag);owner_other.go 返回 unsupported 错误使 Load fail closed;错误信息说明平台限制。
**验证:** `go test ./internal/appconfig/ -count=1` 全过;`GOOS=windows go build ./internal/appconfig/` 通过。

## T12: 组合根注入

**文件:** 修改 `cmd/stable/main.go`、`cmd/stable/chatserve.go`、`cmd/agentworker/main.go`
**依赖:** T6,T9
**步骤:** supervise 路径构造 sbx:=sandbox.New() 传入 Supervise(c, p, sbx)→runChatService;chatserve 两处构造注入;agentworker isolator:=sandbox.New();NewKiCadRefresher 调用方传 sbx;全部构造点确认无 LinuxManager 直引。
**验证:** `grep -rn "LinuxManager{}" --include="*.go" | grep -v internal/platform` 为空;`make test` 全过。

## T13: 门禁脚本与 Makefile

**文件:** 新建 `scripts/check-platform.sh`;修改 `Makefile`(platform-check 目标)
**依赖:** T5–T12
**步骤:** 按 plan 第 5 段实现四项检查(业务包违例扫描含白名单与允许 token、LinuxManager 构造点扫描、GOOS 双编译)。
**验证:** `make platform-check` 跑通且零违例。

## T14: 编译清单关闭

**文件:** 新建 `docs/spec_docs/S01/compile-inventory.md`
**依赖:** T13
**步骤:** 运行 GOOS=windows/darwin go build ./...;残余问题逐项修复或记录;清单成文(来源/症状/处置/探针结果),测试文件编译问题单独一节标「不阻塞」。
**验证:** 双 GOOS build 退出码 0;清单每项有处置记录。

## T15: Linux 回归

**文件:** 无新文件
**依赖:** T14
**步骤:** `make test` 全过;S00 能力表 26 条可运行验证逐条重放(appconfig/candidate/store Reconcile/sandbox(bwrap 实跑)/conversation/execution/redact/runtime/python 双桥)。
**验证:** 全绿并记录输出摘要;任何失败修复后复跑。

## T16: e2e 关键路径(AC9)

**文件:** 无新文件
**依赖:** T15
**步骤:** 构建→`stable up`(或定向 e2e)→候选验收关键路径重放(m03 验收/重启恢复 e2e 或等价定向测试,视环境资源选择)。
**验证:** 行为与迁移前一致(e2e 断言通过:验收 receipt、formal 翻转、无残留进程)。

## T17: S00 能力表同步与收尾

**文件:** 修改 `docs/spec_docs/S00/platform-capability-matrix.md`
**依赖:** T16
**步骤:** 按 N4 追加位置更新(如 secureOpen→platform/secfile、Flock→platform/lock、6 构造点清零等证据位置变更),不重写历史结论;Linux 状态复核无降级;`git ls-files docs/spec_docs/S01/` 确认五份文档跟踪。
**验证:** 位置更新条目可核对;AC8 检查通过。

## 执行顺序

```
B1: T1 ∥ T2 ∥ T3 ∥ T4                    (全新文件,互不冲突)
B2: T5                                     (paths 全仓迁移,独占)
B3: T6                                     (sandbox 迁移,全仓 import,独占)
B4: T7 ∥ T8 ∥ T9 ∥ T10 ∥ T11              (文件集互斥:candidate/runtime/conversation+dependency/store/appconfig)
B5: T12                                    (组合根,cmd/*)
B6: T13 → B7: T14 → B8: T15 → B9: T16 → T17
```

**并行边界:**
- B1 四任务仅新增文件,零冲突,可完全并行。
- T5、T6 各自全仓改 import,必须独占批次,避免相互覆盖。
- B4 五任务文件集互斥(candidate/runtime/conversation+dependency/store/appconfig),可并行;若实际执行中发现共享文件,先协调串行。
- 每批次汇合点运行 `make test` + 定向测试,绿了才提交(N6);go test 按包定向,全量测试错峰执行(本机内存规则)。
- 门禁与 GOOS 编译属轻量构建;B4 并行期内不做全量 build,留到批次汇合点。
