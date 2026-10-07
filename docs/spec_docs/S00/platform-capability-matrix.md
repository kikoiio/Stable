# Stable 平台能力表(S00 交付物)

> 本文档是平台可移植性路线图(docs/platform-portability-roadmap.md)阶段 0 的唯一交付物:定义支持范围、逐能力记录各平台状态、安全不变量与可执行验收条件。阶段 1–5 以本文档的验收条件编号为门槛引用源。撰写依据:docs/spec_docs/S00/ 四件套(已批准)。
>
> 撰写与验证环境:2026-10-06,Ubuntu linux/amd64,go1.26.0。标注「已执行」的验证均为本机实测;手工类验收条件涉及启动运行时/加载用户模型配置,按用户级安全边界约定不在本阶段自动执行,留作各平台验收时的人工条目。

## 1. 支持范围声明

**当前受支持的平台:Linux(x86_64,CLI/TUI)。** 本表对 Linux 列给出的是经代码审计与可运行验证确认的当前真实行为。S00–S05 解耦系列已结束；矩阵中的非 Linux 状态用于说明实现与证据边界，不代表后续必须实现或支持这些平台。

**S05 Linux 发布包基线**:版本化 tar.gz 仅在 Ubuntu 26.04 x86_64 runner 上构建和包级验收。本轮证据不声明其他 Linux 发行版/架构的发布包支持；macOS/Windows 仍未做真实环境验收，但这不构成本系列待办或支持承诺。

**macOS 与 Windows:未评估(无支持承诺)。** 本系列不要求其后端实现、实机验收或支持声明。矩阵中的等价机制线索只解释平台差异，不构成排期或产品承诺。

**「支持」的判定依据**:某平台对某能力行声明「支持」,必须满足该行验收条件的全部条目;声明「降级」必须满足标注〔降级〕的条目子集,且不得静默降低安全语义。平台整体声明「支持」还必须通过第 4 节发布门槛清单。KiCad 规则:无头 ERC 检查是任何平台声明「支持」的前提;交互式 GUI 会话允许以「降级」状态存在。

## 2. 矩阵总览

| 能力行 | Linux | macOS | Windows |
|--------|-------|-------|---------|
| C01 路径与安装布局 | 支持 | 待真实环境验证 | 待真实环境验证 |
| C02 本机 IPC | 支持 | 待真实环境验证 | 待真实环境验证 |
| C03 单实例锁 | 支持 | 待真实环境验证 | 待真实环境验证 |
| C04 进程树托管 | 支持 | 降级(待真实环境验证) | 降级(待真实环境验证) |
| C05 私密文件与安全存储 | 支持 | 待真实环境验证 | 待真实环境验证 |
| C06 安全根目录访问 | 支持 | 契约已实现(待真实环境验证) | 契约已实现(待真实环境验证) |
| C07 候选目录事务(原子验收) | 支持 | journaled-move 契约已实现(待真实环境验证) | journaled-move 契约已实现(待真实环境验证) |
| C08 网络隔离(沙箱执行与网络授权) | 支持 | 未评估(无支持承诺) | 未评估(无支持承诺) |
| C09 无头 KiCad 检查 | 支持 | 未评估(无支持承诺) | 未评估(无支持承诺) |
| C10 交互式 GUI 会话 | 支持(可降级) | 未评估(无支持承诺) | 未评估(无支持承诺) |

「支持」指当前真实行为已实现且有证据;各行的已知缺口、现状缺失与接线问题如实记录于对应小节与第 6 节,不因「支持」状态而隐去。

## 3. 逐行能力详述

每行小节统一字段:范围界定 / Linux 状态与证据 / macOS、Windows 状态 / 等价机制线索 / 安全不变量 / 能力不可用行为 / 验收条件 / 边界与矩阵外备注。

状态取值:`支持 / 降级 / 不支持 / 未评估(无支持承诺)`;Linux 列不使用「未评估」。
证据类型:`可运行验证`(附命令,2026-10-06 本机已执行)/ `仅代码审阅`(附原因)。
验收条件编号:`AC-<域>-<序号>`;域缩写:C01=PATH、C02=IPC、C03=LOCK、C04=PROC、C05=SECRET、C06=ROOT、C07=CAND、C08=NET、C09=KCAD、C10=GUI。编号一经使用不重命名。声明「支持」须满足该行全部条目;声明「降级」须满足标注〔降级〕的子集。

### C01 路径与安装布局

**范围界定**:含——配置/状态/用户技能目录解析(XDG 默认、$HOME 回退、STABLE_CONFIG/STABLE_STATE_DIR 等环境变量覆盖)、bin/libexec/share 安装布局推导、libexec 子程序与 share 资源定位、TUI 用户目录来源;不含——socket/锁权限与通信语义(C02/C03)、配置文件权限位与归属检查(C05)。

**Linux 状态:支持**——XDG 默认、$HOME 回退与环境变量覆盖均有实现与单元测试;布局推导与打包/安装脚本闭环且经 EvalSymlinks 兼容 symlink 安装。已知不统一:TUI 用户命令目录硬编码 `~/.config` 不尊重 XDG_CONFIG_HOME(技能目录已 XDG 感知),属阶段 2 已规划工作。

**证据**:
- `internal/appconfig/config.go` — `ConfigPath()`(62–75 行):STABLE_CONFIG 覆盖 → XDG_CONFIG_HOME → `$HOME/.config`,拼 `stable/config.json`;`StateDir()`(93–106 行):STABLE_STATE_DIR → XDG_STATE_HOME → `$HOME/.local/state`;`Load()` 依次应用 STABLE_PROVIDER/MODEL/BASE_URL 等 env(173–178 行 env 始终优先于配置文件 state_dir);`Init()`(244–269 行)以 0700 目录 + O_EXCL 0600 模板创建;`UserSkillsDir()`(81–91 行)XDG 感知且不受 STABLE_CONFIG 重定位影响。
- `internal/runtime/paths.go` — `Resolve()`(16–48 行):`os.Executable()` → EvalSymlinks → Bin/Root/Libexec/Share;State 取 abs 且 `State=="/"` 显式拒绝(34–36 行);State 下拼 goals/、state.db、temporal.db、control.sock、chat.sock、supervisor.lock 与日志;`Prepare()` 仅建 goals 0700。
- `internal/tui/model.go` — `userCommandsDir()`(237–243 行)硬编码 `~/.config/stable/commands` 不读 XDG;同文件 `userSkillsDir()`(227–233 行)走 appconfig.UserSkillsDir()——同文件两种来源;460 行帮助文案固定 `~/.config` 字样。
- `cmd/stable/chatserve.go` — `chatserveHelperPath()`(151–176 行)独立实现第二套布局推导(libexec → dev-install → 同级三候选)。
- `scripts/package_linux.sh` / `scripts/install_linux.sh` — 发布包创建 bin/libexec/share/licenses,安装到 `$HOME/.local/opt/stable` 并 symlink 到 `~/.local/bin/stable`,与 Resolve 推导闭环。

**证据类型与已执行验证**:可运行验证——`go test ./internal/appconfig/...`(ok,覆盖 XDG 默认/回退/STABLE_* 覆盖/TestUserSkillsDir* 三条链)、`go test ./internal/runtime/...`(ok)。仅代码审阅——`runtime.Resolve` 布局推导本身无 paths_test.go,可运行性由 doctor/install 间接覆盖;`make install-dev` 布局验收为手工条目(会覆盖本机安装,不自动执行)。

**macOS 状态**:未评估(无支持承诺)。**Windows 状态**:未评估(无支持承诺)。

**等价机制线索**(未经验证,不构成承诺):macOS——惯例基线为 `~/Library/Application Support`、`~/Library/Caches`、`~/Library/Logs`,.app bundle 下布局为 Contents/MacOS、Contents/Resources,布局推导需换锚点;Unix 技术手段(os.Executable/EvalSymlinks)可用。Windows——应改用 os.UserConfigDir(%AppData%)/os.UserCacheDir(%LocalAppData%),点目录非标准;bin/libexec/share 无惯例,需安装器自定义。

**安全不变量**:
1. 环境变量覆盖路径(STABLE_CONFIG/STABLE_STATE_DIR)不得绕过私密性与归属检查——无论路径指向哪里,Load() 对文件与父目录的 0600/0700/uid 检查必须原样适用。
2. 布局推导必须从可信锚点(os.Executable+EvalSymlinks)出发,子程序只允许从推导出的 Libexec/既定候选解析,不得回退 PATH 搜索或公共可写目录;State 解析为 `/` 必须拒绝。
3. 路径解析失败不得回退到全局可写位置或其他用户目录;HOME 不可解析时配置/状态域必须报错终止。

**能力不可用行为**:现状——HOME 不可解析时 ConfigPath/StateDir/UserSkillsDir 返回 error,CLI 拒绝启动;TUI 用户目录失败收敛为「空用户贡献」并提示位置;libexec 子程序缺失时 doctor 报 missing、运行期以错误终止;State=="/" 显式报错。现状缺失——路径域来源不统一(如 TUI commands 不尊重 XDG)无用户可见说明。

**验收条件**:
- AC-PATH-1:`go test ./internal/appconfig/...` → 全过(已执行,ok)。
- AC-PATH-2〔降级〕:HOME 不可解析/用户目录缺失 → TUI 仍可启动、内置命令可用、用户技能与命令为空且提示目录位置;不崩溃、不回退公共可写目录(手工)。
- AC-PATH-3:STABLE_STATE_DIR=/ 启动 → 显式报错拒绝继续(手工)。
- AC-PATH-4〔降级〕:libexec 子程序缺失 → doctor 报 missing、运行期错误终止,不静默(手工)。
- AC-PATH-5:`make install-dev`(或 package+test-package)后 doctor → libexec/share 资源全部 found,symlink 布局推导正确(手工,涉及覆盖本机安装)。

**边界与矩阵外备注**:布局推导双轨并存(runtime.Resolve 与 chatserveHelperPath)是阶段 1 resolver 收敛点;应用级缓存目录不存在(XDG_CACHE_HOME 仅为沙箱内 KiCad 设置);doctor 等处 Join 元素内嵌 `/` 子路径字符串;`owner_unix.go` 无 build tag 阻塞 appconfig 包非 Unix 编译(实测见 V11,编译面归本域、权限细节归 C05)。

### C02 本机 IPC

**范围界定**:含——supervisor 控制通道(control.sock)、会话服务通道(chat.sock)、沙箱网络代理通道(proxy.sock)、沙箱会话控制 socket(session.sock)四类 UDS listener/client 及客户端发现与拨号;不含——Temporal 的 127.0.0.1 TCP(见备注)、沙箱内 HTTP CONNECT/SOCKS5 listener 的授权语义(C08)。

**Linux 状态:支持**——四类 UDS 通道均以 0600 socket + 0700 父目录实现当前用户访问边界,服务端/客户端协议有可运行测试。边界纯靠文件权限,无对等凭证认证(能连接即视为当前用户本人)。

