# J00 兼容性报告

## 运行信息

- 提交：`433200934c58ce12ec6f8b977495de1c93bfb564`
- Workflow：`lceda-j00.yml`
- Run：[38071508148](https://github.com/kikoiio/Stable/actions/runs/38071508148)
- Runner：GitHub-hosted Ubuntu x64（初次 smoke；三平台 matrix 已加入 workflow）
- 触发参数：`run_probe=false`、`download_client=false`
- 公开 artifact：`lceda-j00-evidence-38071508148`

## 已验证

- workflow 可在干净 runner 中建立项目内 `.tmp`、独立 HOME/XDG/profile、运行目录和 artifact 目录。
- runner 资源、memory PSI、vmstat、磁盘和 cgroup 信息可记录；本次 runner 的 memory PSI 为 0 压力。
- Python 契约测试和静态检查通过。
- workflow 使用 `contents: read`，失败清理、证据脱敏和 artifact 上传均通过。
- artifact 中未发现 token、password、secret、authorization、api-key 或 bearer 等敏感字段；路径已脱敏。
- 官方下载页确认 Pro 4.1.71 候选包：Linux x64、Windows x64、Mac x64/arm64；候选文件名已记录在 `fixtures/lceda/j00/manifest.json`。
- 无账号请求官方下载 API 返回 HTTP 200、业务码 422（“尚未登录，请登录后再试”）；直链重定向后未提供归档。因此没有绕过认证下载，也没有把候选版本当作已验证客户端。

## 未验证与阻塞

当前夹具标记为 `substitute`，仅用于确定性契约测试。官方页面列出了候选版本，但下载 URL 由需登录的 API 签发，`manifest.json` 尚未包含可公开取得且已校验的安装包 URL、SHA-256 和大小，因此本次没有下载或启动真实客户端。

以下能力保持 `unverified`：

- Linux、Windows、macOS 客户端发现、CLI、MCP/桥接和隐藏窗口。
- 官方 `.eprj3` 工程识别、器件/引脚/网络读取。
- 官方参数修改、保存、重开和实际持久化。
- 原理图规则、网络、BOM、网表、截图和报告导出。
- 工程 UUID、profile、自动保存、恢复和云端同步隔离。
- 真实客户端崩溃、超时、取消、重启后的状态协调。

J00 不能进入“已验证闭环”状态。下一次真实试验前，必须替换公开夹具并补齐客户端来源、版本和 SHA-256；在此之前不得把本地 substitute 结果当作嘉立创兼容性证据。
