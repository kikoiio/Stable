# M09-E 团队、消息与只读协调器 Checklist

> 状态：已批准，实施中（2026-10-07）。用户批准 M09-E 四份规格文档；验收项取得实际证据后逐项勾选。

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

| 本地定向检查 | 实际结果 | 覆盖范围与限制 |
|---|---|---|
| conversation/sessionlog/workspace 定向 Go 测试 | 通过（2026-10-07）；选择 `TestTeam*`、`TestThreeWay*` | 验证FIFO队首阻塞/硬上限、容量等待重启转 interrupted、提交后发布失败补偿、terminal/member-state恢复间隙、team event 投影与三方合并；不是AC2/AC9或全量验收。首次默认`/tmp`链接因配额失败，改用缓存盘`TMPDIR`后通过。 |
