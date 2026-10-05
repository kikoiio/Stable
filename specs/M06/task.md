# M06 计划与任务交互 Tasks

> 状态:已批准(2026-10-05)。基于已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。每步完成即运行该任务「验证」;每任务完成即提交。

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `internal/redact/redact.go` (+测试) | 共用 ReplaceAll 脱敏 |
| 新建 | `internal/planfile/planfile.go` (+测试) | 计划路径/Ensure/Exists |
| 新建 | `internal/commands/registry.go`、`loader.go` (+测试) | 命令注册表、.md 加载、热更新、展开 |
| 新建 | `internal/prompt/plan_mode.go` (+测试) | 计划工作流提醒(全文/精简) |
| 修改 | `internal/permission/model.go`、`policy.go` (+测试) | Authority.PlanFilePath、ModePlan 分支 |
| 新建 | `internal/todo/task.go`、`store.go`、`tools.go` (+测试) | 任务模型/存储/工具 schema |
| 修改 | `internal/sessionlog/events.go`、`validate.go`、`projection.go` (+测试) | 3 新事件+校验+投影 |
| 新建 | `internal/execution/tools_schema.go` (+测试) | 6 个新工具 schema |
| 修改 | `internal/execution/executor_factory.go`、`tool_executor.go` (+测试) | sinks 注入与四分支+计划直写 |
| 新建 | `internal/conversation/plan.go` (+测试) | PlanState、PlanApprovalService、reminder 注入 |
| 修改 | `internal/conversation/protocol.go`、`session.go`、`run.go`、`questions.go`、`polls.go` (+测试) | 新 op、问询适配、轮询、session_load |
| 修改 | `internal/tui/completion.go`、`model.go` (+测试) | 注册表接线、/plan /help |
| 新建 | `internal/tui/question_dialog.go`、`plan_dialog.go`、`proposal_dialog.go` (+测试) | 三弹层 |
| 修改 | `cmd/stable/chatserve.go`、`internal/runtime/supervisor.go` | 工具白名单 |
| 新建 | `tests/e2e/m06_interaction.sh` | 端到端场景 |

## T1: 提取共用脱敏包

**文件:** `internal/redact/redact.go`、`redact_test.go`;改 `internal/inputhistory/redact.go`、`internal/conversation`、`internal/execution` 中的同模式实现
**依赖:** 无
**步骤:**
1. 新建 `internal/redact`,提供 `Redact(text string, credentials []string) (string, error)`,保留「凭据过短(<8 字节)拒绝、空凭据跳过、`[credential redacted]` 占位」语义
2. inputhistory/conversation/execution 删除各自的本地实现,改为调用 `redact.Redact`(inputhistory 保留薄包装以免大面积改调用点)
**验证:** `go build ./...`;`go test ./internal/inputhistory/... ./internal/conversation/... ./internal/execution/...` 全绿(脱敏相关既有用例不变)

## T2: planfile 模块

**文件:** `internal/planfile/planfile.go`、`planfile_test.go`
**依赖:** 无
**步骤:**
1. `PlanPath(projectRoot, sessionID)` 返回 `<root>/.stable/plans/<sessionID>.md`
2. `Ensure` 创建目录(0700)与文件(0600,仅当不存在),返回路径与 existed
3. `Exists` 判定;路径穿越(会话 ID 含 `/`、`..`)报错
**验证:** `go test ./internal/planfile/...` — 路径拼接、二次 Ensure 幂等、非法 sessionID 拒绝

## T3: commands 模块