**证据**:
- `internal/runtime/paths.go` — control.sock/chat.sock/supervisor.lock 全在 State 目录(0700)。
- `internal/runtime/supervisor.go` — Supervise() 先 os.Remove 清陈旧 socket(150),net.Listen("unix")(156)后 os.Chmod(…, 0600)(163);Control() 500ms DialTimeout 发 newline JSON "status"/"down"(49–62);waitChatSocket 轮询 dial 作就绪探测(405–424)。
- `internal/conversation/service.go` — Serve() 删既有 SocketPath(102)、listen(105)后 chmod 0600,chmod 失败即关 listener 返回错误(109–112);acceptLoop 每连接一个 goroutine。
- `internal/conversation/client.go` — openStream()/Request() 用 2s DialContext("unix"),失败统一报 "session service is not reachable; start the runtime with stable"(61–68, 167–171)。
- `internal/sandbox/network.go` — NetworkProxy.Serve() 要求绝对 socket 路径、已存在即报错不抢占(92–95),listen 后 chmod 0600(106),退出时 os.Lstat+os.SameFile 校验后才删除(111–118)。
- `internal/sandbox/process_linux.go` — 代理 socket 放 MkdirTemp(".stable-network-")+chmod 0700 私有目录(84–91),只读挂载进沙箱 /run/stable-network(102)。
- `internal/sandbox/session.go` — createSessionControlSocket() 用 MkdirTemp(".stable-session-control-")+0700 规避 sun_path 108 字节限制,session.sock listen+chmod 0600(306–334)。
- 认证面:全仓 grep 无 SO_PEERCRED/ucred/SCM_CREDENTIALS/token 握手——边界仅由 socket 0600+目录 0700 构成。

**证据类型与已执行验证**:可运行验证——`go test ./internal/conversation/ -run 'TestSessionProtocolClientLifecycle|TestConcurrentSessionRunsKeepSessionAndRunOwnership' -count=1`(ok,真实 UDS 全链路)、`go test ./internal/sandbox/ -count=1`(ok,含 TestNetworkProxyHTTPConnectAndSOCKS5,未授权目标 403)。仅代码审阅/手工——socket 权限位 ls -l 目检与跨用户 connect 抽验需运行时环境(涉及加载用户配置,不自动执行)。

**macOS 状态**:未评估(无支持承诺)。**Windows 状态**:未评估(无支持承诺)。

**等价机制线索**(未经验证,不构成承诺):Windows——命名管道 + 当前用户 SID ACL(路线图已点名;Win10+ 有 AF_UNIX 但权限模型不同);macOS——UDS + 0600/0700 语义与 POSIX 相近,可考虑 launchd socket activation;macOS sun_path 为 104 字节,session.go 短路径假设需复核。

**安全不变量**:
1. 所有本机 IPC endpoint 必须落在仅当前用户可进入的目录内,socket 节点对其他用户不可连接;禁止以无认证的本机 TCP 作为等价替代。
2. 能连接 IPC 通道的主体等同当前用户;通道不得把请求转发到超出该会话已授权范围的资源(proxy 仅 dial 已 pin 的 grant)。
3. socket 清理/重建必须做 SameFile 类校验或路径独占,防止删除或复用被替换的 socket 路径。

**能力不可用行为**:现状 fail closed——客户端 dial 失败即报 "session service is not reachable…"/"runtime is not running" 并退出,不静默换传输;NetworkProxy socket 路径已存在时报错拒启。现状缺失——chmod 发生在 listen 之后,存在 socket 节点按 umask 默认权限短暂存在的窗口,无缓解无告警;无「IPC 不可用原因」的 doctor 级诊断。

**验收条件**:
- AC-IPC-1:`go test ./internal/conversation/ -run TestSessionProtocolClientLifecycle` → 通过(已执行,ok)。
- AC-IPC-2:`go test ./internal/sandbox/ -run TestNetworkProxyHTTPConnectAndSOCKS5` → 通过且未授权目标被拒(已执行,ok)。
- AC-IPC-3〔降级〕:runtime 未启动时 `stable chat` → 明确报错并以非零退出,不静默退化(手工)。
- AC-IPC-4:`stable up` 后 State 目录中 control.sock/chat.sock 为 0600、State 目录 0700(手工)。
- AC-IPC-5:`stable runtime status`(未运行)→ 报 not running,无副作用(手工)。
- AC-IPC-6〔降级〕:以另一普通用户身份 connect control.sock → 被拒(可选抽验,需第二账号)。

**边界与矩阵外备注**:Temporal dev server 走 127.0.0.1 无认证 TCP(supervisor.go 151–176),属本机进程间通信但未入本行,阶段 2 需归类;沙箱内 local proxy 的 127.0.0.1:0 listener 与本行强耦合;session.go 写死 Linux sun_path 108 字节假设。

### C03 单实例锁

**范围界定**:含——supervisor 运行时单实例锁(supervisor.lock + syscall.Flock)、持有/释放/等待/幂等启动;不含——chatserve 独立 dev 入口(显式无锁)、TUI 多实例(仅客户端)、Temporal 端口占用检查(辅助手段)。

**Linux 状态:支持**——Flock(LOCK_EX) 为内核管理的进程级锁,进程崩溃时内核自动释放,天然无陈旧锁问题;启动有 20s 重试等待与 status 幂等出口。缺口:无任何自动化测试覆盖,证据为代码审阅。

**证据**:
- `internal/runtime/supervisor.go` — `Supervise()` 以 os.OpenFile(p.Lock, O_CREATE|O_RDWR, 0600) 打开锁文件(123–127);syscall.Flock(fd, LOCK_EX|LOCK_NB) 非阻塞抢锁(130–132),失败循环重试至 20s deadline,期间每 100ms Control(p,"status") 探测,已运行则幂等 return nil,超时报 "runtime already starting or running";拿锁后 defer Flock(LOCK_UN)(143),持有贯穿整个 supervise 生命周期;拿锁后再做一次 status 双检(144–146)。
- `internal/runtime/paths.go` — `p.Lock = State/supervisor.lock`(42),锁文件 0600 与 socket 同在 0700 State 目录;崩溃后锁文件残留但无锁语义,下次启动复用。
- `cmd/stable/chatserve.go` — 独立运行会话服务的 dev 入口,不取任何锁(31–34 注释自认)。
- 反证:全仓 grep `syscall.Flock` 仅 supervisor.go:132/143 两处。

**证据类型与已执行验证**:仅代码审阅——Supervise 依赖完整运行环境(libexec/temporal、agentworker),单元测试无法轻量承载,锁逻辑无独立抽象可注入;手工验证序列(两次 up 幂等、kill -9 后 up、down 后 up、ls -l 锁文件)需运行时且涉及加载用户配置,不自动执行。

**macOS 状态**:未评估(无支持承诺)。**Windows 状态**:未评估(无支持承诺)。

**等价机制线索**(未经验证,不构成承诺):Windows——命名 mutex(CreateMutex,进程死亡自动 abandoned)或 LockFileEx;macOS——Darwin 亦有 flock/LOCK_EX,语义相近可沿用,未验证。

**安全不变量**:
1. 单实例锁必须在进程死亡时由内核/平台机制自动释放,不得引入需人工清理的陈旧锁状态。
2. 锁文件及其所在目录不得对其他用户可写(防本地用户预置/替换锁文件干扰实例判定)。

**能力不可用行为**:现状 fail closed——20s 拿不到锁则报 "runtime already starting or running" 并以错误退出,不静默双开;等待期间探测到已运行实例则幂等返回。现状缺失——拿锁超时不告知持有者 PID 或诊断指引。

**验收条件**:
- AC-LOCK-1〔降级〕:锁被另一活进程持有时 `stable up` → 最多等 20s 报 "runtime already starting or running",非零退出(手工)。
- AC-LOCK-2〔降级〕:`kill -9` supervise 后立即 `stable up` → 20s 内成功(内核自动释放,无陈旧锁)(手工)。
- AC-LOCK-3:`stable down` → 立即 `stable up` 成功(正常释放路径)(手工)。
- AC-LOCK-4:锁文件模式 0600 且属主当前用户(手工 ls -l)。

**边界与矩阵外备注**:单实例实为三重机制组合——Flock(权威互斥)+ control.sock status 探测(Up 幂等判定)+ Temporal 端口 dial;Up 把「socket 可连」视为「已运行」,socket 文件被同用户第三方预置则误判(边界内已知风险);chatserve 多开无保护。

### C04 进程树托管

**范围界定**:含——子进程启动的进程组与父死信号(Setpgid/Pdeathsig)、健康检查(零信号探测+/proc 状态+TCP/日志/socket 就绪)、停止与清理(SIGTERM→SIGKILL 升级、整进程组终止)、跨调用 GUI 残留清理(eeschema/Xvfb,宿主与 guest 两侧)、误杀防护;不含——沙箱隔离强度(C08)、IPC/锁(C02/C03)、KiCad 工具探测(C09)。

**Linux 状态:支持**——启动、健康检查、整树停止、跨调用清理四条路径均已实现且有可运行测试;沙箱命令与会话均以独立进程组+组级 SIGTERM→SIGKILL 终止,GUI 残留经 /proc cmdline 前缀校验后终止。两处已知弱点(见现状)为记录项,不构成降级。

**证据**:
- `internal/sandbox/session.go` — StartIsolatedSession 以 `SysProcAttr{Setpgid: true, Pdeathsig: SIGKILL}` 启动会话 bwrap(L118);stopManagedSession 对 -pid 进程组 SIGTERM→2s→SIGKILL,再 2s 观察退出,失败显式报错(L272–307);启动期四条失败路径(退出/握手失败/30s 就绪超时/ctx 取消)均整组停止并摘除(L146–182);ValidateSession 按 generation+candidateID 拒绝 stale 句柄(L257–270)。
- `internal/sandbox/process_linux.go` — 一次性沙箱命令设 Setpgid(无 Pdeathsig,L112);runProcessGroup 在 ctx 取消(默认 2 分钟超时)时整组 SIGTERM→2s→SIGKILL 并等待 Wait(L136–159);网络代理 helper 缺失返回 ErrUnavailable fail closed(L77–79)。
- `internal/runtime/supervisor.go` — processAlive 用 Signal(syscall.Signal(0)) 零信号+/proc/<pid>/stat 非 Z 双重判活(L426–436);stopProcess 对 temporal/worker 仅 SIGTERM→5s→Kill,不设进程组、只杀直接子进程(L437–450);waitPort(TCP)/waitReady(日志)就绪探测与秒级 watchdog(L202–251)。
- `internal/runtime/sessions.go` — StopSessionProcesses 从 SQLite 读 runtime_handle,terminateIf 先读 /proc/<pid>/cmdline 做 NUL 归一后的前缀精确匹配("eeschema <design>"/"Xvfb <display> "),design 必须为 State 内绝对路径,pid<=1 拒绝,通过才 SIGTERM(仅 SIGTERM,无升级)(L28–85)。
- `workers/computer/bridge.py` — guest 侧以 Popen 启动 Xvfb(-nolisten tcp)与 eeschema 并写 runtime_handle(L174–200);pid_alive 用 /proc stat 非 Z+cmdline(L32–38);stop() SIGTERM 两进程;clear_owned_lock 等待 eeschema 退出后删 KiCad `~<name>.lck` 锁文件。
- `cmd/stable/main.go` / `cmd/stable/oneshot.go` — down/demo 路径在 runtime 停止后调用 StopSessionProcesses 清理跨调用 GUI 残留并打印计数。
- `tests/e2e/m03_computer_session_test.go` — 会话停止后 pgrep 断言无 Xvfb/eeschema 残留、旧 generation handle 被拒、正式工程 digest 不变(L83–198)。

