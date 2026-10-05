# Windows、macOS 与 Linux 支持的解耦路线

## 结论

当前实现**尚未系统地解耦应用逻辑与操作系统相关部分**。项目已把部分 Linux 沙箱代码放在带 build tag 的文件中，也有 `core`、`conversation`、`decision` 等职责相对清楚的包；但运行时、文件安全、进程生命周期和产品打包仍直接依赖 Linux/Unix 机制。现有可交付形态是 Linux x86_64 CLI/TUI，不能据此认为 macOS 或 Windows 已得到支持。

## 当前实现盘点

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

1. 先明确 Windows、macOS、Linux 的目标版本、架构、CLI/TUI 支持范围，以及 KiCad 自动检查和 GUI 工作流是否都要求首发支持。
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

**完成标志：** 三个平台都可从开发构建启动 CLI/TUI，runtime 可重复启动/关闭和崩溃恢复；IPC、配置私密性与进程清理通过各平台定向验收。发行安装包在阶段 5 完成。

### 阶段 3：安全文件访问与候选验收事务

1. 将 `openat2`、`O_NOFOLLOW`、Unix `Stat`/设备号检查封装到安全文件适配器，明确 POSIX symlink 与 Windows symlink/reparse point 的检查规则。
2. 为候选目录创建、清单读取、ERC 报告读取统一根目录约束，覆盖路径穿越、链接替换、并发修改和跨卷场景。
3. 替换 `RENAME_EXCHANGE` 的单平台实现时，保留重启可恢复的 journal；对不能原子交换目录的平台实现并验证分阶段提交/回滚协议。

**完成标志：** 故障注入覆盖交换前、交换中、交换后崩溃；任何平台都不会静默使用非原子覆盖来降低验收安全性。

### 阶段 4：逐平台沙箱、网络与 KiCad 工作流

1. Linux adapter 继续使用现有 namespace/bwrap 约束并作为隔离强度基线。
2. 分别设计并审查 macOS 与 Windows 的隔离后端、网络授权代理、子进程会话和只读/可写挂载语义；不能仅以“命令能运行”视为沙箱完成。
3. 拆分 headless KiCad checker 与 GUI 会话能力，适配各平台的 KiCad 安装布局、GUI 启动、截图/显示能力和依赖检测。
4. 若某个平台暂时无法满足威胁模型，明确禁用需要该边界的执行/验收能力，并让 `doctor` 与 UI 解释原因。

**完成标志：** 每个平台的隔离逃逸、未经授权网络访问、项目写入边界、超时/取消、GUI 生命周期均有针对性验收；安全能力未就绪的平台不会开放受保护功能。

### 阶段 5：构建、发布与支持声明

1. 将 Linux 专用打包逻辑拆成平台产物构建步骤；为 Windows/macOS 选择对应 Temporal CLI 发行物或明确替代部署方式，并解决 SQLite CGO 工具链问题。
2. 建立 Linux、macOS、Windows CI：至少做编译/单元检查；目标系统 runner 上验证安装、启动关闭、重启恢复、IPC、文件安全和候选验收。
3. 生成独立安装包及校验和，更新安装说明、`doctor` 提示、README 和支持矩阵；只对完成阶段验收的平台标注“支持”。

**完成标志：** CI 产物、安装升级/卸载和支持文档一致；每个平台的声明与实际具备的功能、隔离等级相符。

## 建议顺序与优先级

依次完成 **阶段 0 → 1 → 2 → 3 → 4 → 5**。阶段 2/3/4 可在接口稳定后按依赖并行推进，但平台安全设计与验收应由同一负责人端到端闭环。沙箱和候选验收事务是安全关键路径，优先于打包美化或 UI 平台分支；不要先发布一个“能编译、但隔离或目录事务语义变弱”的版本。

当前 Linux 安装方式见 [install-linux.md](install-linux.md)。本路线是基于当前源码的解耦规划，不代表 Windows/macOS 已支持，也未通过跨平台构建验证。
