# Proactive Agent Linux CLI

首个发布包面向 Ubuntu 26.04 x86_64。它包含应用、Temporal CLI 本地开发服务和固定 KiCad 实验资源。Temporal 开发服务仅用于单机 MVP。

## 系统依赖

安装 Python 3、KiCad 9（含 `kicad`、`eeschema` 和 `kicad-cli`）、Xvfb、`xvfb-run`、`xprop`、`xwininfo`、ImageMagick 的 `import`。Ubuntu 可用系统包管理器安装 `python3 kicad xvfb x11-utils imagemagick`；若系统将 `xvfb-run` 放在独立包，请按 `doctor` 结果补齐。运行时无需 Go、仓库源码或 Codex CLI 登录。

## 安装

```bash
tar -xzf proactive-agent-0.1.0-linux-amd64.tar.gz
./proactive-agent-0.1.0-linux-amd64/install.sh
export PATH="$HOME/.local/bin:$PATH"
proactive-agent doctor
```

安装只写入 `~/.local/opt/proactive-agent/0.1.0` 和 `~/.local/bin/proactive-agent`。运行数据默认保存在 `~/.local/state/proactive-agent`。

## 模型配置

明确指定提供商和模型。支持 `openai`、`anthropic`、`gemini`、`openai-compatible`。环境变量示例：

```bash
export PROACTIVE_PROVIDER=openai
export PROACTIVE_MODEL=YOUR_MODEL_ID
export OPENAI_API_KEY=YOUR_PRIVATE_KEY
proactive-agent config check
```

Anthropic 使用 `ANTHROPIC_API_KEY`，Gemini 使用 `GEMINI_API_KEY`。兼容接口使用 `PROACTIVE_API_KEY`，还需 `PROACTIVE_BASE_URL`，例如 `http://127.0.0.1:8000/v1`；远程地址必须是 HTTPS。

也可创建仅自己可读的 `~/.config/proactive-agent/config.json`：

```bash
install -d -m 700 ~/.config/proactive-agent
cat > ~/.config/proactive-agent/config.json <<'JSON'
{"model":{"provider":"openai","model":"YOUR_MODEL_ID","api_key":"YOUR_PRIVATE_KEY"}}
JSON
chmod 600 ~/.config/proactive-agent/config.json
proactive-agent config check
```

不要把真实密钥发到聊天中。配置目录须为 `0700`，文件须为 `0600`；环境变量优先于配置文件。配置与状态目录可分别通过 `XDG_CONFIG_HOME`、`XDG_STATE_HOME` 或 `PROACTIVE_CONFIG`、`PROACTIVE_STATE_DIR` 指定。端口可通过 `PROACTIVE_TEMPORAL_PORT` 调整。

## 运行

```bash
proactive-agent up
proactive-agent runtime status
proactive-agent goal start --goal demo01
proactive-agent goal status --goal demo01
proactive-agent goal notify --goal demo01 --event check01 --kind external_check_failed
proactive-agent goal export --goal demo01 --out "$HOME/demo01-delivery"
proactive-agent logs
proactive-agent down
```

`goal start` 会复制包内固定传感器板文件到用户状态目录，只有授权副本可以被修订。目标为 `verified` 时，导出记录应包含与当前设计摘要对应的 clean ERC 报告。重复通知按事件 ID 去重。`down` 保留状态，重新 `up` 恢复。没有真实模型密钥时可以用本地模拟服务验证协议，但那不代表真实供应商 API 已经过在线验证。

卸载：先运行 `proactive-agent down`，再删除 `~/.local/bin/proactive-agent` 和 `~/.local/opt/proactive-agent/0.1.0`。运行数据与私有配置由用户自行决定是否删除。
