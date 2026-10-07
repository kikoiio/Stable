# M09-F 受控工作树与并行写入 Checklist

> 状态：规格已批准，实施代码已提交；AC1–9 仍需逐项验收。SHA `e24331a` 的 Go 全量、package 与真实 Linux writer/sandbox/quota workflow 通过；通用 E2E `e2e-core` 在依赖恢复阶段失败（2026-10-08）。A/B/C/D旧CI不作为F实现证据。

## 审批与追溯

- [x] [spec](spec.md)、[plan](plan.md)、[task](task.md)、本checklist获用户批准；批准日期：2026-10-07；范围：按四份规格实现 M09-F。
- [ ] T1–T12按依赖完成，共享契约、文件所有权、manifest版本及权限边界均有审阅记录。
- [ ] 实际代码SHA、GitHub Actions workflow/run/job、失败历史、修复SHA和复验结果已填写；测试未使用真实provider/用户definitions/真实工程清理。

## AC1 归属与进入退出

- [ ] workspace ID/label分离，label不用于路径/ref，创建与所有query/lifecycle操作拒绝跨session/project/Goal/WorkItem或伪造根。
- [ ] 两个session binding及多个独立child同时存在；没有全局cwd切换，单项单writer/generation约束生效。
- [ ] 活跃run期间禁止变更其binding/authority；默认exit/keep保留变更且确认writer真实退出，父正常完成/断线保持D后台语义。

## AC2 私有 Git 与基线

- [ ] 私有bare由sanitized当前文件快照生成，dirty/untracked/ignored regular数据不遗漏，源变化拒绝结果；所有文件/字节上限真实执行。
- [ ] 正式Git refs/index/config/hooks及service数据在create/run/export前后保持原值，没有正式worktree add/prune/reset/fetch、源history/credentials复制。
- [ ] 私有bare/commonDir在本项service-owned目录；无remotes/alternates/replace/external hooks、objects hardlinks、共享兄弟refs，Git净化env/config与属性攻击fixture通过。
- [ ] 根metadata不复制，嵌套Git/submodule/symlink/hardlink/specialfile拒绝，未执行source settings/.worktreeinclude/hooks或自动依赖安装。

## AC3 受控 child 写与 command

- [ ] `run_agent`同步、后台、definition isolation三入口分别验证真实workspace/lease；缺省D、explore/plan和A/B/C仍只读，角色工具规则与父权限交集生效。
- [ ] file tools只写本checkout；正式/兄弟/state/privateGit/受保护路径与generation伪造被拒绝，snapshot与tool call/result配对。
- [ ] command的真实Linux隔离覆盖子进程、根替换/软链接/硬链接/`.git/.stable`mask攻击，无formal/state/bare挂载，无凭据/网络/递归分派。
- [ ] 无sandbox失败关闭；hard磁盘quota无能力时command unavailable而不是裸执行/仅监控假上限；未擅改本机quota/mount/tmpfs。
- [ ] plan模式和权限deny仍先于写执行；definition permissionMode/remote/工具参数扩权拒绝。

## AC4 三方导出、冲突与用户接收

- [ ] B/F/W相等/仅一方变/双方相同/双方不同表覆盖bytes、mode、创建、删除；rename按delete/add，冲突有绑定digest的路径摘要，无自动文本merge/force旁路。
- [ ] 两工作树不同文件依次导出并接受不会回退先前正式改动；相同文件冲突阻断，用户手工合并W后，逐路径user resolution可继续导出，不要求W等于旧B/F；生成候选有新真实版本。
- [ ] conflict preview/resolution绑定真实user/session/workspace/generation、完整B/F/W digests及所有精确冲突路径的W/F选择；缺失/额外路径、源变化、过期或模型决策拒绝；resolution不改正式根或baseline，导出后仍须新候选review/用户accept。
- [ ] export停止writer、冻结digest、流式same-volume candidate；并发正式/工作树改变、超限、写盘失败不产生伪ready结果，幂等export返回同一候选引用。
- [ ] 导出走既有freeze/review/checkers及显式用户review_accept；普通任务/Goal都不能自行接受、提交、merge、push或改变目标证据/成功。