**证据类型与已执行验证**:可运行验证——`go test ./internal/sandbox/ -count=1`(ok,bwrap 实跑,含 TestRunProcessGroupStopsDescendantsOnContextCancellation 组级终止)、`go test ./internal/runtime/...`(ok);`go test ./internal/runtime -run '^TestStopSessionProcessesIgnoresUnrelatedPID$' -count=1`(2026-10-07 PASS,伪造 SQLite runtime_handle 指向无关 sleep 进程后未发送信号)、`python3 -m unittest workers/computer/test_candidate.py`(2026-10-07 PASS,7 项，含进程身份不匹配及非自有锁保护);M03 computer session e2e(2026-10-06 PASS)覆盖真实会话清理、stale 与 formal digest 不变。原“误杀防护无自动测试”缺口已关闭。

**macOS 状态**:未评估(无支持承诺)。**Windows 状态**:未评估(无支持承诺)。

**等价机制线索**(未经验证,不构成承诺):macOS——无 /proc,判活用 kill(pid,0) 或 libproc;killpg 可用但无 Pdeathsig,父死联动需 kqueue(EV_PROCID)或看门狗;僵尸判定改 waitpid/WNOHANG。Windows——Job Object(JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE+TerminateJobObject)同时覆盖 Pdeathsig 缺位;健康检查用 OpenProcess+GetExitCodeProcess;cmdline 前缀校验改 QueryFullProcessImageName。

**安全不变量**:
1. 进程终止只作用于 Stable 自身启动的进程:组级 kill 前必须先以 Setpgid 建立独立进程组;跨调用清理必须先经 /proc cmdline 前缀精确匹配且目标设计路径位于 State 内,pid<=1 拒绝;禁止按进程名全局杀。
2. 隔离能力不可用时 fail closed:沙箱 Probe 失败必须返回 ErrUnavailable 并拒绝启动,不得以无沙箱方式降级;停止失败必须显式上抛,不得静默当作已清理。

**能力不可用行为**:现状——Probe 失败返回 ErrUnavailable 不降级;启动中途失败立即整组停止;stopManagedSession 终止失败返回错误。现状缺失——supervisor 层 stopProcess 的 SIGKILL 失败与 StopSessionProcesses 的 SIGTERM 失败(信号错误被 `_ =` 忽略)均静默,无「清理未完成」的用户可见说明;应然为写入日志/doctor 并说明残留与手动处置方式。

**验收条件**:
- AC-PROC-1:`go test ./internal/sandbox/ -run '^TestRunProcessGroupStopsDescendantsOnContextCancellation$' -count=1` → 通过(已执行,V6 全包 ok)。
- AC-PROC-2:`go test ./internal/runtime/... ./internal/sandbox/... -count=1` → 通过(已执行)。
- AC-PROC-3〔降级〕:`python3 -m unittest discover -s workers/computer` → 通过(已执行,OK)。
- AC-PROC-4:`stable down` → 报 stopped N leftover GUI session process(es) 计数如实(手工)。
- AC-PROC-5:`bash tests/e2e/m03_computer_session.sh`(需沙箱+GUI)→ 停止后 pgrep 无残留、stale handle 被拒、formal digest 不变(已执行,2026-10-06,PASS)。
- AC-PROC-6〔降级〕:向 computer_sessions 写入指向无关进程的伪造 runtime_handle 后 StopSessionProcesses → 无关进程不收到任何信号(`TestStopSessionProcessesIgnoresUnrelatedPID`,2026-10-07,PASS)。

**边界与矩阵外备注**:bwrap --die-with-parent 承担部分进程树清理责任,抽象进程托管契约时须显式化;guest 侧 bridge.py 是进程托管的另一半实现(Python),平台审计不能只看 Go;Pdeathsig 语义是「创建线程死亡」而非「父进程死亡」,靠 --die-with-parent 兜底(仅代码审阅);工具超时(600s)经 ctx 取消落到 runProcessGroup 组终止,抽象时需一并考虑;会话控制 socket 目录经 MkdirTemp 落 /tmp(路径差异归 C01)。

### C05 私密文件与安全存储

**范围界定**:含——模型配置文件(config.json,含 api_key)的读取准入检查(权限位/归属)、配置文件与目录创建权限、凭证进入日志/会话记录/快照清单前的剥离;不含——IPC socket 权限(C02)、状态目录非凭证数据的存储私密性(见备注)、项目/报告产物权限(0644 惯例,非本域)、OS keyring 集成(不存在)。

**Linux 状态:支持**——配置文件强制 0600(perm&0077==0)+目录 0700+UID 归属检查,违规即拒绝加载且错误信息含修复指令;写入 O_EXCL 0600/0700,umask 只可能收紧,无 group/other 可读窗口;有可运行测试覆盖拒绝路径。

**证据**:
- `internal/appconfig/config.go`(Load,114–127)— IsRegular + Perm()&0077!=0 即拒绝(group/other 任意位不可接受,0600/0400 可,0640/0644 不可);ownedByCurrentUser 校验 UID;再查直接父目录 perm&0077 与归属;违规返回错误不读内容,错误信息明示修复("config file must be private (chmod 600)" 等)。
- `internal/appconfig/config.go`(Init,244–269)— MkdirAll(0700) 后显式 Chmod(0700)(已存在宽权限目录会被收紧,只收紧不放宽);配置文件 O_WRONLY|O_CREATE|O_EXCL 0600;已存在文件原样返回,违规交给 Load 拒绝。
- `internal/appconfig/owner_unix.go`(8–11)— syscall.Stat_t 仅比较 Uid==os.Getuid();无 build tag,依赖 syscall.Stat_t,非 Unix 平台无法编译(实测 V11)。
- `internal/appconfig/config_test.go`(TestLoadAndValidate,10–48)— 0600 含 api_key 配置 Load 通过;chmod 0644 后 Load 必须报错;Summary() 不含 key 字面量。
- `cmd/stable/main.go`(66–69、123–126)— Load 失败直接退出,无「警告后继续」路径。
- `internal/conversation/run.go`(167–177)— redactRunCredential:凭证进入模型/会话日志前被 redact.Redact 剥离。
- `internal/runtime/supervisor.go`(254–261、361)— snapshotCredentials(apiKey) 把 API key 列入绝不进入快照清单的凭证表。

**证据类型与已执行验证**:可运行验证——`go test ./internal/appconfig/ -count=1`(ok,覆盖 0600 通过/0644 拒绝/Summary 脱敏)、`go test ./internal/redact/`(ok)、`go test ./internal/conversation/`(IPC 子集 ok;redact 相关断言含于包测试)。仅代码审阅/手工——`stable config init` 权限目检(隔离 XDG_CONFIG_HOME 下执行)涉及配置命令,按用户级安全边界留待人工。

**macOS 状态**:未评估(无支持承诺)。**Windows 状态**:未评估(无支持承诺)。

**等价机制线索**(未经验证,不构成承诺):macOS——POSIX 权限位与 Stat_t 归属语义与 Linux 相同,owner_unix.go 预期可复用;可选迁 Keychain。Windows——用户 profile 继承 ACL 默认排除其他本机用户;凭证可迁 Credential Manager/DPAPI;归属判定需 SID/ACL 检查替代 Stat_t.Uid(现文件在 Windows 无法编译,天然阻止静默弱化)。

**安全不变量**:
1. 模型配置与凭证在磁盘上的可读范围在任何平台都不得超出当前用户:存储文件及其目录对 group/other 必须零权限位;违规必须拒绝使用(fail closed),不得静默接受、不得自动放宽。
2. 凭证不得进入比配置文件保护更弱的持久化介质:日志、会话记录、快照清单写入前必须剥离凭证;平台引入新存储(如 OS 凭证库)可读边界不得宽于「仅当前用户」。
3. 归属校验不可缺失:私密存储必须绑定当前用户归属检查(POSIX UID/平台等价物);平台无法提供归属判定时必须视为能力不可用,不得跳过检查继续加载。

**能力不可用行为**:现状——权限或归属任一违规 → Load 返回错误,CLI 退出,错误信息说明修复方式;不存在「检查失败但用环境变量继续」的降级路径;归属机制缺失的平台无法编译该包,不存在「静默产出无检查构建」。现状缺失——状态目录内非凭证私密数据(会话日志、state.db)无独立 0600 级准入检查;应然为:平台无法提供「仅当前用户」目录边界时,禁用对应持久化功能并提示,而非以更宽权限静默落盘(阶段 2 接口化时覆盖)。

**验收条件**:
- AC-SECRET-1〔降级〕:`go test ./internal/appconfig/ -run TestLoadAndValidate` → 通过,含 0644 被拒断言(已执行,ok)。
- AC-SECRET-2〔降级〕:`go test ./internal/appconfig/...` → 全过(已执行)。
- AC-SECRET-3〔降级〕:审阅 Load 与 main.go 调用链 → 权限/归属违规为「拒绝加载并退出」,错误信息含修复指令,无静默接受路径(已审阅)。
- AC-SECRET-4:隔离 XDG_CONFIG_HOME 下 `stable config init` → 目录 0700、模板 0600;对已存在宽权限目录重跑 → 被收紧至 0700(手工;涉及配置命令,按安全边界留待人工)。
- AC-SECRET-5〔降级〕:`go test ./internal/redact/ ./internal/conversation/...` → 通过,证明凭证写入会话日志/快照清单前被剥离(redact 已执行;conversation 全包为后续复验项)。
- AC-SECRET-6(平台扩展):任何新平台私密存储实现 → 以非当前用户身份读取必须失败,且归属检查不可用时拒绝加载而非跳过。

**边界与矩阵外备注**:Load 只检查 config.json 与直接父目录,更上层目录不校验(0700 目录已阻断遍历,机制覆盖边界,阶段 2 明确);状态目录私密性弱于配置——Paths.Prepare 仅建 goals 0700,已存在目录不复查,state.db/chat.log 等文件权限由创建方默认决定(内容为对话记录等非凭证数据,凭证落盘前已剥离),建议阶段 2 一并接口化;全仓写文件默认 0644 是惯例,0600 完全靠 appconfig 显式保证,抽象安全存储接口时需防新私密文件沿用 0644;无任何 keyring/secret-service 集成。

### C06 安全根目录访问

**范围界定**:含——正式项目根(FormalRoot)与候选根(CandidateRoot)内文件的安全解析/读取(清单构建、diff 读取、快照 blob、回滚物化)、候选创建时根校验、ERC 报告主机侧读取、执行层工具参数根内约束与沙箱 profile 根校验;不含——目录交换原子性(C07)、沙箱内 guest IO 隔离(C08)、用户配置私密性(C05)。

