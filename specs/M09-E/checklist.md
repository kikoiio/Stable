# M09-E 团队、消息与只读协调器 Checklist

> 状态：规格已批准，实施代码已提交；AC1–9 与完整场景仍按实际证据逐项验收。最终 SHA `f458c91` 的 Go build/unit、package 与 E2E 全部 jobs 通过；E AC1–9 仍有逐项验收缺口（2026-10-08）。

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

| 本地定向检查 | 实际结果 | 覆盖范围与限制 |
|---|---|---|
| conversation/sessionlog/workspace 定向 Go 测试 | 通过（2026-10-07）；选择 `TestTeam*`、`TestThreeWay*` | 验证FIFO队首阻塞/硬上限、容量等待重启转 interrupted、提交后发布失败补偿、terminal/member-state恢复间隙、team event 投影与三方合并；不是AC2/AC9或全量验收。首次默认`/tmp`链接因配额失败，改用缓存盘`TMPDIR`后通过。 |
| team conversation/sessionlog 定向 Go 测试 | 通过（2026-10-07）；`TestTeamMemberContinuesAcrossRestartWithExplicitRoleChangeAcceptance`、`TestTeamRecovery*`、`TestTeamRoleMetadataCanChangeOnlyOnExplicitLeadResume`，`TMPDIR=/home/neo/.cache/tmp GOMAXPROCS=2 go test -p 1` | Fake child runner验证同一成员两轮、摘要与消息延续、角色正文未入日志、重启不自动重跑、角色变化先提示后显式接受；recovery把存活idle/awaiting_plan成员标为interrupted。未覆盖共享池屏障/队满公平、双parent取消和完整AC2/AC6。 |
| `team_send` 协议入口与 idle member handoff | `TestLeadMessageAutomaticallyResumesIdleTeamMember`；最终 SHA `f458c91` 的 E2E `unit` job | `validateClient` 和 `handleTeamRequest` 真实入口返回有序、接收者固定的消息；sessionlog投影与响应一致；同一逻辑member收到消息后仅启动一个新turn。覆盖此子场景，不覆盖 p2p/broadcast 批次恢复、共享池公平或完整 AC2/AC3。 |
| 双有效 session 同名 team 隔离 | `TestSameTeamNameIsIsolatedAcrossValidSessions`；Go run 37675602216 `build-and-test` 与 E2E run 37675602223 `unit` 成功 | 两个有效 lead run 在同一 project 各自创建 `research`；team ID 独立，两个 session 的 list/get 仅返回本 session 团队并拒绝查询对方 ID。仍未覆盖 Goal/WorkItem owner、跨 session 消息与 stop 全路径，AC1 保持未完成。 |
| team TUI 定向 Go 测试 | 通过（2026-10-07）；`TestTeamCommands`，`TMPDIR=/home/neo/.cache/tmp GOMAXPROCS=2 go test -p 1` | 验证 `/team ... resume ... --accept-role-change` 编码显式确认；不代表完整client/TUI协作路径或AC9。 |
