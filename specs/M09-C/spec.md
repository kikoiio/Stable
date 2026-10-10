# M09-C Hook Agent 执行 Spec

> 状态：已批准（2026-10-07）。本项承接 M07-B 保留的 `agent` hook action，复用 M09-A 只读子 agent 与 M09-B 的资源、事件和恢复约束。

## 背景

M07-B 已定义四类 hook 事件与 `agent` 动作字段，但 agent 动作仍在调用时报「未启用」。M09-A 提供只读子 agent、服务级资源池和协作事件；M09-B 将这些能力用于 fork skill。本项启用 hook agent，覆盖 Session 与 Goal 两类 run。

## 目标

- 启用 hook 配置中的 `agent` 动作，以 `message` 为任务提示，兼容空 `message` 时使用 `command`。
- 将 hook 提示和当前事件字段交给只读子 agent；不附父对话历史。
- 继承触发 run 的 provider/model、项目授权根和现有权限审批路径；复用 M09-A 的只读工具集、资源池、预算及持久协作事件。
- 同步 hook 等待子项结果；async hook 立即返回，完成后记录结果并将摘要加入现有 hook 通知队列。`pre_tool_use` 继续同步执行。
- 保持 M07-B 的拒绝语义：`reject` 是静态配置，模型输出不能改变工具调用的允许/拒绝决定。

## 功能需求

- **F1 动作输入：** `agent` action 使用非空 `message`；为空时回退到 `command`。两者都为空时配置校验报告错误。传入内容由提示和当前 hook 事件字段构成，包括事件名、工具名、工具参数、文件路径和消息；不传父会话历史、思考流或其它 session 内容。
- **F2 执行边界：** Session 和 Goal 的 run_start、run_end、pre_tool_use、post_tool_use 事件可触发 agent action。子项继承触发 run 的 provider/model、项目授权根和 permission gate，仅可使用 M09-A 的只读工具集；模型 provider 通信允许，写入、命令、MCP、外部网络工具和递归委派不可用。
- **F3 同步与异步：** 同步 hook 等待子项终态。非 pre_tool_use 的 async hook 立即返回，子项完成后持久化结果并将摘要排入现有通知队列。pre_tool_use 必须同步执行，以保持拒绝判断发生在权限门与工具执行之前，即使配置了 `async: true` 也如此。
- **F4 拒绝与失败：** `reject: true` 按 M07-B 语义静态拒绝；模型生成内容不作为动态允许或拒绝决定。子项失败遵循 `on_error`；`on_error: reject` 在 pre_tool_use 失败时拦截。provider 错误、超时、取消、中断和资源池拒绝都有明确原因。
- **F5 预算与事件：** 复用 M09-A 服务级资源池（最多 3 个并发、最多 32 个排队）和 M09-B 单项预算（最多 8 轮、3 分钟、50,000 字节工具输出、8 KiB 摘要、64 KiB 输入）。agent action 的 `timeout: 0` 使用 3 分钟；正值只能缩短时限。输入超限或队列已满时不启动子项，并按 hook 错误规则处理。hook 结果与协作状态关联至所属父 run 并持久化。
- **F6 生命周期与恢复：** 取消父 run 时取消关联子项；服务重启后将未完成子项标为 `interrupted`，不自动重跑。已持久事件支持游标续读。
- **F7 审计与隐私：** 记录 hook ID、事件、父 run 关联、协作状态、脱敏后的摘要和错误；不记录思考流或 child 原始 transcript。结果进入现有 hook journal 和通知回流路径，并遵循既有脱敏、截断规则。

## 非功能需求

- **N1 权限隔离：** 只读工具复用 M09-A 授权根校验和 permission gate。hook 输入不扩大文件、工具或网络权限。
- **N2 资源有界：** 使用 F5 的共享资源池和单项上限；超时不能超过 3 分钟。输入超限、队列满和其它启动失败可观察，且不启动超限子项。
- **N3 持久一致：** 协作事件写入所属 run 的事件流，按游标可恢复；服务重启只补记中断状态，不重跑未完成任务。父 run 取消会结束其子项。
- **N4 最小留存：** 输入上下文不含父对话历史；事件只保存脱敏、截断后的任务元数据、阶段摘要、结果和错误，不保存思考流或 child 原始 transcript。
- **N5 兼容与可观察：** Session 与 Goal run 均可看到 hook ID、排队/运行/终态和摘要，错误原因可查。M07-B 既有 hook 配置、拒绝语义及 `command`、`prompt`、`http` 动作不变。

## 不做的事

- 不引入模型基于自然语言输出动态批准或拒绝工具调用；`reject` 和 `on_error` 保持 M07-B 语义。
- 不给 hook agent 父会话历史，不添加额外上下文模式。
- 不创建脱离父 run 生命周期的独立后台任务，不在重启后恢复执行；未完成子项只标为 `interrupted`。
- 不开放写入、命令、MCP、外部网络、递归委派或工作树操作。
- 不改变 M07-B hook 配置、事件集合以及 `command`、`prompt`、`http` 动作；不实现 hook 编辑 UI、fork skill 或其它 M09 能力。

## 验收标准

- **AC1 动作路由：** `agent` action 接受非空 `message`，为空时兼容回退 `command`；两者都缺失时给出配置校验错误；现有动作仍走原路径。
- **AC2 两类 run：** Session 与 Goal 的四类 hook 事件都能启动 agent 子项；输入包含 hook 提示和对应事件字段，不包含父对话历史。
- **AC3 只读与继承：** fake provider 验证子项继承 provider/model 和授权根；只读访问遵循 permission gate，写入、命令、MCP、外部网络工具和递归委派不可用。
- **AC4 拒绝语义：** `reject: true` 保持静态拒绝；pre_tool_use 在权限门和工具执行前完成；`async` 不绕过该顺序；`on_error: reject` 只在失败时拦截；模型输出不会动态改变 allow/deny。
- **AC5 同步与异步：** 同步 hook 等待终态；非 pre_tool_use 的 async hook 立即返回，并在结束后持久化结果、通知和可读状态。
- **AC6 预算与错误：** worker、队列、轮次、时限、输入、工具输出和摘要上限均生效；队列满、输入超限、provider 失败、超时和取消有明确原因，并按 `on_error` 处理。
- **AC7 事件恢复：** hook/child 事件关联正确父 run，支持游标续读；取消父 run 会停止子项；重启把运行中子项标为 `interrupted` 且不重跑，恢复后事件投影一致。
- **AC8 隐私与兼容：** 不持久化思考流或 child transcript；参数、结果和错误通过脱敏与长度限制；M07-B 其它动作及 M09-A/B 既有行为保持有效。
