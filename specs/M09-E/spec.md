# M09-E 团队、消息与只读协调器 Spec

> 状态：已批准，实施中（2026-10-07）。用户批准 M09-E 四份规格文档及其定义的实现范围；验收项取得实际证据后更新 checklist。M09 整体还包括 M09-F。

## 背景与目标

源项目 `internal/teams` 提供团队成员、跨轮消息、共享任务板、计划与关闭请求及 coordinator 工具过滤。Stable 的 M09-D 已提供一次具名只读任务；E 将它扩展为可继续分配工作的逻辑成员，并把身份、消息、状态和恢复纳入现有 session 事件事实。Linux 本轮采用进程内服务，不启动终端窗格或外部 teammate 进程。

用户可创建团队、指定成员角色、拆分依赖任务、点对点或广播发送消息，查看结果、批准计划、请求退出和强制停止。普通父 agent 可协调同一受信团队；显式 coordinator 模式只提供团队工具。成员读取项目并协调任务，不能写文件、运行命令、使用网络/MCP 或递归创建成员。团队完成状态不会成为长期目标的成功证据或候选接收决定。

## 功能需求

- **F1 团队身份：** 团队具有服务生成 ID、显示名、创建者 run、所属 session 和完整 WorkRef。成员具有独立 ID、唯一队内名称、具名角色快照和模型；`lead` 为受信父身份，不能由成员名称占用。名称不能充当磁盘路径或全局跨团队路由。列表支持来源、只读能力、状态和分页。
- **F2 多轮成员：** 成员首次任务以及后续待处理消息分别构成有界 turn，通过 A/B/C/D 同一个池执行；一个成员最多一个 queued/running turn。完成后进入 idle，持久摘要可供下一 turn 使用。空闲成员不持有 worker；消息可唤醒同一逻辑成员。角色正文和权限快照在 spawn 时固定，手动重载不会改变已有成员。成员不继承父历史或兄弟 transcript。
- **F3 持久消息：** 支持队内点对点、发往 lead 和广播；发送者身份来自服务端 run/member 关联，不能接受客户端 `from` 字段。消息有稳定 ID、team ID、发送者、固定接收者集合、类型和游标。广播以发送瞬间的活跃成员集合确定接收者，排除发送者；一条持久广播事实保证全部接收者同时接受或同时拒绝。未知/关闭成员、越权接收者、额度超限或写盘失败不声称发送成功。
- **F4 消息交接：** 只有入队接受并持久关联到 destination turn 的消息才算交接。当前 turn 已开始后收到的消息留下一 turn，不插入正在执行的模型轮中。每个 destination turn 仅交接一个固定批次；重试查询、订阅及调度不会重复交接。destination 未实际 queued 的首事件间隙可恢复；重启不自动重放模型。已交接但interrupted的批次保留未完成状态，显式resume可在新turn引用为retry，查询/重启自身不能触发retry。成员向 lead 的消息只进入事件和下一次匹配 WorkRef 的父上下文，不自动启动 lead 模型 run。
- **F5 依赖任务板：** 团队任务独立于 M06 session todo 和 Goal work item。支持 create/get/list/update、归属成员、状态、双向依赖视图与版本号。阻塞由未完成依赖推导；自依赖、环、未知任务和跨队依赖被拒绝。未解除依赖的任务不能进入 in_progress/completed。并发认领采用 expected_revision 和单个事件锁，只能有一个成功归属；成员只能认领未归属任务并更新自己的任务，lead 可分配或纠偏。完成依赖仅更新可执行状态，不自动新建成员或模型 turn。
- **F6 计划协议：** spawn 可要求 plan approval；成员先调查并用 `team_plan_submit` 提交有界计划，再进入 awaiting_plan。lead 用稳定 request ID 批准或拒绝；答复和反馈持久关联同一成员、team 和请求，重复同值响应幂等、冲突/过期响应拒绝。批准仅允许后续只读 turn；拒绝带反馈进入修订。计划审批没有修改工程权限的效果。计划正文以脱敏协议数据保存，不读取或写入正式项目中的计划文件。
- **F7 关闭与取消：** lead 可发送关闭请求，成员空闲时服务回复同意并关闭；运行成员在 turn 后处理请求，也可显式拒绝。请求不能靠普通消息中的文本前缀触发。lead/用户可强制 stop，立即请求取消但直到实际 child 退出才发布 stopped。team close 冻结成员加入、消息和任务更新，取消活动 turn，全部退出后发布 closed，保留历史。父正常结束或 TUI 断开不取消团队；父显式取消仅取消它创建/唤醒的活动 turn，成员随后 interrupted，需要显式 resume，其它父/团队不受影响。
- **F8 协调器：** 用户可在一次 run 启动前选择 coordinator，并绑定已授权 team；从第一轮到终态保持静态工具集与对应指引。仅允许 E 的 team_* 查询、调度、消息、任务板和协议工具；文件读取、搜索、写入、命令、MCP、网络、D 的 run_agent、fork skill、其它 todo 工具均拒绝。schemas 与 executor 双重限制一致，用户退出 coordinator 作用于下一 run，不在当前 run 中途扩权。普通父 run 默认保持原工具行为。
- **F9 恢复与展示：** TUI 展示团队、成员 turn/idle/awaiting_plan/interrupted 状态、消息、任务依赖、待答复请求与结果，游标重连不覆盖正在执行的父 run。服务启动恢复 queued/running/stopping turn 为 interrupted，不重新调用 provider；已 child terminal 但 run outcome/idle 标记缺失按真实终态补齐。idle、请求、任务板和消息事实恢复可查询，但所有存活成员置 interrupted，需用户或匹配 lead 显式 resume 才能处理积压。重复恢复不重复终态、通知或 tool result。