## AC5 Metadata 事务与兼容

- [ ] v2 candidate/manifest/review/transaction版本字段与旧版本读取策略已实现；不重算旧digest继续旧用户确认，含Git旧候选需重新导出/预览。
- [ ] 正式`.git`目录、linked `.git`regular指针与`.stable`分别测试保全、交换、归位；candidate/workspace注入替换metadata硬拒绝。
- [ ] 每个intent/保全/交换/归位/finalize crash点幂等恢复，accept/rewind共用契约；unknown/并发metadata目标冲突保留双方并blocked。
- [ ] 正式Git pointer不被解析为外部写权限；旧未完成Git事务明确reconcile，不猜测清理，不把只完成内容交换报告成接受完成。

## AC6 中断、归属与清理

- [ ] creating/queued/running/stopping/exporting/removing各中断点恢复两次无模型/command重跑、重复export或重复终态。
- [ ] 清理核对service ownership、root身份、sandbox PID启动身份与generation；不能误杀/删除用户或其它任务资源。
- [ ] dirty/untracked/ignored文件、新private提交、kept状态和未知Git/manifest结果全部保留；自动stale清理默认关闭。
- [ ] clean remove只有完整基线相等、无binding/writer才执行；dirty discard要求真实用户ID+generation+digest确认，模型bool/过期确认/跨session不可触发。
- [ ] 子进程未实际退出、持久失败或rootidentity不明时保留blocked lease/操作记录，不宣称资源释放或删除完成。

## AC7 有界资源

- [ ] 3 workers/32 queue与D/A/B/C共池，单materializer/8 pending、20,000 files/128 MiB snapshot/16 MiB file真实边界和拒绝路径有屏障证据。
- [ ] 每项512 MiB/每project2 GiB/16未删除项、child8轮/3分钟/50,000输出/8 KiB摘要/64 KiB输入生效；定义/请求只能收窄。
- [ ] create/materialize3分钟、command90秒并受childdeadline、query最多30秒、stop清理10秒边界生效；等待不占额外childworker、不产生未接受无界goroutine。
- [ ] 配额/持久化/取消/队列失败可见，临时资源只清理自身；构建/全量/容器/大数据走已授权云端，未完成检查如实保留。

## AC8 入口、恢复、隐私与目标事实

- [ ] `/worktrees`、create/enter/exit/keep/export/resolve/remove、`/agent --worktree`与父工具形成真实service闭环，完整用法/错误/状态/冲突反馈可见。
- [ ] 独立task/workspace session游标重连、重复通知去重，不覆盖父ActiveRunID/stream；恢复能看到同一工作树/candidate/保留原因。
- [ ] 用户冲突resolution、discard与候选接受是单独可审阅决策；目录/事件/日志/TUI不显示角色正文、凭据、thinking、raw transcript或无限diff。
- [ ] 后台成功/summary/export不成为Goal证据，候选接受后仍需独立目标复检；Session/Goal WorkRef沿可信事件保持关联。

## AC9 组合回归与源差异

- [ ] 源create/session/agent/async/teammate/cleanup/setup逐项比对，明确哪些适配、哪些源仅解析/缺完整运行、哪些平台unsupported；未运行源项目。
- [ ] A/B/C/D、已实施E、普通Session/Goal、候选accept/rewind/recovery、权限/plan/hook回归通过。E未实施写“不适用/待实施”，不能勾成组合已通过。
- [ ] Linux真实sandbox/quota/privateGit/metadata故障集成通过；其它平台能力不足返回unavailable，不假写跨平台运行通过。
- [ ] 更新M09总进度与迁移地图，依据D/E/F实际验收决定整体状态，不提前宣称M09完成。

## 验证记录（批准实施后填写）