**文件:** `internal/commands/registry.go`、`loader.go` 及测试
**依赖:** 无
**步骤:**
1. `Registry`:Register(冲突 panic)/RegisterOptional(冲突 false)/Find(含别名)/List(按名排序)
2. `Parse(input) (name, args string)`:首段 `/name` 与余下参数
3. `Loader`:递归扫描两目录(深度 ≤3、单文件 ≤256KB、总数 ≤200,超限跳过并出报告);项目同名覆盖用户级;命令名 = 相对路径小写、空格→`-`、目录用 `:` 连接;frontmatter 手写解析 description/argument-hint/aliases;`Commands()` 以目录 mtime 为缓存键,未变直接返回
4. `ExpandPrompt(body, args)`:`$ARGUMENTS` 全量替换;无占位符且有参数时追加 `\n\n## User Request\n\n<args>`
**验证:** `go test ./internal/commands/...` — 冲突内置优先、命名空间、两种参数形态、mtime 热更新(改文件后 Commands() 结果变化)、越界文件跳过并报告

## T4: 计划提醒文本

**文件:** `internal/prompt/plan_mode.go`、`plan_mode_test.go`
**依赖:** 无
**步骤:**
1. 改编源端五阶段为 Stable 版:声明计划模式只读语义与唯一可写计划文件(含路径);Phase 1 直接读文件调研(无子代理);Phase 2 设计;Phase 3 用 ask_user 澄清;Phase 4 写最终计划(推荐方案、关键文件、验证章节);Phase 5 调 exit_plan_mode 并结束回合
2. `BuildPlanModeReminder(planPath string, planExists bool, iteration int) string`:首次全文,此后每 5 次一次全文、其余精简版
**验证:** `go test ./internal/prompt/...` — iteration=1 全文、2–5 精简、6 全文;文本含计划路径

## T5: ModePlan 策略分支

**文件:** `internal/permission/model.go`、`policy.go`、`policy_test.go`
**依赖:** 无
**步骤:**
1. `Authority` 增加 `PlanFilePath string`
2. `Policy.Decide`:Mode==ModePlan 且 OpWrite 且解析目标 == PlanFilePath → allow;ModePlan 其余判定路径与 ModeDefault 相同(读 allow,写/命令 ask);非 plan 模式对 PlanFilePath 的写维持既有硬边界(正式工程只读 → deny)
**验证:** `go test ./internal/permission/...` — 三种模式 × 计划文件/候选/正式路径判定矩阵

## T6: todo 模块

**文件:** `internal/todo/task.go`、`store.go`、`tools.go` 及测试
**依赖:** T1
**步骤:**
1. `Task`/`UpdatePatch`/状态常量;ID 用项目既有 `RandomID("task")` 风格
2. `Store`:`.stable/tasks/<sessionID>.json`,0600;Load/Save;Save 前对 subject/description/activeForm/metadata 做 `redact.Redact`,失败则整体拒绝写入
3. `TaskList`:互斥锁内 Load→修改→Save→`onChange([]Task)`;Create(上限 100)/Get/List/Update(status 迁移 pending→in_progress→completed,deleted 移除并清理 Blocks/BlockedBy 悬空引用);sessionID 路径穿越拒绝
4. `tools.go`:task_create/task_get/task_list/task_update 四个 schema 常量(照源端字段,新增 `deleted` 说明)
**验证:** `go test ./internal/todo/...` — CRUD、依赖清理、并发(多 goroutine Update 不丢更新)、凭据拒绝落盘、100 上限

## T7: sessionlog 三新事件

**文件:** `internal/sessionlog/events.go`、`validate.go`、`projection.go` 及测试
**依赖:** T6
**步骤:**
1. `EventPlanMode`/`EventPlanApproval`/`EventTodo` 常量与 payload struct(todo 快照用本包 `TaskSnapshot` 结构,conversation 层负责与 `todo.Task` 互转)
2. validate:`plan_approval` 每个 RequestID 仅允许一次终态(submitted 后仅一个终态追加);`todo_update` Revision 严格递增、任务数 ≤100;`plan_mode` 直通
3. projection:新增 `ItemPlanMode`/`ItemPlanApproval`/`ItemTodo` 与渲染数据;`prompt.MessagesFromItems` 与 `sessionConversationMessages` 不消费这三类(不进模型上下文)
**验证:** `go test ./internal/sessionlog/...` — 重复终态拒绝、revision 回退拒绝、超 100 拒绝、投影 Item 断言

