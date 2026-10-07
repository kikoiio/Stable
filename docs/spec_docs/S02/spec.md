# S02 目录、身份、IPC 与运行时生命周期(阶段 2)Spec

> 状态:已完成（2026-10-07 系列收尾确认）。S02 对应平台边界路线图阶段 2。真实 OS 行为清单为条件性平台支持检查，不属于本系列完成门槛；非 Linux 默认路径契约和 stub 的实现状态不构成支持承诺。

> 范围解释：下文中的“三平台”是平台 API/源码路径与可在 Linux 执行的契约测试目标，不等同于 macOS/Windows 可运行产品或真实主机验收。S02 完成不产生非 Linux 支持承诺；S05 已完成本系列的 Linux 发布与安装验收。

## 背景

S01(阶段 1)已把操作系统依赖抽到 `internal/platform` 六域包(paths/ipc/lock/proc/secfile/sandbox),Linux 实现迁入 adapter,业务包平台 API 门禁零违例。但仍存在:

- 非 Linux 后端全是 fail-closed 的 unsupported stub:ipc(Unix socket)、lock(flock)、proc(/proc、进程组、Pdeathsig)在 windows/darwin 上只能编译、一运行就明确拒绝;
- 私密文件处理散落业务包:约 15 个文件 35 处 `os.Chmod(0700/0600)` 直调,Windows 上 os.Chmod 基本是空操作,私密性形同虚设;appconfig 的 owner 检查(owner_linux.go)也是 Linux-only;
- 路径默认值 XDG-only:appconfig 的配置/状态/技能目录只用 ~/.config、~/.local/state 推导;TUI 还自行硬编码拼接 ~/.config/stable/commands;
- doctor 与进程处理假定 Linux:doctor 硬编码 Xvfb/xprop 等 Linux 工具清单;sessions.go 仍直调 syscall.SIGTERM 与 /proc 格式。

路线图阶段 2 的目标是:把路径、IPC、私密存储和进程生命周期机制置于平台边界；Linux 保持可运行，其他平台实现/编译契约或明确报告 unsupported。该目标不要求三个平台都能启动产品。

## 目标

- 为 Linux/macOS/Windows 提供平台路径、IPC、私密存储及进程管理的适配代码或明确的 unsupported stub，并通过适用的源码编译/契约检查；非 Linux 的真实系统访问边界须留待未来支持立项验收;
- Linux 现有行为逐项不变(回归基线);
- doctor、运行状态与关闭流程不再假定 Linux 进程信息格式。

## 功能需求

- F1 平台路径策略:配置、状态、技能、命令目录的默认值按平台惯例推导:Linux 保持 XDG(~/.config/stable、~/.local/state/stable)不变;macOS 用 ~/Library/Application Support/stable(配置与状态同基目录);Windows 配置用 %APPDATA%\stable、状态用 %LOCALAPPDATA%\stable。显式覆盖(STABLE_CONFIG、STABLE_STATE_DIR、XDG_CONFIG_HOME/XDG_STATE_HOME)语义不变:环境变量显式指定时各平台一致生效。TUI 不再自行拼接配置/命令/技能目录,统一从 appconfig 的来源取,技能/hooks 提示文案中的路径与实际来源一致。
- F2 安全存储接口:提供「私密目录/文件」的创建与权限收紧接口:POSIX 用权限位(行为与现状逐项一致),Windows 用当前用户 ACL(拒绝其他用户读取);appconfig 的 owner 检查迁入该接口。业务包所有 0700/0600 调用点(配置、会话日志、输入历史、todo、计划文件、执行临时目录、候选快照等约 15 个文件)全部迁移,业务包不再直接调用 os.Chmod 做私密化。权限或归属不满足时的错误信息说明实际修复方式(Linux 提示 chmod 600/归属检查,Windows 提示 icacls/ACL 修复)。
- F3 平台 IPC:监听/拨号接口经平台适配器提供；Linux Unix domain socket 为真实验收基线，macOS Unix socket 与 Windows 命名管道属于源码/编译契约范围，真实连通性和 ACL 未在本系列验收。现有语义保持:removeStale(supervisor/会话服务先删陈旧 socket,网络代理不删)、拨号超时、路径来源统一由 paths 提供。
- F4 单实例锁:提供非阻塞独占锁适配接口；Linux 锁语义作为运行验收基线，macOS flock 与 Windows 等价机制属于代码路径/编译契约范围，真实主机行为未在本系列验收。锁文件以私密方式创建;supervisor 现有的 20 秒重试循环与状态码语义不变。
- F5 进程托管:以适配接口封装进程存活检测、停止、子进程属性配置、进程组停止和命令行读取；Windows Job Object、macOS setpgid 等实现状态按源码与编译证据登记，真实进程树行为未在本系列验收。sessions.go/supervisor.go 中残留的 syscall.SIGTERM 直调与 /proc 格式假定全部经进程托管接口。
- F6 doctor 平台化:工具发现按平台清单执行(Windows/macOS 查找 kicad-cli/python3 等名称及其平台后缀差异),X11 专属工具(Xvfb、xprop、xwininfo、import)仅 Linux 列入;进程状态查询走进程托管接口;对当前平台不可用的能力(如非 Linux 沙箱、GUI 会话)明确报告 unsupported 及原因,而不是误报缺失。

