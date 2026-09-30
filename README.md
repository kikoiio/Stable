# Stable

面向单机 MVP 的 Linux CLI：本地 Temporal 开发服务 + agent worker + 固定 KiCad 传感器板实验。

## 安装与运行

发布包面向 Linux x86_64。运行时需要 Python 3、KiCad 9、Xvfb、`xvfb-run`、`xprop`、`xwininfo` 和 ImageMagick 的 `import`。

```bash
tar -xzf stable-0.1.0-linux-amd64.tar.gz
./stable-0.1.0-linux-amd64/install.sh
export PATH="$HOME/.local/bin:$PATH"
stable doctor
stable config init
```

编辑 `~/.config/stable/config.json`，填写模型提供商、模型名和 API 密钥，然后运行 `stable config check`。也可通过 `STABLE_PROVIDER`、`STABLE_MODEL` 和对应提供商的密钥环境变量配置。运行 `stable help` 可查看命令。

```bash
stable up
stable chat --create-goal "修复传感器连接，ERC 必须全过，J1 连接要恢复" --goal demo01
stable chat --confirm prop-XXXXXXXX --goal demo01   # 确认验收标准提案后开始运行
stable chat --goal demo01 --say "优先检查 J1 附近的连线"
stable goal status --goal demo01
stable goal export --goal demo01 --out "$HOME/demo01-delivery"
stable down
```

`stable chat` 是常驻对话会话的瘦终端：目标创建（自然语言验收标准转译 + 显式确认）、运行中纠偏、agent 提问答复都在会话内完成；终端关闭不影响目标运行。也可以用 `stable goal create --from goal.json` 以结构化定义文件非交互创建。

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
make run           # 本地开发运行（Ctrl-C 停止）
make clean         # 删除 run/ 与 dist/
```

版本号唯一来源是 `./VERSION`；发布包构建通过 `-ldflags` 注入 CLI。

运行数据默认在 `~/.local/state/stable`；`stable down` 会停止运行时并清理遗留的
KiCad GUI 会话进程。
