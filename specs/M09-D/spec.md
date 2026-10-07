# M09-D Agent 定义与后台任务 Spec

> 状态：四份文档已获用户批准，正在实现（2026-10-07）。验收项取得实际证据后勾选。

## 背景

M09-A 提供同步只读批量委派，M09-B/C 复用该能力执行 fork skill 与 hook agent。它们没有覆盖源端通用具名 agent 定义、后台启动、任务查询/取消及完成通知。源端依据为 `mewcode-golang/internal/agents/{definition,loader,agent_tool,subagent}.go`。源端任务管理主要在内存中；Stable 必须适配 M05 的持久事件和重启中断语义。

## 目标

- 用户和父 agent 能选择具名角色，执行明确分配的任务；支持同步结果和后台任务句柄。
- 后台任务在调用方继续工作、父 run 正常结束或 TUI 断开后继续，能查询、取消和在重连后恢复可观察状态。
- 定义与后台入口仍使用同一受限 child runner、权限门和服务级资源池；本子项以只读 child 为边界，后续工作树写入单独验收。

## 功能需求

- **F1 定义发现与校验：** 提供内建 `explore`、`plan` 与本阶段只读的 `general-purpose` 角色。加载用户和授权项目中的 Markdown 定义；按内建、用户、项目优先级覆盖同名项。角色目录、名称、description、正文、model、tools/disallowedTools、maxTurns、background 均受校验和长度限制；单文件错误不阻断其它有效定义，列表显示来源、有效能力与拒绝原因。手动重载重新生成完整快照，已删除或无效的定义不继续生效。
- **F2 入口：** 用户可查看/重载角色，并通过具名角色启动一个 session 所属后台任务。父 Session/Goal agent 可调用同一具名入口，选择同步等待或后台返回。后台启动返回稳定的任务 ID、独立 run ID 和初始状态；未知角色、缺少指令、输入超限或资源池满时不报告成功启动。定义设置 background 时强制后台执行。
- **F3 权限和上下文：** child 只收到角色正文与本次显式任务，不附父历史或兄弟结果。沿用父 provider、授权项目根和 permission bounds；模型默认继承，可在相同 provider 内使用明示 model，不能切换凭据或增加 provider。有效工具是现有只读工具集与定义工具规则的交集；写入、命令、MCP、网络、递归委派不可用。定义或调用参数不能提升父权限。
- **F4 生命周期：** 同步请求等待 child 终态。后台请求成功入队后立即返回；排队、运行、阶段摘要及终态持久关联任务与所属 session/工作项。父正常完成、TUI 断开不取消已接受任务；父 run 被显式取消时取消当时归属它的 queued/running 任务。用户可独立取消指定任务；其它 session/父 run 的任务继续。
- **F5 查询与结果交接：** 用户和父 agent 可列举当前 session 的任务、查询指定任务状态与脱敏摘要、取消任务；可选择有界等待终态。终态可按游标重放。完成/失败/取消通知进入所属 session 的可观察事件和下一次父 run 上下文；正常完成不自动发起新的模型 run。重复查询不会重新执行任务，重复通知按任务/终态事件去重。
- **F6 预算：** 使用同一个 3 workers、32 queue 的池；单项最多 8 轮、3 分钟、50,000 字节工具输出和 8 KiB 摘要；编码后的任务输入最多 64 KiB。定义 maxTurns 仅可缩短轮次，调用 timeout 仅可缩短时间。后台入口不能增加独立 worker 池，也不能通过未入队 goroutine 绕过内存与队列约束。查询等待最多 30 秒，不持有 child worker。
- **F7 失败与恢复：** 队列满、provider 错误、输入/轮次/输出预算耗尽、timeout、取消、持久化失败都有明确状态与原因。服务重启把持久 queued/running 任务标为 interrupted，不重新调用模型；重复恢复不重复终态。终态记录失败时保留可被重启恢复的非终态事实，并向客户端报告记录失败，不能把它伪装成已持久成功。
- **F8 展示与兼容：** TUI 展示角色目录、后台任务 ID/名称/状态/阶段摘要及结果；重连后显示同一状态。任务工具调用和结果配对，正文和思考流不会通过定义列表、后台事件或摘要泄露。M09-A 批量 Session 委派、M09-B fork skill、M09-C hook agent、普通 run 与权限/候选流程保持有效。

## 拟定的配置与调用约定

