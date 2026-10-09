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

在 TUI 中输入 `/delegate <任务>` 可启动普通会话 run，并允许父 agent 按需把独立调查批量委派给只读子 agent。子 agent 只能读取、搜索和列举授权项目内容；不能写文件、运行命令或调用外部网络工具。执行模型所需的 provider 请求沿用父 run 配置。

委派工具同步等待本批任务完成。服务级资源池默认最多同时运行 3 项，并最多排队 32 项；每项最多运行 8 轮工具调用或 3 分钟（以先到者为准），结果摘要上限为 8 KiB。取消父 run 会取消活动任务并取消排队项；服务重启会把未完成任务及父 run 记为 `interrupted`，不会自动重跑。

`/agents` 查看内建 `explore`、`plan`、`general-purpose` 及自定义只读角色，`/agents reload` 重载。定义位于 `~/.config/stable/agents/*.md` 和授权项目的 `.stable/agents/*.md`，项目定义覆盖用户与内建角色。Markdown frontmatter 支持 `name`、`description`、`model`、`tools`、`disallowedTools`、`maxTurns`、`background`；正文是角色指令。未知字段、符号链接和超限文件会明确拒绝；角色只能收窄现有只读能力。

`/agent <角色> <任务>` 启动独立后台任务，`/tasks`（或 `/tasks next`）分页查看，`/tasks get <ID>` 查询，`/tasks stop <ID>` 请求取消。父 agent 可通过 `run_agent` 同步等待或后台提交，用 `task_output` 有界等待、`task_stop` 取消。它们与委派、fork、hook 共用池；父正常结束和 TUI 断线后后台任务继续，父显式取消会停止关联任务。取消显示实际退出后的终态；服务重启把未完成任务记为 `interrupted`，不重新执行模型。结果保存在 session 日志，脱敏摘要交接给下次同属工作项的父 run，完成不会自动启动新 run。M09-D 不提供 child 写入、团队或工作树功能。

标记为 `mode: fork`（或旧式 `context: fork`）的技能通过 `/技能名 <参数>` 启动当前 session 下的独立 fork run，也可由父 agent 通过 `load_skill` 工具调用。`fork_context` 支持 `none`（默认）、`recent`（最近 5 轮可见对话）和预算内的 `full`。fork skill 只开放读、搜、列工具，沿用 M09-A 的共享资源池和单项预算；运行状态与脱敏摘要写入 session run 事件，可按游标续读。服务重启中断未完成的 fork skill，不自动重跑。

## M09-E/F 实施状态

M09-E 增加持久化团队协作、消息、任务和只读协调器；M09-F 增加受控工作树、隔离写入和显式候选接收。两项仍在逐条验收，未整体验收完成。当前状态与可复核证据见 [M09-E checklist](specs/M09-E/checklist.md) 和 [M09-F checklist](specs/M09-F/checklist.md)。

2026-10-10，SHA `c71becd` 修复了团队查询未绑定 RunID 与 WorkRef 的授权缺口，并补充工作树中断恢复、formal root 替换和候选导出竞态回归。该 SHA 的 Go `build-and-test`/`test-package` 与真实 M09 Workspace Linux `writer-sandbox-volume` 通过；其 E2E 因被更新 SHA 取代而取消。`cc172cd` 的 Linux 首次失败因 named writer 测试把 baseline 当成 checkout 检查，已用受信 authority 的 `CandidateRoot` 修正并由 c71becd 云端复验。SHA `190c38d` 加入伪造 WorkRef 的 task/message/request 查询、coordinator 直接调用拒绝及 stale preview resolution 回归；其 M09 Workspace Linux 通过，Go 与 E2E 因被后续验证提交取代而停止。`d035d4f` 的 Go build/unit 与 package、M09 Workspace Linux 通过；E2E 因被更新 SHA 取代而取消。SHA `75bf1bf` 将真实后台子 shell 写攻击纳入 bounded-volume sandbox 验收，M09 Workspace Linux run 37989525976、Go run 37989525968 和包含六个成功作业的 E2E run 37989525958 均通过。SHA `20c59f6` 的 workspace writer/named task 屏障测试、Linux run 37991166802 和 E2E run 37991166801（六个作业）通过；Go `test-package` 通过，但 `build-and-test` run 37991166757 受 `internal/core` 取消时序测试失败影响，该测试定向复跑通过。SHA `c278079` 的 Go run 37992169097 和 Linux run 37992169077 通过，E2E run 37992169080 在四个作业成功后被更新提交取代。SHA `3dbfc63` 的 Go run 37993154508 与 M09 Workspace Linux run 37993154606 通过；其 E2E 被后续 SHA 取代。SHA `2541ad1` 的 Go run 37993652243 与 M09 Workspace Linux run 37993652092 通过，E2E run 37993652099 在四个作业成功后被新 SHA 取代；该 SHA 补充了 Goal AllowedRoot symlink 重定向拒绝和真实 v12 SQLite acceptance migration fail-closed 回归。SHA `7d95344` 修复 team child summary/error 持久化前的角色正文脱敏，并增加删除冲突两种选择到实际候选导出的回归；M09 Workspace Linux run 37994288235 已通过，Go run 37994288130 与 E2E run 37994288152 正在运行。SHA `8fbc82a` 增加 approved-plan append-gap recovery 与生产 delegation 3/32 容量回归；Go 和 Linux 通过。SHA `8c9a0cd` 按持久顺序选择未消费批准，并防止显式 resume 后重试批准重复启动；Go build/unit 与 package、M09 Workspace Linux 通过，E2E 运行尚未完成。SHA `4d67162` 加入 watcher 最终成员状态写入重试、remove quarantine inode 验证与云端回归；M09 Workspace Linux run 37996017844 已通过，Go package job 已通过，Go `build-and-test` 与 E2E run 37996017819 仍运行。后续本地增加 remove 最终 crash cut 与 intent-only team recovery 测试，定向测试已通过，待提交及云端复验。M09-E/F 尚未完成整体验收。

后续本地回归已覆盖 M09-E TUI 自身的 parent-stream EOF 恢复状态机，以及 M09-F 同内容不同 inode 的 `.stable` 目录替换恢复；两项定向测试通过。当前组合 SHA 的 Go、M09 Workspace Linux 与 E2E 云端结果见 checklist；M09-E/F 仍未完成整体验收。

SHA `a8fbddd` 的 Go `build-and-test`、`test-package`、M09 Workspace Linux 和六个 E2E jobs 均通过：Go run [37998687925](https://github.com/kikoiio/Stable/actions/runs/37998687925)、Workspace Linux run [37998687877](https://github.com/kikoiio/Stable/actions/runs/37998687877)、E2E run [37998687884](https://github.com/kikoiio/Stable/actions/runs/37998687884)。该 SHA 补充 team 请求响应者拒绝、coordinator TUI 启停闭环、rename 冲突和生产默认20,000文件上限回归。Go 首轮曾发现旧 TUI task-board fixture 直接伪造 `TeamUser` 标记；夹具现通过真实 TUI/socket 查询，复验全绿。

当前本地下一批还补充 E 身份/恢复/协调器拒绝/限额与 F workspace 所有权、plan-mode writer、metadata 恢复、重启用量、workspace 数和 child 输出预算证据；新增定向用例均通过，待提交及对应 SHA 云端复验。M09-E/F checklist 与 M09 总体仍保持未整体验收状态。
