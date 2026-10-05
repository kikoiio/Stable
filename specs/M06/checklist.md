# M06 计划与任务交互 Checklist

> 状态:验收完成(2026-10-06)。基于已批准的 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md)。全部 25 项以运行代码或观察行为的证据勾选;验收中发现并修复 2 项缺口(018df26 transcript 渲染、测试时序竞态),修复后复验通过。

## 斜杠命令体系

- [x] **C01 / AC1**:补全列表来自注册表,含内置与自定义命令。(证据:`go test ./internal/tui/...` m06_test 补全断言与 `./internal/commands/...` 13 项测试通过;内置 10 命令 + 自定义命令均出现在补全。)
- [x] **C02 / AC1**:会话运行中新增/修改/删除 `.md` 命令文件后,无需重启,补全与执行即可感知。(证据:e2e `TestM06CustomCommandsHotReloadAndExpansion` 断言新增/删除文件后补全列表变化;commands loader mtime 缓存单测。)
- [x] **C03 / AC1**:`$ARGUMENTS` 替换与无占位符附加两种形态正确;子目录命名空间命令可补全并执行。(证据:commands loader 单测两形态与 `git:log` 命名;e2e 断言展开文本经 run 进入会话日志首条 user 消息。)
- [x] **C04 / AC1**:自定义命令与内置同名时内置优先且可观察;`/help` 输出与实际可用命令一致。(证据:registry `RegisterOptional` 冲突返回 false 单测;T12 `/help` 测试列出全部注册命令。)
- [x] **C05 / N5**:超限(深度/大小/数量)与非法(不可读、frontmatter 坏)命令文件被跳过并出报告,TUI 不崩溃。(证据:commands loader 单测——深度 4、超 256KB、坏 frontmatter、别名冲突文件均跳过且带路径原因出现在报告。)

## 计划模式与审批

- [x] **C06 / AC2**:`/plan` 切换计划模式,`plan_mode` 事件落会话日志,状态栏可见;重启后回到默认模式。(证据:conversation `TestPlanModeToggleOp` 事件断言;e2e 计划场景全新服务初始 plan_state=default;状态栏「计划模式」段 T13 测试;PlanState 为 Service 内存态,新服务即默认。)
- [x] **C07 / AC2**:计划模式下读放行、候选区写与命令仍询问、仅当前计划文件免询问写;越出该路径的写仍走审批。(证据:permission `TestPlanModeMatrix` 判定矩阵——计划文件写 allow、候选写 ask、正式工程写 deny、命令 ask、读 allow,且 `.stable/plans/` 同级文件精确路径 deny;T9 直写集成测试。)
- [x] **C08 / AC2**:非计划模式下调用 `exit_plan_mode` 返回明确错误。(证据:execution 单测 + e2e `TestM06ExitPlanModeOutsidePlanMode` 返回「当前不在计划模式」且无 plan 事件。)
- [x] **C09 / AC3**:计划审批三选项分别产生:后续运行 acceptEdits 语义 / 后续运行保持 default / 保持计划模式且反馈文本进入下次运行上下文。(证据:conversation `TestPlanResolveBranches` 四分支断言;e2e auto 场景断言后续写真实走候选区且正式工程不变;feedback 场景断言模式保持 plan 且反馈进消息队列。)
- [x] **C10 / AC3**:计划提交、批准、取消、反馈落 `plan_approval` 事件,重启投影一致;审批标识与目标提案 prop-XXX 独立。(证据:sessionlog `TestPlanApprovalValidation` 单次终态;e2e 断言 submitted/approved_auto 事件与 replay;ID 独立性核对——计划审批 `sessionlog.NewID()`、提问 `q-`、提案 `prop-` 前缀互异;transcript 渲染计划审批块 `TestTranscriptRendersM06Events`。)

## 提问

- [x] **C11 / AC4**:`ask_user` 校验 1–4 题、每题 2–4 选项,超限返回明确错误结果;multiSelect 与「其他」自由输入可用。(证据:execution 参数校验表测试;TUI model 测试多选 Space/Enter 逗号连接、`o` 自由输入、空拒绝。)
- [x] **C12 / AC4**:弹层答复与 `/reply`(带 question id)答复都使问题置为已答且 agent 收到答案继续;重复答复被 M05 校验拒绝。(证据:e2e `TestM06AskUserReplyRoundTrip`——pending_question 事件、questions 推送、reply 后置 replied、agent 第二轮收到答案、重复 reply 报 already answered;T11 闭环单测。)
- [x] **C13 / AC4**:运行结束后遗留的 pending 问题经 `/reply` 答复,产生排队用户消息,下次运行进入上下文。(证据:e2e `TestM06AskLeftoverReplyQueuedForNextRun`——取消后问题仍 pending、晚期 reply 产生排队 EventMessage、下次运行 provider 侧实测上下文含该文本。)
- [x] **C14 / N2**:提问答复、todo、计划文件落盘前应用凭据脱敏;无法安全脱敏时拒绝写入,不落明文。(证据:e2e todo 场景注入凭据后文件与会话日志均为 `[credential redacted]`;todo store 短凭据拒写且文件字节不变单测;T9 计划直写短凭据拒写;ask Prompt 落盘前经 `redactProviderCredential`。)

