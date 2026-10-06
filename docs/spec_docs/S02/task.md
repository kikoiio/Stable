# S02 目录、身份、IPC 与运行时生命周期(阶段 2)Tasks

> 状态:已批准(2026-10-06)。依据已批准的 spec.md 与 plan.md(docs/spec_docs/S02/)。
> 执行约定:并行子代理须遵守文件所有权边界(不同任务不同文件);启动批量编译/全量测试等重型操作前检查内存(`free -h`、/proc/pressure/memory)并与主代理协调;每个任务完成即运行其验证,先有证据再标记完成;每组逻辑相关任务完成后提交一次。

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 修改 | `internal/platform/paths/paths.go` | Resolve 签名变更 |
| 新建 | `internal/platform/paths/user.go` + `user_{linux,darwin,windows}.go` | 用户基目录推导 |
| 新建 | `internal/platform/paths/user_test.go` | 路径单测 |
| 修改 | `internal/appconfig/config.go` | 路径下沉、UserCommandsDir、owner/chmod 委托 secfile |
| 删除 | `internal/appconfig/owner_linux.go`、`owner_other.go` | owner 检查迁入 secfile |
| 修改 | `internal/tui/model.go` | 目录来源统一与文案动态化 |
| 修改 | `cmd/stable/main.go`、`cmd/stable/chatserve.go` | Resolve 调用适配 |
| 修改 | `internal/platform/secfile/secfile.go` | 私密 API 声明 |
| 新建 | `internal/platform/secfile/private.go`、`private_unix.go`、`private_windows.go`、`private_test.go` | 私密文件 API 实现 |
| 修改 | 16 个业务文件(见 T9–T14) | os.Chmod/f.Chmod → secfile API |
| 修改 | `internal/platform/ipc/ipc.go`、`ipc_linux.go→ipc_unix.go` | unix 泛化 |
| 删除 | `internal/platform/ipc/ipc_other.go` | — |
| 新建 | `internal/platform/ipc/pipe_name.go`、`pipe_name_test.go`、`ipc_windows.go` | Windows 命名管道 |
| 修改 | `internal/platform/lock/lock_linux.go→lock_unix.go` | unix 泛化 |
| 删除 | `internal/platform/lock/lock_other.go` | — |
| 新建 | `internal/platform/lock/lock_windows.go` | LockFileEx 实现 |
| 修改 | `internal/platform/proc/proc.go` | 新增 Terminate/AdoptChild |
| 新建 | `internal/platform/proc/proc_unix.go`、`proc_darwin.go`、`proc_windows.go` | 三平台进程实现 |
| 删除 | `internal/platform/proc/proc_other.go` | — |
| 修改 | `internal/runtime/supervisor.go`、`sessions.go`、`doctor.go` | 调用面清理 |
| 新建 | `internal/runtime/doctor_tools_{linux,darwin,windows}.go` | 平台工具清单 |
| 修改 | `scripts/check-platform.sh` | 门禁扩展 |
| 修改 | `go.mod`、`go.sum` | go-winio 依赖 |
| 修改 | `docs/spec_docs/S00/platform-capability-matrix.md` | C01–C05 状态更新 |
| 新建 | `docs/spec_docs/S02/real-os-acceptance.md` | 真实 OS 待验清单 |

## T1: paths.Resolve 签名变更与调用方适配

**文件:** `internal/platform/paths/paths.go`、`cmd/stable/main.go`、`cmd/stable/chatserve.go`
**依赖:** 无
**步骤:**
1. `Resolve(c appconfig.AppConfig)` 改为 `Resolve(stateDir string)`,内部 `filepath.Abs(c.StateDir)` 改为 `filepath.Abs(stateDir)`;删除对 `stable/internal/appconfig` 的 import。
2. 适配调用方:`cmd/stable/main.go` 两处、`cmd/stable/chatserve.go` 一处改为 `paths.Resolve(c.StateDir)`。

**验证:** `go build ./...` 退出码 0;`go vet ./internal/platform/paths/ ./cmd/stable/` 无新告警。

## T2: paths 用户基目录推导

