# S02 目录、身份、IPC 与运行时生命周期(阶段 2)Plan

> 状态:已批准(2026-10-06)。依据已批准的 spec.md(docs/spec_docs/S02/spec.md)。方案:扩展 S01 建立的 internal/platform 六域包,不新增顶层组件。

## 架构概览

改动集中在三类:

1. **platform 包补全非 Linux 实现**:ipc/lock 把 Linux 实现泛化为 unix(覆盖 darwin),新增 windows 实现;proc 新增 darwin/windows 实现;secfile 扩展出「私密文件 API」(F2 主战场);paths 扩展出「用户基目录推导」(F1 落点)。
2. **appconfig/TUI/doctor/runtime 改为从平台来源取值**:appconfig 的路径推导与 owner 检查下沉到 platform;TUI 删除硬编码路径;doctor 的工具清单按 GOOS 拆分(build-tag 文件);sessions/supervisor 清掉残留直调。
3. **门禁扩展**:scripts/check-platform.sh 允许 unix/darwin/windows build tag,新增「业务包无私密 chmod 直调」扫描。

## 核心数据结构与接口

```go
// ── internal/platform/paths ──
// 用户基目录推导(GOOS 各自的默认值文件 + XDG 覆盖层,纯函数可跨平台测试)
func UserConfigHome() (string, error) // linux: XDG_CONFIG_HOME|~/.config
                                      // darwin: XDG_CONFIG_HOME|~/Library/Application Support
                                      // windows: XDG_CONFIG_HOME|%AppData%
func UserStateHome() (string, error)  // linux: XDG_STATE_HOME|~/.local/state
                                      // darwin: XDG_STATE_HOME|~/Library/Application Support
                                      // windows: XDG_STATE_HOME|%LocalAppData%

// 签名变更:去掉对 appconfig 的反向依赖(appconfig 改为调用本包)
func Resolve(stateDir string) (Paths, error)   // 原 Resolve(c appconfig.AppConfig)
// Paths 结构体字段不变

// ── internal/appconfig(签名不变,实现下沉) ──
func ConfigPath() (string, error)     // STABLE_CONFIG | join(UserConfigHome(), "stable", "config.json")
func StateDir() (string, error)       // STABLE_STATE_DIR | join(UserStateHome(), "stable")
func UserSkillsDir() (string, error)  // join(UserConfigHome(), "stable", "skills")
func UserHooksPath() (string, error)  // 不变
func UserCommandsDir() (string, error)// 新增:join(UserConfigHome(), "stable", "commands"),供 TUI 用

// ── internal/platform/secfile 扩展(私密文件 API;openat2/Exchange 等原语不动) ──
func MkdirAllPrivate(path string, perm os.FileMode) error
    // MkdirAll + 权限确保(POSIX: mkdir 后显式 Chmod 防 umask;Windows: 当前用户 ACL)
func OpenFilePrivate(path string, flag int, perm os.FileMode) (*os.File, error)
    // 等价 os.OpenFile,但新建时权限真实成立
func ChmodPrivate(path string, perm os.FileMode) error
    // POSIX: os.Chmod 透传;Windows: 0700/0600 → 当前用户 ACL,0444 → 只读属性
func OwnedByCurrentUser(info os.FileInfo) (bool, error)
    // 从 appconfig/owner_linux.go 迁入;Windows: 文件 owner SID == 当前用户 SID
func IsPrivate(info os.FileInfo) (bool, error)
    // appconfig Load 的私密性判定:POSIX 用 perm&0077;Windows 查 ACL/owner,fail-closed

// ── internal/platform/ipc(对外 API 不变) ──
// 实现文件重组:ipc_linux.go → ipc_unix.go(//go:build unix,linux+darwin 共用)
// 新增 ipc_windows.go:命名管道(github.com/Microsoft/go-winio)
// 纯函数抽到 GOOS 中立文件以便 Linux 上契约测试:
func PipeName(path string) string   // state socket 路径 → \\.\pipe\stable\<sha1 前 16 位>
func CurrentUserPipeSDDL() string   // "D:P(A;;GA;;;<当前用户 SID>)" 的构造逻辑

// ── internal/platform/lock(对外 API 不变) ──
// lock_linux.go → lock_unix.go(flock,linux+darwin 共用)
// 新增 lock_windows.go:LockFileEx 非阻塞独占;锁文件经 secfile 私密创建

// ── internal/platform/proc(微调 + 新增两个跨平台函数) ──
// 保留:Alive / StopProcess / StopGroup / KillGroup / ConfigureChild / Cmdline
// 新增:
func Terminate(p *os.Process) error   // unix: SIGTERM;windows: TerminateProcess
                                      // 替换 sessions.go 的 syscall.SIGTERM 直调
func AdoptChild(cmd *exec.Cmd) error  // windows: Start 后把进程绑入 Job Object
                                      //(KILL_ON_JOB_CLOSE,父死亡即回收);unix: no-op
// pgid 专属 API(StopGroup/KillGroup)在 windows 返回 unsupported:
// windows 的进程树回收由 Job Object 承担(AdoptChild + StopProcess 级联)
// darwin:ConfigureChild 只设 Setpgid(无 Pdeathsig,能力表记降级)
// darwin Cmdline:sysctl kern.proc.args;windows:NtQueryInformationProcess 读 PEB
//   (均纯 Go;darwin 实现受阻时降级为 unsupported 并记能力表)
```

