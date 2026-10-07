# Stable

面向单机 MVP 的 Linux CLI：本地 Temporal 开发服务 + agent worker + 固定 KiCad 传感器板实验。

## 安装与运行

发布包在 Ubuntu 26.04 x86_64 上构建和验收。运行时需要 Python 3、KiCad 9、Xvfb、`xvfb-run`、`xprop`、`xwininfo` 和 ImageMagick 的 `import`。macOS、Windows、其他 Linux 发行版和 ARM64 尚未完成真实环境验收。

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
