# M09 并行协作与工作树：范围与进度

> 2026-10-09 核对。M09 **整体未完成**。依据 [迁移地图](../../mewcode-migration-map.md) 中 M09 的完整范围核对源项目；不能把 M09-A/B/C 的验收通过等同于整个 M09 完成。SHA `6e79cf4ed5e02ebce6259ad8cd3009cebea14c56` 的 Go build/unit、package、E2E 全部六个 jobs 与 M09 Workspace Linux 均通过（[Go](https://github.com/kikoiio/Stable/actions/runs/37928211260)、[E2E](https://github.com/kikoiio/Stable/actions/runs/37928211231)、[Workspace Linux](https://github.com/kikoiio/Stable/actions/runs/37928211202)）。其后新增 E/F 定向回归尚未包含在该 SHA，将随下一 SHA 组合复验；E/F AC1–9 仍未整体验收。

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
| M09-F 受控工作树与并行写入 | `internal/workspace` ownership、manifest、配额预算、私有 Git、materializer、独立 lifecycle service 与 B/F/W 合并；candidate metadata 事务和身份恢复；conversation/TUI 生命周期、按完整 B/F/W digest 绑定的逐路径 conflict resolution、用户 dirty-discard 预览/确认、D named-agent writer lease、受限文件工具和持久配额；CI 使用 disposable ext4 验证真实磁盘上限与 metadata 攻击 | 本轮补入 workspace TUI lifecycle session/run 绑定、真实 TUI create→service restart→list restore、按 formal root 串行化 candidate acceptance、candidate/review manifest policy mismatch 拒绝，以及真实 TUI→service 冲突决策→候选 review/accept 闭环；F AC1–9 的真实 writer、跨进程 crash/恢复和完整并行 export/accept 矩阵仍须逐项审计 | SHA `8d1f5bf` 的 [Go run 37889881839](https://github.com/kikoiio/Stable/actions/runs/37889881839)、[E2E run 37889881688](https://github.com/kikoiio/Stable/actions/runs/37889881688) 与 [M09 Workspace Linux run 37889881682](https://github.com/kikoiio/Stable/actions/runs/37889881682) 全部通过；规格已批准 |

源文件位于本机 `/home/neo/Projects/mewcode-golang`。核对使用源码读取，未执行源项目、真实 provider 或重型构建。该目录没有 Git 元数据，源版本无法固定；E/F checklist 的源行为签证项因此仍保持开放。

2026-10-09 增补局部证据：E 新增真实共享池满载/FIFO、任务依赖 service/replay、父/兄弟历史隔离、消息 handoff 恢复、shutdown/plan restart、跨 session/Goal scope 和 team task/todo 隔离测试；F 新增冲突决策到候选导出、dirty clean-remove 保留、manifest policy mismatch、TUI lifecycle dispatch 和真实 TUI 到候选接受 service 闭环。SHA `6121f54`、`f61e2fb`、`35b130b`、`ac817fa`、`26eafc5`、`39bf411`、`220b070`、`99ef134`、`3488e7a`、`a4a4581`、`880f7f4`、`29eee81`、`1ca6e0e` 的 Go build/unit、package、E2E 全部 jobs 和 Workspace Linux 云端验证均通过。E/F AC1–9 的剩余范围见各 checklist，整体 M09 尚未完成。

本轮新增 E 的 TUI busy-member stop 重试 service 证据，F 的真实 TUI workspace create/restart/list-restore 定向测试，并发候选接受曾复现 formal transaction topology 损坏，修复为 formal-root 完整事务串行化；SHA `8d1f5bf` 的 Go/package、全部 E2E 与 Workspace Linux 均通过。这些增量只缩小 E/F AC 缺口，不表示对应 AC 已验收。

2026-10-09 继续补入 F 的 TUI create→conversation service restart→冲突决策→candidate review/accept 组合测试，SHA `d4f6677` 本地定向 TUI 测试连续 3 次通过。此 SHA 尚无云端 Go/package、E2E 或 Workspace Linux workflow 结果；测试通过临时 fixture 写工作树内容，真实 `run_agent` writer/sandbox 组合仍待补证。M09-E/F AC1–9 仍未整体验收，M09 整体保持未完成。

随后并行补入 E 任务 assignee 归属校验及 F workspace ownership 跨 Project/Goal/WorkItem 伪造 scope 拒绝测试，分别记录于 E/F checklist；定向测试通过。SHA `a156e76` 包含两项实现/测试，需等待该 SHA 的云端组合验证；SHA `23e89f1` 的 Go 与 Workspace Linux workflow 已通过，E2E 仍在运行。E/F 整体验收仍未完成。

继续并行补入 E 父 run 正常完成后已接收 child 继续运行、已完成任务拒绝成员认领，以及 F clean-remove 保留 ignored/private Git 数据、活跃 writer/binding 防移除和 manifest 字节配额精确边界测试；对应定向测试通过，记录在 E/F checklist。SHA `b0d994c` 包含这些新增 E/F 证据，待云端复验；M09-E/F 仍有广泛 AC 与组合场景开放。

随后补入 E coordinator 对伪造 direct MCP tool name 的 hard-deny 测试与真实 TUI create→spawn→send→messages→get service 闭环；F 修复 rewind 和 acceptance 在 finalized→cleanup 崩溃窗口留下 spent staging 的问题，并验证 staging identity 被替换时拒绝删除、receipt/formal root 状态一致后才清理。SHA `c74202a` 包含 acceptance cleanup 实现，`eccc28d` 增加 finalize 前后替换 spent root 的负向恢复测试；SHA `b21ece0` 补上 handoff event append 失败的持久补偿与显式重试。SHA `121ccec` 增加 stop failure/deadline 下保留 blocked writer lease，`4ed4544` 增加真实 TUI/service 两轮摘要与待处理消息续接；`4463358` 扩展 coordinator direct-tool 拒绝矩阵，`c0d2262` 覆盖有界消息批次，`f2d6ac4` 覆盖 mode-only 三方合并/冲突，`748cead` 验证 broadcast recipient snapshot。SHA `15371d8` 的云端 Go 因两个测试夹具缺陷失败、Workspace Linux 成功，E2E 因夹具缺陷及 dependency-change runtime 启动失败；夹具已在 `8500c24` 修正。`9d18862` 的云端复验仍在运行。审阅还确认普通 session run 尚未从 workspace binding 派生 writer authority，不能在缺少 lease/quota/accept 闭环时只改根路径。后续提交等待云端验证，M09-E/F 仍未整体验收。

`6e79cf4ed5e02ebce6259ad8cd3009cebea14c56` 的 Go build/unit、package、E2E 六个 jobs 与 Workspace Linux 全部成功。随后并行补入 E 的 Goal/WorkItem 消息列表 scope 拒绝及同一 member 的串行 turn/显式续跑证据；F 的 dirty/untracked/ignored private baseline、rewind 遇未知受保护 metadata 时 blocked 并保留数据、leased file tool 拒绝 checkout symlink 逃逸证据。上述新定向测试及四包合并定向验证均通过，随后 F AC7 又补入生产 16 MiB 单文件 quota 精确边界测试，E AC4 补入跨 team/伪造 task ID 无副作用测试，定向测试通过；这些新改动尚未云端复验。M09-E/F AC1–9 与源端差异、真实 sandbox/恢复组合仍有开放项，整体 M09 保持未完成。

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

最新验证更新：SHA `dfaf16b` 的 Go `build-and-test`/`test-package` 与 Workspace Linux `writer-sandbox-volume` 全部通过；E2E 的 `e2e-sessions`、`e2e-m03`、`e2e-m04` 与 `cases` 成功，`e2e-core` 和 `unit` 当时仍在运行。随后新增 SHA `69484b3` 的 lead/member 8 KiB 消息边界测试，等待与本轮其他增量一起云端验证。普通 session run 未从 workspace binding 派生 writer authority 仍是 M09-F 未完成项，未将 M09 标记完成。

2026-10-09 后续补入 E 的 32 KiB 消息 handoff、pending aggregate quota、父 run 失败后 child 续行、plan request 冲突重复响应、child terminal credential 脱敏与 stop 不伪完成 task 证据；F 修复候选导出失败残留路径与 remove recovery stale binding 重试，并补 workspace root replacement、writer cancel/restart generation、manifest file-count 和 binding cleanup 边界测试。SHA `d6be6bd` 与其后 SHA `7413680` 的 Go build/unit、package、全部 E2E jobs 与 Workspace Linux 均通过；更新后的本地提交等待云端复验。普通 session lead run 现已从 active workspace binding 获取独占 generation lease，通过受信 per-run executor 写入 checkout；RunStarted 保存 workspace ID/generation，取消后等待 runner 实际退出再 settle lease，并在 failure/start 与 service close 路径回收。定向测试覆盖 checkout/formal 隔离、启动失败、恢复、admission race、Keep/Exit 和 Close。该切片尚未云端验证；Goal、AgentTask 和 team writer 不在本次范围。M09-E/F AC1–9 及组合验收仍开放，M09 整体保持未完成。

`48be60c` 的首次云端组合复验中，Go/unit 与 Workspace Linux contract 失败于两个旧 workspace test fixture 缺失 binding-admission capability；E2E `unit`/`e2e-m03` 同因失败，已由 `c86865d` 补齐 fixture 并经定向 workspace 测试验证。E2E `e2e-core` 的 `waiting_restart.sh` 另外报 `database is locked`，等待后续 workflow 判断是否为暂态。同期新增 E 父 socket disconnect child continuity、TUI task board create/update/list；F bound lead run→export→review→accept 与 Close timeout/retry 测试，均通过定向本地测试。上述新组合代码及 fixture 修复尚待云端复验。

SHA `21d66d7061a31b43f72a24ea79810c52e3584798` 的 Go build/unit、package、全部 E2E jobs 与 Workspace Linux 均通过；`database is locked` 未在复验中重现。随后新增 `c8c5797` 的真实 TUI plan-request approval service 路径，定向本地测试 `-count=3` 通过，尚待最新 SHA 云端复验。该增量仍只补 E AC5/AC9 子场景；E/F 的 AC1–9 与 M09 全局组合验收尚未完成。

随后继续推进 E/F：team task/request/team/member 历史查询补齐稳定排序、100 项上限和 cursor 续读；增加成员发消息给 lead 不自动起新 lead run、单个 member stop 不影响 sibling、parent cancellation 不伪完成 task 的 E 证据。F 增加后台 writer 在 requester disconnect/parent completion 后保留 generation lease、workspace chmod 经 export/review/accept 保留 mode、checkout 变化使旧 conflict resolution 失效、stale writer executor 跨 generation 写入被拒、linked `.git` pointer acceptance 后 inode/内容保全等测试。其后又新增 E 的跨 team request 拒绝、member 不得增建成员、stale plan revision no-op、parent failure 下双 sibling 续行、broadcast interrupted recipient 经重启只重交给目标成员；F 的 TUI blocked-writer reason/checkout 保留与 stop retry service 闭环、双 session 并行 writer binding 隔离。以上定向本地测试通过并记录在 E/F checklist。SHA `772e710f6cd8cb61b31f041492198347df8ab0d7` 的 Go/package、E2E 全部 jobs 与 Workspace Linux 已通过；其后代码提交待下一轮组合云端验证。M09-E/F AC1–9 与全局源端差异和组合验收仍开放，整体 M09 未完成。