## 非功能需求

- N1 Linux 回归基线:路径、socket 权限、锁行为、进程停止语义、错误文案逐项与 S01 验收时一致;make test 全过;现有集成/端到端测试不改动断言即通过。
- N2 门禁延续:make platform-check 保持零违例,且随新代码扩展继续覆盖(业务包无 syscall/unix/GOOS 直调、无私密 chmod 直调)。
- N3 非 Linux 代码的验收方式(明确标注,不冒充已验证):本机只能做「三向编译绿(linux/windows/darwin)+ 可在 Linux 运行的接口契约测试(把平台无关逻辑拆成纯函数以便测试,如 ACL 描述符构造、路径推导)」。真实 OS 上的启动/重启恢复/IPC/ACL 行为列为条件性平台支持检查；若未来决定支持 macOS/Windows，再单独确定目标与验收。S02 及本系列均不声明已验证。
- N4 能力表同步:S00 能力矩阵 C01–C05 行(路径/IPC/锁/进程树/私密文件)按实际结果更新 macOS/Windows 状态(支持/降级/待真实环境验证),补充证据位置;不重写历史结论;C06–C10 不动(属阶段 3/4)。
- N5 文档落盘:S02 五份文档落 docs/spec_docs/S02/ 并加 .gitignore 白名单(同 S00/S01 方式),不影响其他忽略状态。

## 不做的事

- 发行与安装:本阶段不处理；Linux 发布由 S05 完成，其他平台发行不在本系列范围;
- Temporal CLI / worker 的非 Linux 发行物获取:S02 只保证平台正确的二进制定位与启动路径(resolver 已处理 .exe 后缀),非 Linux Temporal CLI 发行物不在本系列支持范围;
- 沙箱与网络隔离的非 Linux 后端:bubblewrap/namespace 等价物(阶段 4);非 Linux 上沙箱能力继续 fail-closed 并如实报告;
- KiCad 安装布局、GUI 会话、截图/显示能力发现(阶段 4);
- 候选验收事务的非原子平台回滚协议(阶段 3);secfile 的 openat2/RENAME_EXCHANGE 等安全文件原语本阶段不动;
- SQLite 驱动选型:维持 S01 的 stub 方案(非 Linux 打开 store 返回 unsupported),非 Linux store 驱动不在本系列支持范围。

## 验收标准

- AC1(对应 F1):三平台路径推导单测——Linux 默认值与现状逐字节相同(回归);macOS/Windows 默认值符合所选惯例;环境变量覆盖在各平台一致生效。TUI 中不再存在自拼的配置/命令/技能目录路径(验证:单测 + grep 门禁 + 双 GOOS 编译)。
- AC2(对应 F2):业务包 os.Chmod 私密化直调为 0(grep 门禁);Linux 上接口行为与现状一致(新建目录/文件权限位逐项不变、错误文案不变或更明确);Windows ACL 构造逻辑有可在 Linux 运行的契约单测(验证:make test + 门禁 + 定向单测)。
- AC3(对应 F3):ipc 接口三向编译通过;Linux 上现有 socket 集成测试不改断言全过;Windows/macOS 实现的可拆逻辑(如管道名推导、ACL 描述符)有契约单测(验证:go build 三向 + 定向测试)。
- AC4(对应 F4):lock 接口三向编译通过;Linux 上 supervisor 锁重试行为不变(现有测试过);Windows 锁的可拆逻辑有契约单测(验证:同上)。
- AC5(对应 F5):业务包无 /proc、syscall.Signal 直调(门禁扫描 = 0);三向编译通过;Linux 上停止/重启/崩溃恢复相关测试全过(验证:门禁 + go test ./internal/runtime/... 等)。
- AC6(对应 F6):doctor 在 Linux 上输出与现状一致;windows/darwin 编译通过;对不可用能力输出明确 unsupported 原因(验证:Linux 实测 + 代码审阅 + 双 GOOS 编译)。
- AC7(对应 N1/N2):make test 全过且 make platform-check 零违例(验证:退出码 0)。
- AC8(对应 N3):真实 OS 待验清单成文(列出哪些项只能在真实 Windows/macOS 上验证、仅在未来单独决定支持相应平台后执行),S02 报告不声称这些项已验证(验证:文档审阅)。
- AC9(对应 N4/N5):S00 矩阵 C01–C05 的 macOS/Windows 状态与证据位置已更新、C06–C10 未动;git ls-files docs/spec_docs/S02/ 可见五份文档且其他忽略状态不变(验证:文档审阅 + git 命令)。

## 系列收尾更新（2026-10-07）

S02 已完成并纳入 S00–S05 解耦系列收尾。平台 API、Linux 行为和非 Linux compile-time/contract 边界已按本阶段验收；真实 macOS/Windows ACL、IPC 与进程行为没有由本系列验证，也不列为待办。
