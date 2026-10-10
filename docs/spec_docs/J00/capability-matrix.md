# J00 能力矩阵

| 能力 | 状态 | 当前证据 | 限制 |
|---|---|---|---|
| 云端 Ubuntu x64 runner 建立 | verified | Run 38071508148，环境 artifact | 仅证明 runner 与脚本边界 |
| 资源与 memory PSI 记录 | verified | `environment.txt` | 不代表客户端资源需求已测量 |
| 独立 HOME/XDG/profile/.tmp | verified | workflow prepare smoke | 未与真实客户端状态结合 |
| Python 契约与 fake backend | verified | 本地 17 项测试、Run 38071508148 | 不模拟官方客户端能力 |
| workflow 权限与 artifact 脱敏 | verified | `contents: read`、artifact sensitivity scan | 仅使用公开输入 |
| 嘉立客户端发现与版本 | unverified | 无真实客户端 run | 下载 URL、版本、SHA-256 未确认 |
| CLI/MCP/桥接 | unverified | 无真实客户端 run | 官方 Linux 行为未测 |
| `.eprj3` 工程读取 | unverified | 当前夹具为 repository-local substitute | substitute schema 不是官方格式证据 |
| 参数修改/保存/重开 | unverified | 仅有 fake/substitute contract | 未通过真实客户端 |
| 规则与网络检查 | unverified | 未启动真实客户端 | 检查器 schema 和覆盖未知 |
| BOM/网表/截图/报告导出 | unverified | 未启动真实客户端 | 产物来源和格式未知 |
| 候选身份与云同步隔离 | unverified | 只有本地隔离代码/契约测试 | UUID、profile 登记和自动保存未实测 |
| 崩溃/超时/取消恢复 | unverified | 只有 fake backend 场景 | 真实进程状态未知 |

J01 开始条件尚未满足：需要至少一条真实客户端读取→修改→保存→重开→检查→导出闭环，以及候选隔离和进程清理证据。
