# M09-E 团队、消息与只读协调器 Checklist

> 状态：规格已批准，实施代码已提交；AC1–9 与完整场景仍按实际证据逐项验收。SHA `26eafc55` 的 Go build/unit、package 与 E2E 全部 jobs 通过；E AC1–9 仍有逐项验收缺口（2026-10-09）。

## 功能验收

- [x] [spec](spec.md)、[plan](plan.md)、[task](task.md)、本checklist获用户批准；批准日期：2026-10-07；范围：按四份规格实现 M09-E。

- [ ] **AC1 身份：** 两session、两Goal/WorkItem和相同显示名不同team验证受信owner/root/actor；创建/查询/消息/stop全部隔离，保留lead、唯一名称和活动容量生效。（验证：临时service/domain fixtures。）
- [ ] **AC2 多轮：** 同一member连续两个有界turn，输入仅角色/身份/上一摘要/明确批次；没有父历史或兄弟transcript；idle不占worker，一member不并行；首spawn队满拒绝，后续消息waiting_capacity公平重试。（验证：fake输入捕获、共享池屏障、调用/active计数。）
- [ ] **AC3 消息：** p2p/lead/broadcast持久有序、固定接收者、全或无投递；额满和写盘失败不误成功；批次handoff/restart destination gap、并发新消息和重连不丢失/重复；lead消息不自动起模型run。（验证：临时日志/故障注入/token去重。）
- [ ] **AC4 任务板：** CRUD、revision、owner、canonical依赖和blocks视图一致；未知/环/自依赖/跨队拒绝，未解除依赖禁止进行/完成；两成员抢claim只有一个成功，取消不会伪完成，M06 todo与Goal工作项独立。（验证：taskgraph与并发service fixtures。）
- [ ] **AC5 请求：** plan-required→提交→批准/拒绝修订/过期完整，错误sender/request/team和冲突重复拒绝；批准不解锁写入；typed关闭在idle确认、busy延期/拒绝、强停和team-close等到实际退出才终态。（验证：协议与取消屏障，恶意文本前缀。）
- [ ] **AC6 取消恢复：** 父completed/failed/断socket后团队继续；父显式取消及独立stop只作用关联turn；service关闭清理自身worker/watchers；首queued前、queued/running/stopping、child terminal/run/idle/close gap恢复两次无重跑；积压保留、显式resume才继续且预算不清零。（验证：双parent/双team、持久fixture/provider计数。）
- [ ] **AC7 协调器：** run开始前绑定授权team，prompt/schemas/hard executor一致、当前run模式静态；文件读写/搜索/command/MCP/network/fork/D递归/todo明确拒绝；member不能伪lead、越权新建成员；用户仍可close/stop并退出下一run模式。（验证：direct-call fake provider及gate/hook/audit。）
- [ ] **AC8 有界与隐私：** service/team/member/turn累计额度、message pending/batch、lead32KiB、task/request/query全上限生效；入队/终态/关闭持久失败明确可恢复；日志/UI没有原始角色、thinking、credential、child transcript；消息/计划作为脱敏显式参考数据。（验证：边界/超限/故障/敏感标记fixtures。）
- [ ] **AC9 组合回归：** 完整client/TUI协作路径、M05 compaction前后team facts不丢、游标重连不覆盖parent；A/B/C/D、M06 todo、普通Session/Goal、权限、候选与独立目标验证保持通过。（验证：fake集成及已授权云端Go/E2E/package。）

## 用户与父工具完整场景

