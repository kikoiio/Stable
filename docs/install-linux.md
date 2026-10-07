# Stable Linux CLI

发布包的目标环境为 Ubuntu 26.04 x86_64。包由 GitHub Actions 在成功的 workflow 中构建并验收，归档与 SHA-256 文件可从该 workflow run 的 Artifacts 下载；当前不发布到 GitHub Releases。它包含应用、Temporal CLI 本地开发服务和固定 KiCad 实验资源。Temporal 开发服务仅用于单机 MVP。macOS、Windows、其他 Linux 发行版和 ARM64 尚未完成真实环境验收。

## 系统依赖

安装 Python 3、KiCad 9（含 `kicad`、`eeschema` 和 `kicad-cli`）、Xvfb、`xvfb-run`、`xprop`、`xwininfo`、ImageMagick 的 `import`。Ubuntu 可用系统包管理器安装 `python3 kicad xvfb x11-utils imagemagick`；若系统将 `xvfb-run` 放在独立包，请按 `doctor` 结果补齐。运行时无需 Go、仓库源码或 Codex CLI 登录。

## 安装

```bash
tar -xzf stable-0.1.0-linux-amd64.tar.gz
sha256sum -c stable-0.1.0-linux-amd64.tar.gz.sha256
./stable-0.1.0-linux-amd64/install.sh
export PATH="$HOME/.local/bin:$PATH"
stable doctor
```

安装只写入 `~/.local/opt/stable/0.1.0` 和 `~/.local/bin/stable`。运行数据默认保存在 `~/.local/state/stable`。

## 模型配置

明确指定提供商和模型。支持 `openai`、`anthropic`、`gemini`、`openai-compatible`。环境变量示例：

```bash
export STABLE_PROVIDER=openai
export STABLE_MODEL=YOUR_MODEL_ID
export OPENAI_API_KEY=YOUR_PRIVATE_KEY
stable config check
```

Anthropic 使用 `ANTHROPIC_API_KEY`，Gemini 使用 `GEMINI_API_KEY`。兼容接口使用 `STABLE_API_KEY`，还需 `STABLE_BASE_URL`，例如 `http://127.0.0.1:8000/v1`；远程地址必须是 HTTPS。

也可创建仅自己可读的 `~/.config/stable/config.json`：

```bash
install -d -m 700 ~/.config/stable
cat > ~/.config/stable/config.json <<'JSON'
{"model":{"provider":"openai","model":"YOUR_MODEL_ID","api_key":"YOUR_PRIVATE_KEY"}}
JSON
chmod 600 ~/.config/stable/config.json
stable config check
```

不要把真实密钥发到聊天中。配置目录须为 `0700`，文件须为 `0600`；环境变量优先于配置文件。配置与状态目录可分别通过 `XDG_CONFIG_HOME`、`XDG_STATE_HOME` 或 `STABLE_CONFIG`、`STABLE_STATE_DIR` 指定。端口可通过 `STABLE_TEMPORAL_PORT` 调整。

## 一键体验

配置好模型后，在任意目录直接输入 `stable`：自动启动本地运行组件（如尚未运行）并进入交互对话会话。会话内用 `/goal <自然语言目标>` 生成验收标准提案，审阅终端显示的标准后用 `/confirm <提案ID>` 确认并开始运行。运行中可直接输入文字纠偏、`/reply` 答复提问、`/status` 查看进度。退出会话不影响目标继续运行。

只想看一次性快速演示可运行 `stable demo`：自动运行一个内置目标，完成后把交付记录导出到当前目录的 `stable-out/<目标ID>/`，并停止它启动的运行组件。目标需要人工处理时以非零码退出并显示原因。

## 运行

```bash
stable
> /goal 修复传感器连接，ERC 必须全过，J1 连接要恢复
> /confirm prop-XXXXXXXX
> 优先检查 J1 附近的连线
> /reply 按 J1.2 处理
> /status
> /quit
```

### 对话式会话

`stable` 进入交互终端：`/goal <目标描述>` 设置新目标，`/focus <目标ID>` 聚焦已有目标，直接输入文字即向当前目标发送纠偏消息，`/reply` 回答 agent 的提问，`/confirm`、`/reject` 处理验收标准提案，`/status` 查看所有目标。会话状态保存在运行时内，终端关闭不影响目标运行，重新接入可看到完整历史；多个终端可同时接入。

创建目标分两步：先 `--create-goal "自然语言描述"`，系统把描述转译为固定词汇表内的验收标准清单（当前支持 `kicad.erc_clean` 与 `sensor.connection_present`），确认提案后目标才创建运行；无法机器验证的描述会被拒绝并说明原因。目标运行中可随时 `--say` 纠偏（在下一决策轮生效，不打断进行中的操作）；agent 遇到超出能力的情形会在会话中提问并等待，回答后继续。

### 非交互目标定义

脚本与自动化可跳过转译，直接用 JSON 定义文件创建目标：

```bash
cat > goal.json <<'JSON'
{"objective":"Repair the sensor connector and obtain a clean KiCad ERC",
 "criteria":[{"id":"erc-clean","kind":"kicad.erc_clean","payload":{"max_violations":0}}],
 "check_interval_seconds":30}
JSON
stable goal create --goal demo01 --from goal.json
```

### 目标生命周期

目标文件是包内固定传感器板的隔离副本（位于用户状态目录），只有授权副本可以被修订。当且仅当全部验收标准以当前设计版本的客观证据通过时，目标为 `verified`；导出记录包含与设计摘要对应的 clean ERC 报告、会话记录和验收标准的转译确认链路。重复通知按事件 ID 去重。`down` 保留状态，重新 `up` 恢复。没有真实模型密钥时可以用本地模拟服务验证协议，但那不代表真实供应商 API 已经过在线验证。

## 升级、回退与卸载

升级时下载并校验新版本归档，解包后运行其中的 `install.sh`。安装器先校验包并在用户目录暂存，再切换 `~/.local/bin/stable`；旧版本目录会保留。需要回退时，运行旧版本解包目录中的 `install.sh`，入口会切回该版本。

卸载时先停止运行时和 KiCad 会话，再执行：

```bash
stable down
stable-uninstall
```

卸载会移除 Stable 管理的程序版本和命令入口，保留 `~/.config/stable` 中的配置与凭证、`~/.local/state/stable` 中的运行数据库和目标数据，以及用户自行放置的其他文件。
