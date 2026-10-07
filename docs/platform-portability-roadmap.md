# 操作系统机制与功能逻辑解耦路线

> **系列状态（2026-10-07）：S00–S05 已完成。** 本系列目标是把操作系统机制隔离在平台边界，使功能逻辑不直接依赖 OS API；Linux 是当前实现与验收基线。macOS/Windows 后端支持、实机发布和支持承诺不属于本系列完成条件，未来若要支持须另行立项。

## 结论

本路线原始盘点指出运行时、文件安全、进程生命周期和产品打包直接依赖 Linux/Unix 机制。S00–S05 已完成平台边界抽取、能力接口与 Linux adapter、Linux 安全工作流和发布闭环。当前可交付形态为 Ubuntu 26.04 x86_64；macOS/Windows 的部分契约或编译路径不代表实机支持，也没有本系列的支持承诺。

## 初始实现盘点（S00 基线）

| 领域 | 当前状态 | 代表位置 |
| --- | --- | --- |
| 用户目录与配置 | `ConfigPath`、`StateDir` 默认使用 XDG 与 `$HOME/.config`、`$HOME/.local/state`；通用配置代码检查 Unix 文件模式和当前用户归属。TUI 另有硬编码的 `~/.config` 路径。 | `internal/appconfig/config.go`、`internal/appconfig/owner_unix.go`、`internal/tui/model.go` |
| 包布局与运行时 | 可执行文件、libexec 和 share 的布局按 `bin/libexec/share` 推导；可执行文件名、socket 文件名和日志路径也在运行时代码中拼接。 | `internal/runtime/paths.go`、`internal/runtime/supervisor.go` |
| IPC 与单实例锁 | supervisor、会话服务和网络代理直接使用 Unix domain socket；socket 权限用 `chmod` 设置，单实例锁用 `syscall.Flock`。 | `internal/runtime/supervisor.go`、`internal/conversation/service.go`、`internal/conversation/client.go`、`internal/sandbox/network.go` |
| 子进程与桌面会话 | 进程组、`Pdeathsig`、POSIX signal、`/proc` 查询和 `kill` 写在运行时/沙箱逻辑中；会话清理还特指 KiCad `eeschema` 与 Xvfb。 | `internal/runtime/supervisor.go`、`internal/runtime/sessions.go`、`internal/sandbox/session.go`、`internal/sandbox/process_linux.go` |
| 安全文件操作 | 候选文件读取、ERC 报告读取和目录交换直接调用 `golang.org/x/sys/unix`；包含 `openat2`、`O_NOFOLLOW` 和 Linux `RENAME_EXCHANGE`。其中目录交换承担候选验收的原子性。 | `internal/candidate/workspace.go`、`internal/candidate/erc_checker.go`、`internal/candidate/accept.go` |
| 沙箱与网络隔离 | Linux 后端依赖 bubblewrap、mount/PID/network namespace 和 Linux guest 路径。`network_other.go` 只明确拒绝非 Linux 的 loopback 能力；应用入口仍直接构造 `sandbox.LinuxManager{}`。 | `internal/sandbox/linux.go`、`internal/sandbox/process_linux.go`、`internal/sandbox/network_other.go`、`cmd/agentworker/main.go`、`internal/runtime/supervisor.go` |
| KiCad 与工具检测 | `doctor` 固定查找 Linux 工具集（包括 Xvfb、`xprop`、`xwininfo`、`import`）；GUI 启动、无头检查和资源路径带有 Linux 假设。 | `internal/runtime/doctor.go`、`internal/runtime/sessions.go`、`internal/candidate/erc_checker.go` |
| 构建与安装 | 发布包、Temporal CLI 归档和安装脚本固定 Linux x86_64；当前 `make package` 只调用 Linux 打包脚本。 | `scripts/package_linux.sh`、`scripts/install_linux.sh`、`Makefile`、`docs/install-linux.md` |
| SQLite | 当前驱动是 `github.com/mattn/go-sqlite3`，它依赖 CGO；不同目标平台的构建机和 C 工具链需要纳入发布方案。 | `go.mod`、`internal/store/sqlite.go` |

