# S01 平台边界抽取(阶段 1)Plan

> 状态:已完成（2026-10-07 系列收尾确认）。依据已批准的 spec.md(docs/spec_docs/S01/spec.md)与 S00 能力表(docs/spec_docs/S00/platform-capability-matrix.md)。

## 架构概览

**总原则**:搬移而非重写。每个域把「现有 Linux 实现」原样迁入 platform,接口切面按现有调用点反推,Linux 上调用序列不变。

```
cmd/stable, cmd/agentworker          ← 组合根:构造全部平台依赖并注入
        │ 注入 sandbox.New() / Paths 等
        ▼
业务包 runtime / conversation / candidate / execution / dependency / store / appconfig
        │ 只依赖 internal/platform/* 的接口与抽象
        ▼
internal/platform/{paths, ipc, lock, proc, secfile, sandbox}
        ├── 接口或入口(全平台编译)
        ├── linux 实现(//go:build linux,现有代码迁入)
        └── other stub(//go:build !linux,返回明确 unsupported)
```

- **既有资产**:internal/sandbox/process.go 已是中立接口(SandboxManager/Profile/ErrUnavailable)——sandbox 域整体迁入 internal/platform/sandbox,其余五域新建。
- **stub 语义**:非 Linux 编译通过、运行即明确报错(如 `secfile: secure openat2 is only supported on Linux`),与 S00 fail-closed 决策一致;Linux 行为零变化。

## 核心数据结构(六域接口)

### paths(吸收 runtime/paths.go 与 chatserveHelperPath 双轨)

```go
type Paths struct { Root, Bin, Libexec, Share, State, Goals, Database, TemporalDB,
                    Socket, ChatSocket, Lock, SupervisorLog, TemporalLog, WorkerLog, ChatLog string }
func Resolve(c appconfig.AppConfig) (Paths, error)      // 原样迁入
func (p Paths) Prepare() error
func (p Paths) HelperBinary(name string) string         // Libexec/name+exeSuffix(.exe 后缀由 build tag 文件提供,linux 为空)
func (p Paths) HelperBinaryResolved(name string) string // 吸收 chatserveHelperPath 三候选:libexec → dev-install/libexec → 同级;stat 探测
```

调用点:Up() 的 `p.Root+"/bin/stable"` 改用 `p.Bin`;goal/agentctl、temporal/agentworker spawn 全部改走 HelperBinary;chatserveHelperPath 删除。

### ipc

```go
func ListenPrivate(path string, removeStale bool) (net.Listener, error)
// listen 后 chmod 0600(保持现状顺序,S00 D12 窗口如实保留);removeStale=true 先 os.Remove(supervisor/chatserve 语义),false 已存在即报错(network proxy 语义)
func DialPrivate(path string, timeout time.Duration) (net.Conn, error)
```

调用点:Supervise 控制通道、conversation.Serve、sandbox network/session socket。network.go 的 SameFile 退出清理留在原处(proxy 专属)。

### lock

```go
type Guard interface{ Release() error }
func TryAcquire(path string) (Guard, error)  // 0600 建文件 + Flock LOCK_EX|LOCK_NB
```

supervisor 的 20s 重试循环与 status 幂等探测**原样保留**,仅把 flock 调用换成 TryAcquire。

### proc

```go
func Alive(cmd *exec.Cmd) bool                          // Signal(0) + /proc/<pid>/stat 非 Z,原样迁入
func StopProcess(cmd *exec.Cmd, grace time.Duration)    // SIGTERM→wait→Kill(supervisor 现传 5s)
func StopGroup(pgid int, termGrace, killGrace time.Duration) error  // 进程组 SIGTERM→SIGKILL(sandbox 会话/一次性命令)
func ConfigureChild(cmd *exec.Cmd) error                // Setpgid+Pdeathsig(sandbox session/process_linux 用)
func Cmdline(pid int) (string, error)                   // /proc/<pid>/cmdline NUL→空格(sessions.go GUI 清理用)
```

waitPort/waitReady 是纯 net+文件逻辑,留在 runtime。sessions.go 的 cmdline 前缀匹配逻辑不动,只把 /proc 读取换成 Cmdline。

### secfile

```go
func SecureOpen(root, rel string) (*os.File, error)  // openat2 RESOLVE_BENEATH|NO_SYMLINKS + O_NOFOLLOW + fstat,原样迁入
func OpenNoFollow(path string) (*os.File, error)     // O_RDONLY|O_CLOEXEC|O_NOFOLLOW(erc 报告读取)
func Exchange(dirA, dirB string) error               // 双方 Lstat + 同设备号 + RENAME_EXCHANGE,原样迁入
```