以下是需要本轮审批的具体默认值，不将其视作既有用户偏好：

- 定义目录采用 `~/.config/stable/agents` 和 `<授权项目>/.stable/agents`；不自动读取 `.mewcode/agents`。
- Markdown frontmatter 接受 `name`、`description`、`model`、`tools`、`disallowedTools`、`maxTurns`、`background`；正文为角色指令。model 缺省或 `inherit` 继承当前模型。unknown field 报配置错误。
- 定义文件最多 64 KiB；角色名称最多 64 字节，description 最多 256 字节；工具名称只允许已知工具，不用通配符扩大能力。文件或定义目录中的符号链接不加载。
- 源端 `permissionMode`、`isolation`、`team_name` 等扩大执行范围的参数不在 D 中悄悄降级；明确拒绝并提示对应后续子项。`effort`、skills、hooks、memory、MCP 预载等源端解析但没有完整运行行为的扩展字段列为后续对照项，不宣称支持。
- 用户入口：`/agents [reload]`、`/agent <角色> <任务>`、`/tasks [get <任务ID> | stop <任务ID>]`。具名 slash 任务默认后台，状态页显示本阶段角色均只读。
- 父 agent 工具：`run_agent`、`task_output`、`task_stop`。已有 `delegate_tasks` 和 todo 工具含义不变。

## 非功能需求

- **N1 隔离：** 所属 session、父 run 和项目根由服务端受信请求构造，不能通过 job ID、definition 路径或调用参数跨 session 查询/取消或越权读取。
- **N2 有界资源：** 使用池的原子入队结果后才报告接受；不为未接受请求建立无限 goroutine、通知队列或内存历史。任务列表分页，内存只保留活动任务与有界近期投影，已终结结果从持久日志查询。
- **N3 持久一致：** 事件先记录后广播；队列、取消和完成竞争只产生一次合法终态。终态包含固定关联字段，重连按游标去重。
- **N4 隐私：** provider credential、定义正文和思考流不进入后台状态事件；摘要与错误使用现有脱敏/截断规则。任务调用者显式指令遵循既有 tool input 的审计与脱敏规则，不新增原始 child transcript。
- **N5 验证：** fake provider/runner 和临时定义目录覆盖全部行为；本机只执行轻量定向检查。重型构建和集成继续使用已授权的 GitHub Actions。

## 本子项边界

团队成员多轮驻留、成员间消息、共享依赖任务板、coordinator 模式交给 M09-E；文件写入、命令和工作树交给 M09-F。D 的后台任务在重启后仅恢复事实与中断状态，不自动恢复模型执行。长期目标的成功、证据或候选接收不会由后台摘要改变。

## 验收标准

- **AC1（F1）：** 临时用户/项目目录和内建角色验证覆盖顺序、删改重载、非法 YAML/字段/名称/工具/超长文件/符号链接及来源；有效项继续可用。
- **AC2（F2/F4）：** fake child 屏障证明同步等待、后台成功入队后立即返回 ID、definition background 强制异步；错误输入和队列满不报告已接受。
- **AC3（F3/N1）：** fake provider 捕获正文/任务、model、工具；执行器验证授权根与 permission bounds；禁止能力和越权路径都失败，定义不能扩大权限。
- **AC4（F4/F5）：** 父完成/TUI 断开后任务继续；指定任务和父取消能停止关联 queued/running 项，另一父/session 不受影响；用户与父工具都读到同一终态。
- **AC5（F5/N3）：** 任务事件和独立 run 支持游标续读；完成通知可回流下一父 run；重复查询不重跑，重复订阅不重复投影，completed/failed/canceled 通知不会自动启动新 run。
- **AC6（F6/N2）：** 同步/后台/fork/hook 共用池，默认 worker/queue 与全部单项上限生效；超限返回明确原因；等待查询有界且无额外 child worker。
- **AC7（F7/N3）：** provider 失败、timeout、取消、入队/终态写盘失败覆盖；queued/running 重启只追加一次 interrupted，无 provider 重启调用，tool call/result 仍配对。
- **AC8（F8/N4）：** 定义正文、凭据、thinking/raw transcript 不进入目录列表、状态、session log 和 TUI；有效摘要/错误可见且有长度限制。
- **AC9（F8/N5）：** M09-A/B/C、todo、普通 Session/Goal run、权限门、候选与独立目标验证回归通过；GitHub Actions 记录准确提交 SHA、workflow/run 及失败项。
