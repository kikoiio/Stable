# M09 并行协作与工作树：范围与进度

> 2026-10-09 核对。M09 **整体未完成**。依据 [迁移地图](../../mewcode-migration-map.md) 中 M09 的完整范围核对源项目；不能把 M09-A/B/C 的验收通过等同于整个 M09 完成。D/E/F 实现改动已提交并推送。SHA `99fa8502845d033dd0720c12aacd49d8f109a479` 的 Go build/unit、package、全量 E2E 与 M09 Workspace Linux 全部通过；新增真实 TUI/service 停止请求路径。后续补入 TUI 创建工作树并在 service 重启后恢复列表的定向测试（本地 `-count=3` 通过，待该增量 SHA 的云端组合验证）；E/F checklist 仍有逐项验收缺口。

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
| M09-D agent 定义与后台任务 | `internal/agents/definition.go`、`loader.go`、`agent_tool.go`、`subagent.go`；具名角色、定义覆盖、后台启动、状态/摘要、取消、完成通知 | AC1–9 与完整场景均有逐项证据；候选接受后须独立复核才 verified | SHA `8223aaf` 的 Go build/unit、package 与全量 E2E 通过；[checklist](../M09-D/checklist.md) 保留证据映射 |
| M09-E 团队与协调器 | `internal/teams` 的 scope/预算/任务图，sessionlog typed facts/projection；shared-pool member 首轮 spawn、多轮显式/消息唤醒、child run 持久化/中断恢复、消息批次 handoff、plan/shutdown 请求、强停/延迟关闭、团队/task 工具与 TUI 命令；coordinator 下一 run 开关持久化、team-only schema 和 executor 硬 allowlist；有界 FIFO capacity 队列与 grant generation 取消/关闭校验 | 本轮补入跨 session 消息/stop 拒绝、不同 Goal/root 消息授权、team task 与 M06 todo 独立、真实 TUI stop RPC 与源端行为核对；服务恢复、完整取消矩阵和 E/TUI 组合仍须逐项按 [checklist](../M09-E/checklist.md) 记录。E AC1–9 尚未整体验收 | SHA `99fa850` 的 [Go run 37888123267](https://github.com/kikoiio/Stable/actions/runs/37888123267) 与 [E2E run 37888123283](https://github.com/kikoiio/Stable/actions/runs/37888123283) 全部 jobs 成功；[Workspace Linux run 37888123295](https://github.com/kikoiio/Stable/actions/runs/37888123295) 成功；规格已批准 |
| M09-F 受控工作树与并行写入 | `internal/workspace` ownership、manifest、配额预算、私有 Git、materializer、独立 lifecycle service 与 B/F/W 合并；candidate metadata 事务和身份恢复；conversation/TUI 生命周期、按完整 B/F/W digest 绑定的逐路径 conflict resolution、用户 dirty-discard 预览/确认、D named-agent writer lease、受限文件工具和持久配额；CI 使用 disposable ext4 验证真实磁盘上限与 metadata 攻击 | 本轮补入 workspace TUI lifecycle session/run 绑定、真实 TUI create→service restart→list restore、candidate/review manifest policy mismatch 拒绝，以及真实 TUI→service 冲突决策→候选 review/accept 闭环；F AC1–9 的并行 export/accept、真实 writer 与故障恢复组合仍须逐项审计 | SHA `99fa850` 的 [Go run 37888123267](https://github.com/kikoiio/Stable/actions/runs/37888123267)、[E2E run 37888123283](https://github.com/kikoiio/Stable/actions/runs/37888123283) 与 [M09 Workspace Linux run 37888123295](https://github.com/kikoiio/Stable/actions/runs/37888123295) 全部通过；新 TUI lifecycle test 还需后续 SHA 验证；规格已批准 |

源文件位于本机 `/home/neo/Projects/mewcode-golang`。核对使用源码读取，未执行源项目、真实 provider 或重型构建。该目录没有 Git 元数据，源版本无法固定；E/F checklist 的源行为签证项因此仍保持开放。

2026-10-09 增补局部证据：E 新增真实共享池满载/FIFO、任务依赖 service/replay、父/兄弟历史隔离、消息 handoff 恢复、shutdown/plan restart、跨 session/Goal scope 和 team task/todo 隔离测试；F 新增冲突决策到候选导出、dirty clean-remove 保留、manifest policy mismatch、TUI lifecycle dispatch 和真实 TUI 到候选接受 service 闭环。SHA `6121f54`、`f61e2fb`、`35b130b`、`ac817fa`、`26eafc5`、`39bf411`、`220b070`、`99ef134`、`3488e7a`、`a4a4581`、`880f7f4`、`29eee81`、`1ca6e0e` 的 Go build/unit、package、E2E 全部 jobs 和 Workspace Linux 云端验证均通过。E/F AC1–9 的剩余范围见各 checklist，整体 M09 尚未完成。

本轮新增 E 的 TUI busy-member stop 重试 service 证据（SHA `99fa850` 三条 workflow 均通过），以及 F 的真实 TUI workspace create/restart/list-restore 定向测试（本地 `-count=3` 通过，云端组合待本次代码提交后复验）。这两项只缩小 AC5/AC9 与 AC3/AC6/AC8/AC9 的缺口，不表示对应 AC 已验收。

## 全部完成的条件

- [x] M09-A/B/C 已实现并通过对应 checklist。
- [x] M09-D 的四份规格文档获批，实现与验收通过；AC1–9 和完整场景见 [D checklist](../M09-D/checklist.md)。
- [ ] M09-E 的四份规格文档已获批；实现与验收待完成。
- [ ] M09-F 的四份规格文档已获批；受控工作树、候选检查与用户接收验收待完成。
- [ ] 与源端 agent、团队、后台任务、工作树的行为逐项对照；每个差异写明已适配、源端仅解析、平台不适用或尚未完成，不能用目录迁移代替行为验收。
- [ ] GitHub Actions 上完成组合回归：同步委派、fork skill、hook agent、后台任务、团队消息/取消/恢复、并行工作树候选与冲突接收。

Linux 为当前首发平台，源端 iTerm 专用后端不属于本轮 Linux 验收；tmux/进程内后端取舍须在 M09-E 规格明确。源端仅解析且没有运行效果的 definition 字段单列核对，不能无依据宣称实现了相关能力。

## 下一步

继续完成 [M09-E checklist](../M09-E/checklist.md) 与 [M09-F checklist](../M09-F/checklist.md) 的逐项证据映射和剩余组合场景。D 的 AC1–9 已完成核对；`dependency_change.sh` checker 恢复问题在 SHA `8223aaf` 的 `e2e-core` 通过。M09 整体仍需等 E/F 验收完成后再更新状态。
