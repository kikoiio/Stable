# J00 兼容性报告

## 运行信息

- 提交：`433200934c58ce12ec6f8b977495de1c93bfb564`
- Workflow：`lceda-j00.yml`
- Run：[38071508148](https://github.com/kikoiio/Stable/actions/runs/38071508148)
- Runner：GitHub-hosted Ubuntu x64
- 触发参数：`run_probe=false`、`download_client=false`
- 公开 artifact：`lceda-j00-evidence-38071508148`

## 已验证

- workflow 可在干净 runner 中建立项目内 `.tmp`、独立 HOME/XDG/profile、运行目录和 artifact 目录。
- runner 资源、memory PSI、vmstat、磁盘和 cgroup 信息可记录；本次 runner 的 memory PSI 为 0 压力。
- Python 契约测试和静态检查通过。
- workflow 使用 `contents: read`，失败清理、证据脱敏和 artifact 上传均通过。
- artifact 中未发现 token、password、secret、authorization、api-key 或 bearer 等敏感字段；路径已脱敏。

## 未验证与阻塞

当前夹具标记为 `substitute`，仅用于确定性契约测试。`manifest.json` 尚未包含已确认的公开嘉立创客户端下载 URL、版本和安装包 SHA-256，因此本次没有下载或启动真实客户端。

以下能力保持 `unverified`：

- Linux 客户端发现、CLI、MCP/桥接和隐藏窗口。
- 官方 `.eprj3` 工程识别、器件/引脚/网络读取。
- 官方参数修改、保存、重开和实际持久化。
- 原理图规则、网络、BOM、网表、截图和报告导出。
- 工程 UUID、profile、自动保存、恢复和云端同步隔离。
- 真实客户端崩溃、超时、取消、重启后的状态协调。

J00 不能进入“已验证闭环”状态。下一次真实试验前，必须替换公开夹具并补齐客户端来源、版本和 SHA-256；在此之前不得把本地 substitute 结果当作嘉立创兼容性证据。