因此工作不只是给现有代码增加 `GOOS` 分支。macOS/Windows 需要各自的进程、IPC、文件权限、安全路径、沙箱和桌面工具后端；有些 Linux 安全原语没有一对一替代，必须保留其安全保证或显式关闭依赖该能力的功能。

## 目标边界与必须保留的语义

把操作系统依赖放在组合入口和平台适配器中，由 `cmd/*` 或 runtime composition root 选择实现；核心工作流只依赖能力接口，不直接判断 `runtime.GOOS`、调用 `syscall` 或拼接平台路径。可在 `internal/platform` 或相邻的 ports/adapters 包中逐步建立这些边界：

- **路径与安装布局**：解析配置、状态、缓存、资源和子程序位置；支持显式环境变量覆盖。
- **本机 IPC 与单实例**：提供受当前用户保护的 listener/client 和锁实现。Windows 命名管道 ACL、macOS/Linux Unix socket 权限应满足相同访问边界；不可使用无认证的本机 TCP 作为等价替代。
- **进程托管**：启动、健康检查、停止及清理子进程树；隐藏 PID 探测、进程组、Job Object、signal 等差异。
- **安全文件系统**：在指定根目录内解析/读写文件，防止路径穿越、符号链接或 Windows reparse point 越界，并按各平台提供私密状态存储。
- **候选目录事务**：定义准备、交换、恢复的崩溃一致性契约；缺少等价原子交换能力的平台必须采用经验证的事务/回滚方案，不能静默退化成普通覆盖。
- **隔离执行与外部工具**：统一沙箱、网络授权和能力探测接口；平台实现各自证明隔离强度，能力不可用时 fail closed，并向用户说明受限功能。
- **工具链与桌面集成**：按平台发现 Python、KiCad CLI/GUI、截图及显示后端；把 headless checker 和交互 GUI 会话的能力分开声明。

安全边界必须保持可测试：候选执行不能写入正式项目或未授权路径；私密配置和凭证不能因平台权限模型变化而扩大可读范围；进程停止不能误杀其他用户或不相关实例；验收/恢复不能暴露半写入的项目状态。

## 分阶段实施

### 阶段 0：定义支持范围和能力契约

1. 明确当前功能基线和平台能力状态；此处的能力表用于记录实现与证据，不要求每个平台都实现或获得支持声明。
2. 建立平台能力表：路径、IPC、锁、进程树、私密文件、安全根目录访问、原子候选验收、网络隔离、无头 KiCad、交互 GUI。
3. 为上述接口写清安全不变量和“能力不可用”行为；把需要逐平台审查的等价性列为发布门槛。

**完成标志：** 每项功能对每个平台都有“支持/不支持/降级”的明确状态和可执行验收条件；Linux 当前行为作为回归基线。

### 阶段 1：抽取边界，保留 Linux 行为

1. 从 `appconfig`、`runtime`、`conversation/service`、`candidate` 和 `execution` 中抽出路径、IPC、锁、进程托管、安全文件和 sandbox 接口。
2. 在入口集中构造依赖，移除业务包里直接 `LinuxManager{}` 的创建；现有 Linux 实现迁入 Linux adapter，行为和权限保持不变。
3. 把平台专属 syscall 文件加上明确 build tags；为尚未实现的 OS 提供编译期 stub 或清晰的 unsupported 能力返回。
4. 将系统工具和子程序定位从固定文件名/目录拼接迁入 resolver，集中处理 `.exe`、包内资源和用户安装布局。

**完成标志：** Linux 发布和既有运行流程不变；核心工作流不再直接依赖平台 syscall；Windows/macOS 的源码编译问题有清单且逐项关闭。

