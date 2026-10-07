# M09 并行协作与工作树：范围与进度

> 2026-10-07 核对。M09 **整体未完成**。依据 [迁移地图](../../mewcode-migration-map.md) 中 M09 的完整范围核对源项目；不能把 M09-A/B/C 的验收通过等同于整个 M09 完成。M09-D 实现已提交，最终 SHA 的云端验收进行中；M09-E/F 四份规格均已获用户批准，正在实施。

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
| M09-D agent 定义与后台任务 | `internal/agents/definition.go`、`loader.go`、`agent_tool.go`、`subagent.go`；具名角色、定义覆盖、后台启动、状态/摘要、取消、完成通知 | 主实现已提交；AC checklist 尚待逐项核对 | 最终代码 SHA `ccc3fbc` 的 Go、package、E2E GitHub Actions jobs 全部成功；按 [checklist](../M09-D/checklist.md) 完成条目映射后再标验收通过 |
| M09-E 团队与协调器 | `internal/teams` 的 scope/预算/任务图，sessionlog typed facts/projection；shared-pool member 首轮 spawn、显式 resume、child run 持久化/中断恢复、消息批次 handoff、plan/shutdown 请求、强停/延迟关闭、团队/task 工具与 TUI 命令；coordinator 下一 run 开关持久化、team-only schema 和 executor 硬 allowlist；显式续跑在 pool 满时进入有界 FIFO `waiting_capacity` 队列，容量信号唤醒，重启恢复为需显式续跑的 interrupted 状态；pool 在提交后发布失败会落 interrupted child/member 终态，恢复收敛 terminal-turn/member-state 间隙，service close 停止容量 watcher | 首轮 spawn 满额原子拒绝；等待续跑/恢复的更多故障组合与 compaction/重连/完整团队端到端验收仍需完成。SHA `8a34ea0` 的 Go build/unit、package 与通用 E2E workflow 全部通过；E AC1–9 未逐项通过 | M09-D 后台生命周期；E/F 规格已批准，实施中 |
| M09-F 受控工作树与并行写入 | `internal/workspace` 的 ownership、manifest、配额预算、私有 Git、materializer、独立生命周期 service 与 B/F/W 路径级合并基础；候选 project-v2 metadata 保全/交换代码；service 启动会保守收敛 workspace journal；runtime 注入专用 state root；conversation/TUI 提供 create/list/get/enter/exit/keep/clean-remove、绑定 B/F/W digest 的冲突路径摘要预览与无冲突 candidate 导出；D named-agent 可按请求或 definition isolation 获得单 writer lease、受限文件工具与持久配额账目 | 用户逐路径 conflict resolution、dirty discard 决策、硬磁盘 quota command、真实 writer sandbox 组合、完整 ownership/event projection、完整 lifecycle 与 Team+F 集成仍缺；`90ed0d7` 首次 Go 验证发现测试 fixture 漏设可信 authority，`75594bd` 已修复并等待复验；F AC1–9 未逐项通过 | M03/M04 候选接收与权限门；四份规格已批准，实施中 |

源文件位于本机 `/home/neo/Projects/mewcode-golang`。核对使用源码读取，未执行源项目、真实 provider 或重型构建。

## 全部完成的条件

- [x] M09-A/B/C 已实现并通过对应 checklist。
- [ ] M09-D 的四份规格文档获批，实现与验收通过。
- [ ] M09-E 的四份规格文档已获批；实现与验收待完成。
- [ ] M09-F 的四份规格文档已获批；受控工作树、候选检查与用户接收验收待完成。
- [ ] 与源端 agent、团队、后台任务、工作树的行为逐项对照；每个差异写明已适配、源端仅解析、平台不适用或尚未完成，不能用目录迁移代替行为验收。
- [ ] GitHub Actions 上完成组合回归：同步委派、fork skill、hook agent、后台任务、团队消息/取消/恢复、并行工作树候选与冲突接收。

Linux 为当前首发平台，源端 iTerm 专用后端不属于本轮 Linux 验收；tmux/进程内后端取舍须在 M09-E 规格明确。源端仅解析且没有运行效果的 definition 字段单列核对，不能无依据宣称实现了相关能力。

## 下一步

继续按 [M09-E task](../M09-E/task.md) 与 [M09-F task](../M09-F/task.md) DAG 实施。D 的最终 SHA 云端验收结果记录在 [M09-D checklist](../M09-D/checklist.md)。E/F 仍有实现与运行验收缺口，只有 D/E/F 全部验收通过后才更新整体完成状态。
