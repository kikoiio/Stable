# S02 非 Linux 平台的条件性参考检查

> 本清单是 S02 编写时为未来平台支持预备的检查项。S00–S05 解耦系列已于 2026-10-07 完成；本表不是系列未完成任务，也没有 macOS/Windows 执行或支持承诺。仅当未来另行决定支持相应平台时，才需重新评估这些检查。

S02 在 Ubuntu 上完成 Linux 回归、接口契约测试和 Windows/macOS 交叉编译。以下真实 OS 行为未验证，不因目标编译而视为通过。

| 平台 | 操作 | 期望 |
| --- | --- | --- |
| macOS | 使用不同普通用户检查配置/状态目录、文件和 Unix socket 权限；设置 XDG 与显式 Stable 路径覆盖 | 目录和文件仅当前用户可读写，覆盖值与 Linux 语义一致 |
| macOS | 启动、停止、崩溃后重启 runtime；重复启动并检查 flock 锁 | 进程组停止生效，父死亡降级可见，锁在退出后自动释放 |
| macOS | 运行 `stable doctor` 与 CLI/TUI 启动 | 工具清单使用 macOS 名称，Linux X11 工具不误报缺失 |
| Windows | 使用两个普通用户检查配置/状态文件 ACL、命名管道和锁文件 | 当前用户 SID ACL 拒绝其他用户，管道与 LockFileEx 保持单实例语义 |
| Windows | 启动 runtime、强制终止父进程并检查 Temporal/worker 子进程 | Job Object 或等价托管回收全部子进程，重启不受陈旧 socket/锁影响 |
| Windows | 运行 `stable doctor`、CLI/TUI 和路径覆盖矩阵 | `%APPDATA%`/`%LOCALAPPDATA%` 默认值正确，工具后缀解析正确，不出现 panic |
| 两平台 | 运行完整 e2e 与安装包启动检查 | 记录失败项和能力降级原因；只有满足 S00 发布门槛后才更新为“支持” |

## 当前限制

本清单中的真实 ACL、命名管道、Job Object、macOS 进程组和用户隔离行为尚未在本机执行。S02 报告只引用交叉编译和可在 Linux 运行的契约测试，不把这些项目标记为通过；这些检查不构成本系列后续工作。
