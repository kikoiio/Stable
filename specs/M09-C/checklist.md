# M09-C Hook Agent 执行 Checklist

> 状态：已批准（2026-10-07）。按已批准 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md) 验收；未完成项不得标记通过。

## 功能验收

- [ ] **AC1 Agent action 路由：** `message` 优先，空时回退 `command`；两者都空时配置报告错误；command/prompt/http 仍使用原行为。
- [ ] **AC2 Session/Goal 事件覆盖：** run_start、run_end、pre_tool_use、post_tool_use 都可触发 agent child；提示仅含 action 指令和当前事件字段，不含父历史。
- [ ] **AC3 权限与继承：** fake provider 确认父 provider/model、WorkRef、授权根和 permission bounds 传递正确；child 只用 M09-A 只读工具集，拒绝写入、命令、MCP、网络和递归委派。
- [ ] **AC4 静态拒绝：** `reject: true` 不由模型输出改变；pre_tool_use 在 permission gate 与工具之前同步完成；`async: true` 不绕过；`on_error: reject` 只在失败时拒绝。
- [ ] **AC5 同步/异步结果：** 同步 action 等待终态；除 pre_tool_use 外，async 立即返回并在完成后 journal、更新事件、排入通知队列。
- [ ] **AC6 预算和失败：** 3 workers、32 queue、8 rounds、3 分钟、50,000 字节工具输出、8 KiB 摘要和 64 KiB 输入有硬限制；队列满、超限、provider 错误、超时、取消和中断均有明确结果并遵循 `on_error`。
- [ ] **AC7 事件和恢复：** queued/running/terminal 事件关联正确父 run 并可按游标续读；父取消停止当时已排队/运行 child；取消终态后的 run_end agent hook 执行一次有界任务；服务重启将未完成项标记 `interrupted` 且不重跑。
- [ ] **AC8 隐私：** HookFired 记录 child run ID；任务结果走既有脱敏和长度限制；session log、TUI 和父 agent 均不暴露思考流或 child 原始 transcript。

## 兼容回归

- [ ] M07-B command/prompt/http action、配置加载/热重载、静态拒绝、hook 顺序与通知回流回归通过。
- [ ] M09-A `delegate_tasks` 批量 Session 委派和资源池行为回归通过；`RunBatch` 仍拒绝 Goal work。
- [ ] M09-B slash 与 LoadSkill fork skill、上下文模式、取消和中断恢复回归通过。
- [ ] Session/Goal 普通 run、工具调用事件配对和 TUI 游标续读回归通过。

## 验证记录

- [ ] fake-provider 验证不调用真实 provider 或外网，并覆盖 AC1–AC8。
- [ ] 本机只运行轻量定向验证；重型集成验证在 GitHub Actions 执行，并记录 workflow/run 链接、结论及失败项。若云端运行未完成或失败，明确记录未覆盖部分，不标记通过。
- [ ] 最终 diff、格式化和文档状态检查完成；没有未审阅的生成文件或运行结果。