## T8: execution schema 与依赖注入

**文件:** `internal/execution/tools_schema.go`(新)、`executor_factory.go` 及测试
**依赖:** T5, T6
**步骤:**
1. `tools_schema.go`:ask_user(questions 1–4、每题 2–4 选项、multiSelect)、exit_plan_mode(空 schema)、task_create/get/list/update 六个 schema(照源端字段名)
2. `executor_factory.go`:`WithQuestionSink/WithPlanSink/WithTodoProvider` options;deps 透传到 toolRunExecutor
**验证:** `go test ./internal/execution/...` — schema 结构断言;未注入 sink 时调用相应工具返回明确错误结果

## T9: execution 四分支与计划直写

**文件:** `internal/execution/tool_executor.go` 及测试
**依赖:** T2, T5, T7, T8
**步骤:**
1. `Execute` 前置分支:`write_file`/`edit_file` 且解析目标 == `Authority.PlanFilePath` → 主机侧直写(redact → 写文件 → ToolResult),不进沙箱、不进候选区
2. `ask_user`:校验题数/选项数(超限返回错误结果)→ `QuestionSink.Ask` 阻塞;ctx 取消返回 "Question cancelled"
3. `exit_plan_mode`:非计划模式(PlanFilePath 空)返回错误;否则 `PlanSink.SubmitPlan` 阻塞,auto/manual 返回「计划已批准,end turn」文本;feedback/cancel 按错误类型返回对应文本
4. `task_*`:→ `TodoProvider.For(sessionID)` 对应方法,结果渲染为文本清单
**验证:** `go test ./internal/execution/...` — 各分支正常/异常路径;计划直写后候选 manifest 无变化

## T10: conversation 计划服务与 op

**文件:** `internal/conversation/plan.go`(新)、`session.go`、`run.go`、`protocol.go`、`plan_test.go`
**依赖:** T2, T4, T5, T7
**步骤:**
1. `PlanState`(map+互斥,重启清空);op `plan_mode`:切换 Mode、`planfile.Ensure`、Append `EventPlanMode`、返回状态;再次切换记录 Reason
2. `PlanApprovalService`:Submit(submitted + Append + 广播 `plan_approval_pending`)、Resolve(choice/feedback/cancel → 终态 Append + 插入普通用户消息 + 广播 `plan_approval_resolved`);auto 批准同步 PlanState.ExecutionMode=acceptEdits 并置 Mode=default(Reason=plan_approved)
3. op `plan_resolve`(校验会话归属与 pending 态)
4. `BuildAuthority` 增加 mode/planFilePath 参数;run 开始时:plan 模式则 PlanFilePath 填入 Authority、Runs++ 并按 `BuildPlanModeReminder` prepend 上下文(不落事件)
5. session_load 响应附带 PlanState
**验证:** `go test ./internal/conversation/...` — op 状态机、事件断言、Authority 断言、reminder 注入次数;非归属会话 resolve 拒绝

## T11: conversation 问询/todo/轮询

**文件:** `internal/conversation/questions.go`、`polls.go`、`protocol.go` 及测试
**依赖:** T7, T10
**步骤:**
1. `AskAdapter`(实现 execution.QuestionSink):Append `EventQuestion` → 广播 `questions` → 轮询(500ms)该 QuestionID 的 `EventReply` → 组装 `AskResponse`;ctx 取消退出;维护 sessionID→活跃等待计数
2. `replyQuestion` 扩展:该问题无活跃等待者时,追加 `EventReply` 后再追加一条 `EventMessage`(user,答复文本)排队
3. `TodoProvider` 实现:按 sessionID 建/复用 TaskList,onChange Append `EventTodo` + 广播 `ServerMsg{Type:"todo"}`
4. poll 循环扩展:广播 pending questions 与 pending plan approvals
**验证:** `go test ./internal/conversation/...` — Ask→reply 闭环、等待者计数、无等待者答复产生排队消息、todo onChange 事件与广播、poll 输出

