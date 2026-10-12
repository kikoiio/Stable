# J00 兼容性报告

## 运行信息

- Workflow：`lceda-j00.yml`
- 最新 smoke Run：[38273843956](https://github.com/kikoiio/Stable/actions/runs/38273843956)，提交 `ded560551b5914e8faa1e8f9eaadc1bd2a0e5d65`
- Linux 证据：[lceda-j00-linux-38273843956](https://github.com/kikoiio/Stable/actions/runs/38273843956/artifacts/11758715532)
- Windows 证据：[lceda-j00-windows-38273843956](https://github.com/kikoiio/Stable/actions/runs/38273843956/artifacts/11758590947)
- macOS 证据：[lceda-j00-macos-38273843956](https://github.com/kikoiio/Stable/actions/runs/38273843956/artifacts/11757928060)
- 触发参数：`download_client=true`、`strict_download=true`、`run_client_smoke=true`、`run_cli_probe=false`、`install_activation=false`
- CLI 激活失败复现 Run：[38269977561](https://github.com/kikoiio/Stable/actions/runs/38269977561)

## 已验证

- workflow 可在干净 runner 中建立项目内 `.tmp`、独立 HOME/XDG/profile、运行目录和 artifact 目录。
- runner 资源、memory PSI、vmstat、磁盘和 cgroup 信息可记录；本次 runner 的 memory PSI 为 0 压力。
- Python 契约测试和静态检查通过。
- workflow 使用 `contents: read`，失败清理、证据脱敏和 artifact 上传均通过。
- artifact 中未发现 token、password、secret、authorization、api-key 或 bearer 等敏感字段；路径已脱敏。
- 官方下载页确认 Pro 4.1.71 候选包：Linux x64、Windows x64、Mac x64/arm64；候选文件名已记录在 `fixtures/lceda/j00/manifest.json`。
- 无账号请求官方下载 API 返回 HTTP 200、业务码 422（“尚未登录，请登录后再试”）；直链重定向后未提供归档。因此没有绕过认证下载，也没有把候选版本当作已验证客户端。
- Linux x64 归档已在 GitHub-hosted Ubuntu runner 下载并通过记录的 SHA-256 校验；客户端在隔离 HOME、Xvfb 和软件渲染下保持运行 20 秒。
- Linux Electron sandbox 配置、Xvfb 清理和 24 项 J00 Python 契约测试通过；Windows/macOS runner 的路径、进程和文件 smoke 通过。
- 三平台证据均由 workflow 脱敏后上传，运行目录、临时 HOME/XDG 路径和客户端日志未泄露到公开 artifact。

## 未验证与阻塞

当前夹具标记为 `substitute`，仅用于确定性契约测试。Linux 客户端已经下载并启动 smoke，但官方 CLI/API 探针尚未完成；Windows/macOS 本次只执行合约级文件/进程 smoke。

Run 38269977561 的 Linux CLI 探针实际打开客户端后收到 `entry=regist` 注册跳转，并分类为 `CLIENT_ACTIVATION_REQUIRED`。当前仓库没有 `LCEDA_PRO_ACTIVATION` secret，因此最新 run 明确跳过 CLI 闭环，没有伪造或绕过激活状态。工作流仍兼容旧名称 `J00_LCEDA_ACTIVATION`。

以下能力保持 `unverified`：

- Linux、Windows、macOS 的 CLI、MCP/桥接和隐藏窗口能力。
- 官方 `.eprj3` 工程识别、器件/引脚/网络读取。
- 官方参数修改、保存、重开和实际持久化。
- 原理图规则、网络、BOM、网表、截图和报告导出。
- 工程 UUID、profile、自动保存、恢复和云端同步隔离。
- 真实客户端崩溃、超时、取消、重启后的状态协调。

J00 不能进入“已验证闭环”状态。下一次真实试验需要在 GitHub Actions 中提供授权的 `LCEDA_PRO_ACTIVATION` secret，并将 `manifest.json` 的 substitute 夹具替换为确认过的 `.eprj3` 工程，或明确批准生成的 disposable offline fixture。完成后再运行 `run_cli_probe=true`、`install_activation=true` 的 Linux job；在此之前不得把 substitute 或 smoke 结果当作嘉立创功能兼容性证据。