**文件:** 新建 `internal/platform/paths/user.go`、`user_linux.go`、`user_darwin.go`、`user_windows.go`、`user_test.go`
**依赖:** 无(与 T1 同包但不同文件,合并提交前确认 T1 已入库)
**步骤:**
1. `user.go`(无 tag):`UserConfigHome()`/`UserStateHome()` 先读 XDG 覆盖层(XDG_CONFIG_HOME/XDG_STATE_HOME),未设则调 GOOS 文件的 `defaultConfigHome()`/`defaultStateHome()`,返回 `filepath.Join(base)` 不追加 stable 子目录。
2. `user_linux.go`(`//go:build linux`):默认 `~/.config` 与 `~/.local/state`(`os.UserHomeDir()` 推导,与现状一致)。
3. `user_darwin.go`(`//go:build darwin`):两者默认 `~/Library/Application Support`。
4. `user_windows.go`(`//go:build windows`):分别 `%AppData%`、`%LocalAppData%`(`os.Getenv("AppData")`/`"LocalAppData"`,空则回退 `os.UserConfigDir()`/`os.UserCacheDir()`)。
5. `user_test.go`:固定 HOME/环境变量矩阵断言 Linux 默认值与现状逐字节相同、darwin/windows 默认值符合惯例、XDG 覆盖在三平台语义一致。

**验证:** `go test ./internal/platform/paths/ -count=1` 退出码 0;`CGO_ENABLED=0 GOOS=windows go build ./internal/platform/paths/` 与 darwin 同样退出码 0。

## T3: appconfig 路径推导下沉

**文件:** `internal/appconfig/config.go`
**依赖:** T2
**步骤:**
1. `ConfigPath`/`StateDir`/`UserSkillsDir`/`UserHooksPath` 的 XDG/`os.UserHomeDir()` 推导改为调用 `paths.UserConfigHome()`/`paths.UserStateHome()`;`STABLE_CONFIG`/`STABLE_STATE_DIR` 覆盖逻辑与优先级不变。
2. 新增 `UserCommandsDir() (string, error)` = `join(UserConfigHome(), "stable", "commands")`。

**验证:** `go test ./internal/appconfig/ -count=1` 退出码 0(Linux 默认路径断言不改仍过)。

## T4: TUI 目录来源统一

**文件:** `internal/tui/model.go`
**依赖:** T3
**步骤:**
1. model.go:238-242 自拼 `~/.config/stable/commands` 改调 `appconfig.UserCommandsDir()`,错误时降级为空列表(沿用现有容错风格)。
2. model.go:460、745 技能/hooks 提示文案中的 `~/.config/stable/...` 改为运行时用 `appconfig.UserSkillsDir()`/`UserHooksPath()` 实际解析结果拼接。

**验证:** `go build ./...` 退出码 0;`grep -rn '\.config' internal/tui/ --include='*.go'` 仅剩注释或无结果;`go test ./internal/tui/... -count=1` 退出码 0(如无测试则 build 即可)。

## T5: secfile 私密 API 中立层

**文件:** `internal/platform/secfile/secfile.go`(追加声明)、新建 `private.go`、`private_test.go`
**依赖:** 无
**步骤:**
1. secfile.go 追加 `MkdirAllPrivate`/`OpenFilePrivate`/`ChmodPrivate`/`OwnedByCurrentUser`/`IsPrivate` 声明与文档注释(转发到 `private*.go` 的 `mkdirAllPrivate` 等小写实现,与包内现有转发风格一致)。
2. private.go(无 tag):Windows ACL 的 SDDL 构造纯函数 `currentUserSDDL(sid string) string`(`D:P(A;;FA;;;`+sid+`)`)、`permDeniesOthers(perm os.FileMode) bool`、按 GOOS 取修复提示的文案构造(POSIX:"chmod 600/700 并确认归属";Windows:"icacls /inheritance:r /grant:r 当前用户:F")。
3. private_test.go:SDDL 构造、perm 判定、文案构造断言(全部可在 Linux 运行)。

**验证:** `go test ./internal/platform/secfile/ -count=1` 退出码 0。

## T6: secfile POSIX 实现

