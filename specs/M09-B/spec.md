# M09-B fork skill 执行 Spec

> 状态：已批准（2026-10-07）。本项承接 M09-A 的只读子 agent 能力，启用 M07-A 保留的 fork skill 入口。

## 背景

M07-A 已支持发现和列出 fork skill，也保留了 `/技能名` 与 `LoadSkill` 两种入口，但调用时会提示 fork 尚未启用。M09-A 已提供可复用的只读子 agent、共享资源池及协作事件能力。本项将 fork skill 的正文和调用参数交给隔离的子 agent 执行，并用 `fork_context` 决定它可见的父会话上下文。

## 目标

- `/技能名 <参数>` 在当前 session 启动独立 fork run；任务状态与最终摘要可持久化、展示并按游标续读。
- 父 agent 调用 `LoadSkill` 时启动 fork 子 agent；返回结果作为工具结果交还父 agent，由父 agent继续汇总。
- 子 agent 使用技能正文、调用参数和对应上下文模式；不接收思考流或其它会话的内容。
- 子 agent 仅使用 M09-A 的只读工具集，并继承当前会话授权根及父 run 的 provider/model。
- 保留 M07-A 已有的 fork 字段解析和技能激活记录；fork 执行不向父会话内联注入技能正文。

## 功能需求

- **F1 fork skill 执行：** 被标记为 fork 的技能可从斜杠命令或 `LoadSkill` 入口执行。斜杠命令创建当前 session 的独立 fork run；`LoadSkill` 在调用它的父 run 内执行子 agent，并将结果返回为工具结果。inline skill 行为保持不变。
- **F2 技能指令与参数：** 子 agent 使用技能正文及按 M06 规则渲染的参数作为任务指令。技能正文不以内联方式写入父会话消息；会话日志只记录技能调用元数据及执行结果。
- **F3 fork 上下文模式：** `fork_context: none` 不包含父会话历史；`recent` 包含最近 5 轮可见对话；`full` 包含调用时可见的完整父会话历史。所有模式都排除思考流；`full` 受现有上下文预算限制。未设置时沿用 M07-A 的 `none` 默认值；未知模式明确报错且不启动子 agent。
- **F4 子 agent 边界：** 子 agent 沿用父 run 已配置的 provider/model 与授权项目根，只提供 M09-A 的读、搜、列工具，不开放写入、命令、MCP、外部网络或递归委派。fork run 的权限不超过其 session/父 run。
- **F5 进度与结果：** 斜杠入口展示 queued、running、阶段摘要与终态，并持久化到 session run 事件，支持游标续读；`LoadSkill` 的子项事件关联调用它的父 run。成功返回摘要，失败、中断或取消返回明确原因；思考流与 child 原始 transcript 不对用户或父 agent 展示。
- **F6 生命周期与预算：** fork skill 使用 M09-A 共享资源池和单项限额。取消所属 run 时停止对应子 agent；服务重启后未完成项标记为 `interrupted`，不自动重跑。
- **F7 激活审计：** 两种入口都记录 `skill_invoked`，区分 slash 与 tool 来源；参数、摘要和错误沿用现有脱敏与大小限制。正文读取失败或任务参数无效时给出可观察错误。

## 非功能需求

- **N1 权限安全：** fork skill 子 agent 复用 M09-A 的只读工具和现有授权根校验；上下文模式只能增加可见历史，不能扩大工具或文件权限。
- **N2 资源有界：** 使用 M09-A 的资源池及预算：同时运行 3 项、排队最多 32 项；每项最多 8 轮、3 分钟、50,000 字节工具输出、8 KiB 结果摘要。技能正文与渲染参数合计最多 64 KiB；超出上限时拒绝启动。
- **N3 记录隐私：** 不记录思考流或 child 原始 transcript；只持久化技能调用元数据、协作状态、过滤后的摘要和错误。`recent`/`full` 仅按用户选择将历史提供给子 agent，并受脱敏和模型上下文预算约束。
- **N4 一致性：** slash fork run 遵循现有 session run 的事件序列、终态和游标语义；父 run 内的 `LoadSkill` 协作事件与父 run 事件序列一致。重复恢复不得重跑已中断的任务。
- **N5 兼容性：** inline skill、现有技能发现与热更新行为保持不变；`mode: fork` 与旧式 `context: fork` 均可触发 fork 行为。仅面向 Linux 本机会话，不改变 M03/M04 受控接收及 M00 目标证据边界。

## 不做的事

- 不改变 M09-A 的 `/delegate` 与 `delegate_tasks` 通用任务委派语义。
- 不实现 hook agent 动作；hook 调用 fork agent 留给后续 M09 子项。
- 不实现后台子任务、服务重启后恢复执行、工作树或分支写入。
- 不向 fork skill 开放写文件、命令、MCP、网络、递归委派或新的权限审批路径。
- 不改变 inline skill 的激活、参数渲染、上下文注入或会话审计行为。
- 不实现额外的 `fork_context` 模式或用户自定义上下文窗口；本项仅支持 `none`、最近 5 轮的 `recent`、预算约束下的 `full`。
- 不支持跨项目、跨会话的 fork 调用，也不把子 agent 输出自动转成候选接收、目标证据或状态变更。

## 验收标准

- **AC1 斜杠入口：** fork skill 命令创建当前 session 的独立 run，显示排队/运行进度及最终结果；run 可从事件日志重放。inline skill 仍按原路径执行。
- **AC2 工具入口：** 父 run 调用 `LoadSkill` 时启动该 fork skill 的子 agent，工具结果包含子项终态、摘要或错误；父 agent 可继续生成最终回复。
- **AC3 上下文模式：** fake provider 记录到的输入符合 `none`、最近 5 轮 `recent`、预算内 `full` 三种定义；不包含思考流或其它 session 内容。未知模式拒绝启动。
- **AC4 权限与隔离：** 子 agent 只能使用读、搜、列工具；尝试写文件、命令、MCP、网络或递归委派均被拒绝，越权路径访问失败，项目文件无变化。
- **AC5 结果与失败：** 成功、失败、取消和中断结果都有可读终态；单项失败不丢失同批其它结果；slash 用户和调用 `LoadSkill` 的父 agent 都能看到相应结果或原因。
- **AC6 预算与取消：** 单项不超过 8 轮、3 分钟、50,000 字节工具输出、8 KiB 摘要；子项输入不超过 64 KiB；父 run 取消会停止其 fork 子项。
- **AC7 重启与续读：** slash fork run 和父 run 中的 fork 子项事件可按游标恢复；服务重启将未完成任务和父 run 标为 `interrupted`，工具调用结果配对，恢复幂等且不重跑。
- **AC8 审计与回归：** fork 调用产生来源明确的 `skill_invoked` 记录，不把技能正文以内联用户消息写入父会话；脱敏及摘要限制生效。现有 inline skill、普通 run 与 M09-A `delegate_tasks` 回归通过。
- **AC9 Linux 验证：** fake-provider e2e 覆盖 slash 与 `LoadSkill` 两种入口、父结果交接、上下文隔离、只读边界、取消和重启恢复；不调用真实 provider 或外网。