## 拟定入口与默认值

以下均为提案，不视作已有配置或用户偏好：

| 项目 | 提案 |
|---|---|
| 后端 | Linux 仅 `in-process`；tmux/iTerm/external teammate 参数明确 unsupported |
| 活动容量 | 每 session 最多 2 个开放团队，service 最多 4 个；每队最多 8 个成员，service 最多 16 个非终态成员；closed 历史不占活动额度 |
| 执行池 | 继续共用 3 workers、32 queue；scheduler 无独立 provider worker/无限等待队列 |
| 每 turn | 最多 8 tool rounds、3 分钟、50,000 字节工具输出、8 KiB 摘要、64 KiB 完整编码输入；角色/调用只能收窄 |
| 成员预算 | 每次 spawn 最多 16 次已接受 turn、累计模型执行最多 10 分钟；resume 不重置已消耗预算，达到上限转 budget_exhausted；重新 spawn 必须显式操作 |
| 消息额度 | 单正文最多 8 KiB；每接收者最多 64 条未交接，队内最多 256 条待交接投递、正文总计最多 2 MiB；广播按接收者投递数计额度 |
| 消息批次 | 单 turn 最多 8 条/正文合计 32 KiB；上一摘要最多 8 KiB；role+身份+摘要+批次完整编码仍须满足 64 KiB |
| lead 交接 | 单次父上下文最多 8 条团队通知/正文合计 32 KiB；余项留后续 run，Goal 必须完整 WorkRef 匹配 |
| 任务板 | 每开放 team 最多 256 项，title 256 字节、description 4 KiB、每任务最多 16 条依赖；完成项仍计该 team 限额，历史从日志分页查询 |
| 请求 | 单计划正文最多 8 KiB，反馈/关闭原因 2 KiB；每成员最多一个未解决计划和一个未解决关闭请求；待处理请求 10 分钟过期，过期不默认批准、不占 worker |
| 查询 | 默认 20、最大 100 项；等待终态最多 30 秒，不占 worker；消息正文/摘要/错误脱敏，错误最多 1 KiB |
| 标识 | team/member 显示名归一化小写，1–64 字节 `[a-z0-9][a-z0-9_-]*`；服务 ID 不取用户路径；`lead` 保留 |

