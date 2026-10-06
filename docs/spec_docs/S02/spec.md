# S02 目录、身份、IPC 与运行时生命周期(阶段 2)Spec

> 状态:已批准(2026-10-06)。S02 = 平台可移植性路线图「阶段 2:目录、身份、IPC 与运行时生命周期」(依据 docs/platform-portability-roadmap.md 与 docs/spec_docs/S00/platform-capability-matrix.md)。用户已确认:S02 扩展 S01 建立的 internal/platform 六域包而非新建 ports 层;非 Linux 后端全量实现、以「三向编译绿 + 可在 Linux 运行的契约级单测」验收,真实 OS 行为验收单列待阶段 5;非 Linux 路径默认值采用平台惯例(macOS ~/Library/Application Support,Windows %APPDATA%/%LOCALAPPDATA%),Linux XDG 不变;doctor 仅平台化基础运行组件,KiCad/GUI 工具链发现留阶段 4;私密文件处理全量迁入安全存储接口。

## 背景

S01(阶段 1)已把操作系统依赖抽到 `internal/platform` 六域包(paths/ipc/lock/proc/secfile/sandbox),Linux 实现迁入 adapter,业务包平台 API 门禁零违例。但仍存在:

- 非 Linux 后端全是 fail-closed 的 unsupported stub:ipc(Unix socket)、lock(flock)、proc(/proc、进程组、Pdeathsig)在 windows/darwin 上只能编译、一运行就明确拒绝;
- 私密文件处理散落业务包:约 15 个文件 35 处 `os.Chmod(0700/0600)` 直调,Windows 上 os.Chmod 基本是空操作,私密性形同虚设;appconfig 的 owner 检查(owner_linux.go)也是 Linux-only;
- 路径默认值 XDG-only:appconfig 的配置/状态/技能目录只用 ~/.config、~/.local/state 推导;TUI 还自行硬编码拼接 ~/.config/stable/commands;
- doctor 与进程处理假定 Linux:doctor 硬编码 Xvfb/xprop 等 Linux 工具清单;sessions.go 仍直调 syscall.SIGTERM 与 /proc 格式。

路线图阶段 2 的目标是:三个平台都能从开发构建启动 CLI/TUI,IPC、配置私密性、进程清理在各平台成立。

## 目标

- 三平台(Linux/macOS/Windows)可从开发构建启动 CLI/TUI(发行包与安装器留阶段 5);
- 私密文件与 IPC 的当前用户访问边界在非 Linux 平台真实成立,而不是空操作或明确报错;
- Linux 现有行为逐项不变(回归基线);
- doctor、运行状态与关闭流程不再假定 Linux 进程信息格式。

## 功能需求

- F1 平台路径策略:配置、状态、技能、命令目录的默认值按平台惯例推导:Linux 保持 XDG(~/.config/stable、~/.local/state/stable)不变;macOS 用 ~/Library/Application Support/stable(配置与状态同基目录);Windows 配置用 %APPDATA%\stable、状态用 %LOCALAPPDATA%\stable。显式覆盖(STABLE_CONFIG、STABLE_STATE_DIR、XDG_CONFIG_HOME/XDG_STATE_HOME)语义不变:环境变量显式指定时各平台一致生效。TUI 不再自行拼接配置/命令/技能目录,统一从 appconfig 的来源取,技能/hooks 提示文案中的路径与实际来源一致。
- F2 安全存储接口:提供「私密目录/文件」的创建与权限收紧接口:POSIX 用权限位(行为与现状逐项一致),Windows 用当前用户 ACL(拒绝其他用户读取);appconfig 的 owner 检查迁入该接口。业务包所有 0700/0600 调用点(配置、会话日志、输入历史、todo、计划文件、执行临时目录、候选快照等约 15 个文件)全部迁移,业务包不再直接调用 os.Chmod 做私密化。权限或归属不满足时的错误信息说明实际修复方式(Linux 提示 chmod 600/归属检查,Windows 提示 icacls/ACL 修复)。
- F3 平台 IPC:监听/拨号接口在三平台真实可用:Linux/macOS 走 Unix domain socket(macOS socket 权限 0600 与 Linux 相同),Windows 走命名管道并施加当前用户 ACL。现有语义保持:removeStale(supervisor/会话服务先删陈旧 socket,网络代理不删)、拨号超时、路径来源统一由 paths 提供。
- F4 单实例锁:非阻塞独占锁接口在三平台真实可用:Linux/macOS 用 flock,Windows 用等价机制(独占打开/LockFileEx)。锁文件以私密方式创建;supervisor 现有的 20 秒重试循环与状态码语义不变。
- F5 进程托管:进程存活检测、停止(宽限期后强杀)、子进程属性配置、进程组停止、命令行读取在三平台真实可用:Windows 用 Job Object(子进程绑定 Job,父死亡即回收)与进程 API 替代 /proc;macOS 用 setpgid 进程组(组停止/组杀),父死亡信号(Pdeathsig)无等价原语,以进程组停止兜底并在能力表记录降级。sessions.go/supervisor.go 中残留的 syscall.SIGTERM 直调与 /proc 格式假定全部经进程托管接口。
- F6 doctor 平台化:工具发现按平台清单执行(Windows/macOS 查找 kicad-cli/python3 等名称及其平台后缀差异),X11 专属工具(Xvfb、xprop、xwininfo、import)仅 Linux 列入;进程状态查询走进程托管接口;对当前平台不可用的能力(如非 Linux 沙箱、GUI 会话)明确报告 unsupported 及原因,而不是误报缺失。