candidate 保留 CleanRelative/BuildManifest/CreateCandidate 等纯逻辑,secureOpen/Openat2/Renameat2 调用点改指 secfile。

### sandbox(整体迁移 internal/sandbox → internal/platform/sandbox)

- process.go(中立接口)、network.go(中立代理逻辑)原样;linux.go/process_linux.go/network_linux.go 带 linux tag 迁入;session.go 改为引用 `newLinuxManager()`(linux.go 返回真实现、other.go 返回 ErrUnavailable stub)+ proc 域函数,成为全平台可编译的中立编排。
- 新增 `func New() SandboxManager`:linux 返回 LinuxManager{},!linux 返回 stub——组合根唯一构造入口。

### store / appconfig

- store:`driver_linux.go`(mattn 空导入,driverAvailable=true)/ `driver_other.go`(false);`Open()` 在不可用时返回 `store: sqlite is unsupported on this platform`。
- appconfig:`owner_unix.go` → `owner_linux.go`(linux tag)+ `owner_other.go`(stub:归属检查返回 unsupported 错误,Load 随之 fail closed)。

## 模块设计(组合根与迁移映射)

**注入链**(LinuxManager 6 处构造点 → 0):

| 现构造点 | 迁移后 |
|----------|--------|
| cmd/stable/main.go → supervise → runtime.Supervise → runChatService(×2 处) | cmd/stable 构造 `sbx := sandbox.New()` 传入 `Supervise(c, p, sbx)` → 透传 runChatService |
| cmd/stable/chatserve.go(×2 处:executorFactory、chatCandidateCheckers) | chatserve 构造 `sandbox.New()` 传入两处 |
| cmd/agentworker/main.go:173(isolator) | `isolator := sandbox.New()` |
| internal/dependency/service.go:46 | `NewKiCadRefresher` 增加 `sbx sandbox.SandboxManager` 参数,由两个调用方(runChatService、chatserve)注入 |

**其余调用点迁移**:

| 现状 | 迁移后 |
|------|--------|
| supervisor.go:123–143 Flock 内联 | platform/lock.TryAcquire,重试循环与 status 幂等探测原样保留 |
| supervisor.go:150–165 socket remove/listen/chmod | ipc.ListenPrivate(p.Socket, true) |
| supervisor.go:426–450 processAlive/stopProcess | proc.Alive / proc.StopProcess(cmd, 5s) |
| supervisor.go:176/186 与 main.go:242 的 filepath.Join(Libexec, …) | p.HelperBinary("temporal"/"agentworker"/"agentctl") |
| supervisor.go:76 p.Root+"/bin/stable" | filepath.Join(p.Bin, "stable")(行为等价,进清单记录) |
| conversation/service.go:102–112 serve socket | ipc.ListenPrivate(path, true),chmod 失败关 listener 语义保留 |
| sessions.go /proc cmdline 读取 | proc.Cmdline(pid),前缀匹配逻辑不动 |
| sandbox/session.go、process_linux.go 的 Setpgid/Pdeathsig/syscall.Kill | proc.ConfigureChild / proc.StopGroup |
| chatserveHelperPath 三候选 | paths.HelperBinaryResolved,chatserveHelperPath 删除 |
| candidate workspace.go secureOpen / accept.go ExchangeProjectDir / erc_checker.go unix.Open | secfile.SecureOpen / secfile.Exchange / secfile.OpenNoFollow |
| appconfig.Load 的 ownedByCurrentUser | owner_linux.go / owner_other.go(stub 返回 unsupported → Load fail closed) |

## 模块交互

```
cmd/stable main
  ├─ paths.Resolve(c) → Paths
  ├─ sandbox.New() → SandboxManager
  ├─ supervise: runtime.Supervise(c, p, sbx)
  │     ├─ lock.TryAcquire(p.Lock)                    (20s 重试循环留在 runtime)
  │     ├─ ipc.ListenPrivate(p.Socket, true)
  │     ├─ proc.Alive / proc.StopProcess              (temporal/worker watchdog)
  │     └─ runChatService(c, p, addr, sbx)
  │           ├─ dependency.NewKiCadRefresher(..., sbx)
  │           ├─ execution.ToolExecutorDeps{Sandbox: sbx, ...}
  │           └─ candidate.KicadERCChecker{Sandbox: sbx, ...} → secfile.OpenNoFollow
  ├─ chatserve: sandbox.New() → conversation.Serve / executorFactory / checkers
  └─ proxy wrapper: sandbox.RunProxyCommand
cmd/agentworker
  └─ sandbox.New() → execution.PythonBridge{Sandbox: …}
candidate
  └─ secfile.SecureOpen / secfile.Exchange / secfile.OpenNoFollow
```