### 阶段 2：目录、身份、IPC 与运行时生命周期

1. 用平台路径策略替代 XDG-only 默认值，并统一 `appconfig` 与 TUI 的配置/技能目录来源。
2. 把 `chmod`/Unix owner 检查改成安全存储接口：POSIX 使用权限位，Windows 使用当前用户 ACL；错误信息说明实际修复方式。
3. 用平台 IPC 与单实例锁实现替代 socket/`Flock` 直调用，并维护相同的当前用户访问边界。
4. 用进程托管器替代 `/proc`、`syscall.Signal(0)` 和 POSIX 进程组逻辑；为 Windows 子进程树使用 Job Object 等平台机制。
5. `doctor` 按平台能力探测运行组件，运行状态和关闭流程不再假定 Linux 进程信息格式。

**完成标志：** 功能逻辑通过平台边界使用 OS 能力；Linux 行为保持可运行并通过回归；没有实现的平台可明确返回 unsupported。此标志不要求所有平台都能启动 CLI/TUI。

### 阶段 3：安全文件访问与候选验收事务

1. 将 `openat2`、`O_NOFOLLOW`、Unix `Stat`/设备号检查封装到安全文件适配器，明确 POSIX symlink 与 Windows symlink/reparse point 的检查规则。
2. 为候选目录创建、清单读取、ERC 报告读取统一根目录约束，覆盖路径穿越、链接替换、并发修改和跨卷场景。
3. 替换 `RENAME_EXCHANGE` 的单平台实现时，保留重启可恢复的 journal；对不能原子交换目录的平台实现并验证分阶段提交/回滚协议。

**完成标志：** Linux 候选验收与恢复语义通过故障注入；尚无经验证等价实现的平台明确保持 unsupported，不会静默使用非原子覆盖。其他平台若未来立项支持，须在该立项中完成对应故障注入。

### 阶段 4：Linux 沙箱、网络与 KiCad 工作流（S04）

1. Linux adapter 继续使用现有 namespace/bwrap 约束并作为隔离强度基线。
2. 将网络授权代理、子进程会话和只读/可写挂载语义置于可替换边界；不能仅以“命令能运行”视为沙箱完成。
3. 拆分 headless KiCad checker 与 GUI 会话能力，让工具发现、GUI 启动、截图/显示能力可由平台 adapter 提供。
4. 当前没有实现的 adapter 明确返回 unsupported，并让 `doctor` 与 UI 解释原因；无需为完成本系列补齐 macOS/Windows 后端。

**完成标志：** Linux 的隔离逃逸、未经授权网络访问、项目写入边界、超时/取消和 GUI 生命周期有针对性验收；其他平台能力不可用时 fail closed。

### 阶段 5：Linux 构建、发布与范围声明（S05）

1. 为当前 Linux adapter 构建版本化安装包与校验和。
2. 在 Ubuntu 26.04 x86_64 runner 验证构建、安装生命周期和既有 Linux 工作流，并上传 CI artifact。
3. 更新安装说明、README 和能力矩阵，声明 Linux 发布包验收范围；不把其他平台列为支持或本系列待办。

**完成标志：** Linux CI 产物、安装升级/卸载和支持文档一致；未实现的 adapter 保持明确不可用。

## 系列收尾与未来范围

S00–S05 已按依赖顺序完成。系列验收关注 OS 依赖是否通过 adapter 隔离、Linux backend 是否保持安全语义、未实现能力是否 fail closed，以及 Linux 发布是否可复验。后续如需提供 macOS/Windows 产品支持，应作为新目标单独澄清工具链、隔离强度、真实 runner 和发布门槛；当前没有此项承诺或未完成任务。

当前 Linux 安装方式见 [install-linux.md](install-linux.md)。macOS/Windows 的代码契约或交叉编译状态仅作实现记录，不代表支持；是否建设这些平台由未来单独决策。
