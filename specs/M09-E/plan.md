# M09-E 团队、消息与只读协调器 Plan

> 状态：已批准，实施中（2026-10-07）。用户批准 M09-E 四份规格文档及其定义的实现范围；验收以确切代码 CI 为准。

## 架构与所有权

新增 `internal/teams` 只承载类型、有限状态投影、任务依赖图和协作规则；不拥有独立模型客户端、全局用户目录或事实文件。`conversation.TeamService` 从受信 `agent.TeamScope` 构造lead/member身份，写session事件、派发共享池turn并管理关闭。execution仅依赖agent包的窄TeamService接口，避免import conversation循环。runtime注入一个service、同一个catalog和同一个PoolDelegator。

每团队永久绑定 `{session_id, work_kind, goal_id, work_item_id, authorized_root}`。用户slash从服务的session配置建立session scope；Goal团队由Goal父run建立，普通session不能操作其运行内容。lead是匹配WorkRef的受信父run权限，creator run保留审计来源但不阻止同一受信WorkRef的新run接管。成员turn带不可伪造的team/member/turn关联；只在注册的活动turn内能调用成员工具。

## 持久事件与投影契约

所有ID由服务产生，复用现有sessionlog append锁、session cursor和run cursor。新增一个严格校验的 `team_event` 类型，payload包含 `team_id`、事件ID、revision、actor来源和类型化字段，覆盖：

| 事件族 | 事实与合法性 |
|---|---|
| team_created/closing/closed | 名称、WorkRef、授权root identity、创建run、配置上限；关闭为单向状态，不能删除历史事实 |
| member_added/state | ID/name/role元数据/provider name/model/只读能力、角色snapshot标记；不存role正文；created→queued/running→idle/awaiting_plan/interrupted/budget_exhausted/stopped |
| member_turn_accepted/terminal | turn/run/task关联、OriginRunID/OriginCallID、消息批次ID、预算扣除/累计耗时、summary/error；一成员一活动turn；独立RunStarted带team/member/turn来源字段 |
| message_sent/handoff | 消息ID、脱敏正文、typed协议字段、受信sender、接收者固定集合；每recipient一次destination引用，queued成功才能确认交接 |
| task_created/updated | task ID、revision、owner、status/description、canonical blocked_by；blocks派生；依赖graph在锁内验证 |
| request_created/responded/expired | 计划/关闭type、request ID、目标actor、正文、有效期和明确决策；sender与响应者匹配，conflict不写入 |
| lead_handoff | 消息/终态seq、destination parent run；destination有run_started后才视为交接；按完整WorkRef隔离 |

projection拒绝越权关联、跳跃状态、重复terminal、非法revision和超限payload。开放team/member和pending投递有界缓存；关闭历史分页投影，不在RAM保留原始全历史。读取采用sessionlog流式折叠/游标，并验证基础归属；如采用派生checkpoint则必须有cursor/hash校验且可完全从事件重建，不能成为第二事实源。M05 compaction不丢失team facts、pending消息及request；实现需定义保留/重建规则并用压缩前后同一查询验收。

角色快照在存活成员内持有，正文不落盘。重启后member只有元数据，resume重新从catalog Resolve相同角色并验证可用性/只读allowlist；角色内容变更须向用户显示变更并由显式resume接受新的快照，不能声称恢复了旧正文。已结束结果仍可读取；provider凭据从runtime受信配置获得，不由事件还原凭据。

## 执行与调度

共享池继续只执行短turn。E在D单项预算/SubmitTask基础上扩展受控child input，使成员工具集可以包含只读项目inspection以及自己的team通信/任务工具。D角色tools/disallowedTools规则继续限制project inspection；team通信/任务能力来自E受信membership，使用独立固定allowlist，不能通过角色字段申请lead能力或接受未知team工具扩权。`TeamMemberExecutor` 首先按team scope和精确allowlist拦截，team工具进入受信service，文件工具转已有readonly executor；不把全权限父executor交给成员。coordinator有单独hard allowlist，不能以schema过滤代替实际拒绝。

仅一个service级scheduler持有最多16个member ID的ready集合。消息写入、task分配、计划响应和resume只合并ready信号；不为每信号生成goroutine或复制大消息。调度采用round-robin，每成员至多一个turn；单一pool非阻塞SubmitTask满时保留已有持久pending事实、标记waiting_capacity，等待pool释放容量信号后公平重试，不能轮询busy-loop。E需新增pool只读capacity通知或服务共享队列wake，仍不新增worker。

member spawn以team/member额度和pool接受原子结果作为成功边界：若首turn队满则拒绝，不留下声称已活动的成员；若已经有成员后续消息被持久接受而池满，消息返回sent并显示waiting_capacity，不能伪装成turn已启动。首次显式任务以lead assignment消息记录并限长/脱敏，spawn失败则同一次意图标aborted，不能把失败spawn的assignment投递给别的同名成员。服务启动前持久member/turn关联、pool queued与handoff有跨事件间隙，采用accepted intent+queued确认投影，失败补aborted intent并释放额度；成功回应必须确认所需事实全部持久成功。

每turn输入由role正文、只读身份说明、最近已脱敏摘要、固定消息批次和必要任务状态构成；全编码64KiB检查在queued之前。超长role导致无法装入最小任务时spawn/resume明确拒绝，不能截断角色并假装完整执行。消息batch最多8条/32KiB，lead batch同上；超出剩余编码预算留未交接。消息是引用数据而非system instructions。累计turn/时间预算由持久accepted/terminal facts计算，取消仍计已接受turn，恢复间断按照已知耗时与保守未结束turn预算计费，不因restart退款或清零。