## 模块设计

### paths(internal/platform/paths)
**职责:** 安装布局解析(S01 已有)+ 用户基目录推导(S02 新增)。
**对外接口:** UserConfigHome/UserStateHome(新增)、Resolve(stateDir string)(改签名)、Paths 结构体与 Prepare/HelperBinary*(不变)。
**依赖:** os、filepath;不再依赖 appconfig。GOOS 默认值放 user_linux.go/user_darwin.go/user_windows.go(各含 build tag),XDG 覆盖层与 join 逻辑放 GOOS 中立的 user.go 便于单测。

### appconfig(internal/appconfig)
**职责:** 配置加载/校验/保存;路径推导委托给 paths;私密性校验委托给 secfile。
**对外接口:** Load/Save/ConfigPath/StateDir/UserSkillsDir/UserHooksPath 签名全不变,新增 UserCommandsDir;owner_linux.go/owner_other.go 删除,改调 secfile.OwnedByCurrentUser/IsPrivate;Load 的私密性错误文案保留 Linux 措辞、Windows 用 icacls 措辞(文案构造函数按 GOOS 拆分)。
**依赖:** platform/paths、platform/secfile。

### secfile(internal/platform/secfile)
**职责:** 原 S01 安全文件原语(openat2/Exchange/SameDevice,本阶段不动)+ S02 私密文件 API(MkdirAllPrivate/OpenFilePrivate/ChmodPrivate/OwnedByCurrentUser/IsPrivate)。
**实现拆分:** API 与 Windows ACL 的 SDDL 描述符构造放 GOOS 中立文件(契约测试可跑);syscall 应用放 *_unix.go/*_windows.go 薄层。
**依赖:** os、x/sys(windows 侧);无内部依赖。

### ipc(internal/platform/ipc)
**职责:** 三平台私有本机监听/拨号。
**实现拆分:** ipc_unix.go(//go:build unix:socket + chmod 0600,linux/darwin 共用,行为与现 linux 实现逐行一致);ipc_windows.go(go-winio ListenPipe/DialPipe + 当前用户 SDDL);pipe_name.go(PipeName/CurrentUserPipeSDDL 纯函数,中立)。
**语义映射(Windows):** removeStale=true → 无操作(管道由内核管理,无陈旧文件);removeStale=false(网络代理「已存在即报错」)→ 先 Dial 探测,连通即返回 already in use 错误(Windows 允许同名管道多监听者,必须显式预检保住单实例语义)。
**依赖:** net、go-winio。

### lock(internal/platform/lock)
**职责:** 三平台非阻塞独占锁。
**实现拆分:** lock_unix.go(flock,逐行沿用现 linux 实现);lock_windows.go(LockFileEx + LOCKFILE_FAIL_IMMEDIATELY,锁文件经 OpenFilePrivate 创建)。
**依赖:** os、x/sys/windows、secfile。

### proc(internal/platform/proc)
**职责:** 三平台进程存活/停止/子进程属性/命令行读取。
**实现拆分:** proc_linux.go(现状保留);proc_darwin.go(Alive=Signal(0)(无 /proc,僵尸检测降级记能力表)、StopProcess=SIGTERM+KILL、Setpgid 组操作、ConfigureChild 只设 Setpgid、Cmdline=sysctl);proc_windows.go(Alive=OpenProcess 探测、StopProcess=Job terminate/宽限、AdoptChild 绑 Job、Terminate=TerminateProcess、Cmdline=PEB 读取);proc.go 保留 API 层。
**依赖:** os/exec、x/sys/windows(windows 侧)。

### runtime(internal/runtime:supervisor/sessions/doctor)
**职责:** 运行时编排不变;调用面清理——sessions.go 的 process.Signal(syscall.SIGTERM) 改 proc.Terminate;supervisor 启动子进程处补 proc.AdoptChild(windows 生效,unix no-op);doctor 工具清单拆 doctor_tools_{linux,darwin,windows}.go(各含 build tag,供门禁豁免),doctor 主体 GOOS 中立,对不可用能力(非 Linux 沙箱 Probe 失败)透出明确原因。
**依赖:** platform/*(不新增)。

### tui(internal/tui)
**职责:** 删除 model.go 自拼 ~/.config/stable/commands,改调 appconfig.UserCommandsDir;技能/hooks 帮助文案中的路径改为实际解析结果。

## 模块交互

**启动链(三平台一致):**

```
cmd/stable → appconfig.Load()
  ├─ paths.UserConfigHome/UserStateHome → ConfigPath/StateDir(私密校验 → secfile.IsPrivate/OwnedByCurrentUser)
  └─ paths.Resolve(cfg.StateDir) → Paths
→ runtime.Up(ctx, cfg, paths)
  ├─ lock.TryAcquire(paths.Lock)         // 20s 重试循环留在 supervisor,不变
  ├─ ipc.ListenPrivate(paths.Socket, true)
  ├─ proc.ConfigureChild(temporal/worker cmd) → cmd.Start() → proc.AdoptChild(cmd)
  └─ 运行中:proc.Alive 监控;停止:proc.StopProcess(windows 经 Job 级联杀树)
```

**会话清理链:** sessions.StopSessionProcesses → proc.Cmdline(pid) 身份匹配 → proc.Terminate(p)。

**TUI:** appconfig.UserCommandsDir/UserSkillsDir → 命令/技能列表与帮助文案。

**doctor:** doctor_tools_*.go 工具清单 → 逐项探测 → proc 状态查询 → 沙箱能力 Probe(不可用则报原因)。

## 文件组织

```
docs/spec_docs/S02/
├── spec.md / plan.md / task.md / checklist.md
└── real-os-acceptance.md   (第 5 份文档:真实 OS 待验清单,AC8 要求)

internal/platform/paths/
├── paths.go                — Resolve(stateDir) 签名变更;Paths 结构不变
├── user.go(新)            — XDG 覆盖层 + join 逻辑(GOOS 中立,可单测)
├── user_linux.go / user_darwin.go / user_windows.go(新,各含 build tag)
├── paths_linux.go / paths_other.go(exeSuffix,保留)
└── user_test.go(新)

internal/platform/secfile/
├── secfile.go              — 原 4 原语声明保留 + 新增 5 个私密 API 声明
├── private.go(新)         — GOOS 中立:SDDL 描述符构造、权限位判定、错误文案
├── private_unix.go(新)    — linux+darwin(POSIX 权限位,逐行等价现状)
├── private_windows.go(新) — ACL 应用(SetNamedSecurityInfo 等)
└── private_test.go(新)

internal/platform/ipc/
├── ipc.go                  — API 不变,注释更新
├── ipc_unix.go             — 由 ipc_linux.go 改名,tag 改 //go:build unix
├── ipc_windows.go(新)     — go-winio 监听/拨号 + 预检语义
├── pipe_name.go(新)       — PipeName / CurrentUserPipeSDDL 纯函数
└── pipe_name_test.go(新);ipc_other.go 删除

internal/platform/lock/
├── lock.go(API 不变)
├── lock_unix.go            — 由 lock_linux.go 改名
├── lock_windows.go(新)    — LockFileEx;锁文件经 secfile 私密创建
└── lock_other.go 删除

internal/platform/proc/
├── proc.go                 — API 层新增 Terminate / AdoptChild
├── proc_linux.go(保留)    — proc_darwin.go(新)、proc_windows.go(新)
└── proc_other.go 删除

internal/appconfig/
├── config.go               — 路径推导委托 paths;私密校验委托 secfile;+UserCommandsDir
└── owner_linux.go / owner_other.go 删除(迁入 secfile)

internal/runtime/
├── supervisor.go           — Resolve 适配;子进程 Start 后补 proc.AdoptChild
├── sessions.go             — syscall.SIGTERM 直调 → proc.Terminate
├── doctor.go               — 工具清单外移,主体 GOOS 中立
└── doctor_tools_{linux,darwin,windows}.go(新,build tag 供门禁豁免)

internal/tui/model.go       — UserCommandsDir 接入,帮助文案路径动态化

cmd/stable/main.go、chatserve.go — paths.Resolve 调用适配

私密 chmod 迁移(16 个业务文件,全部 os.Chmod/f.Chmod → secfile API):
execution/{sandbox_profile,executor_factory,tool_executor}、artifact/store、
planfile/planfile、candidate/{snapshot,workspace,erc_checker}、
sessionlog/{path,gitignore,log}、inputhistory/{path,store}、core/activities、todo/store

scripts/check-platform.sh   — build tag 白名单 +unix/darwin/windows;新增 Chmod 直调扫描
go.mod / go.sum             — 新增 go-winio;x/sys 转直接依赖
docs/spec_docs/S00/platform-capability-matrix.md — C01–C05 状态更新 + 追加 S02 位置更新节
```

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 架构形态 | 扩展 S01 platform 六域包 | spec 已批;复用门禁与能力表对应关系 |
| Windows 命名管道 | 新增 go-winio 依赖 | 标准库不支持;自研 CreateNamedPipe 封装风险高 |
| darwin 复用方式 | linux 实现改 //go:build unix 共用 | socket/flock/chmod 在 darwin 语义相同,逐行复用而非复制 |
| paths→appconfig 解耦 | Resolve(stateDir string) | 打断反向依赖,appconfig 才能下沉调用 paths;调用方仅 3 处 |
| XDG 变量覆盖层 | 三平台都读 XDG_CONFIG_HOME/XDG_STATE_HOME | 兑现 F1「显式覆盖各平台一致生效」 |
| Windows 单实例 IPC | removeStale=false 时先 Dial 预检 | go-winio 允许同名多监听者;预检保住网络代理「已存在即报错」 |
| Windows 进程树回收 | Job Object(KILL_ON_JOB_CLOSE) | 父死亡回收等价 Pdeathsig;pgid 专属 API 返回 unsupported |
| macOS Pdeathsig | Setpgid+组停止兜底,能力表记「支持(降级)」 | 无等价原语,如实降级不静默 |
| Cmdline 非 Linux 实现 | darwin=sysctl;windows=PEB 读取;受阻则降级 unsupported 并记矩阵 | 纯 Go(CGO_ENABLED=0 门槛);实际调用方仅 Linux GUI 清理路径 |
| 私密文件 API 宿主 | secfile 包 | 对应 S00 矩阵 C05 行;不新增顶层包 |
| 门禁 chmod 规则 | 业务包禁 os.Chmod/.Chmod( 直调(含 0444 只读标记) | 规则简单防回归;所有权限收紧走同一接口 |
| 真实 OS 验收 | real-os-acceptance.md 单列清单 | N3/AC8:不冒充已验证,留阶段 5 执行 |