**文件:** 新建 `internal/platform/secfile/private_unix.go`(`//go:build unix`)
**依赖:** T5
**步骤:**
1. `MkdirAllPrivate`:MkdirAll 后对每个新建层级显式 Chmod 到目标 perm(防 umask);已存在目录同样 Chmod 收紧(与业务包现状语义一致)。
2. `OpenFilePrivate`:OpenFile(flag, perm) 后,新建场景 Chmod 收紧。
3. `ChmodPrivate`:透传 os.Chmod。
4. `OwnedByCurrentUser`:从 `internal/appconfig/owner_linux.go` 逐行迁移(Stat_t.Uid == Getuid;darwin 无 Stat_t 断言时按 unix 通用写法处理并在注释说明)。
5. `IsPrivate`:`perm&0077 == 0` + OwnedByCurrentUser。
6. 用临时目录写行为断言测试(0700/0600 实际落盘权限位)。

**验证:** `go test ./internal/platform/secfile/ -count=1` 退出码 0;`go test ./internal/appconfig/ ./internal/store/ -count=1` 回归通过。

## T7: secfile Windows 实现

**文件:** 新建 `internal/platform/secfile/private_windows.go`(`//go:build windows`)
**依赖:** T5
**步骤:**
1. 用 x/sys/windows 实现:MkdirAllPrivate/OpenFilePrivate/ChmodPrivate 对 0700/0600/0077 类 perm 应用 `currentUserSDDL` 构造的当前用户 ACL(SetNamedSecurityInfo 或 SecurityDescriptorFromString 路线);0444 应用 FILE_ATTRIBUTE_READONLY。
2. `OwnedByCurrentUser`:GetNamedSecurityInfo 取 owner SID 与当前 token SID 比较。
3. `IsPrivate`:owner 匹配 + DACL 无其他用户 ACE(查询失败 fail-closed 返回错误)。

**验证:** `CGO_ENABLED=0 GOOS=windows go build ./internal/platform/secfile/` 退出码 0;`go vet` 同 GOOS 无新告警。

## T8: appconfig owner 检查迁入 secfile

**文件:** 删除 `internal/appconfig/owner_linux.go`、`internal/appconfig/owner_other.go`;修改 `internal/appconfig/config.go`
**依赖:** T6、T7
**步骤:**
1. Load 中 `ownedByCurrentUser` 调用改 `secfile.OwnedByCurrentUser`;私密性判定改 `secfile.IsPrivate`。
2. 错误文案改用 private.go 的文案构造(Linux 措辞与现状一致或更明确)。
3. config.go:271 的 `os.Chmod(dir, 0700)` 改 `secfile.MkdirAllPrivate`/`ChmodPrivate` 对应调用。
4. 删除两个 owner 文件。

**验证:** `go test ./internal/appconfig/ -count=1` 退出码 0;`ls internal/appconfig/owner_*` 无结果。

## T9: execution 包 chmod 迁移

**文件:** `internal/execution/sandbox_profile.go`、`executor_factory.go`、`tool_executor.go`
**依赖:** T6、T7
**步骤:** sandbox_profile.go:74、executor_factory.go:236、tool_executor.go:787 的 os.Chmod 改 secfile.ChmodPrivate/MkdirAllPrivate(按语义:建目录用 MkdirAllPrivate,收权用 ChmodPrivate)。
**验证:** `go build ./...` 退出码 0;`go test ./internal/execution/ -count=1` 退出码 0。

## T10: artifact 与 planfile chmod 迁移

**文件:** `internal/artifact/store.go`、`internal/planfile/planfile.go`
**依赖:** T6、T7
**步骤:** store.go:94 `tmp.Chmod(0444)` 改 `secfile.ChmodPrivate(tmp.Name(), 0444)`;planfile.go:56/59/64/125 四处对应迁移(0700 目录、0600 文件)。
**验证:** `go build ./...` 退出码 0;`go test ./internal/artifact/ ./internal/planfile/ -count=1` 退出码 0。

## T11: candidate 包 chmod 迁移

**文件:** `internal/candidate/snapshot.go`、`workspace.go`、`erc_checker.go`
**依赖:** T6、T7
**步骤:** snapshot.go:79/317/329/354、workspace.go:170(Chmod(rootMode) 改 ChmodPrivate(target, rootMode))、erc_checker.go:93/100 对应迁移。
**验证:** `go build ./...` 退出码 0;`go test ./internal/candidate/... -count=1` 退出码 0。

## T12: sessionlog 包 chmod 迁移