- [ ] 建队→两个角色成员→第一轮调查→p2p/broadcast→idle→第二轮继续→结果发lead；普通父run可同时继续，成员消息只在下一匹配父run交接。
- [ ] 创建A/B依赖任务→两个成员并发claim→A完成解除B→B完成；冲突显示最新revision，不改session todo或Goal成功状态。
- [ ] 要求plan approval的成员调查并提交；lead拒绝后修订，批准后继续只读；过期请求不默认批准。错误请求ID/响应者拒绝。
- [ ] 忙成员收到shutdown可延期或拒绝，空闲成员确认退出；用户强stop取消实际child；关闭team禁止新消息/spawn且保留历史。
- [ ] 队满、消息pending满、成员累计预算满均显示准确状态；后续容量释放只唤醒已授权成员，不建立无限goroutine或额外池。
- [ ] 双session/双Goal/WorkItem构造相同name、伪sender/root、其它team/member/task ID全部拒绝；普通消息含`[shutdown]`不会关闭成员。
- [ ] 断线重连还原同一team/member/task/request；服务重启无provider请求，显式resume接管未完成batch；连续恢复与replay无重复终态/tool result。
- [ ] coordinator选择/退出、run静态工具过滤与用户stop/close入口一致；所有读写工程和递归工具直接调用也被拒绝。

## 源端覆盖和工程证据

- [ ] 对照 `teams/teams.go`、`spawn.go`、`inprocess.go`、`runner.go` 的多轮/idle/progress实际行为，记录摘要延续替代完整Conv的差异。
- [ ] 对照 `filemailbox.go`、`tools.go`、`registry.go` 的p2p/broadcast和全局fallback；队内受信路由/精确handoff有实际证据。
- [ ] 对照 `sharedtask.go`、`tasktools.go` 与请求/coordinator源码，记录新增依赖门/CAS及coordinator允许任务板的明确差异，不虚称源端原有保障。
- [ ] 原始transcript保存不迁移；源端仅元信息/解析字段继续区分实际执行；tmux未迁移、iTerm本平台不适用、WorktreePath/写成员归M09-F。
- [ ] 所有fake fixtures临时构造，无真实provider调用、源端运行、用户配置读取、外部窗格/容器或重型本机构建。
- [ ] 格式化、diff、协议与文档引用检查通过，所有新增共享字段已review；实现关闭自身临时进程且保留用户/其它任务服务。
- [ ] GitHub Actions记录代码SHA、workflow/run链接、每个Go/E2E/package job以及失败历史；重跑必须对应修复后的实现提交。
- [ ] README与M09总进度更新为E实现验收状态，M09-F仍独立审批验收，整个M09未完成时不宣称完成。

## 验证记录

E实现与运行验收进行中。每项完成后记录实际证据；D/A/B/C旧CI不作为E实现证据。