用户入口提案：`/teams [get <id> | create <name> | close <id>]`、`/team <id> members`、`/team <id> spawn <name> <role> <任务>`、`/team <id> send <成员|lead|*> <正文>`、`/team <id> tasks`、`/team <id> stop <成员>`、`/team <id> resume <成员>`、`/team <id> requests`、`/team <id> respond <request-id> <approve|reject> [反馈]`、`/coordinator <team-id|off>`。客户端先显示用法并做格式校验，实际身份和权限始终由服务构造。

模型工具提案：`team_create`、`team_get`、`team_list`、`team_close`、`team_member_spawn`、`team_member_get`、`team_member_list`、`team_member_stop`、`team_member_resume`、`team_send`、`team_messages`、`team_task_create/get/list/update`、`team_plan_submit`、`team_request_list`、`team_request_respond`、`team_shutdown_request`。成员无 create/spawn/resume/close/lead-response 权限，只能读队内状态、发送消息、操作自己的任务及提交计划/答复自己的关闭请求。公开 D 角色定义字段不增加 `team_name`；团队角色通过专用 spawn 参数选择。

## 非功能与受信边界

- **N1 权限：** team ID 不是授权凭据；session/Goal/WorkItem/root/provider 由受信 scope 绑定。来自另一 session、另一个 Goal 或 WorkItem 的请求拒绝；项目根更改后只能查询/关闭旧队，恢复执行须重新验证相同授权根。成员消息不能改变 sender、角色、model、工具 allowlist 或 bounds。
- **N2 有界资源：** 接受和预算扣除以持久事实为准；原子拒绝队列满，不产生未接受 goroutine。最多一个 service 调度循环和每个已接受 turn 一个结果 watcher，活动/待调度集合由成员上限限制；空闲无成员轮询 goroutine。关闭和取消与注册共享锁/代次，不能漏取消。
- **N3 一致性：** 事件先写后广播；revision/请求/消息/turn ID 去重。终态、消息交接、任务认领和关闭只能追加一次合法事实；写盘失败不报告成功。事实均沿 session log，派生视图可丢弃重建，不引入第二真相数据库或用户可篡改的全局队名文件。
- **N4 隐私：** role 正文、thinking、child 原始 transcript、provider credential 不保存到团队状态或通知。持久协作消息/计划是显式输入，先脱敏并限长；来源标明非受信参考内容，不拼成系统指令。下一轮仅有身份、角色、最近摘要、指定消息、必要任务状态。
- **N5 验证：** 临时 session、定义目录、fake provider/runner、故障注入与屏障覆盖；不读真实源团队配置，不调用真实模型。重型 Go/E2E/package 回归走已授权 GitHub Actions，记录确切提交与失败历史。

## 源端行为对照与边界

