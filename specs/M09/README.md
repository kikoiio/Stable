# M09 并行协作与工作树：范围与进度

> 2026-10-07 核对。M09 **整体未完成**。依据 [迁移地图](../../mewcode-migration-map.md) 中 M09 的完整范围核对源项目；不能把 M09-A/B/C 的验收通过等同于整个 M09 完成。后续子项目编号与拆分为本轮建议，尚未批准运行实现。

## 已完成

| 子项 | 可观察交付 | 证据 |
|---|---|---|
| M09-A | `/delegate`、同步并行只读委派、共享资源池、事件与取消/中断恢复 | [checklist](../M09-A/checklist.md)，全部勾选 |
| M09-B | fork skill 的 slash/LoadSkill 入口、上下文隔离、只读执行与恢复 | [checklist](../M09-B/checklist.md)，全部勾选 |
| M09-C | Session/Goal 四类 hook agent、同步拒绝与异步结果、父取消与 run_end 恢复 | [checklist](../M09-C/checklist.md)，全部勾选；代码提交 `c621de8` 的 [Go](https://github.com/kikoiio/Stable/actions/runs/37585087842) 与 [E2E](https://github.com/kikoiio/Stable/actions/runs/37585087864) 全部通过 |

上述代码已在分支 `codex/m09-c-hook-agent-validation-20261007` 提交并推送。完成状态指实现及验收，不代表已经合并到主分支。

## 剩余范围

| 建议子项 | 源端行为与文件 | 当前缺口 | 实施前置 |
|---|---|---|---|
| M09-D agent 定义与后台任务 | `internal/agents/definition.go`、`loader.go`、`agent_tool.go`、`subagent.go`；具名角色、定义覆盖、后台启动、状态/摘要、取消、完成通知 | Stable 尚无 agent 定义目录、通用具名 agent 入口、后台任务列表/查询/取消；hook async 和 slash fork 仅覆盖特定入口 | M09-A/C 资源池、M05 持久恢复；本轮已准备四份待审批文档 |
| M09-E 团队与协调器 | `internal/teams/teams.go`、`tools.go`、`sharedtask.go`、`tasktools.go`、`runner.go`、`protocol.go`、`coordinator.go`；团队成员多轮驻留、点对点/广播消息、依赖任务板、计划/关闭请求、纯协调工具集 | 尚无团队注册、持久消息、团队任务板、成员继续工作/空闲循环或 coordinator 模式 | M09-D 后台生命周期；写入成员依赖 M09-F。需独立规格审批 |
| M09-F 受控工作树与并行写入 | `internal/worktree/`、`internal/tools/enter_worktree.go`、`exit_worktree.go`，以及 `Agent` 的 `isolation: worktree`；创建/进入/退出/保留、会话恢复、变更预览、agent 工作树清理 | 尚无工作树工具、工作树归属或隔离写入 child；所有现有 child 固定只读 | M03/M04 候选接收与权限门；需要单独设计私有 Git 管理区、候选导出与冲突处理，并审批 |

源文件位于本机 `/home/neo/Projects/mewcode-golang`。核对使用源码读取，未执行源项目、真实 provider 或重型构建。

## 全部完成的条件

- [x] M09-A/B/C 已实现并通过对应 checklist。
- [ ] M09-D 的四份规格文档获批，实现与验收通过。
- [ ] M09-E 的四份规格文档获批，实现与验收通过。
- [ ] M09-F 的四份规格文档获批，普通任务/长期目标的修改均通过既有候选检查与用户接收，agent 无法自行合并到正式工程。
- [ ] 与源端 agent、团队、后台任务、工作树的行为逐项对照；每个差异写明已适配、源端仅解析、平台不适用或尚未完成，不能用目录迁移代替行为验收。
- [ ] GitHub Actions 上完成组合回归：同步委派、fork skill、hook agent、后台任务、团队消息/取消/恢复、并行工作树候选与冲突接收。

Linux 为当前首发平台，源端 iTerm 专用后端不属于本轮 Linux 验收；tmux/进程内后端取舍须在 M09-E 规格明确。源端仅解析且没有运行效果的 definition 字段单列核对，不能无依据宣称实现了相关能力。

## 下一步

审阅 [M09-D spec](../M09-D/spec.md)、[plan](../M09-D/plan.md)、[task](../M09-D/task.md)、[checklist](../M09-D/checklist.md)。它们是可审阅草案；审批后按任务 DAG 开始运行实现。M09-E/F 的范围仍列为待完成，不提前标记通过。