**文件:** `internal/sessionlog/path.go`、`gitignore.go`、`log.go`
**依赖:** T6、T7
**步骤:** path.go:74、gitignore.go:24、log.go:156(f.Chmod(0600) 改 ChmodPrivate(f.Name(), 0600))迁移。
**验证:** `go build ./...` 退出码 0;`go test ./internal/sessionlog/ -count=1` 退出码 0。

## T13: inputhistory 包 chmod 迁移

**文件:** `internal/inputhistory/path.go`、`store.go`
**依赖:** T6、T7
**步骤:** path.go:30/53、store.go:181/220(f.Chmod)迁移。
**验证:** `go build ./...` 退出码 0;`go test ./internal/inputhistory/ -count=1` 退出码 0。

## T14: core 与 todo 包 chmod 迁移

**文件:** `internal/core/activities.go`、`internal/todo/store.go`
**依赖:** T6、T7
**步骤:** activities.go:796/844、todo/store.go:103/168/171 迁移。
**验证:** `go build ./...` 退出码 0;`go test ./internal/core/ ./internal/todo/ -count=1` 退出码 0。

## T15: 门禁扩展

**文件:** `scripts/check-platform.sh`
**依赖:** T9–T14 全部完成
**步骤:**
1. build tag 白名单 `^//go:build (linux|!linux)\b` 扩为 `(linux|!linux|unix|darwin|windows)\b`。
2. 新增业务包扫描:`os\.Chmod\(|\.Chmod\(` 命中即失败(internal/cmd 下,platform 与带白名单 tag 的文件除外)。
3. 负向自验:临时在业务文件加一处 `os.Chmod` 确认脚本报错,删除后复验归零。

**验证:** `make platform-check` 退出码 0;负向注入确认报错后删除。

## T16: ipc unix 泛化

**文件:** `internal/platform/ipc/ipc_linux.go`→`ipc_unix.go`、删除 `ipc_other.go`、更新 `ipc.go` 注释
**依赖:** 无
**步骤:**
1. `ipc_linux.go` 重命名为 `ipc_unix.go`,build tag 改 `//go:build unix`;内容逐行不变。
2. 删除 `ipc_other.go`(unix+windows 覆盖全部支持目标)。
3. ipc.go 头注释改为「Unix domain socket(macOS/Linux)/命名管道(Windows)」。

**验证:** `go build ./...` 退出码 0;`CGO_ENABLED=0 GOOS=darwin go build ./internal/platform/ipc/` 退出码 0;`go test ./internal/conversation/ ./internal/runtime/ -count=1` 退出码 0。

## T17: ipc Windows 纯函数

**文件:** 新建 `internal/platform/ipc/pipe_name.go`、`pipe_name_test.go`
**依赖:** 无
**步骤:**
1. `PipeName(path string) string`:把 state 内 socket 路径映射为 `\\.\pipe\stable\<sha1(path) 前 16 位>`(确定性,路径含空格/反斜杠安全)。
2. `CurrentUserPipeSDDL() string`:返回 `D:P(A;;GA;;;`+当前用户 SID+`)` 的构造逻辑(SID 获取薄层放 windows 文件,构造逻辑中立可测)。
3. 单测:映射确定性、不同路径不同名、SDDL 格式。

**验证:** `go test ./internal/platform/ipc/ -count=1` 退出码 0。

## T18: 引入 go-winio 依赖

**文件:** `go.mod`、`go.sum`
**依赖:** 无
**步骤:** `go get github.com/Microsoft/go-winio@latest` + `go mod tidy`;确认 x/sys 保持 v0.47.0 或兼容升级。

**验证:** `go build ./...` 退出码 0;`go mod tidy` 后 git diff 无二次变化。

## T19: ipc Windows 实现

**文件:** 新建 `internal/platform/ipc/ipc_windows.go`(`//go:build windows`)
**依赖:** T17、T18
**步骤:**
1. `listenPrivate(path, removeStale)`:go-winio `ListenPipe(PipeName(path), &PipeConfig{SecurityDescriptor: CurrentUserPipeSDDL(), MessageMode: false, InputBufferSize/OutputBufferSize 合理默认})`。
2. removeStale=false 单实例语义:Dial 探测 PipeName(path),连通则返回 "ipc: address already in use" 错误(对齐 unix 侧 EADDRINUSE 语义)。
3. `dialPrivate`:go-winio `DialPipe(PipeName(path), timeout)`。