**Linux 状态:支持**——根内读取统一经 CleanRelative 词法拒绝 + openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)+O_NOFOLLOW+fstat 常规文件校验,关键节点(冻结/评审/验收/ERC 前后)有摘要复核,openat2 不可用时错误上抛无静默回退(前提:Linux 内核 ≥ 5.6)。已澄清:openat2 仅存在于 workspace.go secureOpen;ERC 报告读取用末组件 O_NOFOLLOW+fstat+8MB 上限(报告路径服务自建);设备号检查仅承担同文件系统前置判断,防替换由逐次 openat2+摘要复核承担。

**S03 更新(2026-10-06)**：`internal/platform/secfile` 已提供跨平台 `Root`、根身份重新验证、同卷检查和普通文件读取契约。Linux 继续使用 openat2；Darwin 使用逐组件 openat/O_NOFOLLOW；Windows 使用句柄级 reparse point 与卷/文件身份检查。review、snapshot、ERC 报告读取均已接入该边界；Darwin/Windows 真实主机行为未由本系列验证；本系列不承诺其支持。

**证据**:
- `internal/candidate/workspace.go` `secureOpen`(213–238)— 根 fd(O_PATH|O_DIRECTORY)后 unix.Openat2(OpenHow{O_RDONLY|O_CLOEXEC|O_NOFOLLOW, RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS}),fstat 要求常规文件;RESOLVE_BENEATH 拒绝绝对路径与向上穿越,RESOLVE_NO_SYMLINKS 拒绝任何一级符号链接(含父目录替换与 /proc 魔法链接),O_NOFOLLOW 纵深防御;未用 RESOLVE_NO_XDEV(根内挂载点可跨设备,已知边界)。
- `internal/candidate/workspace.go` `CleanRelative`(36–45)— 词法拒绝绝对路径、NUL、`.`、`..`;`BuildManifest`(47–117)对根 Lstat 拒绝符号链接根,WalkDir 跳过 `.stable`,符号链接/非常规文件条目直接 ErrUnsafePath,每个文件经 secureOpen 读入做 SHA-256。
- `internal/candidate/workspace.go` `CreateCandidate`(119–193)— 候选 ID 拒绝 `/`/`\`/`.`/`..`;formalRoot 与 candidatesParent 均 EvalSymlinks 且要求解析结果等于原路径;unix.Stat 比较两者 Dev(同文件系统,C07 交换前置);目标目录 Lstat 不存在后 Mkdir 0700,copyFile 读侧 secureOpen、写侧 O_EXCL,失败回滚 RemoveAll。
- `internal/candidate/erc_checker.go`(148–165)— ERC 报告主机侧读取 O_RDONLY|O_CLOEXEC|O_NOFOLLOW + fstat 常规文件 + ≤8MB + LimitReader;报告路径服务自建(runRoot MkdirTemp 后 chmod 0700);L61–69/179–192 在 ERC 前后各做一次 BuildManifest 摘要比对,检查期间被改即整体失败。
- `internal/candidate/review.go` `readEntry`(205–227)— diff 读取走 secureOpen 并复核 fstat mode/size 与清单一致、内容 SHA-256 与条目一致;BuildReview 对 checker 返回路径再 CleanRelative 并要求落在 diff 集合内。
- `internal/candidate/snapshot.go` — NewSnapshotStore EvalSymlinks 项目根并拒绝 `.stable` 为符号链接;readManifest 拒绝 ID 含 `/`/`\`;Materialize 每条目 CleanRelative(`../escape` 拒绝)+ staging Lstat 真实目录 + 物化后 digest 比对;writeBlobLocked 从候选根读源走 secureOpen。
- `internal/candidate/rewind.go` — 回滚 staging 为候选根同级 `.rewind-staging-*`(同文件系统);GuardRewindTarget 拒绝正式项目根作为回滚目标。
- `internal/execution/tool_executor.go` / `sandbox_profile.go` — 工具参数经 CleanRelative;checkMappedPath 逐级 Lstat+EvalSymlinks 拒绝穿符号链接、pathWithinOrEqual 包含性检查,写根固定为候选根;profile 根校验真实目录、互不重叠。

**证据类型与已执行验证**:可运行验证——`go test ./internal/candidate/... -count=1`(ok,含 TestManifestPath 词法穿越、TestManifestRejectsSymlink 根内符号链接、TestMaterializeRejectsTraversalEntry 快照穿越、TestGuardRewindTargetRejectsFormalRoot、TestAcceptRejectsStalePreview、TestCandidateReviewRejectsWritesDuringChecker)。仅代码审阅——openat2 内核层语义无直接命中用例(现有测试在 WalkDir 层即拒绝符号链接;建议补 openat2 层逃逸用例,对应 AC-ROOT-3)。

**macOS 状态**:契约已实现，目标编译与 Linux 契约测试通过；真实文件系统行为未由本系列验证；不构成支持承诺。**Windows 状态**:契约已实现，目标编译与 Linux 契约测试通过；真实文件系统行为未由本系列验证；不构成支持承诺。

**等价机制线索**(未经验证,不构成承诺):macOS——无 openat2;逐组件 openat+O_NOFOLLOW+每级 fstat(dev/ino) 复核的 safe-open 循环;根身份用 st_dev/st_ino 比对。Windows——CreateFileW+FILE_FLAG_OPEN_REPARSE_POINT 拒绝 reparse point;GetFileInformationByHandle 比对卷序列号+File ID;路径规范化后前缀包含校验。旧内核(<5.6)——openat 逐目录下探+每级 fstat+O_NOFOLLOW 模拟。

**安全不变量**:
1. 正式项目根与候选根内的一切文件读取/复制必须经根目录约束解析(词法 CleanRelative+内核级 RESOLVE_BENEATH/NO_SYMLINKS+O_NOFOLLOW+常规文件 fstat),任一层失败即整体报错拒绝,绝不回退到无约束普通 open。
2. 候选执行的工具参数与沙箱 profile 只能落在授权根内:写目标只允许候选根,正式项目根永远不是候选/回滚/交换的写入目标;穿越与符号链接(含 reparse point 类等价物)一律拒绝。
3. 依赖根内状态的决策点(冻结、评审、验收交换前、ERC 前后)必须以全树摘要复核确认内容未变,不一致即失败,不得基于过期清单放行。

**能力不可用行为**:现状 fail closed——沙箱为 nil → "isolated KiCad checker is unavailable"/"isolation unavailable; execution refused";openat2 返回 ENOSYS/EINVAL(内核<5.6)→ secureOpen 报错使 BuildManifest/CreateCandidate/评审/验收全部失败,无降级回退;检查期间根被改 → 明确报错。现状缺失——doctor 无内核版本/openat2 探测,用户只会看到操作失败,缺「原因说明」。

**验收条件**:
- AC-ROOT-1:`go test ./internal/candidate/ -run 'TestManifestPath|TestManifestRejectsSymlink' -v` → 全 PASS(已执行,V4 包全过)。
- AC-ROOT-2:`go test ./internal/candidate/...` → 全 PASS,含快照穿越拒绝、正式根回滚目标拒绝、交换前过期预览拒绝、ERC 期间写入失效(已执行)。
- AC-ROOT-3:在候选根内构造指向根外的目录/文件符号链接,调用 BuildManifest/secureOpen → 返回 ErrUnsafePath 或 ELOOP/EAGAIN 类错误,绝不读到根外内容(需新增用例后归入)。
- AC-ROOT-4〔降级〕:目标平台无 openat2 等价物 → 依赖根约束的功能(候选创建/评审/验收/ERC)必须显式禁用并向用户说明,任何相关操作得到明确不可用错误,而非无约束普通 open 静默执行。
- AC-ROOT-5:候选创建→冻结→评审→验收全流程测试通过,根约束不阻断正常流(已执行,V4)。

**边界与矩阵外备注**:RESOLVE_NO_XDEV 未启用(根内挂载点可跨设备解析,需 root/已有 mount 能力,威胁模型内风险低,应记录);candidate 包整体依赖 x/sys/unix Linux-only API(Openat2/Renameat2/RENAME_EXCHANGE)且无 build tag,非 Linux 无法编译;execution/sandbox_profile.go 的根包容校验是同类机制,阶段 1 应与 secureOpen 一并收进安全文件/路径适配器,避免两套语义漂移;ERC 模板路径 /usr/share/kicad/template 属可信系统输入未纳入根约束(跨平台时需进平台工具链解析)。

### C07 候选目录事务(原子验收)

**范围界定**:含——候选目录准备(CreateCandidate,含同设备号前置与失败回滚)、冻结后的原子交换验收(AcceptCandidate/ExchangeProjectDir)、SQLite 验收 journal(acceptance_apply_journal)与重启 reconcile(ReconcileAcceptances)、快照回滚(SnapshotStore/StageRewind/SwapWithStaging/rewind journal);不含——候选内容生成与沙箱执行、ERC 检查本身(C09)、`.stable` 服务子树常规读写。

**Linux 状态:支持**——验收交换主线(原子 RENAME_EXCHANGE + 交换前持久化 journal + 基于 manifest digest 的崩溃分类恢复 + 冲突即 blocked)在代码与测试中完整闭环,无任何普通覆盖/复制降级路径。S03 将 rewind 同步流程接入同一事务 coordinator，并把 `.stable` 服务目录恢复纳入 acceptance 完成与恢复路径。

**S03 更新(2026-10-06)**：事务 coordinator 已统一 acceptance/rewind 调用链。Linux 使用 `atomic-exchange`；Darwin/Windows 明确使用带 rollback 路径和 `old_saved`/`target_installed` phase 的 `journaled-move`，能力不足、跨卷或摘要不一致时 fail-closed。journal schema 已迁移到 v12；真实 Darwin/Windows 崩溃恢复未由本系列验证；不构成支持承诺。

**证据**:
- `internal/candidate/accept.go` — ExchangeProjectDir 对两根 Lstat(拒绝非目录/符号链接)+unix.Stat 同设备号检查后,以 unix.Renameat2(AT_FDCWD,…,RENAME_EXCHANGE) 单次原子交换;AcceptCandidate 流程:journal 幂等检查 → validateAcceptance(状态 reviewed/frozen、决策身份与 digest 匹配、review digest 重算、normal/force 门禁)→ journal 落盘 → 交换前二次 manifest 复核(防 TOCTOU)→ 交换 → `.stable` 回移 → prepared→swapped → FinalizeAcceptance(receipt+状态机一个 SQLite 事务)。
- `internal/store/candidate.go` — SaveAcceptanceDecision 在一个 SQLite 事务内写决策与 acceptance_apply_journal(phase=prepared,old/new digest),先于任何文件系统变更;ReconcileAcceptances 按「磁盘 manifest digest 对照 journal old/new digest」分类四种崩溃结局(已换好→finalize、未换→补交换后 finalize、old==new 不可区分→视为完成、对不上→blockAcceptance 不再自动重试);FinalizeAcceptance 幂等。
- `internal/candidate/snapshot.go` — SnapshotStore 位于 `<project>/.stable/candidate-snapshots`(0700,内容寻址 blob+manifest,字节/条数配额与凭证脱敏);blob/manifest 临时文件+Sync+Rename 落盘;ValidateRestore 逐 blob 哈希校验;Materialize 落 staging 后 digest 比对,「正式项目树永远不是恢复目标」。
- `internal/candidate/rewind.go` — StageRewind 物化到候选根同盘兄弟目录 `.rewind-staging-<snapshotID>`;GuardRewindTarget 拒绝 formal 根;SwapWithStaging 复用同一 ExchangeProjectDir。
- `internal/store/rewind.go` — rewind journal 状态机(prepared/swapped/finalized/blocked);ReconcileRewinds 按磁盘 digest 分类结局;FinalizeRewind 单事务且门禁 status='ready'。
- `internal/conversation/snapshots.go` — rewindSnapshot 仅允许同 session 所有权、ready 未验收候选、无活动 run、无未完成 rewind、无 pending acceptance;session log 先记 RewindPending 意图;rewind 后删除该候选全部 candidate_reviews。
- 恢复触发点:cmd/agentworker/main.go:157、internal/runtime/supervisor.go:110/147、cmd/stable/chatserve.go:66 启动时调 ReconcileAcceptances;ReconcileRewinds 仅出现在测试(接线缺口)。
- 崩溃窗口测试:tests/e2e/m03_accept_restart_test.go(交换后崩溃→重开→两次 reconcile 幂等→receipt 恰 1 条)及 internal/store/candidate_test.go、rewind_test.go、workspace_test.go 各分类用例。

**证据类型与已执行验证**:可运行验证——`go test ./internal/candidate/... -count=1`(ok)、`go test ./internal/store/ -run 'TestReconcile' -count=1`(10/10 PASS,含交换前/交换后/冲突三类崩溃结局与 rewind 四用例)。仅代码审阅/未执行——tests/e2e 的 m03 验收与重启恢复 e2e(重,含真实沙箱与 SQLite 重开)未在本阶段执行。

**macOS 状态**:journaled-move 契约已实现，目标编译与 Linux 状态机测试通过；真实主机崩溃恢复未由本系列验证；不构成支持承诺。**Windows 状态**:journaled-move 契约已实现，目标编译与 Linux 状态机测试通过；真实主机崩溃恢复未由本系列验证；不构成支持承诺。

**等价机制线索**(未经验证,不构成承诺):macOS——无 renameat2(RENAME_EXCHANGE) 等价 syscall,方向为「journal + 两次 rename(formal→备份、staging→formal)+ 重启按 journal 收尾」的分阶段提交;Windows 10+ NT 内核支持 POSIX 语义重命名(FILE_RENAME_FLAG_POSIX_SEMANTICS)可替换目录但无 exchange 语义,大概率同需分阶段提交;两平台均须保留「磁盘 digest 对照 journal 分类结局」恢复模型;均须通过 AC-CAND-7 故障注入后才能声明等价。

**安全不变量**:
1. 正式项目树只允许经单次同文件系统原子交换改变;任何平台不允许静默退化成复制/普通覆盖——无法原子交换或 journal 不可用时验收必须拒绝(store 为 nil 时 AcceptCandidate 直接报 "acceptance journal is unavailable")。
2. 验收/恢复任何时刻不得暴露半写入状态:formal 根在任意崩溃点之后只能是完整旧内容或完整新内容;恢复方无法从 journal+磁盘唯一判定结局时必须 blocked 并向用户报错,不得猜测、不得出具 receipt。
3. rewind 与恢复永远不得以 formal 项目为交换目标(GuardRewindTarget);快照必须先经 blob 哈希验证与 staging digest 复核,再经同一原子交换落位。

**能力不可用行为**:现状 fail closed——journal 不可用→拒绝验收;跨文件系统/符号链接根/非目录→ExchangeProjectDir 报错并记 blocked;决策 ID 重用内容不同→明确报错;存在 prepared 决策无 receipt→"acceptance is pending reconciliation" 拒绝重复验收;恢复冲突→phase=blocked 保留原因;交换后 finalize 失败→"project exchanged; acceptance recovery required"(单向门,靠重启 reconcile 收尾);快照 blob 损坏→拒绝 rewind。现状缺失——ReconcileRewinds 未接入生产启动路径,「重启以 reconcile rewind」的提示实际无法兑现(见差异记录 D7);accept.go 注释承诺的 `.stable` 回移恢复不存在(D6)。

**验收条件**:
- AC-CAND-1:`go test ./internal/candidate/ -run 'TestAcceptGuardsAndAtomicExchange|TestAcceptRejectsStalePreview'` → 通过(已执行,V4 包全过)。
- AC-CAND-2:`go test ./internal/store/ -run TestReconcilePreparedAcceptanceBeforeExchange` → 交换前崩溃,重启补交换、receipt 恰 1 条、二次 reconcile 幂等(已执行,PASS)。
- AC-CAND-3:`go test ./internal/store/ -run TestReconcileAcceptanceAfterExchangeBeforeJournalUpdate` → 交换后崩溃不二次交换,formal=新内容,receipt 恰 1 条(已执行,PASS)。
- AC-CAND-4:`go test ./internal/store/ -run TestReconcileAcceptanceConflictBlocks` → journal 与磁盘 digest 不符 → blocked、无 receipt(已执行,PASS)。
- AC-CAND-5:`go test ./internal/candidate/ -run 'TestStageSwapAndCleanup|TestGuardRewindTargetRejectsFormalRoot'` → rewind 回到快照 digest 且 formal 根永不为目标(已执行,V4 包全过)。
- AC-CAND-6:`go test ./tests/e2e/ -run 'TestM03TrustedCandidateAcceptanceAndRestart|TestM03AcceptanceInterruptedAfterExchangeReopensSQLite'` → 验收幂等、formal 翻转、pending_reverification、重启恢复(未执行,重)。
- AC-CAND-7〔降级〕:无原子交换能力的平台,降级实现必须基于「经验证的分阶段提交/回滚协议 + 重启可恢复 journal」;故障注入须证明任意崩溃点后 formal 根要么完整旧、要么完整新,且必须显式声明降级(不得冒充支持)。

**边界与矩阵外备注**:CreateCandidate 强制候选父目录与 formal 同设备号、父目录不得穿符号链接、准备失败 defer+RemoveAll 回滚——「准备」阶段也是平台依赖点;快照存储依赖 `.stable` 子树被 BuildManifest 排除才在交换中幸存(隐式耦合);blob/manifest 有文件级 fsync 无目录 fsync,极端断电下 rename 落盘顺序无保证(恢复模型按 digest 分类可缓解);journal 持久化依赖 SQLite WAL。

### C08 网络隔离(沙箱执行与网络授权)

**范围界定**:含——bubblewrap 隔离后端的约束构造与校验(只读/可写挂载、PID/网络 namespace、proc/dev/tmpfs)、每 run Probe、网络授权 grant 数据模型与策略判定、host 侧可信代理 NetworkProxy、guest 侧 loopback 代理与代理助手包装、持久隔离会话的网络拒绝、非 Linux fail-closed 形态;不含——单实例锁(C03)、进程组终止细节(C04)、OpNetwork 审批 UI 流程(权限域)、无头 KiCad(C09)。

**Linux 状态:支持**——bubblewrap+mount/PID/network namespace 全链路存在且有可运行测试(默认全断网、授权目标经 pinning 代理可达、未授权端口被拒)。一次性 command、helper 和 bridge 已从可信 authority 接入 `SandboxProfile.NetworkGrants`;持久隔离会话仍明确拒绝网络 grant(能力收窄)。

**证据**:
- `internal/sandbox/linux.go`(//go:build linux)— args()(154–209)构造 `--die-with-parent --new-session --unshare-pid --unshare-net --clearenv --proc /proc --dev /dev --tmpfs /tmp --tmpfs /home`,project 根 --ro-bind 只读、candidate/run 根 --bind 可写、/usr /bin /lib /lib64 /etc/ssl 等按存在只读绑定、HOME=/tmp TMPDIR=/tmp PATH=/usr/bin:/bin;ValidateProfile(33–152)要求三根真实目录、互不重叠/不含符号链接,拒绝传 HOME/PATH/DISPLAY、键名含 KEY/TOKEN 的环境变量,限定只读挂载 guest 路径,有 grant 时必须提供代理助手文件挂载且每个 grant 为 tcp+host+port+pinned IP、无重复。
- `internal/sandbox/process_linux.go` — RunIsolated 先 Probe 再跑;有 grant 时在 run 根旁建 0700 私有目录,host 侧 NetworkProxy 监听 0600 unix socket 并把目录 ro 挂到 /run/stable-network,argv 包装为助手 `--stable-sandbox-proxy <guest socket> -- <原argv>`(76–106);Probe(184–221)两段探测:shell 断言 candidate 可写/project 不可写/host 哨兵不可见,python3 连 1.1.1.1:53 必须失败(确认 netns 无路由);探测前剥离 NetworkGrants。
- `internal/sandbox/network.go`(无 build tag)— PinNetworkGrant 一次解析并 pin IP 集;NetworkProxy.handle 逐连接精确匹配 grant(protocol/host/port,重复匹配报 ambiguous),每次拨号前 ValidateNetworkTarget 重查 DNS 解析仍等于 pinned IPs(否则 "network resolution changed; approval must be renewed"),未授权返回 "network destination is not authorized";socket 0600、拒绝已存在路径。
- `internal/sandbox/network_linux.go` / `network_other.go` — Linux 侧 enableLoopback 用 SIOCSIFFLAGS 把新 netns 内 lo 置 IFF_UP(仅私有 netns 内 loopback,宿主 loopback 不可达);!linux 侧同名函数直接返回 "isolated network proxy is supported only on Linux"。
- `internal/sandbox/process.go` — 平台中立接口 SandboxManager/SandboxProfile(含 NetworkGrants)/ErrUnavailable("Linux isolation is unavailable")。
- `internal/sandbox/session.go`(55–57)— 持久隔离会话带任何 NetworkGrant 即报错拒绝(fail closed)。
- `internal/permission/policy.go` — OpNetwork 未匹配 grant → deny "network destination is not explicitly authorized"(49–52);匹配且无 deny/ask → allow "explicit network grant"(107–109)。
- `internal/execution/sandbox_profile.go`、`tool_executor.go`、`python_bridge.go` — 一次性 command/helper/ERC/GUI bridge 从 authority 构造并 pin 网络 grant;持久 session profile 拒绝 grant;errors.Is(err, sandbox.ErrUnavailable) → ToolDenied "Error: isolation unavailable; execution refused"。
- `LinuxManager{}` 直接构造点(生产代码 6 处):cmd/agentworker/main.go:173、internal/runtime/supervisor.go:366 与 390、cmd/stable/chatserve.go:101 与 222、internal/dependency/service.go:46(dependency 为业务包内构造)。

**证据类型与已执行验证**:可运行验证——`go test ./internal/sandbox/ -count=1`(ok,bwrap 实跑,含 namespace/profile、网络 pinning、代理重验、session grant 拒绝与清理断言);`go test ./internal/execution/ -run 'Test.*(Command|Helper|Network|Isolation|PythonBridge|Session)' -count=1`(ok);`bash tests/e2e/m03_sandbox_network.sh` 与 `bash tests/e2e/m03_sandbox_secrets.sh`(均 PASS,默认断网/获批目标/未授权目标/DNS 变化及敏感标记扫描)。跨平台编译探针(2026-10-06 实测)——`CGO_ENABLED=0 GOOS=darwin go build ./...`、`CGO_ENABLED=0 GOOS=windows go build ./...`通过;运行时能力仍由非 Linux stub 明确返回 unsupported,本阶段不声明 macOS/Windows 支持。仅代码审阅/未执行——细粒度挂载白名单无独立测试,行为由 profile 契约测试与代码推断。

**macOS 状态**:未评估(无支持承诺)。**Windows 状态**:未评估(无支持承诺)。

**等价机制线索**(未经验证,不构成承诺):macOS——Seatbelt/sandbox-exec profile 拒绝 network\*、Endpoint Security 拦截审计;无 namespace 等价物,隔离强度须单独证明。Windows——AppContainer/受限 token + Job Object,WFP 按进程阻断网络;授权代理协议可复用(改命名管道传输)。「授权目标 pinning+每次拨号重验」语义平台无关(NetworkProxy 已平台中立,仅 enableLoopback 与 bwrap 依赖平台)。

**安全不变量**:
1. 默认拒绝:每次隔离执行必须在新 network namespace 中进行(--unshare-net 恒开,有 grant 也不改);任何 egress 只能经 host 侧可信代理,且每次拨号前重验 grant 精确匹配+pinned IP 与实时解析一致;未授权目标必须在对端不可达。
2. Fail closed:bwrap 缺失、根校验失败、助手/代理不可用、探测失败时,RunIsolated/StartIsolatedSession 必须返回 ErrUnavailable 类错误,调用方必须拒绝执行,禁止降级为非沙箱执行;探测失败不得在宿主留下工件。
3. 凭证边界:键名含 KEY/TOKEN、HOME/PATH/DISPLAY、STABLE_\* 会话环境变量一律不得进入沙箱;代理/控制 socket 以 0600/0700 私有目录存在并只读挂载;grant 必须绑定解析后的具体 IP(不认裸域名)。

**能力不可用行为**:现状 fail closed——非 Linux:整个应用因 LinuxManager 等未定义无法编译(实测),network_other 的运行时拒绝几乎是不可达兜底;Linux 无 bwrap:Probe → ErrUnavailable("bwrap not found"),工具链表现为 ToolDenied "isolation unavailable; execution refused";持久会话+grant:直接报错。现状缺失——「隔离不可用」无 doctor 级原因说明(失败原因仅散落在错误信息)。

**验收条件**:
- AC-NET-1:`go test ./internal/sandbox/ -run 'TestBubblewrapArgs|TestBubblewrapArgsWithApprovedNetworkProxy|TestBubblewrapArgsWithSessionControl'` → 通过;断言含 --unshare-pid --unshare-net --clearenv、project 仅 ro-bind、grant 存在时无 --share-net(已执行,V6 包全过)。〔降级〕bwrap 缺失环境允许 skip,但 skip 不得计为通过证据。
- AC-NET-2:`go test ./internal/sandbox/ -run 'TestNetworkGrantPinsExactResolvedTarget|TestTrustedProxyPinsAndRechecksBeforeDial|TestNetworkProxyHTTPConnectAndSOCKS5'` → 换 host/端口/DNS 变更均被拒且未触达拨号器;未授权 CONNECT 403(已执行,V6)。
- AC-NET-3:`bash tests/e2e/m03_sandbox_network.sh`(TestM03SandboxNetwork)→ 默认 profile DIRECT_BLOCKED、授权目标 APPROVED_CONNECTED、未授权端口不可达、解析变化拒绝(已执行,2026-10-06,PASS)。
- AC-NET-4:`go test ./internal/sandbox/ -run TestProbeAndNoHostFallback` → 有 bwrap 时 project 只读/candidate 可写/project 内容不变;无 bwrap 时探测失败且宿主无残留(已执行,V6 包全过)。
- AC-NET-5〔降级〕:任一平台沙箱能力不可用时 RunIsolated/StartIsolatedSession 返回包装 ErrUnavailable 的错误,执行链显示 "isolation unavailable; execution refused",无非沙箱回退(Linux 已验证;其他平台以编译失败+源码审阅为证)。
- AC-NET-6〔非 Linux〕:macOS/Windows 全仓目标编译通过(`CGO_ENABLED=0 GOOS=darwin/windows go build ./...`,2026-10-06);运行时 Linux sandbox 能力仍由 stub 返回 unsupported,不宣称非 Linux 支持。

**边界与矩阵外备注**:全仓 grep runtime.GOOS 零命中,平台选择完全依赖 build tags;代理助手可由 agentctl/agentworker 任一二进制充当(--stable-sandbox-proxy 受信包装入口两处),打包时助手定位是额外平台依赖点;supervisor 的 Flock(C03)与 session.go 的 syscall.Kill 进程组(C04)在沙箱组合根附近,平台边界抽取时应一并处理;authority → 一次性 profile 的 grant 接线由 execution 负责,持久 session 仍拒绝 grant。

### C09 无头 KiCad 检查

**范围界定**:含——候选验收链上的 ERC 检查(Go 侧 KicadERCChecker)、kicad-cli 版本探测与 sch erc 调用、ERC JSON 报告安全读取、依赖链 KiCad 桥(workers/kicad/erc.py)、doctor 对 python3/kicad-cli 的存在性检查;不含——GUI 会话/截图/窗口(C10)、DRC/PCB、判据决策。与 C10 共享 bwrap 沙箱栈、doctor 工具清单、KiCad 配置播种逻辑;kicad-cli 专属本行,Xvfb/eeschema/xwininfo/import 专属 C10。

**Linux 状态:支持**——版本探测→隔离执行→报告安全读取→violations 计数→验收门禁全链路已实现且有测试(前提:主机安装 kicad-cli 且位于沙箱 PATH(/usr/bin:/bin)内、/usr/share/kicad/template 存在、bwrap 可用;缺失时按 fail-closed 阻塞验收而非失效)。已知口径差:doctor 按主机 PATH 查 kicad-cli,执行时按沙箱固定 PATH 解析——doctor 报 OK 不等于沙箱内可用。

**证据**:
- `internal/candidate/erc_checker.go` — kicad-cli 只经 Sandbox.RunIsolated 调用;先 `kicad-cli version` 探测(空或超 128 字符拒绝),后 `kicad-cli sch erc --format json --severity-all --exit-code-violations --output <guest> <sch>`;退出码仅 0/5 合法。
- `internal/candidate/erc_checker.go`(148–166)— 报告 unix.Open(O_RDONLY|O_CLOEXEC|O_NOFOLLOW),常规文件且 ≤8MB;路径写 host 侧 RunRoot/check-\*/erc-<sha8>.json(supervisor.go:390 RunRoot=p.Goals),guest 侧 /workspace/run。
- `internal/candidate/erc_checker.go`(33–52)— seedKicadConfig 固定读 /usr/share/kicad/template/{sym-lib-table,fp-lib-table} 写入 runRoot/.kicad-config/kicad/9.0;模板缺失静默 continue(可致 lib_symbol_issues 误报)。
- `internal/sandbox/linux.go`(179、194)— bwrap --clearenv + --setenv PATH /usr/bin:/bin:沙箱内固定 PATH 解析,主机 PATH 不参与;ValidateProfile(80)禁传 PATH/DISPLAY。
- `internal/candidate/review.go`(82–96、126–138)— checker 报错包装为 FindingUnavailable 计入 Review digest;finding.Files 必须限于候选 diff(102–104);Finding 不保存报告路径,只存 Checker/Version/Result/Reason/Files。
- `internal/candidate/accept.go`(152–184)— 普通验收要求所有 finding==pass,unavailable 同样阻塞;force 需逐条显式确认且必须匹配非 pass finding。
- `internal/runtime/doctor.go`(22、26)— 固定文件检查(libexec/agentctl、agentworker、temporal、fixture、schemas、两个 bridge.py)+ exec.LookPath 主机 PATH 检查 python3、kicad-cli、kicad、eeschema、Xvfb、xvfb-run、xprop、xwininfo、import;缺失提示安装系统包;有缺失即非零退出(main.go:144–146)。
- `internal/dependency/collector.go`(31)+ `workers/kicad/erc.py`(38–68)— 桥不可用即报错;`kicad-cli version` 失败返回 'blocked',reason 'kicad-cli version unavailable; result not verifiable'(不猜版本)。

**证据类型与已执行验证**:可运行验证——`go test ./internal/candidate/... -count=1`(ok,含 ERC pass/fail、报告边界和候选写入失效断言);`python3 -m unittest discover -s workers/kicad`(OK);`bash tests/e2e/m03_kicad_candidate.sh`(TestM03KicadRepairRunsOnlyAgainstCandidateInLinuxSandbox,PASS);`go test ./internal/runtime -run 'Test.*Doctor|Test.*Missing' -count=1`(PASS)。另于 2026-10-07 以当前源码构建完整 CLI/helper 并在 `/tmp` 组装 fixtures/schemas/workers，使用临时 HOME/XDG 与空配置运行 `stable doctor`，退出码 0；隔离 sandbox、KiCad CLI/GUI、窗口/截图、模板及全部安装资源均逐项 OK，未读取用户配置。`make cases` 仍未执行,不作为本阶段通过证据。

**macOS 状态**:未评估(无支持承诺)。**Windows 状态**:未评估(无支持承诺)。

**等价机制线索**(未经验证,不构成承诺):macOS——KiCad .app 内 kicad-cli(/Applications/KiCad/KiCad.app/Contents/MacOS/),沙箱等价物 sandbox-exec/Seatbelt(无 bwrap);库表模板在 .app Resources,/usr/share/kicad/template 需按平台解析。Windows——`C:\Program Files\KiCad\<ver>\bin\kicad-cli.exe`,隔离可考虑 AppContainer/Windows Sandbox,需 .exe 解析与路径 resolver。

**安全不变量**:
1. ERC 必须在验证过的隔离沙箱内执行,Sandbox==nil 直接拒绝("isolated KiCad checker is unavailable"),不得退化为主机直跑 kicad-cli。
2. 检查前后 formal/candidate manifest digest 必须一致,否则拒绝结果;报告只落私有 run root,O_NOFOLLOW+8MB 上限。
3. 版本必须来自真实命令输出;finding.Files 必须限于候选 diff;证据不得越界。

**能力不可用行为**:现状 fail closed——bwrap 缺失 → ErrUnavailable → finding=unavailable → 普通验收被拒;沙箱 PATH 内无 kicad-cli → version 探测失败 → 同上(worker 侧 blocked+明确 reason);候选无变更 .kicad_sch → unavailable 阻塞普通验收;doctor 列 MISSING 并非零退出。现状缺失——doctor 主机 PATH 与沙箱固定 PATH 口径差(doctor 判 OK 的 kicad-cli 若不在 /usr|/bin 下沙箱内仍不可用;Probe 不验证 kicad-cli 沙箱可见性);应然为 doctor 按「沙箱内可见」探测。

**验收条件**:
- AC-KCAD-1:`go test ./internal/candidate/ -run TestKicadERCCheckerRecordsPassAndFail` → 通过,调用序列 version→sch erc,finding 带真实 Version 与受限 Files(已执行,V4 包全过)。
- AC-KCAD-2:`go test ./internal/candidate/ -run TestCandidateReviewRejectsWritesDuringChecker` → 检查期间篡改候选使 review 失败(已执行)。
- AC-KCAD-3:`python3 -m unittest discover -s workers/kicad` → OK(已执行;注:其中按名需真实 kicad-cli 的用例在本机已满足条件)。
- AC-KCAD-4〔降级〕:kicad-cli 缺失环境运行 review 流程 → finding=unavailable 且验收被阻,用户可见 reason,不静默(代码审阅+TestBridgeFailsClosedWithoutSandbox 佐证)。
- AC-KCAD-5:`bash tests/e2e/m03_kicad_candidate.sh`(需 bwrap+kicad-cli+eeschema)→ 真实 ERC 经沙箱完成,Version 非空非 not-run(已执行,2026-10-06,PASS)。
- AC-KCAD-6:`make cases`(需 kicad-cli)→ 场景用例通过(未执行,重)。

**边界与矩阵外备注**:KiCad 配置播种三处重复(Go erc_checker.go、workers/kicad/erc.py、workers/computer/bridge.py)全部硬编码 kicad/9.0 子目录与 /usr/share/kicad/template——KiCad 升版会静默失效,建议阶段 2/4 收敛为单一 resolver;CI(.github/workflows/go.yml)只跑 go build+test,Python bridge 契约仅本地触发;doctor 检查 kicad/xvfb-run/xprop 但运行时不用(超集检查,易误导支持判定)。

### C10 交互式 GUI 会话

**范围界定**:含——沙箱内持久 Xvfb 显示会话(ensure_open/observe/recover/stop 生命周期)、eeschema 启动与首跑对话框自动确认、窗口识别(xwininfo)、截图证据(ImageMagick import)、会话句柄持久化与 stale 检测、遗留进程清理、Unix socket 会话控制;不含——无头 ERC(C09)、真机显示器/Wayland、非 KiCad GUI。允许「降级」状态(用户已批决策)。

**Linux 状态:支持(可降级)**——Xvfb 启动→eeschema 开窗→截图→清理全链路代码完整,Go/Python 测试与 e2e 支撑;显示后端仅 Xvfb;截图工具缺失时会话降级为 stale/blocked(内置降级路径,符合「GUI 可降级」决策)。

**证据**:
- `workers/computer/bridge.py`(163–231 start)— Xvfb `:100..:299 -screen 0 1280x800x24 -nolisten tcp -extension GLX`(沙箱内 EGL 段错误故禁 GLX);/tmp/.X{n}-lock 竞争检查 + /tmp/.X11-unix/X{n} 就绪探测;Popen(['eeschema', <design>])。
- `workers/computer/bridge.py`(97–132)— import_tool 按序找 import-im7/im6 且必须 os.path.isfile(规避 /etc/alternatives symlink 不在沙箱挂载内);截图 `import -window root <run>/screenshots/…`;截图缺失 → status='stale' → ensure_open 返回 blocked(内置降级)。
- `workers/computer/bridge.py`(59–94、204–225)— 窗口识别 xwininfo -root -tree;首跑对话框用 ctypes libX11.so.6+libXtst.so.6 XTestFakeKeyEvent 发送 Enter。
- `workers/computer/bridge.py`(32–40、145–160)— 存活经 /proc stat(排除 Z)+cmdline;stop 为 SIGTERM eeschema/Xvfb;clear_owned_lock 仅删本 handle 记录的 `~<design>.lck`。
- `internal/sandbox/session.go`(54–183、272–307)— 持久会话 bwrap --die-with-parent --new-session --unshare-pid,Setpgid+Pdeathsig;host /tmp MkdirTemp(0700) 建 session.sock(0600) 挂载 /run/stable-session;30 秒 ready 握手;停止进程组 SIGTERM→2s→SIGKILL。
- `internal/sandbox/linux.go`(177–179)— --dir /tmp/.X11-unix(非 root Xvfb 不自建会退出);禁传 DISPLAY(80)。
- `internal/runtime/sessions.go`(28–85)— StopSessionProcesses /proc cmdline 前缀精确匹配才 SIGTERM(防 PID 重用与误杀);由 down/oneshot 调用,输出 "stopped %d leftover GUI session process(es)"。
- `internal/store/sqlite.go`(1109–1112)— runtime_handle 按世代取大 upsert,GUI 会话恢复唯一状态源。
- `internal/execution/python_bridge.go`(138–174)— computer.\* 强制走持久会话(候选绑定),请求失败即 StopIsolatedSession。
- `tests/e2e/m03_computer_session_test.go` — 断言 applied/open/generation=1/窗口身份非空/截图证据在 run root 内非空,停止后 observe 报 stale。

**证据类型与已执行验证**:可运行验证——`go test ./internal/execution/ -run 'TestComputerBridgeUsesPersistentCandidateBoundSession|TestBridgeFailsClosedWithoutSandbox' -count=1`(ok);`python3 -m unittest discover -s workers/computer`(OK,含进程身份不匹配和非自有锁保护);`go test ./internal/runtime -run '^TestStopSessionProcessesIgnoresUnrelatedPID$' -count=1`(PASS,无关 PID 未收到信号);`go test ./internal/sandbox/ -count=1`(ok,含 session control/generation);`bash tests/e2e/m03_computer_session.sh`(TestM03ComputerIsolatedSessionLifecycle,PASS,开窗/截图/generation/stop 无残留)。AC-GUI-3 import 缺失降级路径仍无独立自动化。

**macOS 状态**:未评估(无支持承诺)。**Windows 状态**:未评估(无支持承诺)。

**等价机制线索**(未经验证,不构成承诺):macOS——无 Xvfb 官方构建,可考虑 XQuartz 或原生窗口+screencapture;进程清理以 NSRunningProcess/terminate 替代 /proc+SIGTERM;libXtst 键击注入无直接等价物。Windows——VcXsrv 类 X server 或原生 UI 自动化+PrintWindow 截图;Job Object 替代进程组清理。

**安全不变量**:
1. GUI 会话全程处于 bwrap 沙箱(die-with-parent/Pdeathsig/进程组两级停止);DISPLAY 禁止由外部传入,显示由会话内 Xvfb 自建。
2. 清理不得误杀:Go 与 Python 两侧均须 cmdline 与记录的 design/display 精确匹配才 SIGTERM;锁文件只删自己 handle 记录的。
3. 截图等证据只能落在私有 run root(host 侧经 checkEvidencePath 校验,e2e 断言不逃逸)。

**能力不可用行为**:现状——Xvfb 启动失败 → RuntimeError → blocked(computer_exception),不回退主机显示;eeschema 12 秒未开窗 → 留 failed-<gen>.png 与 windows-<gen>.log 后 stop 会话 → blocked;import 缺失 → 会话可开但截图为空 → status=stale → ensure_open/observe 返回 blocked(降级可见,不静默);bwrap 缺失 → Probe 失败 → ErrOutcomeUnknown,工具调用被拒;doctor 列 MISSING。现状缺失——doctor 清单与真实刚需不对齐(xvfb-run/xprop/kicad 被检查但从未使用;libX11/libXtst .so 依赖无检查项)。

**验收条件**:
- AC-GUI-1:`go test ./internal/execution/ -run TestComputerBridgeUsesPersistentCandidateBoundSession` → computer.\* 复用持久候选绑定会话(已执行,ok)。
- AC-GUI-2:`python3 -m unittest discover -s workers/computer` → 观察绑定候选、formal 拒绝、协议一问一答(已执行,OK)。
- AC-GUI-3〔降级〕:import 缺失环境下会话观察 → status=stale、screenshot_path 空、结果 blocked,且 Xvfb/eeschema 仍被正常清理(无现成自动化;标仅代码审阅 bridge.py:121–132,或补测试)。
- AC-GUI-4〔降级〕:`stable down` → 遗留 eeschema/Xvfb 仅按 cmdline 前缀精确匹配被 SIGTERM,不触碰无关进程(手工;附代码审阅)。
- AC-GUI-5:`bash tests/e2e/m03_computer_session.sh`(需 Xvfb+eeschema+bwrap+import)→ 开窗、generation 递增、截图证据留在 run root、停止后 stale(已执行,2026-10-06,PASS)。

**边界与矩阵外备注**:ctypes dlopen libX11.so.6/libXtst.so.6 是隐式平台依赖,doctor 未覆盖;GUI 会话启动日志(Xvfb/eeschema/windows/failed-\*.png)写 run_root/computer-logs 属可观察性产物;会话控制 socket 的 sun_path 108 字节限制催生 host /tmp 短路径+挂载方案(m04_tools.sh:280 注明嵌套 TMPDIR 会触发),平台路径长度策略需纳入考虑。

## 4. 发布门槛清单

任何平台对任何能力行从「未评估/降级」变更为「支持」,或平台整体声明「支持」,必须先通过以下门槛:

- **G1 逐平台安全设计审查**:该行安全不变量(第 3 节各行 SI)逐条在该平台实现中核实成立;无法成立的,依赖该不变量的功能必须禁用并说明。
- **G2 隔离强度证明**:沙箱/网络隔离等价后端须通过针对性验收——隔离逃逸、未经授权网络访问、项目写入边界、超时/取消(AC-NET 全部条目 + 平台特化用例);「命令能运行」不视为隔离完成。
- **G3 候选事务崩溃一致性**:故障注入覆盖交换前、交换中、交换后崩溃,任意崩溃点后 formal 根只能是完整旧内容或完整新内容(AC-CAND-7 及平台化用例);不能原子交换的平台必须实现经验证的分阶段提交/回滚协议,禁止静默普通覆盖。
- **G4 进程清理边界**:进程停止不得误杀其他用户或不相关实例(AC-PROC-6 平台化);停止失败必须用户可见,不得静默。
- **G5 私密文件边界**:按平台权限模型核实「可读范围不超出当前用户」;归属/权限检查不可用时拒绝加载而非跳过(AC-SECRET-6)。
- **G6 IPC 访问边界**:与 Linux 相同的当前用户访问边界;不可使用无认证的本机 TCP 作为等价替代(AC-IPC 全部条目)。
- **G7 KiCad 规则**:无头 ERC 检查必须可用(AC-KCAD 全部条目)是平台声明「支持」的前提;交互式 GUI 会话允许以「降级」状态存在(满足 AC-GUI 〔降级〕子集并如实标注)。
- **G8 不可用可解释**:能力不可用时 doctor/UI 必须说明原因与受限功能(fail closed + 用户说明);本表各行「现状缺失」中列出的说明类缺口,在对应平台声明「支持」前必须补齐。
- **G9 未来平台决策**:本系列不安排 macOS/Windows 产品支持评估。若未来另行决定支持，须重新确定目标版本、实施范围和逐平台验收门槛；当前两平台维持「未评估(无支持承诺)」。

## 5. 完成标志对照

路线图「阶段 0」完成标志逐条对照:

| 完成标志 | 达成位置与证据 |
|----------|----------------|
| 每项功能对每个平台都有「支持/不支持/降级」的明确状态和可执行验收条件 | 第 2 节矩阵总览:10 行 × 3 平台状态齐全(Linux 全部「支持」,macOS/Windows 统一「未评估(无支持承诺)」);第 3 节每行验收条件共 54 条(AC-PATH-1…AC-GUI-5),每条「运行 X → 期望 Y」,已执行项均有本机实测结果,未执行项明确保留原因 |
| Linux 当前行为作为回归基线 | 第 3 节每行「Linux 状态」记录的是当前真实行为并区分可运行验证与代码审阅;受影响包、Linux e2e 和跨目标构建命令均记录实际结果,可随时重放 |

## 6. 审计差异记录

审计中发现的路线图盘点与代码实况不符或遗漏之处(路线图本身未修改;是否修订由用户决定):

- **D1(C01)**:盘点称「TUI 另有硬编码的 ~/.config 路径」——部分过时:TUI 用户技能目录已走 appconfig.UserSkillsDir()(XDG 感知),仅 userCommandsDir()(model.go:237–243)仍硬编码;实际问题是同文件内两种来源并存。
- **D2(C01/C05)**:`internal/appconfig/owner_unix.go` 无 build tag 且依赖 syscall.Stat_t,appconfig 包在非 Unix 平台直接无法编译(实测 GOOS=windows rc=1)——盘点「检查 Unix 文件模式」低估了编译期耦合强度。
- **D3(C03)**:盘点把 `internal/sandbox/network.go` 列为单实例锁代表位置——失实:syscall.Flock 唯一出现在 internal/runtime/supervisor.go(132/143);network.go 无锁实现,它是网络代理代码。
- **D4(C06)**:盘点「ERC 报告读取……包含 openat2、O_NOFOLLOW」易误读——openat2/RESOLVE_\* 仅在 workspace.go secureOpen;erc_checker.go 对报告只用末组件 O_NOFOLLOW+fstat+8MB 上限。阶段 3「为 ERC 报告读取统一根目录约束」是真实升级项而非已有能力搬迁。
- **D5(C06)**:盘点「Unix Stat/设备号检查」与 openat2 并列为安全原语——实况中设备号检查仅承担「同文件系统」前置判断(CreateCandidate/ExchangeProjectDir),无「fd 与目录身份比对防替换」用途;防替换由逐次 openat2+摘要复核承担。
- **D6(C07)**:accept.go:93–97 注释承诺崩溃后「acceptance recovery can still find them」(.stable 会话日志回移),但 ReconcileAcceptances 及所有启动恢复路径均无该逻辑——承诺的恢复机制不存在,崩溃窗口内会话 transcript 滞留候选目录。
- **D7(C07)**:`ReconcileRewinds`(internal/store/rewind.go)仅被测试调用,三个生产启动点只调 ReconcileAcceptances;rewindSnapshot 在存在未完成 rewind 时要求「重启 runtime 以 reconcile」,而重启并不 reconcile rewind——死锁式提示,未完成 rewind journal 无法经正常途径消除。
- **D8(C07)**:路线图阶段 3「替换 RENAME_EXCHANGE 的单平台实现时,保留重启可恢复的 journal」表述暗示 journal 尚不存在——实际已有完整 SQLite journal(acceptance_apply_journal、rewind_journal)+三处启动 reconcile,盘点偏保守。
- **D9(C08)**:「应用入口仍直接构造 sandbox.LinuxManager{}」属实但不止入口——生产代码共 6 处构造点,其中 internal/dependency/service.go:46 位于业务包;阶段 1 清单应以 6 处为准。
- **D10(C08)**:「network_other.go 只明确拒绝非 Linux 的 loopback 能力」字面属实但遗漏主形态:非 Linux 下整个应用因 LinuxManager 等未定义无法编译(实测 darwin/windows rc=1),network_other 的运行时拒绝几乎是不可达兜底——真正的 fail closed 发生在编译期。
- **D11(C08)**:网络授权「审批→执行」链路未接线——PinNetworkGrant/SandboxProfile.NetworkGrants 仅测试使用,生产代码从不产生 grant,OpNetwork 审批结果未接入沙箱 profile;代理与策略判定机制本身已落地并强制。
- **D12(C02)**:盘点漏列第 4 个 UDS endpoint(internal/sandbox/session.go 会话控制 socket)及其 Linux sun_path 108 字节假设;四处 socket chmod 均发生在 listen 之后,存在短暂 umask 权限窗口(盘点未提及)。
- **D13(C04)**:盘点把「进程组、Pdeathsig、POSIX signal、/proc 查询和 kill」笼统归为运行时/沙箱逻辑——实况分布不均:进程组+Pdeathsig+组级 kill 仅在沙箱路径;supervisor 的 temporal/worker 无独立进程组、只杀直接子进程;/proc 探测在 supervisor.go、sessions.go 与 workers/computer/bridge.py 三处实现互不相同。
- **D14(C09)**:「kicad-cli 检测」实为双轨:doctor 查主机 PATH、执行时按沙箱固定 PATH(/usr/bin:/bin)解析——存在 doctor 报 OK 而沙箱内不可用的口径差;另 doctor 检查 kicad/xvfb-run/xprop 但运行时从不使用(超集检查)。


## 7. S01 位置更新（不改写历史结论）

S01 把操作系统依赖抽到 `internal/platform/{paths,ipc,lock,proc,secfile,sandbox}`。Linux 各行状态仍为「支持」，未降级。下列是证据路径变更，供阶段 1 之后引用；第 3 节原文保留 S00 审计时的位置。

| 原位置（S00 审计） | S01 之后 |
| --- | --- |
| `internal/runtime/paths.go` Resolve/Prepare | `internal/platform/paths`（含 HelperBinary / HelperBinaryResolved；`chatserveHelperPath` 已删除） |
| supervisor/conversation/sandbox unix listen+chmod | `internal/platform/ipc.ListenPrivate` / `DialPrivate` |
| `internal/runtime/supervisor.go` syscall.Flock | `internal/platform/lock.TryAcquire`（20s 重试循环仍在 supervisor） |
| supervisor processAlive/stopProcess；sandbox Setpgid/Pdeathsig/组杀；sessions.go `/proc/.../cmdline` | `internal/platform/proc`（Alive / StopProcess / ConfigureChild / KillGroup / Cmdline） |
| candidate `secureOpen` / `unix.Open` / `Renameat2` 同设备检查 | `internal/platform/secfile`（SecureOpen / OpenNoFollow / Exchange / SameDevice） |
| `internal/sandbox` 整包 | `internal/platform/sandbox`；`New()` 为唯一构造入口 |
| 6 处 `sandbox.LinuxManager{}`（含 `internal/dependency/service.go`） | 0；cmd/stable、chatserve、agentworker 注入 `sandbox.New()` |
| `internal/appconfig/owner_unix.go` | `owner_linux.go` + `owner_other.go`（非 Linux Load fail-closed） |
| `internal/store` 空白导入 go-sqlite3 | `driver_linux.go` / `driver_other.go`；非 Linux `Open` 返回明确 unsupported |

## 8. S02 位置更新（保留 S00/S01 历史结论）

S02 将 C01–C05 的跨平台实现接入 `internal/platform`，并完成 Linux 回归与三向编译。macOS/Windows 的真实 ACL、命名管道、锁、进程托管和启动恢复尚无真实平台证据；可参考 `docs/spec_docs/S02/real-os-acceptance.md` 中的可选检查，但该清单不是本系列待办。

| 能力 | S02 实现位置 | 本机证据 |
| --- | --- | --- |
| C01 路径 | `internal/platform/paths/user_{linux,darwin,windows}.go`、`internal/appconfig/config.go` | paths/appconfig 单测；Windows/macOS `go build` |
| C02 IPC | `internal/platform/ipc/ipc_unix.go`、`ipc_windows.go`、`pipe_name.go` | IPC 契约单测；三向编译 |
| C03 锁 | `internal/platform/lock/lock_unix.go`、`lock_windows.go` | Linux runtime 测试；三向编译 |
| C04 进程 | `internal/platform/proc/proc_{linux,darwin,windows}.go` | Linux runtime 测试；三向编译；非 Linux 为降级待验 |
| C05 私密文件 | `internal/platform/secfile/private_{unix,windows}.go`、业务调用点 | secfile 权限单测、门禁、三向编译 |

可运行验证路径同步：`go test ./internal/sandbox/...` 现为 `go test ./internal/platform/sandbox/...`。S01 回放（2026-10-06）：appconfig TestLoadAndValidate、store TestReconcile、platform/sandbox Bubblewrap/进程组/网络代理/Probe、conversation TestSessionProtocolClientLifecycle、candidate 全包、execution computer bridge、redact、workers/kicad 与 workers/computer unittest 均通过。`make platform-check` 与 `CGO_ENABLED=0 GOOS=windows/darwin go build ./...` 退出码 0。

## 9. S05 Linux 发布验收更新（2026-10-07）

本节记录 S05 Linux 发布验收证据。S00–S05 解耦系列现已完成；这不构成 macOS/Windows 支持声明。

| 项目 | 实际结果 |
| --- | --- |
| 提交 | `31993ea`（推送分支 `s05`） |
| runner | GitHub Actions `ubuntu-26.04`；workflow 日志记录 `Image: ubuntu-26.04`。包名与 Linux x86_64 守卫均通过，产物为 `linux-amd64` |
| 源码 job | `build-and-test` 成功：`go mod verify`、`go build ./cmd/...`、`go test ./...` |
| 包 job | `test-package` 成功：`make test-package` 构建包并顺序通过 install、lifecycle、CLI、runtime/e2e、restart；生命周期日志包含首装/重装、升级/回退、无效包保护、入口切换回滚、卸载与数据保留 PASS |
| workflow run | [Go run 37568537248](https://github.com/kikoiio/Stable/actions/runs/37568537248)；`build-and-test` job 112621724440，`test-package` job 112621724171 |
| workflow artifact | `stable-linux-amd64-37568537248-1`，含版本化 tar.gz 和 `.sha256`；下载后 `sha256sum -c` 通过。归档 SHA-256：`f7b2453df8fe2a4b0b10396b162c98b39a658dadd40c1575fdf95fa598b58cf7` |
| 包版本/依赖 | 包级 `AC1 manifest PASS` 验证 Stable 版本 `0.1.0`、Temporal CLI `1.9.1`、包清单与 CLI；包名为 `stable-0.1.0-linux-amd64.tar.gz` |
| 发布渠道 | 未创建 GitHub Release；产物仅作为 workflow artifact 保存 |

因此，S05 的发布包验收范围是 Ubuntu 26.04 x86_64。其他 Linux 发行版/架构、macOS 与 Windows 未由本系列验收，也不是本系列未完成项；未来若需支持，应另行立项。


## 10. S 系列解耦工作收尾（2026-10-07）

S00–S05 已完成。完成定义是：OS 相关路径、IPC、锁、进程托管、安全文件、沙箱/KiCad 能力和发布机制经平台边界组织；Linux 实现与发布切片完成验收；未实现的平台能力明确不可用。该定义不要求所有平台都有后端，也不把 macOS/Windows 实机验收作为系列退出门槛。

矩阵中 macOS/Windows 的「未评估」「契约已实现」等状态仍应按各行证据理解，不表示支持或已承诺后续交付。其他 Linux 发行版与架构同样没有本系列的支持声明。
