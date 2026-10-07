# Stable

面向单机 MVP 的 Linux CLI：本地 Temporal 开发服务 + agent worker + 固定 KiCad 传感器板实验。

## 安装与运行

本仓库当前交付 Linux CLI；发布包仅在 Ubuntu 26.04 x86_64 上构建和验收。运行时需要 Python 3、KiCad 9、Xvfb、`xvfb-run`、`xprop`、`xwininfo` 和 ImageMagick 的 `import`。S00–S05 已完成操作系统机制与功能逻辑解耦；本系列没有声明 macOS、Windows、其他 Linux 发行版或 ARM64 受支持。未来若要提供这些平台的产品支持，需另行确定范围并验收。

CI 成功运行会在 GitHub Actions 的 workflow artifacts 中提供版本化 tar.gz 与 SHA-256 文件；本仓库当前不通过 GitHub Releases 发布安装包。

```bash
tar -xzf stable-0.1.0-linux-amd64.tar.gz
sha256sum -c stable-0.1.0-linux-amd64.tar.gz.sha256
./stable-0.1.0-linux-amd64/install.sh
export PATH="$HOME/.local/bin:$PATH"
stable doctor
stable config init
```

将新版本归档解包并运行其 `install.sh` 可升级；旧版本目录会保留，重新运行旧包的 `install.sh` 可回退。运行 `stable-uninstall` 可移除 Stable 程序，配置和运行数据会保留。

编辑 `~/.config/stable/config.json`，填写模型提供商、模型名和 API 密钥，然后运行 `stable config check`。也可通过 `STABLE_PROVIDER`、`STABLE_MODEL` 和对应提供商的密钥环境变量配置。运行 `stable help` 可查看命令。

```bash
stable
> /goal 修复传感器连接，ERC 必须全过，J1 连接要恢复
> /confirm prop-XXXXXXXX
> /say 优先检查 J1 附近的连线
> 你好
> /status
> /quit
```

`stable` 会在需要时启动运行时并进入常驻对话。直接输入文字会得到模型回复。`/goal <目标描述>` 生成验收标准提案，终端显示完整标准，`/confirm <提案ID>` 后开始运行并自动聚焦新目标。`/say <文字>` 可纠偏当前目标，`/reply <文字>` 可答复 agent 提问；`/focus <目标ID>` 切换已有目标。退出终端不影响目标运行，再次运行 `stable` 可接回会话。也可以用 `stable goal create --from goal.json` 以结构化定义文件非交互创建。

### 一次性 Print 与 Provider

```bash
stable --print "总结当前目录的 README"
cat question.txt | stable --print
stable provider list
stable provider show
stable provider use openai --model gpt-4.1
stable provider use openai-compatible --model local-model --base-url http://127.0.0.1:8080/v1
```

`stable --print [文本]` 在当前工作目录执行；省略文本时读取 stdin 到 EOF。回答文本只写 stdout，错误写 stderr 并以非零状态退出。Print 创建一次性临时会话，完成、失败或中断后清理记录。它只允许 `read_file`、`glob`、`grep` 等只读工具；写入、命令、MCP、network 和需要审批、提问或计划审批的请求都会被拒绝。Gemini 仅用于目标决策，不能用于流式 Print。

`stable provider list/show/use` 查看 provider 能力、当前有效配置和字段来源，并持久切换 provider/model。切换 provider 会清除旧 provider 的配置密钥和 base URL；凭据从私有配置文件或 provider 专属环境变量读取，环境变量优先。运行时启动期间 `provider use` 会拒绝修改，请先运行 `stable down`。

### Remote 浏览器会话

Remote 默认只监听本机 `127.0.0.1:8765`。启动后在同一台机器打开 `http://127.0.0.1:8765`，运行 `stable remote pair` 并输入一次性配对 token。每次浏览器 WebSocket 连接都要在本机 TUI 批准项目目录；批准只绑定该连接，断开后重新连接需要再次批准。

```bash
stable remote up
stable remote status
stable remote pair
stable remote down
```

如需让局域网设备连接，指定 LAN IP 并同时提供包含该 IP/DNS 名称的 TLS 证书和私钥：

```bash
stable remote up --listen 192.168.1.20:8765 --cert /path/to/fullchain.pem --key /path/to/private-key.pem
```

非 loopback 地址缺少有效证书时不会启动。`stable remote down` 只关闭 Remote 并保留 runtime；`stable down` 会关闭 Remote 和 runtime。浏览器配对凭证只保存在内存，Remote 或 runtime 停止后需要重新配对。远程页面支持会话聊天/流、取消运行、工具权限、计划审批、问题答复和项目目录内目标状态；provider 凭据和管理类操作不会发送给浏览器。

## 仓库布局

- `cmd/stable` — 用户 CLI（打包后安装在 `bin/`）；`cmd/agentctl`、`cmd/agentworker` 是内部组件（`libexec/`）
- `internal/` — 工作流、决策、执行、存储、运行时监督等
- `fixtures/sensor_board` — 固定实验夹具；`schemas/`、`workers/` 随包发布
- `scripts/` — 打包、安装、本地运行、环境检查
- `tests/e2e` — 源码级端到端（通知去重、崩溃恢复、不支持场景安全停止）
- `tests/package` — 发布包验收（安装、CLI 行为、完整 e2e、崩溃重启）
- `tests/cases` — 夹具派生场景用例（该修的修、不该动的交给人），见 [tests/cases/README.md](tests/cases/README.md)

## 常用命令

```bash
make check         # 开发环境依赖检查
make test          # Go 单元测试
make e2e           # 源码级端到端（慢）
make cases         # 场景用例（慢，需要 kicad-cli）
make package       # 构建 dist/ 发布包
make test-package  # 发布包验收（会先重新打包）
make install-dev   # 重新打包并覆盖安装到 ~/.local（自动先 stable down）
make run           # 构建开发版并进入 stable 对话
make clean         # 删除 run/ 与 dist/
```

版本号唯一来源是 `./VERSION`；发布包构建通过 `-ldflags` 注入 CLI。

运行数据默认在 `~/.local/state/stable`；`stable down` 会停止运行时并清理遗留的
KiCad GUI 会话进程。
`make run` 使用 `run/dev-state` 保存开发数据；退出对话后需用相同的 `STABLE_STATE_DIR` 运行开发版 `stable down` 才会停止后台运行时。
