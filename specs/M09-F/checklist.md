# M09-F 受控工作树与并行写入 Checklist

> 状态：规格已批准，实施代码已提交；AC1–9 仍需逐项验收。最新代码 SHA `1ca6e0e0afb8400a61e4245eec7b88baeb6d3cb8` 的真实 Linux writer/sandbox/quota workflow、Go build/unit、package 与 E2E 全部 jobs 通过；A/B/C/D旧CI不作为F实现证据（2026-10-09）。

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
| 最新 writer sandbox + 磁盘 quota + metadata 攻击 | `f8fce2d15bc3262a770300057cc7638209f4c672` | [M09 Workspace Linux run 37664859448](https://github.com/kikoiio/Stable/actions/runs/37664859448), `writer-sandbox-volume` | 成功；覆盖 disposable ext4 上 writer isolation、实际容量耗尽与 metadata 攻击契约；不代表完整 workspace lifecycle/用户接收闭环。 |
| 最终 writer sandbox + 磁盘 quota + metadata 攻击 | `f458c91a587004af839409c65ee8c44bd4b93b13` | [M09 Workspace Linux run 37673116455](https://github.com/kikoiio/Stable/actions/runs/37673116455), `writer-sandbox-volume` | 成功；覆盖 disposable ext4 上 writer isolation、实际容量耗尽与 metadata 攻击契约；不代表完整 workspace lifecycle/用户接收闭环。 |
| 组合代码全量 E2E | `e24331a0ac6d9745f9dc80fc8aed0142d47d2210` | [E2E run 37651671760](https://github.com/kikoiio/Stable/actions/runs/37651671760) | `e2e-core` 的 `dependency_change.sh` checker 版本恢复阶段未收敛，goal agent run 终态为 failed；其它 E2E jobs 通过。 |
| 最新组合代码 E2E | `f8fce2d15bc3262a770300057cc7638209f4c672` | [E2E run 37664859557](https://github.com/kikoiio/Stable/actions/runs/37664859557) | `unit`、`e2e-core`、`e2e-m04`、`e2e-sessions` 成功，`e2e-m03`/`cases` 因 `setup-e2e-deps` 无进展取消；`dependency_change.sh` checker 恢复已通过。通用 E2E 不覆盖 F AC1–9 完整接收闭环。 |
| 最新 Go/package | `f8fce2d15bc3262a770300057cc7638209f4c672` | [Go run 37664859562](https://github.com/kikoiio/Stable/actions/runs/37664859562) | `test-package` 的依赖安装超过 30 分钟上限；`build-and-test` 停滞后取消。全量 Go 单测由同 SHA E2E `unit` job 通过；package acceptance 未完成。 |
| 最终组合代码 E2E | `f458c91a587004af839409c65ee8c44bd4b93b13` | [E2E run 37673116514](https://github.com/kikoiio/Stable/actions/runs/37673116514) | `unit`、`cases`、`e2e-core`、`e2e-m03`、`e2e-m04`、`e2e-sessions` 全部成功；通用 E2E 不覆盖 F AC1–9 完整用户接收闭环。 |
| 最终 Go/package | `f458c91a587004af839409c65ee8c44bd4b93b13` | [Go run 37673116497](https://github.com/kikoiio/Stable/actions/runs/37673116497) | `build-and-test` 与 `test-package` 均成功；仍不替代真实 sandbox/quota 与 F AC1–9 逐项验收。 |
| 最新兼容代码 Go/package + E2E + Linux writer sandbox/quota | `ea24f8e9e82225f3d8879c4165d77a0e5077fdfe` | [Go run 37675602216](https://github.com/kikoiio/Stable/actions/runs/37675602216), [E2E run 37675602223](https://github.com/kikoiio/Stable/actions/runs/37675602223), [M09 Workspace Linux run 37675602212](https://github.com/kikoiio/Stable/actions/runs/37675602212) | Go `build-and-test`/`test-package`、全部 E2E jobs 与 `writer-sandbox-volume` 成功；新增 TUI resolution 请求测试纳入 E2E `unit`。通用 workflow 不替代 F 完整 lifecycle/AC1–9 接收闭环。 |
| D/E/F 当前组合代码复验 | `8223aaf6915669ff87d15264cef5cbdb85293e24` | [Go run 37677736986](https://github.com/kikoiio/Stable/actions/runs/37677736986), [E2E run 37677737032](https://github.com/kikoiio/Stable/actions/runs/37677737032), [M09 Workspace Linux run 37677737015](https://github.com/kikoiio/Stable/actions/runs/37677737015) | Go build/unit、package、全部 E2E jobs 与 `writer-sandbox-volume` 成功；本 SHA 的 E2E `unit` 包含新增 TUI resolution 请求测试。不替代 F 完整 lifecycle/AC1–9 接收闭环。 |
| 最终 dirty-discard acceptance 代码 | `edaa6184fc63ae0ef8dea99829e0cfc51c2d8d01` | [Go run 37680864143](https://github.com/kikoiio/Stable/actions/runs/37680864143), [E2E run 37680863953](https://github.com/kikoiio/Stable/actions/runs/37680863953), [M09 Workspace Linux run 37680864152](https://github.com/kikoiio/Stable/actions/runs/37680864152) | Go `build-and-test`/`test-package`、E2E `unit`/`cases`/`e2e-core`/`e2e-m03`/`e2e-m04`/`e2e-sessions` 与 `writer-sandbox-volume` 全部成功；含 TUI 两阶段确认与 service digest/generation 复核测试。仍不替代 F 全部 AC 与并行/lifecycle 故障组合。 |
| 双 session binding 独立及当前 E/F 组合回归 | `c0f523d40faec92c3682f9f56445b9ce292185ae` | [Go run 37805889376](https://github.com/kikoiio/Stable/actions/runs/37805889376), [E2E run 37805888980](https://github.com/kikoiio/Stable/actions/runs/37805888980), [M09 Workspace Linux run 37807400979](https://github.com/kikoiio/Stable/actions/runs/37807400979) | Go `build-and-test`/`test-package`、E2E 全部 jobs 和 `writer-sandbox-volume` 成功；全量 unit 包含双 session binding ownership 测试。两个 session 各自进入并查询自己的 workspace，跨 session Get/Enter 被拒；不覆盖并行 writer、活跃 run authority 或完整 AC1–9。 |
| 当前 task graph service/replay 与 E/F 组合回归 | `f61e2fb` | [Go run 37824906854](https://github.com/kikoiio/Stable/actions/runs/37824906854), [E2E run 37824906843](https://github.com/kikoiio/Stable/actions/runs/37824906843), [M09 Workspace Linux run 37824906787](https://github.com/kikoiio/Stable/actions/runs/37824906787) | Go `build-and-test`/`test-package`、E2E 全部 jobs 与真实 writer sandbox/quota 成功。F 行为未改动，仍记录当前组合回归；不代表 F AC1–9 完整验收。 |
| 最新 E/F 组合验证 | `35b130bb` | [Go run 37826465763](https://github.com/kikoiio/Stable/actions/runs/37826465763), [E2E run 37826465801](https://github.com/kikoiio/Stable/actions/runs/37826465801), [M09 Workspace Linux run 37826465661](https://github.com/kikoiio/Stable/actions/runs/37826465661) | Go `build-and-test`/`test-package`、E2E 全部 jobs 与真实 writer sandbox/quota 成功。F 行为未改动；不代表 F AC1–9 生命周期/并行写入/用户接收完整验收。 |
| 最新 E/F 组合验证 | `ac817faf` | [Go run 37828073176](https://github.com/kikoiio/Stable/actions/runs/37828073176), [E2E run 37828073232](https://github.com/kikoiio/Stable/actions/runs/37828073232), [M09 Workspace Linux run 37828073186](https://github.com/kikoiio/Stable/actions/runs/37828073186) | Go `build-and-test`/`test-package`、E2E 全部 jobs 与真实 writer sandbox/quota 成功。F 行为未改动；不代表 F AC1–9 生命周期/并行写入/用户接收完整验收。 |
| 最新 E/F 组合验证 | `26eafc55` | [Go run 37869857770](https://github.com/kikoiio/Stable/actions/runs/37869857770), [E2E run 37869857737](https://github.com/kikoiio/Stable/actions/runs/37869857737), [M09 Workspace Linux run 37869857758](https://github.com/kikoiio/Stable/actions/runs/37869857758) | Go `build-and-test`/`test-package`、E2E 全部 jobs 与真实 writer sandbox/quota 成功；首次 E2E 仅因 setup 中 Launchpad PPA 暂时 500 失败，重跑失败 job 后成功。F 行为未改动；不代表 F AC1–9 完整验收。 |
| 当前组合 Go/package、E2E 与真实 writer sandbox/quota | `6121f543e342330dcd90eb38469138b5317074ec` | [Go run 37822847214](https://github.com/kikoiio/Stable/actions/runs/37822847214) `build-and-test`/`test-package`; [E2E run 37822847081](https://github.com/kikoiio/Stable/actions/runs/37822847081); [Workspace Linux run 37822846869](https://github.com/kikoiio/Stable/actions/runs/37822846869) `writer-sandbox-volume` | Go 两 jobs、E2E 所有 jobs 与 disposable ext4 writer sandbox/quota job 全部成功；仍不替代 F AC1–9 生命周期/并行写入/用户接收闭环。 |
| 全量 E2E workflow | `8a34ea045a13435b955c0c1fb8b57afe1f417aba` | [E2E run 37640598926](https://github.com/kikoiio/Stable/actions/runs/37640598926), `unit`, `e2e-core`, `e2e-m03`, `e2e-m04`, `e2e-sessions`, `cases` | 所有 job 通过；workflow 没有覆盖 F 的冲突用户 resolution、真实 writer quota/sandbox 集成和生命周期用户接受闭环。 |
| Go package acceptance | `8a34ea045a13435b955c0c1fb8b57afe1f417aba` | [Go run 37640598695](https://github.com/kikoiio/Stable/actions/runs/37640598695), `test-package` | 通过；run 总结由 in_progress 更新为 success，job 完成于 2026-10-07 15:16 UTC。仅是预览改动前 SHA 的结果。 |
| Preview regression first run / repair | `90ed0d7` → `75594bd2c4279c115c18fb5f22c741f868dcfcb5` | [failed Go run 37643405245](https://github.com/kikoiio/Stable/actions/runs/37643405245), `build-and-test`; [repaired Go run 37644076609](https://github.com/kikoiio/Stable/actions/runs/37644076609), `build-and-test` | 首次单测失败仅因 preview lifecycle fixture 未带可信 authority；修复 fixture 后 build 与 `go test ./...` 通过。package job 与 E2E workflow 仍在运行。 |
| Lifecycle journal restart（定向与云端） | `TestLifecycleServiceRecoversInterruptedJournalOperationsConservatively`；Go run 37879308947 `build-and-test`、E2E run 37879308946 `unit` 成功 | `go test ./internal/workspace -run '^TestLifecycleServiceRecoversInterruptedJournalOperationsConservatively$'` | creating intent 被标为 interrupted 且不创建项目内容；root 已消失的 remove intent 收敛为 removed；writing/stopping 均保留 writer ID，未知 writer 的 stop 返回 unavailable；export intent 标为 interrupted/blocked、保留候选引用且重复启动幂等，不自动调用 exporter。未覆盖真实 PID、exporter 中途故障与真实 sandbox。 |
| 用户冲突决策到候选 review/accept | `TestWorkspaceConflictResolutionExportsReviewedCandidateForAcceptance`；最终 SHA `f458c91` 的 E2E `unit` job | 验证空/额外路径/过期 preview 拒绝，正式根 digest 变化使旧决策失效；重新预览和逐路径用户选择后导出冻结候选，经既有 review/checker 与显式 accept 写入正式文件；重复 export 返回同一候选。属于单 session fake 闭环，不覆盖并行工作树、真实 sandbox/quota 或 lifecycle crash 点，AC4 仍待完整审计。 |
| TUI 冲突决策入口 | `TestWorktreeResolutionDialogSendsCompleteUserChoices`；E2E run 37677737032 `unit` job 成功 | 从冲突 preview response 打开对话框；不完整路径选择按 Enter 不发送；完整选择发出精确 choices 并绑定当前 session、workspace、PreviewID、generation，且保留父 run/stream。尚未串接真实 client service，也不覆盖 TUI dirty-discard；AC8 保持未完成。 |
| TUI dirty-discard 用户确认入口 | `TestWorktreeDiscardDialogRequiresArmedUserConfirmation`，本地定向 Go 测试通过（2026-10-08） | discard preview 打开确认框；未先按 `d` 时 Enter 不发送，armed 后 Enter 才发出绑定 session、workspace、真实预览 digest 与 generation 的 `worktree_discard` 请求，且父 run/stream 不变。尚未串接真实 client service，AC6/AC8 其他生命周期项仍待验收。 |
| dirty-discard service ownership 与内容复核 | `TestUserDiscardRequiresCurrentPreviewDigestAndGeneration`；Go run 37680864143 `build-and-test` 和 E2E run 37680863953 `unit` 成功 | 临时私有 Git 工作树验证其他用户、错误 digest 与预览后内容变化均拒绝；重新预览后只有当前用户/decision/digest/generation 确认才能删除，且目录移除。尚未覆盖真实 writer 进程与并发 remove。 |
| 双 session binding 隔离 | `TestLifecycleServiceBindingsAreIndependentPerSession`；Go run 37805889376 `build-and-test`、E2E run 37805888980 `unit` 成功 | 同一 project 中两个 session 各自创建并进入 workspace；binding 分别指向自己的 workspace，跨 session `Get`/`Enter` 返回 ownership error。未证明同一 session 多 child 并行或活跃 run 期间禁止切换。 |
| Socket client workspace list 与公开快照 | `TestWorkspaceListClientRequestReturnsOwnedPublicSnapshots`；定向 conversation Go 测试通过（2026-10-09） | 真实 Unix socket `Request` 经 conversation service 列出 session 自有 workspace，核对 ID/label/state/cursor，并确认正式根、state 根及 candidate 路径不出现在响应。仅覆盖列表读取，不覆盖 create 活跃 run 授权、lifecycle mutation 或 AC8 完整接线。 |
| Socket client conflict resolution 到候选导出 | `TestWorkspaceConflictResolutionClientRequestRequiresSeparateAcceptance`；Go run 37822847214 `build-and-test`、E2E run 37822847081 `unit` 成功 | 真实 Unix socket 经过 conversation service 预览冲突、提交绑定 preview/generation/逐路径 choice 的用户决策并导出 candidate；正式文件保持不变，candidate 未自动 accepted。与 TUI request 单测及直接 service 闭环互补，仍未串接实际 TUI 完整创建/写入/接收流程，AC8 未完成。 |
| clean-remove 保留 dirty/untracked workspace | `TestRemoveCleanPreservesDirtyUntrackedWorkspace`；本地 `go test ./internal/workspace -run '^TestRemoveCleanPreservesDirtyUntrackedWorkspace$' -count=3` 与 Go run 37822847214 `build-and-test` 成功 | clean remove 遇到 checkout 外部新增 untracked 文件时返回 ownership error，状态仍为 ready，文件字节保留；真实用户 discard 单独路径由既有 digest/generation 测试覆盖。未覆盖并发 remove 或所有 dirty/ignored/private commit 组合，AC6 未完成。 |
| Swapped-phase Git pointer identity replacement | `TestProjectMetadataAcceptanceRecoveryEveryMoveBoundary/git_directory_false/same_digest_git_pointer_replacement`；定向 store Go 测试通过（2026-10-09） | 三次目录 move 后以相同字节、不同 inode 替换 incoming regular `.git` pointer；reconcile 进入 blocked，正式新内容及替换 pointer 保留、无 receipt，重复 reconcile 不改变结果。未覆盖断电/fsync 持久性或全部 transaction crash points。 |
| Sequential workspace candidate acceptance | `TestSequentialWorkspaceCandidateAcceptsPreserveIndependentChanges`；定向 conversation Go 测试通过（2026-10-09） | 两 workspace 从同一 baseline 修改不同文件；首候选经 review/accept 后第二预览无冲突，第二候选及再次接受后的 formal 同时保留两项改动。未覆盖并发 export、同路径冲突或 TUI 决策，AC4 仍需全量审计。 |
| Three-way path existence merge matrix | `TestThreeWayPreviewHandlesAddedDeletedAndRenamedPaths`；本地定向测试与 SHA `eba916a` Go `build-and-test`/E2E `unit` 通过 | 覆盖 formal/workspace 单方新增、相同/冲突新增、双方删除、删除与未改/已改路径，以及 rename 按旧路径删除和新路径添加处理；mode 冲突与 AC4 完整矩阵仍需结合其他测试和组合审计。 |
| Concurrent conflict export idempotency | `TestWorkspaceConflictResolutionExportsReviewedCandidateForAcceptance`；本地定向 conversation 测试 `-count=2` 通过（2026-10-09） | 用户解决冲突后，两路并发 export 返回相同候选 ID，随后继续既有 review/checker/accept 闭环；未模拟 exporter 中途写盘失败或正式根并发变化，不能单独勾选 AC4。 |
| Exclusive writer lease generation fence | `TestWorkspaceWriterLeaseIsExclusiveAndGenerationFenced`；定向 workspace Go 测试通过（2026-10-09） | 同一项第二 RunID acquire 被拒，第一 lease 完成后新 generation 增长；旧 lease 无法 reserve 写入或释放新 lease，snapshot 仍由第二 RunID 持有。未覆盖真实 OS writer 退出、活跃 run binding guard 接线或 remove/discard，AC1/2/6 仍需全量审计。 |
| Concurrent writer leases on independent workspaces | `TestIndependentWorkspacesHoldConcurrentWriterLeases`；本地 `go test ./internal/workspace -run '^TestIndependentWorkspacesHoldConcurrentWriterLeases$' -count=2` 通过（2026-10-09） | 同一 session 的两个独立 workspace 同时保持各自 `StateWriting` lease 与不同 checkout；任一 occupied workspace 拒绝第二 lease；两个 child 分别写入后均独立结算为 `kept` 且变更数正确。未覆盖 `run_agent` 同时执行、sandbox 隔离或并行导出/接收，AC1 仍未整体验收。 |
| Parallel named writers through real Linux sandbox | `TestParallelWorkspaceWriterAgentsUseIndependentLeases`；M09 Workspace Linux run 37874596321 `writer-sandbox-volume` 通过 | 在 disposable ext4 限额卷与真实 agentworker/bwrap 中同时启动两个 named `isolation: worktree` child；屏障期间两个 run 持有不同 workspace 的 writing lease，各自在同名相对路径写入，均独立完成并保留；正式根未变化。未覆盖并行 export/accept、team child 组合或完整 AC1/AC3。 |
| Active-run workspace binding guard wiring | `TestWorkspaceServiceBlocksEnterAndExitDuringActiveRun`；定向 conversation Go 测试通过（2026-10-09） | 通过真实 conversation `workspaceService` 注入 IdleGuard；活跃 run 时 Enter/Exit 返回 unavailable 且 binding 保持，run 清除后 Exit 成功并清空。未覆盖 OS child authority/CWD 竞争窗或并发原子性，AC1/2 仍需全量审计。 |
| Runtime/protocol/TUI 基础接线与冲突预览 | `90ed0d7` | 已接线；[Go run 37644076609](https://github.com/kikoiio/Stable/actions/runs/37644076609) build/unit 通过 | 专用 state root、workspace lifecycle、enter/exit/export 与 `/worktrees preview` 已接线；preview 持久化三方 digest 和最多 100 个冲突路径。后续最终 SHA 的 package/E2E、writer sandbox/quota 与单 session 冲突接收闭环见上方记录；dirty-discard 决策及并行/lifecycle故障组合仍待验收。 |
| 定向契约/快照/private Git | `TestSnapshotIncludesDataAndSkipsProtectedMetadata`、`TestManifestRejectsUnsafeEntries`、`TestPrivateGitSyntheticBaselineAndLinkedCheckout`、`TestPrivateGitValidationRejectsMetadataTampering`；Go run 37874596313 `build-and-test` | [Go run 37874596313](https://github.com/kikoiio/Stable/actions/runs/37874596313) | Go 全量 unit 包含受限快照、危险文件类型拒绝、合成私有基线、正式 Git metadata 不变及私有 Git tamper 检查；未覆盖每个 syscall/crash 边界或所有平台。 |
| 真实sandbox与quota攻击 | `TestParallelWorkspaceWriterAgentsUseIndependentLeases`；M09 Workspace Linux run 37874596321 `writer-sandbox-volume` | [Workspace Linux run 37874596321](https://github.com/kikoiio/Stable/actions/runs/37874596321) | disposable ext4 限额卷上由真实 bwrap child 并行写入两个 workspace 并通过 quota/sandbox workflow；仍未证明完整命令攻击矩阵和 Team+F 组合。 |
| metadata事务移动边界恢复 | `TestProjectMetadataAcceptanceRecoveryEveryMoveBoundary`；Go run 37874596313 `build-and-test` | [Go run 37874596313](https://github.com/kikoiio/Stable/actions/runs/37874596313) | Go 全量 unit 注入 directory 和 regular `.git` pointer 的 move-boundary 故障并验证冲突保留、blocked 与重复 reconcile 幂等；不覆盖断电/fsync 保证及全部 candidate/rewind 版本迁移。 |
| Candidate/review manifest policy mismatch | `TestAcceptanceRejectsReviewWithDifferentManifestPolicy`；本地定向 `go test ./internal/candidate -run '^TestAcceptanceRejectsReviewWithDifferentManifestPolicy$'` 通过（2026-10-09） | Project-policy candidate 配 Legacy-policy review 时在 digest/accept 前返回明确 policy mismatch；仍未覆盖旧 transaction crash matrix 与全部 rewind 路径，AC5 未整体验收。 |
| TUI workspace lifecycle command dispatch | `TestWorktreeCommandsDispatchSessionScopedLifecycleRequests`；本地定向 TUI Go test `-count=3` 通过（2026-10-09） | list/create/get/enter/exit/keep/export/resolve/remove/discard-preview 均发送 session-scoped 请求，create 绑定活动 lead run；父 run、cursor 与 stream 保持。实际 service 创建与写入的完整 lifecycle、恢复仍未覆盖。 |
| TUI 冲突决策到用户接受的真实 service 闭环 | `TestWorktreeTUIResolutionExportAndAcceptanceUsesConversationService`；SHA `1ca6e0e0afb8400a61e4245eec7b88baeb6d3cb8` | [Go run 37886754199](https://github.com/kikoiio/Stable/actions/runs/37886754199) `build-and-test`/`test-package`; [E2E run 37886754201](https://github.com/kikoiio/Stable/actions/runs/37886754201) 全部 jobs; [Workspace Linux run 37886754300](https://github.com/kikoiio/Stable/actions/runs/37886754300) `writer-sandbox-volume` | TUI 通过真实 Unix socket 完成 conflict preview、逐路径选择、candidate export、review 与显式 accept；正式文件在 accept 前不变、accept 后才更新。初始 workspace 由临时 LifecycleService fixture 创建；未覆盖 TUI create、真实 `run_agent` 写入或生命周期重启恢复，因此 F AC4/AC8/AC9 仍未整体验收。此测试发现并修复 `review_accept` 漏传 session ID。 |
| 并行导出/用户接受/TUI恢复 | `TestSequentialWorkspaceCandidateAcceptsPreserveIndependentChanges`、`TestWorkspaceConflictResolutionExportsReviewedCandidateForAcceptance`、`TestWorkspaceConflictResolutionClientRequestRequiresSeparateAcceptance`；Go run 37874596313 `build-and-test`、E2E run 37874596359 `unit` | [Go run 37874596313](https://github.com/kikoiio/Stable/actions/runs/37874596313), [E2E run 37874596359](https://github.com/kikoiio/Stable/actions/runs/37874596359) | 已验证顺序候选接受保留独立文件、并发重复 export 幂等、真实 socket resolution 后单独接受；仍未覆盖同时 export 与 accept 的竞争、并行工作树接受及重启后的真实 TUI 恢复闭环。 |
| Go全量 / package | SHA `eba916a` | [Go run 37874596313](https://github.com/kikoiio/Stable/actions/runs/37874596313), `build-and-test`, `test-package` | Go build、`go test ./...` 与 package acceptance 均通过。 |
| E2E及M09组合 | SHA `eba916a` | [E2E run 37874596359](https://github.com/kikoiio/Stable/actions/runs/37874596359), [Workspace Linux run 37874596321](https://github.com/kikoiio/Stable/actions/runs/37874596321) | 所有 E2E jobs 与 `writer-sandbox-volume` 成功；M03 isolated computer bridge 首次 EOF，复跑失败 job 后全绿。workflow 未覆盖 E/F AC1–9 全部 lifecycle 和组合场景。 |
| 当前 Go、package、E2E 与 Workspace Linux 组合回归 | SHA `60a3cc669e8cd950c8bcb0cf4ed9345b92b4cf96` | [Go run 37876744035](https://github.com/kikoiio/Stable/actions/runs/37876744035), [E2E run 37876744085](https://github.com/kikoiio/Stable/actions/runs/37876744085), [Workspace Linux run 37876743971](https://github.com/kikoiio/Stable/actions/runs/37876743971) | Go `build-and-test`/`test-package`、E2E 全部 jobs 与 `writer-sandbox-volume` 均成功；E2E `e2e-m03` 首次 isolated computer bridge EOF，单独复跑失败 job 后成功。通用回归仍不替代 F AC1–9 的生命周期、并行导出/接收组合验收。 |
| Workspace interrupted-operation coverage 与 E/F 组合复验 | SHA `39bf4117274a381c0ad4023fd8ceeaac086e3636` | [Go run 37879308947](https://github.com/kikoiio/Stable/actions/runs/37879308947), [E2E run 37879308946](https://github.com/kikoiio/Stable/actions/runs/37879308946), [Workspace Linux run 37879308940](https://github.com/kikoiio/Stable/actions/runs/37879308940) | 三条 workflow 全部成功；新增 interrupted `writing`/`stopping` writer 不被误判退出、export candidate 引用保留且重复恢复幂等。通用回归仍不替代 F AC1–9 生命周期、并行导出/接收组合验收。 |
| Idle shutdown integration 与当前 E/F 组合复验 | SHA `220b0705b0a9b7f2a3b1d79be8874e4866b860c5` | [Go run 37880499789](https://github.com/kikoiio/Stable/actions/runs/37880499789), [E2E run 37880499793](https://github.com/kikoiio/Stable/actions/runs/37880499793), [Workspace Linux run 37880499781](https://github.com/kikoiio/Stable/actions/runs/37880499781) | Go `build-and-test`/`test-package`、E2E 全 jobs 与 `writer-sandbox-volume` 全部成功；F behavior 未改动，E test 增量随完整组合回归通过。仍不替代 F AC1–9 生命周期与并行导出/接收验收。 |
| 当前 E/F 组合复验（pending plan restart coverage） | SHA `99ef134de7cdf2cd33360dfa9b313aea59caaf17` | [Go run 37881598859](https://github.com/kikoiio/Stable/actions/runs/37881598859), [E2E run 37881598843](https://github.com/kikoiio/Stable/actions/runs/37881598843), [Workspace Linux run 37881598849](https://github.com/kikoiio/Stable/actions/runs/37881598849) | Go `build-and-test`/`test-package`、E2E 全部 jobs 与 `writer-sandbox-volume` 全部成功；F 行为未变更。通用回归仍不代表 F AC1–9 生命周期、并行导出/接收及完整 TUI 用户闭环通过。 |
| 当前 E/F 组合验证（TUI dispatch 与候选策略保护） | SHA `29eee81afdbc8e6b4a0bda906c99b26bd8ffc601` | [Go run 37885100777](https://github.com/kikoiio/Stable/actions/runs/37885100777), [E2E run 37885100807](https://github.com/kikoiio/Stable/actions/runs/37885100807), [Workspace Linux run 37885100829](https://github.com/kikoiio/Stable/actions/runs/37885100829) | Go `build-and-test`/`test-package`、E2E 全部 jobs 与 `writer-sandbox-volume` 全部成功。包含 TUI lifecycle 命令 session/run 绑定、manifest policy mismatch rejection；仍不代表 F AC1–9 完整 service/TUI 接收闭环。 |
| TUI conflict resolution 到候选用户接受及当前 E/F 组合回归 | SHA `1ca6e0e0afb8400a61e4245eec7b88baeb6d3cb8` | [Go run 37886754199](https://github.com/kikoiio/Stable/actions/runs/37886754199), [E2E run 37886754201](https://github.com/kikoiio/Stable/actions/runs/37886754201), [Workspace Linux run 37886754300](https://github.com/kikoiio/Stable/actions/runs/37886754300) | Go `build-and-test`/`test-package`、E2E `unit`/`cases`/`e2e-core`/`e2e-m03`/`e2e-m04`/`e2e-sessions` 与 `writer-sandbox-volume` 全部成功；该 SHA 验证新真实 TUI/service 接收测试及 SessionID 修复。F AC1–9 其他并行、lifecycle 故障与恢复组合仍开放。 |