| 源文件/行为 | 源端可观察实现或缺口 | E 提案与验收归属 |
|---|---|---|
| `teams.go`、`spawn.go`、`inprocess.go`、`runner.go` | 真正多轮 Conv、成员 goroutine、idle mailbox polling、进度；in-process 与终端后端 | 适配逻辑多轮成员；有界摘要+消息延续，不保存完整 Conv/transcript；空闲调度不占 worker |
| `filemailbox.go`、`tools.go` | JSON inbox、p2p/broadcast、mark-all-read；全局名称路由和文件 fallback | 适配为 session 事件、队内身份、精确批次交接；禁止 unknown recipient fallback，防丢失并发新邮件 |
| `sharedtask.go`、`tasktools.go` | 任务CRUD、归属、Blocks/BlockedBy；源码追加依赖和保存，不实现完整环检验/原子跨进程认领 | 适配并补明确依赖合法性、阻塞门和expected_revision；不宣称源端已有这些保障 |
| `protocol.go`、`runner.go` | 计划/关闭 request ID 与响应；源码计划通过会改变checker，空闲关闭支持文本前缀 | 保留协议行为，审批不扩大只读权限；typed facts，关闭拒绝/超时/强停均可观察；写入归F |
| `coordinator.go` | 静态白名单 Agent/SendMessage/TaskStop/SyntheticOutput/TeamDelete；明确屏蔽文件和共享任务板 | 映射到Stable专用team_*；允许团队任务板是本轮明确行为差异，方便审阅依赖；无SyntheticOutput能力时不虚称支持 |
| `teamfile.go`、`registry.go` | config与名称映射；Member AgentType/Model/WorktreePath/JoinedAt部分只用于落盘/恢复元信息 | 团队角色/model本轮有实际调用验证；WorktreePath仍F，显示字段不等于执行隔离 |
| `transcript.go` | 实际保存原始成员对话 | 出于既有事件隐私边界不迁移原始transcript；仅显式协作内容与有界摘要 |
| `backend.go`、`tmux.go`、`iterm.go` | 平台检测及终端窗口启动/停止 | Linux E明确仅进程内；tmux留后续独立需求，iTerm为本平台不适用，不计缺失的已支持功能 |
| `agents/definition.go` 的 effort/skills/hooks/memory/MCP预载 | 源定义解析/保存字段；本次读取未建立完整运行语义证据 | 继续明确unsupported/后续对照，不能靠team spec宣称已迁移；team_name入口由专用团队服务适配 |

E 不实现文件写入、worktree、自动 merge、候选接收、Temporal 团队工作流、自动服务重启后的模型续跑或无预算驻留。M09-F 仍需独立设计审批。

## 验收标准

- **AC1（F1/N1）：** 两个session、两个Goal/WorkItem、同名不同队fixture验证注册/重名/保留名称/容量/授权根和全部查询、消息、取消边界；不存在全局名称越权。
- **AC2（F2/F4/N2）：** fake provider两次turn输入捕获证明同一逻辑成员继续、已接受消息批次与最近摘要可见、无父历史/兄弟transcript；idle不占worker；每成员无并行turn，共享池队满不误接受。
- **AC3（F3/F4/N3）：** p2p、lead和广播顺序/固定接收者/额度/故障注入验证原子投递；destination gap、并发新消息及重复订阅不会丢失或重复交接；消息不自动启动lead。
- **AC4（F5）：** 依赖环/未知/跨队/自依赖拒绝，双向视图一致；未解除依赖不能执行/完成；两个成员并发认领只一方成功，权限与版本冲突可见，M06 todo保持独立。
- **AC5（F6/F7）：** 同一request的计划批准/拒绝/修订/过期、错sender/错team/重复冲突响应；批准仍只读；关闭idle自动确认、busy延期/拒绝、强停和全队关闭仅在实际退出后终态。
- **AC6（F7/F9）：** 父正常终态/断线继续；父显式取消、独立stop与service关闭仅作用正确集合；重启首queued前、queued/running、terminal-gap恢复幂等、积压保留且provider不重跑，显式resume才能继续。
- **AC7（F8/N1）：** coordinator schemas/prompt/executor静态一致；文件、命令、MCP、网络、fork、D递归和todo拒绝；恶意member不能伪装lead或扩大权限；普通Session/Goal工具保持有效。
- **AC8（F2–F4/N2/N4）：** 所有turn/成员累计/消息批次/任务/请求/活动成员/上下文限额、并发关闭和写盘失败有明确状态；原始角色、thinking、credential和transcript不进入日志/UI。
- **AC9（F9/N5）：** CLI/client/TUI完整建队→spawn→两轮协作→依赖完成→请求与停止→重连恢复；A/B/C/D、权限、候选/Goal独立验收及Go/E2E/package云端回归通过，记录代码SHA和每个job状态。
