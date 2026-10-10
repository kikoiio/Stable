# Stable

面向单机 MVP 的 Linux CLI：本地 Temporal 开发服务 + agent worker + 固定 KiCad 传感器板实验。

## 安装与运行

本仓库当前交付 Linux CLI。v0.1.0 发布包在 Ubuntu 26.04 x86_64 上构建和验收；运行时需要 Python 3、KiCad 9、Xvfb、`xvfb-run`、`xprop`、`xwininfo` 和 ImageMagick 的 `import`。S00–S05 已完成操作系统机制与功能逻辑解耦；本系列没有声明 macOS、Windows、其他 Linux 发行版或 ARM64 受支持。未来若要提供这些平台的产品支持，需另行确定范围并验收。发布范围、安装步骤、校验与验收记录见 [Linux v0.1.0 发布说明](docs/releases/0.1.0-linux.md)；发布包与 SHA-256 文件可从 [GitHub Releases](https://github.com/kikoiio/Stable/releases/tag/v0.1.0) 下载。

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

在 TUI 中输入 `/delegate <任务>` 可启动普通会话 run，并允许父 agent 按需把独立调查批量委派给只读子 agent。子 agent 只能读取、搜索和列举授权项目内容；不能写文件、运行命令或调用外部网络工具。执行模型所需的 provider 请求沿用父 run 配置。

委派工具同步等待本批任务完成。服务级资源池默认最多同时运行 3 项，并最多排队 32 项；每项最多运行 8 轮工具调用或 3 分钟（以先到者为准），结果摘要上限为 8 KiB。取消父 run 会取消活动任务并取消排队项；服务重启会把未完成任务及父 run 记为 `interrupted`，不会自动重跑。

`/agents` 查看内建 `explore`、`plan`、`general-purpose` 及自定义只读角色，`/agents reload` 重载。定义位于 `~/.config/stable/agents/*.md` 和授权项目的 `.stable/agents/*.md`，项目定义覆盖用户与内建角色。Markdown frontmatter 支持 `name`、`description`、`model`、`tools`、`disallowedTools`、`maxTurns`、`background`；正文是角色指令。未知字段、符号链接和超限文件会明确拒绝；角色只能收窄现有只读能力。

`/agent <角色> <任务>` 启动独立后台任务，`/tasks`（或 `/tasks next`）分页查看，`/tasks get <ID>` 查询，`/tasks stop <ID>` 请求取消。父 agent 可通过 `run_agent` 同步等待或后台提交，用 `task_output` 有界等待、`task_stop` 取消。它们与委派、fork、hook 共用池；父正常结束和 TUI 断线后后台任务继续，父显式取消会停止关联任务。取消显示实际退出后的终态；服务重启把未完成任务记为 `interrupted`，不重新执行模型。结果保存在 session 日志，脱敏摘要交接给下次同属工作项的父 run，完成不会自动启动新 run。M09-D 不提供 child 写入、团队或工作树功能。

标记为 `mode: fork`（或旧式 `context: fork`）的技能通过 `/技能名 <参数>` 启动当前 session 下的独立 fork run，也可由父 agent 通过 `load_skill` 工具调用。`fork_context` 支持 `none`（默认）、`recent`（最近 5 轮可见对话）和预算内的 `full`。fork skill 只开放读、搜、列工具，沿用 M09-A 的共享资源池和单项预算；运行状态与脱敏摘要写入 session run 事件，可按游标续读。服务重启中断未完成的 fork skill，不自动重跑。

## M09-E/F 实施状态

M09-E 增加持久化团队协作、消息、任务和只读协调器；M09-F 增加受控工作树、隔离写入和显式候选接收。M09 按用户批准的最小验收集完成；E/F checklist 保留的未勾选扩展矩阵不是该最小集的完成条件。范围、证据、限制及最终 CI 记录见 [M09 minimum acceptance](specs/M09/minimum-acceptance.md)、[M09-E checklist](specs/M09-E/checklist.md) 和 [M09-F checklist](specs/M09-F/checklist.md)。

2026-10-10，SHA `c71becd` 修复了团队查询未绑定 RunID 与 WorkRef 的授权缺口，并补充工作树中断恢复、formal root 替换和候选导出竞态回归。该 SHA 的 Go `build-and-test`/`test-package` 与真实 M09 Workspace Linux `writer-sandbox-volume` 通过；其 E2E 因被更新 SHA 取代而取消。`cc172cd` 的 Linux 首次失败因 named writer 测试把 baseline 当成 checkout 检查，已用受信 authority 的 `CandidateRoot` 修正并由 c71becd 云端复验。SHA `190c38d` 加入伪造 WorkRef 的 task/message/request 查询、coordinator 直接调用拒绝及 stale preview resolution 回归；其 M09 Workspace Linux 通过，Go 与 E2E 因被后续验证提交取代而停止。`d035d4f` 的 Go build/unit 与 package、M09 Workspace Linux 通过；E2E 因被更新 SHA 取代而取消。SHA `75bf1bf` 将真实后台子 shell 写攻击纳入 bounded-volume sandbox 验收，M09 Workspace Linux run 37989525976、Go run 37989525968 和包含六个成功作业的 E2E run 37989525958 均通过。SHA `20c59f6` 的 workspace writer/named task 屏障测试、Linux run 37991166802 和 E2E run 37991166801（六个作业）通过；Go `test-package` 通过，但 `build-and-test` run 37991166757 受 `internal/core` 取消时序测试失败影响，该测试定向复跑通过。SHA `c278079` 的 Go run 37992169097 和 Linux run 37992169077 通过，E2E run 37992169080 在四个作业成功后被更新提交取代。SHA `3dbfc63` 的 Go run 37993154508 与 M09 Workspace Linux run 37993154606 通过；其 E2E 被后续 SHA 取代。SHA `2541ad1` 的 Go run 37993652243 与 M09 Workspace Linux run 37993652092 通过，E2E run 37993652099 在四个作业成功后被新 SHA 取代；该 SHA 补充了 Goal AllowedRoot symlink 重定向拒绝和真实 v12 SQLite acceptance migration fail-closed 回归。SHA `7d95344` 修复 team child summary/error 持久化前的角色正文脱敏，并增加删除冲突两种选择到实际候选导出的回归；M09 Workspace Linux run 37994288235 已通过，Go run 37994288130 与 E2E run 37994288152 正在运行。SHA `8fbc82a` 增加 approved-plan append-gap recovery 与生产 delegation 3/32 容量回归；Go 和 Linux 通过。SHA `8c9a0cd` 按持久顺序选择未消费批准，并防止显式 resume 后重试批准重复启动；Go build/unit 与 package、M09 Workspace Linux 通过，E2E 运行尚未完成。SHA `4d67162` 加入 watcher 最终成员状态写入重试、remove quarantine inode 验证与云端回归；M09 Workspace Linux run 37996017844 已通过，Go package job 已通过，Go `build-and-test` 与 E2E run 37996017819 仍运行。后续本地增加 remove 最终 crash cut 与 intent-only team recovery 测试，定向测试已通过，待提交及云端复验。M09-E/F 尚未完成整体验收。

后续本地回归已覆盖 M09-E TUI 自身的 parent-stream EOF 恢复状态机，以及 M09-F 同内容不同 inode 的 `.stable` 目录替换恢复；两项定向测试通过。当前组合 SHA 的 Go、M09 Workspace Linux 与 E2E 云端结果见 checklist；M09-E/F 仍未完成整体验收。

SHA `a8fbddd` 的 Go `build-and-test`、`test-package`、M09 Workspace Linux 和六个 E2E jobs 均通过：Go run [37998687925](https://github.com/kikoiio/Stable/actions/runs/37998687925)、Workspace Linux run [37998687877](https://github.com/kikoiio/Stable/actions/runs/37998687877)、E2E run [37998687884](https://github.com/kikoiio/Stable/actions/runs/37998687884)。该 SHA 补充 team 请求响应者拒绝、coordinator TUI 启停闭环、rename 冲突和生产默认20,000文件上限回归。Go 首轮曾发现旧 TUI task-board fixture 直接伪造 `TeamUser` 标记；夹具现通过真实 TUI/socket 查询，复验全绿。

SHA `2eca48a` 的 Go run [37999887455](https://github.com/kikoiio/Stable/actions/runs/37999887455) 与 M09 Workspace Linux run [37999887767](https://github.com/kikoiio/Stable/actions/runs/37999887767) 通过。E2E run [37999887580](https://github.com/kikoiio/Stable/actions/runs/37999887580) 的 `e2e-core` 在 `dependency_change.sh` 的 `PROJECT TABLE REMOVAL` 收敛后断言失败：证据和依赖已 current，但读取时 Goal 仍为 active；其余五个 jobs 通过。该 SHA 不含相关脚本或运行时代码改动，待后续 SHA 复验。

SHA `ad14e4f` 的 Go run [38005341959](https://github.com/kikoiio/Stable/actions/runs/38005341959) 与 M09 Workspace Linux run [38005341823](https://github.com/kikoiio/Stable/actions/runs/38005341823) 通过；E2E run [38005341767](https://github.com/kikoiio/Stable/actions/runs/38005341767) 仍在运行。该 SHA 增加 lead 通知批次字节边界、proof 过期、存储上限和 live writer 退出失败用例。

SHA `56a5111` 的 Go run [38006389753](https://github.com/kikoiio/Stable/actions/runs/38006389753) 与 M09 Workspace Linux run [38006389754](https://github.com/kikoiio/Stable/actions/runs/38006389754) 通过；E2E run [38006389755](https://github.com/kikoiio/Stable/actions/runs/38006389755) 仍在运行。该 SHA 修复 workspace list/get/preview 服务端 30 秒上限，并补 stop/close 与 child terminal 持久故障恢复交叉测试。

上一个云端验证 SHA `af412caf3adbb0281ad2549c6ec66ac2a61ec841` 包含 E 队列满/64 pending/16轮预算、跨 team recipient socket 拒绝、F same-size post-manifest mutation、ambiguous durable terminal append 恢复、coordinator `tool_search` deny-before-side-effects 及 205 路径冲突分页。其 M09 Workspace Linux run [38009393203](https://github.com/kikoiio/Stable/actions/runs/38009393203) 通过；Go run [38009393324](https://github.com/kikoiio/Stable/actions/runs/38009393324) 的 `test-package` 通过，但 `build-and-test` 被一个测试夹具错误阻断：`TestTeamSendSocketRejectsAnotherTeamsMemberIDWithoutFacts` 假设仓库 `.tmp` 目录预先存在，CI 中不存在。此夹具已修正，待新 SHA 复验；E2E run [38009393222](https://github.com/kikoiio/Stable/actions/runs/38009393222) 当时仍运行。

后续 M09 批次新增同 Goal 跨 WorkItem 的 socket 消息身份拒绝、待审批 plan request 跨真实 service 重启/TUI 重连保持原 ID 并显式 resume、team child raw text/thinking 的 transcript 隐藏、F 手工冲突合并经 socket resolution→export→review→显式 accept，以及 project-v2 protected metadata 的 atomic-exchange crash recovery。另修正上述 socket 测试临时目录假设，并修复 E 首次 spawn intent 补偿后的不可恢复成员状态、F 同文件系统 bind mount 漏检；后者使用 statx mount ID fail-closed 检查并加入 disposable ext4 workflow 攻击 fixture。conversation、TUI、store、sandbox 定向 Go 测试及格式/diff 检查通过，真实 bind-mount 验证仅由云端 M09 Workspace workflow 执行。E/F AC1–9 与 M09 总体仍未完成。