**验证:** `CGO_ENABLED=0 GOOS=windows go build ./internal/platform/ipc/` 退出码 0。

## T20: lock 三平台实现

**文件:** `internal/platform/lock/lock_linux.go`→`lock_unix.go`、删除 `lock_other.go`、新建 `lock_windows.go`
**依赖:** T5、T7(锁文件经 secfile.OpenFilePrivate 创建)
**步骤:**
1. lock_unix.go:tag 改 `//go:build unix`,内容逐行不变。
2. lock_windows.go:OpenFilePrivate(O_CREATE|O_RDWR, 0600) + `windows.LockFileEx(LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY)`;Release 解锁+关闭。
3. 删除 lock_other.go。

**验证:** `go build ./...` 退出码 0;`CGO_ENABLED=0 GOOS={darwin,windows} go build ./internal/platform/lock/` 退出码 0;`go test ./internal/runtime/ -count=1` 退出码 0(supervisor 锁重试回归)。

## T21: proc API 层与 unix 公共实现

**文件:** `internal/platform/proc/proc.go`、新建 `proc_unix.go`(`//go:build unix`)
**依赖:** 无
**步骤:**
1. proc.go 追加 `Terminate(p *os.Process) error` 与 `AdoptChild(cmd *exec.Cmd) error` 声明(转发风格同现有)。
2. proc_unix.go:Terminate = p.Signal(SIGTERM);AdoptChild = no-op(nil)。

**验证:** `go build ./...` 退出码 0;`CGO_ENABLED=0 GOOS=darwin go build ./internal/platform/proc/` 退出码 0。

## T22: proc darwin 实现

**文件:** 新建 `internal/platform/proc/proc_darwin.go`(`//go:build darwin`)
**依赖:** T21
**步骤:**
1. alive:Signal(0) 判活(无 /proc,注释注明僵尸检测降级)。
2. stopProcess/killGroup/stopGroup:SIGTERM/SIGKILL + kill(-pgid)(与 linux 行为一致)。
3. configureChild:SysProcAttr{Setpgid: true}(无 Pdeathsig,注释注明降级)。
4. cmdline:sysctl `kern.proc.args.<pid>`(纯 Go);实现受阻则返回 unsupported 错误并在注释标注「能力表 C04 darwin 降级」。

**验证:** `CGO_ENABLED=0 GOOS=darwin go build ./internal/platform/proc/` 退出码 0。

## T23: proc windows 实现

**文件:** 新建 `internal/platform/proc/proc_windows.go`(`//go:build windows`)
**依赖:** T21
**步骤:**
1. alive:OpenProcess + 探测退出状态。
2. stopProcess:优先 Job terminate,回退 TerminateProcess;grace 语义对齐。
3. AdoptChild:创建 Job Object(KILL_ON_JOB_CLOSE)并 AssignProcessToJobObject。
4. killGroup/stopGroup(pgid):返回 unsupported(树回收由 Job 承担)。
5. Terminate:TerminateProcess。
6. configureChild:CREATE_SUSPENDED 可选优化不做,no-op 返回 nil(绑定在 AdoptChild)。
7. cmdline:NtQueryInformationProcess 读 PEB→RTL_USER_PROCESS_PARAMETERS→CommandLine;受阻则 unsupported+注释标注能力表降级。

**验证:** `CGO_ENABLED=0 GOOS=windows go build ./internal/platform/proc/` 退出码 0。

## T24: proc 收尾与调用面清理

**文件:** 删除 `internal/platform/proc/proc_other.go`;修改 `internal/runtime/sessions.go`、`internal/runtime/supervisor.go`
**依赖:** T21、T22、T23
**步骤:**
1. 删 proc_other.go。
2. sessions.go terminateIf 的 `os.FindProcess(pid)`+`process.Signal(syscall.SIGTERM)` 改 `os.FindProcess`+`proc.Terminate(process)`;删除 syscall import。
3. supervisor.go temporal/worker `cmd.Start()` 成功后调 `proc.AdoptChild(cmd)`(错误即失败启动,windows 关键、unix no-op)。

**验证:** `go test ./internal/runtime/... -count=1` 退出码 0;`grep -n syscall internal/runtime/*.go` 仅剩门禁允许的 SIGTERM 常量场景或无结果。