后台turn沿service context和单项timeout；父显式取消在同一注册锁/取消代次下取消当时OriginRunID的turn。turn来源取最近有效lead assignment/send/resume的父run；member互发消息继承接收成员当前lead assignment来源，不把发送者的独立child run伪装成lead。来源更新与批次选择一同持久，取消旧父不会误取消已由新父接管的turn。正常父终态不停止member。结果watcher数量至多pool queued+running但实际E至多16活动member；终态持久失败保留可恢复事实并向调用者/查询显示persist error，不发布虚假idle。空闲不启动计时goroutine；有限请求过期由单一scheduler timer推进。关闭等候只观察done，不持child worker。

## 消息、任务与请求

发送执行顺序为身份/收件者校验→pending容量检查→脱敏及长度校验→单条message_sent写入→广播与ready信号。广播一条事实携带收件人集合，写盘失败没有部分投递。采用工具CallID/请求client token作幂等键；重试同输入返回原消息ID，复用键不同输入拒绝。每recipient-handoff保留message ID + destination turn ID；没有queued事实的destination不能吞邮件，queued后重启interrupted不会自动再执行，用户显式resume可选择尚未完成的已交接batch并在新turn关联为显式retry，旧事实不改写。

依赖任务图在team事件锁内操作；expected_revision必填于claim/update，冲突返回最新revision而不默默覆盖。canonical边为task的blocked_by，blocks由反向查询投影；环检验可在最多256节点上进行。取消member保留任务归属并使其blocked/interrupted原因可见，lead可明确重分配；不能自动标完成。模型declared completed只表示团队协作状态，Goal成功或候选验收仍由原流程负责。

计划/关闭请求的关系由request ID、requester、responder、team/member和type固定。普通team_send不能制造控制事件。plan_required成员首调查turn可发送消息/查询自己的任务，但执行状态停在awaiting_plan，lead-approved继续只读；reject触发明确修订turn，expire进入idle且plan仍未批准。busy shutdown先标deferred；当前turn如果主动用request_list查询，可显式答复拒绝，拒绝保留member。没有拒绝的请求在turn结束达到idle后由服务确认并关闭，不另起无预算模型turn。强stop不等模型同意。team_close拒绝新写入并强取消所有turn，实际退出与持久结果全部确认后closed；持久失败保留closing供恢复补齐。

## 协议与工具

窄接口草案如下，具体JSON字段实现前按spec定稿；scope只由受信host构造，客户端/模型参数不能直接供scope。

```go
type TeamScope struct {
    Parent ParentRun
    TeamID, MemberID, TurnID string // 后三项由已注册member执行器注入
}
type TeamService interface {
    Call(context.Context, TeamScope, TeamOperation) (TeamResult, error)
}
```

TeamOperation采用封闭kind+typed args，不包含sender/root/bounds/credential。list/get的结果不返回角色正文。普通lead经现有pre/post hooks、permission gate、tool call/result配对路径；团队host动作使用受信project scope的受控read权限门加service身份限制，数据修改仅为协作事件。member动作无递归parent hook，沿既有child隐私策略；coordinator仍走parent audit/hooks且其hook child权限不扩大coordinator模型自身工具集。

conversation增加 `team_create/get/list/close`、`team_member_spawn/get/list/stop/resume`、`team_send/messages`、`team_task_create/get/list/update`、`team_request_list/respond`及coordinator run参数。客户端变更型请求带稳定idempotency token；query复用Limit/AfterSeq，有界wait。未知字段/后台或模型参数的非法类型明确拒绝。

TUI以session cursor复用独立run派发机制，保留父ActiveRunID，分别渲染team/member状态、消息和requests；所有详情仅按当前授权scope请求。coordinator绑定持久session设置供下一普通run使用，Goal仅由相应Goal run参数显式启用，模式不会悄悄从session扩展至不相关Goal。用户slash始终可stop/close并退出下一run的coordinator，避免工具过滤造成无法收尾。

## 文件边界

| 所有者 | 文件范围 | 内容 |
|---|---|---|
| domain | `internal/teams/{types,projection,taskgraph,limits}.go` | 有限状态、依赖、身份规则，无provider和外部终端 |
| 事件 | `internal/sessionlog/{events,validate,log}`与team投影 | team facts、来源/CallID、handoff、恢复合法性 |
| 共享池/child | `internal/agent/{delegation,delegation_runner}`、team接口 | 可控turn input、capacity通知，维持single pool与现有预算 |
| service | `internal/conversation/team_*.go` | 受信身份、调度、消息、请求、任务、关闭/恢复 |
| 接入负责人 | `conversation/{protocol,service,run,client}`、`runtime/supervisor.go` | 单实例绑定、lead交接、scope/取消、协议 |
| 工具 | `internal/execution/team_*.go`、factory/dispatch | typed args、成员/lead/coordinatorhard allowlist与audit |
| UI | `internal/tui`、必要`cmd/stable`入口 | slash、分页、请求、coordinator和独立turn展示 |

共享events/protocol/service/run文件由主负责人统一安排；轻量独立模块可并行，重型验证由主负责人云端集中执行。

## 验证与风险闭合

以fake child屏障验证共享池竞争、每成员single turn及不占worker的idle；fake provider捕获两轮输入与工具边界。故障注入覆盖message/intent/queued/handoff/child terminal/run terminal/idle/close写盘间隙；重启两次不调用provider，resume才继续。两session/Goal/WorkItem、双parent取消、重复call/token和version冲突验证隔离。消息中带`[shutdown]`、伪role instructions和credential markers不能改变权限或触发控制。M05 compaction、TUI重连及A–D fake回归一起完成后，在已授权GitHub Actions进行全量Go/E2E/package验收。