| 全量回归（非 E AC 完成证明） | 代码 SHA | Workflow / run / job | 结果与限制 |
|---|---|---|---|
| Go build + `go test ./...` | `8a34ea045a13435b955c0c1fb8b57afe1f417aba` | [Go run 37640598695](https://github.com/kikoiio/Stable/actions/runs/37640598695), `build-and-test` | 通过；仅证明该 SHA 的全量 Go 检查通过，E AC1–9 仍未完成逐项验收。 |
| E2E workflow | `8a34ea045a13435b955c0c1fb8b57afe1f417aba` | [E2E run 37640598926](https://github.com/kikoiio/Stable/actions/runs/37640598926), `unit`, `e2e-core`, `e2e-m03`, `e2e-m04`, `e2e-sessions`, `cases` | 所有 job 通过；现有 workflow 未单独覆盖 checklist 要求的完整 team 多轮/恢复组合，不能据此勾选 AC9。 |
| Go package acceptance | `8a34ea045a13435b955c0c1fb8b57afe1f417aba` | [Go run 37640598695](https://github.com/kikoiio/Stable/actions/runs/37640598695), `test-package` | 通过；run 总结由 in_progress 更新为 success，job 完成于 2026-10-07 15:16 UTC。仅是预览改动前 SHA 的结果。 |
| Preview regression first run / repair | `90ed0d7` → `75594bd2c4279c115c18fb5f22c741f868dcfcb5` | [failed Go run 37643405245](https://github.com/kikoiio/Stable/actions/runs/37643405245), `build-and-test`; [repaired Go run 37644076609](https://github.com/kikoiio/Stable/actions/runs/37644076609), `build-and-test` | 首次单测失败仅因 preview lifecycle fixture 未带可信 authority；修复 fixture 后 build 与 `go test ./...` 通过。package job 与 E2E workflow 仍在运行。 |
| 组合代码 Go build/unit + package | `e24331a0ac6d9745f9dc80fc8aed0142d47d2210` | [Go run 37651671848](https://github.com/kikoiio/Stable/actions/runs/37651671848), `build-and-test`, `test-package` | 两个 jobs 均通过；包含 lead 消息自动唤醒 idle member 的 fake-runner 集成测试。此结果不替代 E AC1–9 的逐项证明。 |
| 组合代码 E2E | `e24331a0ac6d9745f9dc80fc8aed0142d47d2210` | [E2E run 37651671760](https://github.com/kikoiio/Stable/actions/runs/37651671760) | `unit`、`cases`、`e2e-m03`、`e2e-m04`、`e2e-sessions` 成功；`e2e-core` 的 `dependency_change.sh` 在 checker 版本恢复后未收敛，goal agent run 终态为 failed。不能记录全量 E2E 通过。 |
| 修复后组合代码 E2E | `f8fce2d15bc3262a770300057cc7638209f4c672` | [E2E run 37664859557](https://github.com/kikoiio/Stable/actions/runs/37664859557) | `unit`、`e2e-core`、`e2e-m04`、`e2e-sessions` 成功；`e2e-m03`、`cases` 因 `setup-e2e-deps` 无进展取消。`dependency_change.sh` checker 恢复已通过；现有 workflow 仍不能替代 E AC1–9 的完整多轮/恢复组合验收。 |
| 修复后组合代码 Go/package | `f8fce2d15bc3262a770300057cc7638209f4c672` | [Go run 37664859562](https://github.com/kikoiio/Stable/actions/runs/37664859562) | `test-package` 的 `setup-e2e-deps` 超过 30 分钟上限；`build-and-test` 停滞在同一步后取消。全量 Go 单测由同 SHA 的 E2E `unit` job 通过；package acceptance 未完成。 |
| 最终组合代码 Go/package | `f458c91a587004af839409c65ee8c44bd4b93b13` | [Go run 37673116497](https://github.com/kikoiio/Stable/actions/runs/37673116497) | `build-and-test` 与 `test-package` 均成功；不替代 E AC1–9 的逐项证明。 |
| 最终组合代码 E2E | `f458c91a587004af839409c65ee8c44bd4b93b13` | [E2E run 37673116514](https://github.com/kikoiio/Stable/actions/runs/37673116514) | `unit`、`cases`、`e2e-core`、`e2e-m03`、`e2e-m04`、`e2e-sessions` 全部成功；`dependency_change.sh` checker 恢复通过。仍缺完整 team 多轮/取消/恢复与 client/TUI AC9 组合场景。 |
| 最新兼容代码 Go/package + E2E | `ea24f8e9e82225f3d8879c4165d77a0e5077fdfe` | [Go run 37675602216](https://github.com/kikoiio/Stable/actions/runs/37675602216), [E2E run 37675602223](https://github.com/kikoiio/Stable/actions/runs/37675602223) | Go `build-and-test`、`test-package` 与 E2E `unit`、`cases`、`e2e-core`、`e2e-m03`、`e2e-m04`、`e2e-sessions` 全部成功；新增双有效 session 同名 team 隔离测试纳入全量 unit。仍不替代 E AC1–9 完整协作/恢复验收。 |
| D/E/F 当前组合代码复验 | `8223aaf6915669ff87d15264cef5cbdb85293e24` | [Go run 37677736986](https://github.com/kikoiio/Stable/actions/runs/37677736986), [E2E run 37677737032](https://github.com/kikoiio/Stable/actions/runs/37677737032) | Go build/unit、package 与 E2E 全部 jobs 成功，含新增 E 双 session 同名 team 隔离和 D 独立目标复核测试；E AC1–9 仍未整体验收。 |
| Goal/WorkItem owner 隔离及当前 E/F 组合回归 | `108b964564d6bce9a08ca7285b5302f733a7a7de` | [Go run 37804001646](https://github.com/kikoiio/Stable/actions/runs/37804001646), [E2E run 37802252496](https://github.com/kikoiio/Stable/actions/runs/37802252496), [M09 Workspace Linux run 37801933759](https://github.com/kikoiio/Stable/actions/runs/37801933759) | Go `build-and-test`/`test-package`、E2E 全部 jobs 和 `writer-sandbox-volume` 成功。首次 Go run 37802254996 的 build/unit 因无关的 `TestLegacyHistoryIsNotReplayedButLiveGoalMessagesBroadcast` socket read 10 分钟超时；单独 Go run 复验通过。新增 Goal/WorkItem 测试只补 AC1 owner 子场景，AC1–9 仍未整体验收。 |
| Service task claim race 与当前 E/F 组合回归 | `c0f523d40faec92c3682f9f56445b9ce292185ae` | [Go run 37805889376](https://github.com/kikoiio/Stable/actions/runs/37805889376), [E2E run 37805888980](https://github.com/kikoiio/Stable/actions/runs/37805888980), [M09 Workspace Linux run 37807400979](https://github.com/kikoiio/Stable/actions/runs/37807400979) | Go `build-and-test`/`test-package`、E2E 所有 jobs 与 `writer-sandbox-volume` 全部成功。包含并发 service claim/replay 测试；只补 AC4 中同 revision 竞争仅一个持久 owner 的子场景，AC1–9 与完整 task-board 场景仍未整体验收。 |
| Task dependency service/replay 与当前组合回归 | `f61e2fb` | [Go run 37824906854](https://github.com/kikoiio/Stable/actions/runs/37824906854) `build-and-test`/`test-package`; [E2E run 37824906843](https://github.com/kikoiio/Stable/actions/runs/37824906843); [Workspace Linux run 37824906787](https://github.com/kikoiio/Stable/actions/runs/37824906787) `writer-sandbox-volume` | Go 两 jobs、E2E 全部 jobs 与 writer sandbox/quota 成功。dependency service/replay 测试纳入 Go/unit；不替代 AC4/AC1–9 完整验收。 |
| Parent history isolation 与当前 E/F 组合回归 | `35b130bb` | [Go run 37826465763](https://github.com/kikoiio/Stable/actions/runs/37826465763) `build-and-test`/`test-package`; [E2E run 37826465801](https://github.com/kikoiio/Stable/actions/runs/37826465801); [Workspace Linux run 37826465661](https://github.com/kikoiio/Stable/actions/runs/37826465661) `writer-sandbox-volume` | Go 两 jobs、E2E 全部 jobs 与 writer sandbox/quota 成功。新增 parent-history privacy test 在 Go/unit 覆盖；不替代 sibling isolation、compaction/reconnect 或 AC1–9 完整验收。 |
| Parent/sibling context isolation 与当前 E/F 组合回归 | `ac817faf` | [Go run 37828073176](https://github.com/kikoiio/Stable/actions/runs/37828073176) `build-and-test`/`test-package`; [E2E run 37828073232](https://github.com/kikoiio/Stable/actions/runs/37828073232); [Workspace Linux run 37828073186](https://github.com/kikoiio/Stable/actions/runs/37828073186) `writer-sandbox-volume` | Go 两 jobs、E2E 全部 jobs 与 writer sandbox/quota 成功；Go/unit 包含双成员私有任务上下文和父历史隔离测试。未覆盖 compaction/reconnect 与 AC1–9 完整验收。 |
| Interrupted message retry after restart 与当前 E/F 组合回归 | `26eafc55` | [Go run 37869857770](https://github.com/kikoiio/Stable/actions/runs/37869857770) `build-and-test`/`test-package`; [E2E run 37869857737](https://github.com/kikoiio/Stable/actions/runs/37869857737); [Workspace Linux run 37869857758](https://github.com/kikoiio/Stable/actions/runs/37869857758) `writer-sandbox-volume` | Go 两 jobs、E2E 全部 jobs 与 writer sandbox/quota 成功。首次 E2E run 的 `e2e-core` setup 因 Launchpad PPA `GPGKeyTemporarilyNotFoundError` 失败，未启动 `make e2e`；仅复跑失败 job 后全绿。新测试在 Go/unit 覆盖显式重试，不替代 AC3/AC6 全场景。 |
| Shared pool admission 与容量等待公平恢复 | `6121f543e342330dcd90eb38469138b5317074ec` | [Go run 37822847214](https://github.com/kikoiio/Stable/actions/runs/37822847214) `build-and-test`; [E2E run 37822847081](https://github.com/kikoiio/Stable/actions/runs/37822847081) `unit` | 全池/队列满时首 member spawn 返回 queue-full 且未留 member/turn facts；释放容量后两 member 可启动，消息令二者持久进入 `waiting_capacity`，恢复按 FIFO，原始 team history 中每条消息仅一条 handoff。覆盖 AC2 子场景，不证明父/兄弟 transcript 隔离、idle worker 计数及完整多轮恢复矩阵。 |
| 当前组合 Go/package、E2E 与 Workspace Linux | `6121f543e342330dcd90eb38469138b5317074ec` | [Go run 37822847214](https://github.com/kikoiio/Stable/actions/runs/37822847214) `build-and-test`/`test-package`; [E2E run 37822847081](https://github.com/kikoiio/Stable/actions/runs/37822847081); [Workspace Linux run 37822846869](https://github.com/kikoiio/Stable/actions/runs/37822846869) `writer-sandbox-volume` | Go 两 jobs、E2E `unit`/`cases`/`e2e-core`/`e2e-m03`/`e2e-m04`/`e2e-sessions` 与 writer sandbox/quota job 全部成功。只记该 SHA 的回归，不替代 E AC1–9 完整场景。 |

| 本地定向检查 | 实际结果 | 覆盖范围与限制 |
|---|---|---|
| conversation/sessionlog/workspace 定向 Go 测试 | 通过（2026-10-07）；选择 `TestTeam*`、`TestThreeWay*` | 验证FIFO队首阻塞/硬上限、容量等待重启转 interrupted、提交后发布失败补偿、terminal/member-state恢复间隙、team event 投影与三方合并；不是AC2/AC9或全量验收。首次默认`/tmp`链接因配额失败，改用缓存盘`TMPDIR`后通过。 |
| team conversation/sessionlog 定向 Go 测试 | 通过（2026-10-07）；`TestTeamMemberContinuesAcrossRestartWithExplicitRoleChangeAcceptance`、`TestTeamRecovery*`、`TestTeamRoleMetadataCanChangeOnlyOnExplicitLeadResume`，`TMPDIR=/home/neo/.cache/tmp GOMAXPROCS=2 go test -p 1` | Fake child runner验证同一成员两轮、摘要与消息延续、角色正文未入日志、重启不自动重跑、角色变化先提示后显式接受；recovery把存活idle/awaiting_plan成员标为interrupted。未覆盖共享池屏障/队满公平、双parent取消和完整AC2/AC6。 |
| `team_send` 协议入口与 idle member handoff | `TestLeadMessageAutomaticallyResumesIdleTeamMember`；最终 SHA `f458c91` 的 E2E `unit` job | `validateClient` 和 `handleTeamRequest` 真实入口返回有序、接收者固定的消息；sessionlog投影与响应一致；同一逻辑member收到消息后仅启动一个新turn。覆盖此子场景，不覆盖 p2p/broadcast 批次恢复、共享池公平或完整 AC2/AC3。 |
| 双有效 session 同名 team 隔离 | `TestSameTeamNameIsIsolatedAcrossValidSessions`；Go run 37677736986 `build-and-test` 与 E2E run 37677737032 `unit` 成功 | 两个有效 lead run 在同一 project 各自创建 `research`；team ID 独立，两个 session 的 list/get 仅返回本 session 团队并拒绝查询对方 ID。仍未覆盖 Goal/WorkItem owner、跨 session 消息与 stop 全路径，AC1 保持未完成。 |
| Goal/WorkItem team owner 隔离 | `TestGoalTeamIsIsolatedByGoalAndWorkItem`；E2E run 37802252496 `unit` 与 Go run 37804001646 `build-and-test` 成功 | 同一 session 下，匹配的 Goal/WorkItem 可创建并查询 team；更换 WorkItem 或 Goal 后不能查询原 team。只覆盖查询隔离，不覆盖 Goal 消息/stop、不同授权 root 等完整 AC1。 |
| Team task service 并发 claim | `TestTeamTaskServiceConcurrentClaimsPersistOneOwner`；Go run 37805889376 `build-and-test`、E2E run 37805888980 `unit` 成功 | 两个 lead 更新以相同 expected revision 指派不同的有效成员；一项更新成功、一项返回 revision conflict，sessionlog replay 仅保留 revision + 1 和一个 assignee。仍未证明取消语义、完整 CRUD/依赖组合或 AC4 全面通过。 |
| Team task dependency service/replay contract | `TestTeamTaskServicePersistsDependencyGateAndRevision`；本地 conversation 定向测试通过（2026-10-09） | 真实 service 创建并重放依赖图；未知依赖和成环更新拒绝，依赖未完成时进行/完成均拒绝，完成前后 canonical `blockedBy` 与查询投影 `blocks` 一致，stale revision 拒绝。仍未覆盖 M06 todo/Goal 工作项隔离及取消不伪完成，AC4 未整体验收。 |
| Team member parent history isolation | `TestTeamMemberContinuesAcrossRestartWithExplicitRoleChangeAcceptance`；SHA `35b130bb` Go `build-and-test` 与 E2E `unit` 成功 | 在 session event log 放置父对话标记，确认首轮与显式续跑输入均不包含父历史；续跑只携带成员摘要和明确交付消息。尚未覆盖 sibling 内容隔离与 compaction 前后组合，AC2/AC9 未整体验收。 |
| Team sibling context isolation | `TestTeamMembersReceiveOnlyTheirOwnTurnContext`；本地 conversation 定向测试 `-count=3` 与 SHA `ac817faf` Go `build-and-test`/E2E `unit` 通过 | 两个真实 service member 使用不同私有任务标记；各自输入仅包含本成员指派内容，第二成员未收到第一成员指令/摘要，二者均未收到父 session 消息。未覆盖跨 session sibling、消息 recipients 边界与 compaction 重连组合，AC2/AC9 未整体验收。 |
| Running-turn message handoff | `TestTeamMessageSentDuringRunningTurnIsHandedOffOnlyToNextTurn`；定向 `go test ./internal/conversation -run '^TestTeamMessageSentDuringRunningTurnIsHandedOffOnlyToNextTurn$' -count=1` 通过（2026-10-09） | gated child runner 确认运行中的首轮不接收后到消息、不并行启动；显式 resume 后同一 member 的新 turn 收到消息，日志只持久化一条指向该 turn 的 handoff。未覆盖发送与 handoff 并发锁竞态、容量/重启 replay 或 AC3 全量。 |
| Interrupted message handoff retry after restart | `TestInterruptedMessageHandoffIsExplicitlyRetriedAfterRestart`；本地定向测试 `-count=3` 与 SHA `26eafc55` Go `build-and-test`/E2E `unit` 通过 | 消息已持久交付至中断 turn 后运行恢复；恢复没有调用 child runner，显式 resume 在新 turn 中只收到一份消息，第二条 handoff 以 `RetryOfTurnID` 指向前一中断 turn。覆盖 AC3 destination retry/restart 子场景；未覆盖追加 handoff 写入失败和完整 client/TUI 重连。 |
| Busy-turn force stop ordering | `TestStopTeamMemberRemainsStoppingUntilChildExits`；定向 conversation 与 sessionlog request tests 通过（2026-10-09） | busy child 被 lead force-stop 后持久化唯一 approved typed shutdown，状态保持 `Stopping`，实际 child exit 后才写唯一 terminal；重复 stop 不重复写 request。覆盖并修复了 service response actor 对活动 turn 的协议校验；未覆盖 shutdown defer/reject、plan approval/expiry 或 AC5 全量。 |
| Plan approval, rejection and read-only revision | `TestTeamPlanApprovalAutomaticallyStartsReadOnlyFollowUp`；定向 conversation Go 测试通过（2026-10-09） | plan-required member 通过真实 team tool 提交请求；伪 member responder 和 stale revision 被拒绝；lead rejection 唤醒只读修订 turn并携带有界反馈；修订请求获批后再启动只读 turn，工具 allowlist 不变。未覆盖 AC5 全量错误请求/响应者矩阵。 |
| Team request expiry lifecycle | `TestExpiredTeamRequestIsPersistedAndCannotBeAnswered`；本地定向 conversation 测试 `-count=3` 通过（2026-10-09） | 直接响应与列表查询分别将到期 plan/shutdown request 持久化为单次 `expired` 终态；重放确认 revision 增长且重复 list 不写第二条过期事件。未覆盖服务关闭期间跨截止时间、错误 responder/请求ID全矩阵或 AC5 全量。 |
| Busy shutdown defer/reject protocol | `TestBusyTeamShutdownCanBeDeferredThenRejectedByMember`；本地定向 conversation 测试 `-count=3` 通过（2026-10-09） | 忙成员收到 lead shutdown 后可延期再拒绝；lead 不能伪作成员 responder，延迟/拒绝期间 child 仍 running，实际结束后回到 idle。未覆盖 shutdown approval 在 idle 的独立路径或 team close/AC5 全量。 |
| Team close waits for child exit | `TestCloseTeamWaitsForActualChildExit`；本地定向 conversation 测试 `-count=3` 通过（2026-10-09） | 活动 child 存在时关闭进入 `closing`，成员与 turn 不提前终态；gated runner 实际返回后才停止成员并关闭 team，未创建重复 child。未覆盖 crash/restart close gap 与 AC5/AC6 全量。 |
| Parent cancellation scope for team children | `TestCancelParentRunCancelsOnlyItsTeamChildren`；定向 conversation Go 测试通过（2026-10-09） | 同一 session/team 的两个 active parent 各自启动 child；取消 A 只取消 A child 并仅调用 `Runner.Cancel(A)`，B 保持 running；随后单独取消 B 清理。未覆盖 parent 正常终态/断线继续、service close 与全部 crash gaps，AC6 仍未整体验收。 |
| Coordinator mode snapshot per run | `TestCoordinatorModeIsSnapshottedForEachRun`；定向 conversation Go 测试通过（2026-10-09） | 真实 `startRun` 启动 coordinator A；A 活跃时关闭 session 模式，A 的可信 flag/team-only schemas/prompt 不变；后续普通 B 使用默认 schemas 且无 coordinator prompt。未覆盖全部 direct-call/tool 矩阵或 TUI close/stop，AC7 仍未整体验收。 |
| team TUI 定向 Go 测试 | 通过（2026-10-07）；`TestTeamCommands`，`TMPDIR=/home/neo/.cache/tmp GOMAXPROCS=2 go test -p 1` | 验证 `/team ... resume ... --accept-role-change` 编码显式确认；不代表完整client/TUI协作路径或AC9。 |
| TUI team list 经真实 conversation socket/service | `TestTeamListTUIRequestUsesConversationServiceAndPreservesParentRun`；本地定向 `go test ./internal/tui -run '^TestTeamListTUIRequestUsesConversationServiceAndPreservesParentRun$' -count=1` 通过（2026-10-09） | 有效 session/team facts 经 Unix socket 被 `/teams list` 查询，TUI 更新 team state、system event 和状态栏；请求前后父 run ID、游标与 stream 保持。只覆盖读取入口，未覆盖写操作、重连或 AC9 完整组合。 |
| 真实共享池满载与 FIFO 容量恢复 | `TestTeamCapacityUsesSharedPoolAndResumesWaitingMessagesFairly`；本地 conversation 定向测试 `-count=3`、Go run 37822847214 `build-and-test` 与 E2E run 37822847081 `unit` 均通过 | 真实 `PoolDelegator` 以 1 worker/1 queue 屏障验证首 spawn 满载不提交事实、后续两 member 消息持久等待容量、先入队者先恢复、每 member 恢复一个 turn且各有单条 durable message handoff。仍未覆盖 transcript隔离、服务重启及完整 AC2。 |