## 文件组织

```
internal/platform/
├── paths/paths.go, paths_linux.go(exeSuffix=""), paths_other.go(exeSuffix=".exe")
├── ipc/ipc.go, ipc_linux.go, ipc_other.go
├── lock/lock.go(中立类型), lock_linux.go, lock_other.go
├── proc/proc.go(中立签名), proc_linux.go, proc_other.go
├── secfile/secfile.go(中立签名), secfile_linux.go, secfile_other.go
└── sandbox/  ← internal/sandbox 整体迁入
    ├── process.go(中立接口,原样) network.go(中立)
    ├── linux.go process_linux.go network_linux.go   //go:build linux
    ├── session.go(改用 newLinuxManager()+proc)
    ├── other.go(New() stub + newLinuxManager stub)  //go:build !linux
    └── 对应 _test.go 随迁
internal/runtime/   — 删 paths.go;supervisor.go 去 Flock//proc//socket 内联实现改调 platform;sessions.go 改调 proc.Cmdline
internal/conversation/ — service.go 改用 ipc.ListenPrivate
internal/candidate/ — workspace.go/accept.go/erc_checker.go 改调 secfile
internal/dependency/ — NewKiCadRefresher 增加 Sandbox 参数
internal/store/ — driver_linux.go/driver_other.go 拆分
internal/appconfig/ — owner_linux.go/owner_other.go
docs/spec_docs/S01/compile-inventory.md — 编译问题清单(开发期产出)
scripts/check-platform.sh + Makefile 目标 platform-check — grep 门禁 + GOOS 双编译
```

## 门禁与编译清单设计

**grep 门禁**(scripts/check-platform.sh + `make platform-check`):

1. 业务包违例扫描:`grep -rn "golang.org/x/sys/unix\|runtime.GOOS\|syscall\." internal/ cmd/ --include="*.go"`,排除白名单(internal/platform/**、持有 `//go:build linux` 或 `!linux` 的文件),输出必须为空。`syscall.SIGTERM/SIGINT`(signal 常量,全平台可移植)列入允许 token。
2. 构造点扫描:`grep -rn "LinuxManager{}" --include="*.go"` 仅允许出现在 internal/platform/sandbox 内。
3. 无 tag 平台文件扫描:含平台 API 的文件必须持有 build tag——由第 1 条反向保证。
4. `GOOS=windows go build ./...` 与 `GOOS=darwin go build ./...` 退出码必须为 0。

**编译清单**(docs/spec_docs/S01/compile-inventory.md,开发期滚动维护):每项记录——包/文件、症状(undefined/requires cgo)、处置(迁 platform / build tag 拆分 / stub / 记录不阻塞)、验证探针结果。测试文件编译问题单独一节标注「不阻塞」。

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| sandbox 包去留 | 整体迁 internal/platform/sandbox | 已批准 spec F1;既有中立接口是资产,迁移后六域结构统一 |
| stub 粒度 | 每域 !linux stub 文件,运行即报 unsupported | 非 Linux 不可用是 S00 既定状态;编译通过即可满足阶段 1 完成标志 |
| lock 接口形态 | TryAcquire + 调用方保留重试循环 | 重试/幂等语义是 supervisor 业务编排,抽走会改变行为;只抽 flock 原语 |
| chmod 时序 | 保持 listen 后 chmod 原样 | S00 D12 记录的 umask 窗口是已知现状；修复属于行为变更，不在 S01/S00–S05 范围内 |
| waitPort/waitReady 归属 | 留 runtime | 纯 net+文件逻辑,非平台依赖 |
| exeSuffix 实现 | paths_linux/other.go 提供 const | 编译期确定,无运行时分支 |
| Up() 的 bin/stable 拼接 | 改用 p.Bin | 行为等价的最小修正,进清单记录 |
| gate 信号常量 | 允许 syscall.SIGTERM/SIGINT | os/signal 需要且全平台可移植;门禁允许 token 显式列举 |
| 测试文件平台化 | 清单化不阻塞 | 用户已批口径;测试仍仅在 Linux 执行 |
| 提交粒度 | 每域或每组调用点迁移一提交 | N6;每提交点 make test 绿 |