## T12: TUI 注册表接线

**文件:** `internal/tui/completion.go`、`model.go` 及测试
**依赖:** T3
**步骤:**
1. 初始化 Registry:内置 /sessions /goals /search /review /say /reply /confirm /reject /plan /help(/plan KindLocal 切换计划模式,/help 列出 Registry.List())
2. `refreshCompletions` 改走 Registry + Loader(Loader 挂在 Model,输入时惰性刷新);被拒文件报告进状态栏一次性提示
3. 提交路径:KindPrompt → ExpandPrompt → chat;KindLocal → 闭包
**验证:** `go test ./internal/tui/...` — 补全项来自注册表(含自定义)、/help 输出、/plan 触发 op、同名内置优先

## T13: TUI 三弹层与优先级队列

**文件:** `internal/tui/question_dialog.go`、`plan_dialog.go`、`proposal_dialog.go`(新)、`model.go` 及测试
**依赖:** T10, T11, T12
**步骤:**
1. 三个弹层渲染与键位:提问(1–4 选项、space 多选、其他自由输入、esc 跳过)、计划审批(↑↓ 三选项 + 反馈输入 + esc)、提案(c 确认 / r 拒绝 / esc 稍后)
2. 弹层优先级函数:权限审批 > 提问 > 计划审批 > review > 提案;Update key 分发与 View 渲染按此短路
3. `handleResult`/`applyRunMessage` 新 case:`questions`/`plan_approval_pending`/`todo`/`proposal` 更新状态并触发弹层;`plan_resolve`/`reply(question_id)` op 发送
4. 状态栏显示计划模式;session_load 后按 PlanState/Questions 恢复
**验证:** `go test ./internal/tui/...`(model 测试)— 优先级顺序、各弹层键位→op 断言、提案确认/拒绝与文本命令等价、esc 不改状态

## T14: 工具白名单

**文件:** `cmd/stable/chatserve.go`、`internal/runtime/supervisor.go`
**依赖:** T8
**步骤:** 两处 nameMap 追加 `ask_user`、`exit_plan_mode`、`task_create`、`task_get`、`task_list`、`task_update`;schema 经 `tools_schema.go` 常量追加,保持按名排序
**验证:** `go build ./...`;两处白名单单测(如存在)通过;schema 数量断言更新

## T15: 端到端

**文件:** `tests/e2e/m06_interaction.sh`(新)
**依赖:** T9, T13, T14
**步骤:**
1. 场景串接(fake provider 脚本):自定义命令 .md 新增→补全可见→执行展开;改文件→无需重启生效
2. /plan 进入→agent 写计划文件(候选 manifest 无变化)→exit_plan_mode→auto 批准→后续写走 acceptEdits
3. ask_user→弹层答复→agent 继续;运行取消后遗留问题 /reply→排队消息下次运行可见
4. task_create/update→transcript 可见→重启恢复
5. /goal 提案→弹层确认→状态与文本命令一致
**验证:** `make e2e`(或定向运行该脚本)全部断言通过

## T16: 全量验证收尾

**文件:** —
**依赖:** T15
**步骤:** `gofmt -l internal cmd` 为空;`go vet ./...`;`go test ./...` 全绿;核对每任务已按序提交
**验证:** 三条命令输出干净;`git log --oneline` 含各任务提交

## 执行顺序

```
B1(并行): T1  T2  T3  T4  T5
B2:        T6(T1)        T12(T3)
B3:        T7(T6)        T8(T5,T6)
B4(并行): T9(T2,T5,T7,T8)   T10(T2,T4,T5,T7)   T14(T8)
B5(并行): T11(T7,T10)        T13(T10,T11,T12)
B6:        T15(T9,T13,T14) → T16
```

并行冲突协调:T9/T10 不同包可并行;T10→T11 因共享 protocol.go 串行;T12→T13 因共享 model.go 串行;T14 只追加名字与 schema 引用,与 T9 无文件冲突。
