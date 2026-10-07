# M10-A 本地 Print 与 Provider 选择 Spec

## 背景

Stable 目前要求通过交互终端进入 TUI；模型 provider 通过配置文件或环境变量设置。M10-A 为脚本和终端用户补充一次性 print 入口与 provider 管理命令。

## 目标

- `stable --print [文本]` 接收命令行文本；未提供文本时读取 stdin。回答输出到 stdout，错误输出到 stderr。
- print 自动启动本地服务，创建临时 session，并复用已有 agent runner 与权限门；只允许现有 executor 分类为只读的工具。
- 临时 session 不出现在会话列表；运行结束后清理其 transcript 和 run 记录。
- provider 命令支持查看可用项、当前配置，并持久切换 provider 与 model。切换时清除旧 API key 和 base URL；凭据仍从私有配置或对应环境变量读取。
- 明确显示 Gemini 只支持目标决策，不能用于 print 的流式对话。

## 功能需求

- **F1 Print 输入与启动：** `stable --print [文本]` 接收一次用户输入；未提供文本时读取 stdin 至 EOF。它从当前工作目录运行；若本地 runtime 未启动则自动启动。
- **F2 Print 执行：** 每次 print 创建一个临时 session，并复用 Stable 现有的 agent runner、只读工具和权限判断。仅现有 executor 分类为 `OpRead` 的工具可执行。其他操作类型一律拒绝并取消 run，即使存在精确允许规则也不例外。执行完成后，该 session 不出现在会话列表，其 transcript 和 run 记录会被清理。
- **F3 Print 输出与失败：** 只将模型回答文本写到 stdout；错误和运行失败信息写到 stderr，并以非零状态退出。Gemini 不支持流式 print；使用 Gemini 时给出明确错误。
- **F4 无交互边界：** `OpWrite`、`OpCommand`、MCP、network、legacy 及其他非 `OpRead` 操作一律拒绝，且不等待审批。若运行请求 `ask_user`、计划审批等交互操作，print 取消 run、明确失败并非零退出，不等待 TUI。只读操作遵循现有权限判断。
- **F5 Provider 查询：** `stable provider list` 显示四种已配置 provider 及其能力；`stable provider show` 显示当前有效 provider、model 和配置来源，不显示凭据值。Gemini 标记为仅用于目标决策。
- **F6 Provider 切换：** `stable provider use <provider> --model <model>` 持久更新私有配置；openai-compatible 可附带 `--base-url`。切换 provider 时清除旧的 `api_key` 和 `base_url`。不提供命令行密钥参数；凭据从私有配置或 provider 专属环境变量读取，环境变量优先于配置文件。
- **F7 切换时机：** runtime 运行时，`provider use` 拒绝修改并提示先运行 `stable down`；runtime 关闭时才更新配置。`STABLE_PROVIDER`、`STABLE_MODEL` 等环境变量仍按现有优先级覆盖配置。

## 非功能需求

- **N1 凭据保护：** provider 凭据不得出现在 stdout、stderr、print 输出或会话记录中；`provider show` 不显示密钥值。
- **N2 配置完整性：** provider 切换保留配置文件中的其他设置；更新期间失败时不得留下部分写入或降低现有私有权限。
- **N3 临时数据清理：** print 完成、失败或收到中断时，都尝试清理临时 session、transcript 和 run 记录；清理失败须可观察，不能报告为完全成功。
- **N4 可重复验证：** print/provider 命令可通过本地 fake provider 和临时配置验证，不依赖真实 API key 或外网。

## 不做的事

- 不实现 WebSocket remote、远程认证或远程工作目录授权；远程入口另行立项。
- 不增加 Gemini 流式 print；Gemini 仍只用于目标决策。
- 不在运行中的服务内热切换 provider；`provider use` 仅在 runtime 关闭时修改配置。
- 不提供 TUI provider 选择器、provider 自动故障转移、多配置档案或模型目录下载。
- 不让 print 创建或接管长期目标，也不提供可恢复的持久 print session。
- 不支持 print 期间等待审批、用户提问或计划审批；这类交互按已确认规则失败退出。
- 不在 print 中执行文件写入、命令、MCP、network 或其他非只读操作；不生成或保留 candidate 改动。

## 验收标准

- **AC1（F1）：** `stable --print "提示"` 能提交一次输入；不带文本时从 stdin 读取到 EOF。runtime 未启动时会自动启动；运行根目录为当前工作目录。
- **AC2（F2/F4）：** print 使用现有 agent runner；`OpRead` 工具可按现有权限规则运行。写入、命令、MCP、network、legacy 等非 `OpRead` 操作即使有精确允许规则也被拒绝，run 被取消且命令非零退出；`ask_user` 或计划审批不会挂起等待。
- **AC3（F3）：** 成功时 stdout 仅包含回答文本；失败详情出现在 stderr，进程返回非零状态。凭据与控制状态不出现在回答输出中。
- **AC4（F2/N3）：** 成功、失败和中断后，临时 session 不出现在会话列表，其 transcript 与 run 记录已清理；清理失败会被报告。
- **AC5（F5/N1）：** `provider list` 显示四种 provider 及能力；`provider show` 显示当前 provider、model 和配置来源，不泄漏凭据；Gemini 明确标记为不支持 print。
- **AC6（F6/F7/N2）：** runtime 关闭时，`provider use` 更新 provider/model，并按规则处理 base URL 与旧密钥，同时保留其他配置项；runtime 运行时拒绝修改。provider 环境变量优先于文件配置。
- **AC7（F3/F5）：** 选择 Gemini 后运行 print，会得到明确的不支持错误并非零退出；不会把 Gemini 当作流式 provider 启动 print。
- **AC8（N4）：** 使用本地 fake provider 和临时私有配置可验证参数输入、stdin、输出通道、失败状态、配置切换和临时数据清理；无需真实密钥或外网。