## 非功能需求

- N1 Linux 回归基线:路径、socket 权限、锁行为、进程停止语义、错误文案逐项与 S01 验收时一致;make test 全过;现有集成/端到端测试不改动断言即通过。
- N2 门禁延续:make platform-check 保持零违例,且随新代码扩展继续覆盖(业务包无 syscall/unix/GOOS 直调、无私密 chmod 直调)。
- N3 非 Linux 代码的验收方式(明确标注,不冒充已验证):本机只能做「三向编译绿(linux/windows/darwin)+ 可在 Linux 运行的接口契约测试(把平台无关逻辑拆成纯函数以便测试,如 ACL 描述符构造、路径推导)」。真实 OS 上的启动/重启恢复/IPC/ACL 行为验收单列为待阶段 5 CI/用户机器执行的清单,S02 内不声明已验证。
- N4 能力表同步:S00 能力矩阵 C01–C05 行(路径/IPC/锁/进程树/私密文件)按实际结果更新 macOS/Windows 状态(支持/降级/待真实环境验证),补充证据位置;不重写历史结论;C06–C10 不动(属阶段 3/4)。
- N5 文档落盘:S02 五份文档落 docs/spec_docs/S02/ 并加 .gitignore 白名单(同 S00/S01 方式),不影响其他忽略状态。

## 不做的事

- 发行与安装:打包脚本、安装器、校验和、CI 矩阵(阶段 5);
- Temporal CLI / worker 的非 Linux 发行物获取:S02 只保证平台正确的二进制定位与启动路径(resolver 已处理 .exe 后缀),各平台 Temporal CLI 归档的获取方式留阶段 5;
- 沙箱与网络隔离的非 Linux 后端:bubblewrap/namespace 等价物(阶段 4);非 Linux 上沙箱能力继续 fail-closed 并如实报告;
- KiCad 安装布局、GUI 会话、截图/显示能力发现(阶段 4);
- 候选验收事务的非原子平台回滚协议(阶段 3);secfile 的 openat2/RENAME_EXCHANGE 等安全文件原语本阶段不动;
- SQLite 驱动选型:维持 S01 的 stub 方案(非 Linux 打开 store 返回 unsupported),驱动选型留阶段 5。

## 验收标准

- AC1(对应 F1):三平台路径推导单测——Linux 默认值与现状逐字节相同(回归);macOS/Windows 默认值符合所选惯例;环境变量覆盖在各平台一致生效。TUI 中不再存在自拼的配置/命令/技能目录路径(验证:单测 + grep 门禁 + 双 GOOS 编译)。
- AC2(对应 F2):业务包 os.Chmod 私密化直调为 0(grep 门禁);Linux 上接口行为与现状一致(新建目录/文件权限位逐项不变、错误文案不变或更明确);Windows ACL 构造逻辑有可在 Linux 运行的契约单测(验证:make test + 门禁 + 定向单测)。
- AC3(对应 F3):ipc 接口三向编译通过;Linux 上现有 socket 集成测试不改断言全过;Windows/macOS 实现的可拆逻辑(如管道名推导、ACL 描述符)有契约单测(验证:go build 三向 + 定向测试)。
- AC4(对应 F4):lock 接口三向编译通过;Linux 上 supervisor 锁重试行为不变(现有测试过);Windows 锁的可拆逻辑有契约单测(验证:同上)。
- AC5(对应 F5):业务包无 /proc、syscall.Signal 直调(门禁扫描 = 0);三向编译通过;Linux 上停止/重启/崩溃恢复相关测试全过(验证:门禁 + go test ./internal/runtime/... 等)。
- AC6(对应 F6):doctor 在 Linux 上输出与现状一致;windows/darwin 编译通过;对不可用能力输出明确 unsupported 原因(验证:Linux 实测 + 代码审阅 + 双 GOOS 编译)。
- AC7(对应 N1/N2):make test 全过且 make platform-check 零违例(验证:退出码 0)。
- AC8(对应 N3):真实 OS 待验清单成文(列出哪些项只能在真实 Windows/macOS 上验证、预计阶段 5 执行),S02 报告不声称这些项已验证(验证:文档审阅)。
- AC9(对应 N4/N5):S00 矩阵 C01–C05 的 macOS/Windows 状态与证据位置已更新、C06–C10 未动;git ls-files docs/spec_docs/S02/ 可见五份文档且其他忽略状态不变(验证:文档审阅 + git 命令)。
