# M09-C Hook Agent 执行 Task

> 状态：已批准（2026-10-07）。任务依赖已按 M09-C Plan 排列；Checklist 全部获批前不开始实现。

## 任务与依赖

| ID | 任务 | 依赖 | 覆盖 |
|---|---|---|---|
| T1 | 扩展共享池单项 `RunTask` 支持 Session/Goal，复用现有 workers、队列、child runner、权限、budget、取消和事件 reporter；保持 `RunBatch` Session-only | 无 | AC3、AC6、AC7 |
| T2 | 将父 run/work 执行范围从 Conversation/ToolExecutor 传给 HookRunner 的 run_start/run_end/pre_tool_use/post_tool_use 调用 | 无 | AC2、AC3、AC4 |
| T3 | 实现 HookAgentCoordinator 与 HookGate 分派：提示字段校验和回退、事件上下文编码、输入/timeout 限额、同步/异步、错误映射和静态拒绝流程 | T1、T2 | AC1–AC6 |
| T4 | 持久化 child run 关联、async task 跟踪与取消、重启中断恢复；在 TUI 展示 hook 子项状态和摘要 | T3 | AC5、AC7、AC8 |
| T5 | 完成 fake-provider 与回归验证；在已授权的 GitHub Actions 上运行重型集成验证并记录结果 | T1–T4 | AC1–AC8 |

## T1 — 共享池单项 Session/Goal 执行

- 为 `agent.Delegator`/`PoolDelegator` 增加单项 `RunTask`，使用同一 service-scoped FIFO worker/queue。
- 单项验证允许 `WorkSession` 与 `WorkGoal`；批量 `RunBatch` 保持现有 Session-only 校验及行为。
- 复用 child run ID、只读 runner、父 provider/model、WorkRef、项目授权根、permission bounds、预算、进度与取消路径。
- 队列满、输入超限、无效 parent scope、取消和 child 失败都返回确定的终态及原因。
- 增加定向验证：Session 与 Goal 都可提交；并发和队列边界、取消、预算仍生效；既有批量委派行为不变。

## T2 — 父 run/work 范围传递

- 为 HookRunner 增加内部调用范围，至少包含父 run ID、WorkRef、provider/model、项目根、permission bounds 与 child executor factory。
- run_start/run_end 从当前 run 请求构建范围；工具前后 hook 从 `ToolExecutor` 当前 `RunRequest` 传入范围。
- 保持 pre_tool_use 在权限门和工具执行前，post_tool_use 在工具结果产生后；保留现有工具名、参数、路径和消息字段。
- 更新 HookRunner fake 与实现，验证 Session/Goal 两种 run 的父范围没有串用，且旧 hook 调用顺序不变。

## T3 — Hook agent 执行与错误语义

- 在 `internal/conversation` 新增 HookAgentCoordinator；由 HookGate 识别 agent action 并调用注入的执行器，其他 action 继续走现有路径。
- `message` 优先，空时回退 `command`；两者都空则配置校验失败。按 JSON 编码的提示与事件 payload 检查 64 KiB 输入上限。
- 通过共享池 `RunTask` 提交一项只读子任务；使用当前 hook 事件字段，不加载父对话历史。
- 0 timeout 使用 3 分钟；正 timeout 截到 3 分钟上限。超限、队列满、provider 错误、超时和取消映射为可观察结果。
- 同步 action 等待终态；非 pre_tool_use async action 由服务生命周期跟踪且立即返回；pre_tool_use 始终同步。
- 成功摘要返回 hook 结果；失败遵循 `on_error`。`reject: true` 保持静态拒绝，模型输出不产生动态 allow/deny；pre_tool_use 执行顺序覆盖 permission gate 和工具。
- 对输出执行既有 provider credential 脱敏与字符限制；不保存思考流或 child transcript。

## T4 — 事件、取消、恢复与展示

- 在 HookFired 中增加可选 child run ID，并扩展校验、投影和审计测试。
- DelegationEvent 使用父 run ID 流关联 hook ID/task name；queued/running/terminal 状态可被现有游标订阅恢复。
- DelegationEvent 保留所属 session ID，使 async run_end 的父 run 流在 run 已结束后仍可写入。
- 跟踪异步 action 的 task handle：父 run 取消时停止当时已排队/运行 child；post-terminal run_end hook 继续启动一次有界任务，由 service lifetime 管理。
- 服务关闭或重启时中止未完成的 child，恢复投影为 `interrupted`，不自动重跑。
- run_end 输出仍记录到原父 run 的 hook 事件并进入现有通知队列；TUI 展示 hook ID、状态、阶段摘要和终态，不展示思考流/child transcript。
- 验证重启恢复幂等、child 与父 run 关联正确、通知脱敏截断和 run_end 取消后的行为。

## T5 — 验收与回归

- fake-provider 覆盖 Session/Goal 四类事件、provider/model 继承、只读工具边界、输入和时间预算、async 和同步行为、pre_tool_use 拒绝顺序。
- 覆盖 `message`/`command` 回退、缺少提示、`on_error`、队列满、timeout、父取消、服务重启中断和游标恢复。
- 回归 M07-B 的 command/prompt/http、hooks 加载和静态 reject；回归 M09-A `delegate_tasks` 和 M09-B fork skill。
- 不调用真实 provider 或外网；本机只运行轻量定向验证。重型集成验证按用户已授权范围使用 GitHub Actions，并在 checklist 记录 workflow/run 结果。

## 完成定义

- T1–T5 均完成，AC1–AC8 有可复核的验证证据。
- M07-B 既有 action、M09-A 批量委派和 M09-B fork skill 回归通过。
- 运行文档记录 GitHub Actions 验证结果；若云端运行未完成或失败，如实记录原因和未覆盖项。