## todo

- [x] **C15 / AC5**:task_create/get/list/update 增改查与依赖(Blocks/BlockedBy)正确;deleted 移除任务并清理悬空引用。(证据:todo 19 项单测——CRUD 全路径、deleted 与依赖变更即时清理悬空引用、并发 -race。)
- [x] **C16 / AC5**:重启后当前会话清单恢复;每次变更在 transcript 可见;todo 文件不进候选区、rewind 后仍存在。(证据:e2e `TestM06TodoPersistenceRedactionAndRestart` 重启后 Revision 接续且 task_list 恢复;transcript 任务清单块 `TestTranscriptRendersM06Events`;todo 存储在项目 `.stable/tasks/`,候选 manifest 仅覆盖候选根(T9 直写测试证明候选区不吸纳此类写入)。)

## 提案弹窗

- [x] **C17 / AC6**:提案弹层确认/拒绝与 `/confirm` `/reject` 文本命令到达同一状态机结果;「稍后」不改变提案状态;pending 提案在 transcript 可见。(证据:T13 弹层测试——c/r 复用既有 op 且 Esc 不改状态;e2e `TestM06ProposalProtocolStateMachines` reject→rejected、confirm→confirmed+revision 1+pending_reverification。)

## 队列与审计

- [x] **C18 / AC7**:权限审批 > 提问 > 计划审批 > review > 提案 同时待决策时按序呈现;退出当前弹层可处理下一个。(证据:T13 `pendingDialog()` 优先级测试——五类同时构造断言渲染顺序与首个按键归审批、Esc 逐层降落。)
- [x] **C19 / N1**:命令执行、模式切换、计划审批、提问答复、提案确认全部落会话日志,重启后投影一致;损坏日志明确失败。(证据:e2e 各场景按会话日志事件断言;sessionlog 校验对非法追加/损坏记录拒绝(M05 故障注入用例随全量测试回归通过);T7 投影往返测试。)

## 集成与编译

- [x] **C20**:六个新工具在 chatserve 与 runtime 两处白名单可见;未注入对应 sink 时调用返回明确错误而非 panic。(证据:`TestChatserveToolSchemas`/`TestRuntimeToolSchemas` 断言 12 工具按名排序无重复;T9 nil sink 返回「提问通道不可用/审批通道不可用/任务清单通道不可用」。)
- [x] **C21**:计划文件写入不进沙箱、不进候选区(候选 manifest 无变化);plan/todo 文件权限 0600、目录 0700。(证据:T9 直写测试断言候选 store 无记录;e2e 计划场景候选区未创建;planfile/todo 权限位单测(显式 Chmod 防 umask、符号链接拒绝)。)
- [x] **C22**:agentworker 沙箱 helper 不感知新工具,既有 unknown tool 行为不变。(证据:cmd/agentworker 零改动(git 范围核对);既有测试随全量通过。)
- [x] **C23 / 编译**:`gofmt -l` 无输出,`go vet ./...` 干净。(证据:验收时执行——paths.go 历史对齐问题已顺带修复;两命令均干净。)
- [x] **C24 / 测试**:`go test ./...` 全部通过。(证据:验收终验 33 包全 ok、0 FAIL;期间发现的 `TestAskAdapterAskReplyLoop` 间歇失败经定位为测试自身时序竞态(等待者注册先于事件落盘属产品有意设计),已改为轮询断言并 -count=8 复验通过。)

## 端到端场景

- [x] **C25 / 全链路**:`tests/e2e/m06_interaction.sh` 全部场景通过。(证据:验收时亲自复跑——8 场景(命令热更新、计划 auto 审批、feedback 分支、非计划模式报错、ask_user 问答、遗留排队、todo 持久化脱敏重启、提案状态机)全 PASS,末行 `M06 e2e: all scenario groups passed.`;`m05_sessions.sh` 回归通过。)
