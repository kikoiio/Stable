# S01 编译问题清单

> 开发期滚动记录。门槛：`GOOS=windows go build ./...` 与 `GOOS=darwin go build ./...` 退出码 0（非测试代码）。测试文件编译问题单独一节，标注「不阻塞」。

验证环境：2026-10-06，Ubuntu linux/amd64，go1.26.0。探针命令均带 `CGO_ENABLED=0`。

## 非测试代码（已关闭）

| 包/文件 | 症状 | 处置 | 探针 |
| --- | --- | --- | --- |
| `internal/sandbox/session.go` 无 tag，引用 `LinuxManager`/`syscall.Pdeathsig` | S00 探针：`GOOS=darwin/windows go build ./internal/sandbox/` 失败 | 整包迁入 `internal/platform/sandbox`；session 改调 `proc.ConfigureChild`/`proc.KillGroup`/`ipc.ListenPrivate`；`New()` linux 返回 `LinuxManager`，!linux stub 返回同一类型但方法 fail-closed | `GOOS=windows go build ./internal/platform/sandbox/` 0；`GOOS=darwin go build ./internal/platform/sandbox/` 0 |
| `internal/appconfig/owner_unix.go` 无 tag，`syscall.Stat_t` | S00：`GOOS=windows go build ./internal/appconfig/` 失败 | 拆 `owner_linux.go` + `owner_other.go`（Load fail-closed） | `GOOS=windows go build ./internal/appconfig/` 0 |
| `internal/candidate` 直用 `unix.Openat2`/`Renameat2`/`unix.Open` | 依赖链把 unix 带进全仓 | 迁入 `internal/platform/secfile`；candidate 只调接口 | `GOOS=windows go build ./internal/candidate/` 0 |
| `internal/runtime/supervisor.go` `syscall.Flock`、unix listen、`/proc` | 业务包平台 API | 改调 `lock`/`ipc`/`proc` | 随全仓双 GOOS 编译 |
| `internal/conversation/service.go` unix listen/chmod | 同上 | `ipc.ListenPrivate(path, true)` | 同上 |
| `internal/store` 空白导入 `mattn/go-sqlite3`（CGO） | 非 Linux 编译要 C 工具链 | `driver_linux.go` 空导入；`driver_other.go` `driverAvailable=false`；`Open()` 返回 `store: sqlite is unsupported on this platform` | `CGO_ENABLED=0 GOOS=windows go build ./internal/store/` 0 |
| `internal/runtime/sessions.go` 空白导入 sqlite3 | 会把 CGO 拉进 runtime | 删除 blank import，沿用 linux 下 store 注册的驱动；cmdline 改 `proc.Cmdline` | 随 runtime 双 GOOS |
| `cmd/stable/chatserve.go` `chatserveHelperPath` 第二套布局 | 双轨推导 | 删除；统一 `paths.HelperBinaryResolved`（T5 已落地） | grep 无符号 |
| 6 处业务包 `sandbox.LinuxManager{}` | 组合根外直构 | 全部改为注入 `sandbox.New()`（cmd/stable、chatserve、agentworker、dependency、runtime） | `grep LinuxManager{}` 仅 `internal/platform/sandbox` |
| 全仓 `GOOS=windows/darwin go build ./...` | S00 失败且被 candidate→sandbox 遮蔽 | 六域 stub + 组合根注入后关闭 | `make platform-check`：双 GOOS 退出码 0 |

行为等价的最小修正（进清单，非重写）：`Up()` 由 `p.Root+"/bin/stable"` 改为 `filepath.Join(p.Bin, "stable")`。

## 测试文件（不阻塞）

| 文件 | 症状 | 处置 | 说明 |
| --- | --- | --- | --- |
| `internal/store/migration_continuous_test.go` 空白导入 sqlite3 | 非 Linux 测试编译需要 CGO | 记录不阻塞 | 测试仍只在 Linux 执行 |
| `internal/platform/sandbox/linux_test.go`、`process_linux_test.go` | 带 `//go:build linux` | 随迁，保持 linux tag | 不阻塞 |
| `tests/e2e/*` 原 `sandbox.LinuxManager{}` | 类型从具体类型改为接口 | 改为 `sandbox.New()`；`probeSecretBoundary` 参数改为 `SandboxManager` | Linux 下 `go test ./tests/e2e` 已通过 |
| `cmd/agentworker/assembly_test.go` `LinuxManager{Bwrap: ...}` | 仍构造具体类型以注入假 bwrap | 保留；门禁只禁止空字面量 `LinuxManager{}` | 不阻塞跨平台 `go build`（测试文件不参与） |

## 探针摘要

```
CGO_ENABLED=0 GOOS=windows go build ./...   # exit 0
CGO_ENABLED=0 GOOS=darwin go build ./...    # exit 0
make platform-check                         # grep 零违例 + 双 GOOS
make test / go test ./... -count=1          # Linux 全绿
```
