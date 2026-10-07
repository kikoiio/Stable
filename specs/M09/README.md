# M09 并行协作与工作树：范围与进度

> 2026-10-08 核对。M09 **整体未完成**。依据 [迁移地图](../../mewcode-migration-map.md) 中 M09 的完整范围核对源项目；不能把 M09-A/B/C 的验收通过等同于整个 M09 完成。D/E/F 实现改动已提交并推送；SHA `e24331a0ac6d9745f9dc80fc8aed0142d47d2210` 的 Go 全量、package 与 M09 workspace Linux 验收通过，通用 E2E 的 `e2e-core` 在 `dependency_change.sh` checker 版本恢复阶段失败。D/E/F checklist 尚有逐项验收缺口。

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
| M09-D agent 定义与后台任务 | `internal/agents/definition.go`、`loader.go`、`agent_tool.go`、`subagent.go`；具名角色、定义覆盖、后台启动、状态/摘要、取消、完成通知 | 主实现已提交；AC1–8 有证据映射，AC9 等待本轮 E2E 复验 | SHA `e24331a` 的 Go build/unit 与 package 通过；E2E run `37651671760` 的 `e2e-core` 在 dependency checker 恢复时失败。按 [checklist](../M09-D/checklist.md) 保持 AC9 待验 |
| M09-E 团队与协调器 | `internal/teams` 的 scope/预算/任务图，sessionlog typed facts/projection；shared-pool member 首轮 spawn、多轮显式/消息唤醒、child run 持久化/中断恢复、消息批次 handoff、plan/shutdown 请求、强停/延迟关闭、团队/task 工具与 TUI 命令；coordinator 下一 run 开关持久化、team-only schema 和 executor 硬 allowlist；有界 FIFO capacity 队列与 grant generation 取消/关闭校验 | 实现与自动 lead-message handoff 已接通，fake runner 用例在全量 Go 通过；服务恢复、取消/关闭和完整 E/TUI 组合仍须逐项按 [checklist](../M09-E/checklist.md) 记录。E AC1–9 尚未整体验收 | SHA `e24331a` 的 Go build/unit 与 package 通过；通用 E2E 尚待 `e2e-core` 完成；规格已批准 |
| M09-F 受控工作树与并行写入 | `internal/workspace` ownership、manifest、配额预算、私有 Git、materializer、独立 lifecycle service 与 B/F/W 合并；candidate metadata 事务和身份恢复；conversation/TUI 生命周期、按完整 B/F/W digest 绑定的逐路径 conflict resolution、用户 dirty-discard 预览/确认、D named-agent writer lease、受限文件工具和持久配额；CI 使用 disposable ext4 验证真实磁盘上限与 metadata 攻击 | Linux writer/sandbox/quota 组合 workflow 已通过；F AC1–9 的私有 Git、生命周期故障、用户决策完整闭环和 Team+F 组合仍需逐项审计，不把单项 workflow 当成整体验收 | SHA `e24331a` 的 Go build/unit、package 与 M09 Workspace Linux 通过；通用 E2E 尚待 `e2e-core` 完成；规格已批准 |

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

继续完成 [M09-E checklist](../M09-E/checklist.md) 与 [M09-F checklist](../M09-F/checklist.md) 的逐项证据映射；定位并复验 E2E `dependency_change.sh` checker 恢复失败。只有 D/E/F 全部验收通过后才更新整体完成状态。