| 检查 | 代码SHA | Workflow / run / job | 结果与限制 |
|---|---|---|---|
| Go build + `go test ./...` | `8a34ea045a13435b955c0c1fb8b57afe1f417aba` | [Go run 37640598695](https://github.com/kikoiio/Stable/actions/runs/37640598695), `build-and-test` | 通过；通用单测不替代真实 writer sandbox/quota 与 AC1–9 组合验收。 |
| 组合代码 Go build/unit + package | `e24331a0ac6d9745f9dc80fc8aed0142d47d2210` | [Go run 37651671848](https://github.com/kikoiio/Stable/actions/runs/37651671848), `build-and-test`, `test-package` | 两个 jobs 均通过；仍不替代真实 sandbox/quota 与 F AC1–9 逐项验收。 |
| 真实 writer sandbox + 磁盘 quota + metadata 攻击 | `e24331a0ac6d9745f9dc80fc8aed0142d47d2210` | [M09 Workspace Linux run 37651671803](https://github.com/kikoiio/Stable/actions/runs/37651671803), `writer-sandbox-volume` | 成功；使用 disposable ext4 loop volume、真实 helper 与 fake model fixture，验证受限容量及 metadata attack cases；不代表完整 workspace lifecycle/用户接收闭环。 |
| 组合代码全量 E2E | `e24331a0ac6d9745f9dc80fc8aed0142d47d2210` | [E2E run 37651671760](https://github.com/kikoiio/Stable/actions/runs/37651671760) | `e2e-core` 的 `dependency_change.sh` checker 版本恢复阶段未收敛，goal agent run 终态为 failed；其它 E2E jobs 通过。 |
| 全量 E2E workflow | `8a34ea045a13435b955c0c1fb8b57afe1f417aba` | [E2E run 37640598926](https://github.com/kikoiio/Stable/actions/runs/37640598926), `unit`, `e2e-core`, `e2e-m03`, `e2e-m04`, `e2e-sessions`, `cases` | 所有 job 通过；workflow 没有覆盖 F 的冲突用户 resolution、真实 writer quota/sandbox 集成和生命周期用户接受闭环。 |
| Go package acceptance | `8a34ea045a13435b955c0c1fb8b57afe1f417aba` | [Go run 37640598695](https://github.com/kikoiio/Stable/actions/runs/37640598695), `test-package` | 通过；run 总结由 in_progress 更新为 success，job 完成于 2026-10-07 15:16 UTC。仅是预览改动前 SHA 的结果。 |
| Preview regression first run / repair | `90ed0d7` → `75594bd2c4279c115c18fb5f22c741f868dcfcb5` | [failed Go run 37643405245](https://github.com/kikoiio/Stable/actions/runs/37643405245), `build-and-test`; [repaired Go run 37644076609](https://github.com/kikoiio/Stable/actions/runs/37644076609), `build-and-test` | 首次单测失败仅因 preview lifecycle fixture 未带可信 authority；修复 fixture 后 build 与 `go test ./...` 通过。package job 与 E2E workflow 仍在运行。 |
| Lifecycle journal restart（本地定向） | 工作树未提交 | `go test ./internal/workspace -run TestLifecycleServiceRecoversInterruptedJournalOperationsConservatively` | creating intent 被标为 interrupted 且不创建项目内容；root 已消失的 remove intent 收敛为 removed；未知 writer 保留 writer ID 并让 stop 返回 unavailable；第二次 startup 幂等。未覆盖真实 PID、export candidate 与真实 sandbox。 |
| Runtime/protocol/TUI 基础接线与冲突预览 | `90ed0d7` | 已接线；[Go run 37644076609](https://github.com/kikoiio/Stable/actions/runs/37644076609) build/unit 通过 | 专用 state root、workspace lifecycle、enter/exit/export 与 `/worktrees preview` 已接线；preview 持久化三方 digest 和最多 100 个冲突路径。package/E2E、writer sandbox/quota、用户逐路径 resolution 与 discard 决策仍待验收。 |
| 定向契约/快照/private Git | 待填 | 待填 | 未执行 |
| 真实sandbox与quota攻击 | 待填 | 待填 | 未执行 |
| metadata事务全部crash点 | 待填 | 待填 | 未执行 |
| 并行导出/用户接受/TUI恢复 | 待填 | 待填 | 未执行 |
| Go全量 / package | 待填 | 待填 | 未执行 |
| E2E及M09组合 | 待填 | 待填 | 未执行 |