## T25: doctor 平台化

**文件:** `internal/runtime/doctor.go`、新建 `doctor_tools_linux.go`、`doctor_tools_darwin.go`、`doctor_tools_windows.go`
**依赖:** 无
**步骤:**
1. doctor.go:27 工具清单外移到 `doctorTools() []string`;三个 GOOS 文件各含 build tag:linux 沿用现清单(python3/kicad-cli/kicad/eeschema/Xvfb/xvfb-run/xprop/xwininfo/import);darwin:[python3, kicad-cli, kicad, eeschema];windows:同 darwin 名称(resolver 已处理 .exe)。
2. doctor 主体对沙箱能力 Probe 失败时透出 Probe 的 unsupported 原因文本(现状已有 Probe,确认 doctor 把原因带入结果而非笼统报错)。

**验证:** `go test ./internal/runtime/... -count=1` 退出码 0(单测驱动 Doctor() 断言 Linux 清单不变;不运行 stable CLI,遵守配置文件禁令);`CGO_ENABLED=0 GOOS={darwin,windows} go build ./internal/runtime/` 退出码 0。

## T26: S00 能力矩阵更新

**文件:** `docs/spec_docs/S00/platform-capability-matrix.md`
**依赖:** T2–T25 完成
**步骤:**
1. 第 2 节矩阵 C01–C05 的 macOS/Windows 状态由「未评估(计划评估)」更新为实际结果(支持/支持(降级)/待真实环境验证),注明「开发构建级,真实 OS 验收待阶段 5」。
2. 第 3 节 C01–C05 各行末尾追加 S02 简短状态与证据位置(新包文件路径、契约测试名);不重写历史结论。
3. 追加「第 8 节 S02 位置更新」表(类似第 7 节,记录新位置:owner 检查→secfile、路径→paths/user、flock→lock_unix、/proc→proc_unix 等)。

**验证:** `git diff docs/spec_docs/S00/` 确认 C06–C10 与历史小节未改动。

## T27: 真实 OS 待验清单

**文件:** 新建 `docs/spec_docs/S02/real-os-acceptance.md`
**依赖:** T26
**步骤:**
1. 列出只能在真实 Windows/macOS 验证的项:三平台 CLI/TUI 启动、重启/崩溃恢复、命名管道连通与 ACL 拒绝、LockFileEx 独占、Job Object 级联回收、ACL 私密文件生效、darwin 组停止、doctor 输出;每项写「操作→期望」。
2. 标注执行时机(阶段 5 CI/用户机器)与负责人占位。

**验证:** `git ls-files docs/spec_docs/S02/` 输出五份文档。

## T28: 汇合全量验证

**文件:** 无(验证任务)
**依赖:** T15、T19、T20、T24、T25、T27
**步骤:** 运行 `make test` 与 `make platform-check`;资源紧张时先查 `free -h` 与 memory PSI 再执行,失败定位修复后重跑。
**验证:** 两命令退出码 0,输出留存 `/tmp/s02-final-*.log`。

## 执行顺序

```
T1 ──→ T2 ──→ T3 ──→ T4
T5 ──→ T6 ──→ T7 ──→ T8 ──→ T9  ┐
                        T10 ├─(并行)→ T15 ─┐
                        T11 │              │
                        T12 │              ├─→ T28
                        T13 │        T26 ──┤
                        T14 ┘        T27 ──┘
T16 ──→ T17 ──→ T18 ──→ T19 ──────────────┐
T20(依赖 T5,T7) ─────────────────────────┤
T21 ──→ T22 ┐                             │
        T23 ┴──→ T24 ──→ T25(仅依赖自身) ─┘
```

**可执行的最大并行批次**(批次内任务互不改同一文件):

1. 批次 1:T1、T5、T16、T17、T18、T21(六条独立线同时开)
2. 批次 2:T2、T6、T20(依赖 T5 后半)、T22、T23、T25
3. 批次 3:T3、T7、T24(依赖 T21–T23 完成)
4. 批次 4:T4、T8、T19
5. 批次 5:T9、T10、T11、T12、T13、T14(六个迁移并行,文件互不相交)
6. 批次 6:T15、T26
7. 批次 7:T27 → T28(汇合门禁)
