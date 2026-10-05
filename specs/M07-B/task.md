# M07-B Hooks Tasks

> 状态:待批。基于已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。每步完成即运行该任务「验证」;每任务完成即提交。

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `internal/hooks/hooks.go`、`hooks_test.go` | 类型、Validate、条件求值、Fire/FireOne、command 执行 |
| 新建 | `internal/hooks/loader.go`、`loader_test.go` | 两文件合并加载、自动 id、拒绝报告 |
| 修改 | `internal/appconfig/config.go`(+测试) | `UserHooksPath()` |
| 修改 | `internal/sessionlog/events.go`、`log.go`、`validate.go`、`projection.go`(+测试) | 2 新事件+白名单+校验+投影 |
| 修改 | `internal/execution/executor_factory.go`、`tool_executor.go`(+测试) | HookRunner 接口、option、pre/post 插入点 |
| 新建 | `internal/conversation/hooks.go`、`hooks_test.go` | HookGate:合并视图、once、async、队列、落盘 |
| 修改 | `internal/conversation/protocol.go`、`service.go`(+测试) | op 与 ServerMsg 类型、持有 HookGate、op 分发 |
| 修改 | `internal/conversation/run.go`(+测试) | 通知注入、RunStart/RunEnd 触发 |
| 修改 | `cmd/stable/chatserve.go`、`internal/runtime/supervisor.go` | HookGate 构造与双通道注入 |
| 修改 | `internal/tui/model.go`(+测试) | `/hooks` 命令、hook_list/hook_report 处理 |
| 修改 | `internal/tui/transcript.go`(+测试) | 2 类事件渲染 |
| 新建 | `tests/e2e/m07b_hooks_test.go`、`m07b_hooks.sh` | 端到端场景 |

## T1: hooks 引擎核心

**文件:** `internal/hooks/hooks.go`、`hooks_test.go`;`go.mod`(yaml.v3 已为直接依赖,确认即可)
**依赖:** 无
**步骤:**
1. `Event` 常量 4 项(run_start/run_end/pre_tool_use/post_tool_use);`Action`(yaml tag:command/message/url/method/headers/body/timeout)、`Hook`(id/event/if/action/reject/once/async/on_error + `Source` 加载器填充)、`Context`(event/tool_name/tool_args/file_path/message)、`Result`(hook_id/output/success/rejected/timed_out)类型
2. `Validate(list []Hook) error`:逐条聚合(`errors.Join`),标签用显式 id 或 `hook[i]`;事件白名单、按动作类型必填字段(command 需非空 command;prompt 需非空 message;http 需合法 http(s) URL;agent 需 message 或 command)、timeout 非负、on_error ∈ {""/fail/ignore/reject}
3. `EvaluateCondition(cond, ctx) bool`:移植源端 `splitComposite`/`evaluateLeaf`/`resolveVar`——`==`/`!=`/`=~`(正则)/`=*`(glob)与 `&&`/`||`/`!` 左结合组合、无操作符真值检查;变量 `tool`/`event`/`file_path`/`message`/`args.<字段>`
4. `FireOne(h Hook, ctx Context) Result`:动作执行——command 经 `bash -c`(`STABLE_EVENT`/`STABLE_TOOL`/`STABLE_FILE_PATH` 环境变量,stdout+stderr 合并 TrimSpace,默认超时 10min,deadline 区分 `TimedOut`);prompt 输出=message;http/agent 返回固定错误「<action> 动作未启用(http 留 M07-C / agent 留 M09)」;`Rejected = h.Reject || (!Success && OnError=="reject")`
5. `Fire(list []Hook, ctx Context) []Result`:按序对 event 匹配且条件通过的 hook 执行 FireOne(不含 once/async)
**验证:** `go test ./internal/hooks/...` — 条件四种比较与组合、Fire 顺序与短路无关性(全部执行)、reject 计算、超时标记、环境变量注入(http/agent 未启用文本)

## T2: hooks 配置加载与合并

**文件:** `internal/hooks/loader.go`、`loader_test.go`
**依赖:** T1
**步骤:**
1. `LoadFiles(userPath, projectPath string) LoadResult`:单文件规则——缺失→静默跳过;符号链接→整文件拒绝;>`MaxFileBytes=256KB`→整文件拒绝;不可读/非法 YAML→整文件拒绝;解析 `hooks:` 键下数组
2. 逐条 `Validate` 失败→该条拒绝;文件内显式 id 重复→后者拒绝;缺 id→自动 `user:N`/`project:N`(文件内序号,1 起);单文件条数 >`MaxHooksPerFile=100`→超出部分拒绝
3. 合并:用户条目在前、项目条目在后;跨文件显式 id 冲突→项目覆盖用户(用户条目移除、保留项目条目位置);`Source` 填 `user`/`project`
4. `Rejections []string` 格式「<路径>: <原因>」
**验证:** `go test ./internal/hooks/...` — 合并优先级、文件内重复、自动 id、symlink/超大小/坏 YAML/非法 hook 拒绝且报告完整、两文件均缺失时零 hook 无错误

## T3: appconfig 用户 hooks 路径

**文件:** `internal/appconfig/config.go`、`config_test.go`
**依赖:** 无
**步骤:** `UserHooksPath() (string, error)`:与 `UserSkillsDir` 同目录基(XDG_CONFIG_HOME/`$HOME/.config`)+ `stable/hooks.yaml`;测试覆盖 XDG 与 HOME 两分支
**验证:** `go test ./internal/appconfig/...`

## T4: sessionlog 两新事件

**文件:** `internal/sessionlog/events.go`、`log.go`、`validate.go`、`projection.go` 及各自测试
**依赖:** 无
**步骤:**
1. `EventHookFired`/`EventHookReload` 常量;`HookFired`(hook_id/event/action/source/success/rejected/output/run_id)、`HookReload`(before/after)Data 类型;`MaxHookOutput = 8*1024` 常量
2. Append 与 replay 两处白名单各追加 2 项
3. validate.go:`hook_fired` 校验 hook_id/event/action 非空、output 长度 ≤ MaxHookOutput、run_id 可空;`hook_reload` 校验 before/after 非负;**无 owned-append 唯一性约束**(同类事件允许多条)
4. 投影:`ItemHookFired`/`ItemHookReload` ItemKind + 2 case;确认 `prompt.MessagesFromItems` 不消费两类(不进模型上下文)
**验证:** `go test ./internal/sessionlog/...` — append/replay 往返、字段校验拒绝、投影 Item 断言、多条 hook_fired 合法

## T5: execution HookRunner 与工具插入点

**文件:** `internal/execution/executor_factory.go`、`tool_executor.go` 及测试
**依赖:** 无
**步骤:**
1. `executor_factory.go`:`HookRunner` 接口(PreToolUse 返回 rejected/message;PostToolUse 接收定稿 result)+ `WithHookRunner` option + `ToolExecutorDeps.HookRunner` 字段
2. `tool_executor.go` `Execute`:EventToolCall 落盘之后、`executeHostTool` 分支之前插入 pre 调用——`rejected` 为真时 `outcome = {CallID, ToolName, Status: ToolDenied, IsError: true, Content: "Blocked by hook <id>: <msg>"}` → `finish`(tool_result 落盘)→ 返回;权限门未执行
3. post 经 named-return `defer`:条件「pre 已通过且未被拒绝 && `ctx.Err() == nil` && HookRunner 非 nil」,取 `finish` 定稿后的 `outcome.Content` 调用 `PostToolUse`
**验证:** `go test ./internal/execution/...` — fake HookRunner 断言:拒绝路径 Gate 未被调用且结果含 hook id、pre 先于主机工具分支(主机工具同样被 hook)、post 收到脱敏截断后内容、pre 拒绝后无 post、取消路径无 post

## T6: conversation HookGate

**文件:** `internal/conversation/hooks.go`、`hooks_test.go`
**依赖:** T1, T2, T4
**步骤:**
1. `HookGate` 结构:`service *Service`(Bind 挂接)、`userPath/projectPath`、`loaded []hooks.Hook`、`rejections []string`、`modTimes map[string]time.Time`、`onceFired map[string]map[string]bool`、`queue map[string][]string`(均为 gate 自己的 mutex 保护)
2. `ensureLoaded`:两文件 mtime 对比 → 变化或首次 → `hooks.LoadFiles` 重建;`Reload()` 强制重建并返回前后数量
3. `fire(sessionID, ev, hctx, runID)`:遍历合并列表——event 匹配 → 条件求值(通过才消耗 once:检查并标记 `onceFired[session][id]`)→ `Async` 则 goroutine 内执行+落盘+入队、否则同步执行 → 输出 `redactRunCredential` 脱敏 → 截断 `MaxHookOutput` → `svc.eventMu` 下落 `hook_fired`(经 `appendEventLocked` 同模式)→ 非空输出入 `queue[session]`(上限 20 条,超限丢最旧并在新条目附「(更早通知已丢弃)」标注)
4. `PreToolUse`:同步触发全部匹配 pre hook,遇首个 `Rejected` 短路返回 `(true, 该 hook 输出或 "blocked by hook <id>")`;其余输出照常入队
5. `RunStart(sessionID, runID, intent)` / `RunEnd(sessionID, runID, status, message)` / `PostToolUse(sessionID, toolName, args, result)`:调 `fire`(run 级事件 `run_id` 落盘;tool 级 Context 填 tool_name/file_path(从 args 的 file_path 类字段提取)/message)
6. `DrainNotifications(sessionID) string`:取走清空队列,组合 `<hook-notification id="...">\n<output>\n</hook-notification>` 段
7. `List() ([]HookSummary, []string)` / `Rejections()`:`ensureLoaded` 后返回
**验证:** `go test ./internal/conversation/...` — fire 顺序与 once 消耗(条件不通过不消耗)、async 不阻塞且输出最终入队、队列上限丢弃标注、pre 短路、落盘事件断言、mtime 热更新、Restart 无持久状态(queue/once 为空)

## T7: conversation 接线与 run 触发

**文件:** `internal/conversation/protocol.go`、`service.go`、`run.go` 及测试
**依赖:** T6
**步骤:**
1. `protocol.go`:`validOp` 加 `hooks_list`/`hooks_reload`;`HookSummary{ID, Event, Action, Source string; Reject, Once, Async bool}`、`ServerMsg` 加 `HookList *HookListMsg{Hooks []HookSummary; Rejections []string}` 与 `HookReport *HookReportMsg{Before, After int}`
2. `service.go`:`Deps.Hooks *HookGate`(nil 关闭);`Service.hooks`;Serve 内 `deps.Hooks != nil` 则 `Bind(s)`;op 分发:`hooks_list`(无会话要求)→ `HookList`;`hooks_reload` → `Reload()` + `hook_reload` 事件落请求 SessionID 会话(ClientMsg.SessionID 非空时)+ `HookReport`
3. `run.go` `startRun`:技能清单块调用处之后(事件锁外)`s.hooks.DrainNotifications(sessionID)`,非空则追加为 user 消息——会话 run 并入 skillPrefix 同层(system→技能段→hook 通知段,历史之前);目标 run 前置 `request.Messages` 首位;事件锁释放后、`Runner.Start` 前调 `s.hooks.RunStart(sessionID, request.RunID, request.Intent)`(两类 run 均触发)
4. `run.go` `consumeRun`:循环内累积 `EventTextDelta` payload 的 text;`outcome := <-handle.Done` 后、`finalizeRunCandidate` 前调 `s.hooks.RunEnd(sessionID, runID, string(outcome.Status), message)`——message:`RunCompleted` 取累积文本(截断 `MaxHookOutput`),其余取「<status>: <error 摘要>」
**验证:** `go test ./internal/conversation/...` — 两类 run 的注入位置、RunStart 在 Runner.Start 前且 RunStarted 事件之后、run 终态四分支均触发 RunEnd、通知注入后清空不重复、ops 分发与 hook_reload 事件

## T8: 进程接线

**文件:** `cmd/stable/chatserve.go`、`internal/runtime/supervisor.go`
**依赖:** T7
**步骤:**
1. chatserve:`userHooksPath()`(仿 `userSkillsDir`,错误返回空串);`hookGate := conversation.NewHookGate(nil, userHooksPath(), filepath.Join(*projectRoot, ".stable", "hooks.yaml"))`;executorFactory 选项追加 `execution.WithHookRunner(hookGate)`;`Deps.Hooks: hookGate`
2. supervisor:同构(项目路径基于 `p.Share` 拼 `.stable/hooks.yaml`)
**验证:** `go build ./...`;`go test ./cmd/... ./internal/runtime/...`(既有测试不回归)

## T9: TUI /hooks 命令与报告

**文件:** `internal/tui/model.go` 及测试
**依赖:** T7
**步骤:**
1. `registerBuiltins` 新增 `hooks`:无参 → 发 `hooks_list` op(带当前 SessionID);参数 `reload` → 发 `hooks_reload` op;描述「列出 hooks(服务侧合并视图)」
2. `ServerMsg` 处理:`hook_list` → 格式化渲染进 chat 视区(每行 id、event、action、来源、reject/once/async 标记;无 hook 时提示放置路径 `.stable/hooks.yaml` 与 `~/.config/stable/hooks.yaml`;rejections 逐条列出),样式仿 `/skills`;`hook_report` → 状态栏「Hooks 重载: N → M」
**验证:** `go test ./internal/tui/...` — 命令注册与 op 发送、hook_list 渲染(含拒绝报告)、hook_report 状态栏

## T10: TUI transcript 渲染

**文件:** `internal/tui/transcript.go` 及测试
**依赖:** T4
**步骤:** `hook_fired` → 单行块「Hook `<id>`(`<event>`) ✓/✗[已拒绝]: <输出截断>」;`hook_reload` → 「Hooks 重载: N → M」;重放渲染一致
**验证:** `go test ./internal/tui/...` — 两块渲染与重放一致性断言

## T11: 端到端

**文件:** `tests/e2e/m07b_hooks_test.go`、`tests/e2e/m07b_hooks.sh`(风格对齐 `m07a_skills_test.go`)
**依赖:** T7, T8, T9, T10
**步骤:**(fake provider 脚本,真实 conversation unix socket 驱动)
1. 项目+用户两级 `hooks.yaml`(command 的 run_start/prompt 的 run_end/pre_tool_use reject/post_tool_use 各一)→ `/hooks` 列表含两级来源与合并结果;跨文件同 id 项目覆盖
2. 会话 run:transcript 出现 run_start/run_end 的 hook_fired 块;触发 reject hook 的工具调用 → 工具结果为「Blocked by hook <id>: …」且权限审批未弹出;post_tool_use 输出在下一轮 run 以系统提醒可见(注入一次不重复)
3. `once` hook 同会话第二次不触发;`async` hook 输出最终入队;http/agent 动作返回「未启用」错误文本
4. 会话中修改 hooks.yaml → 下一次触发即用新配置;`/hooks reload` 报告数量变化
5. 重启服务 → transcript 投影一致(hook_fired/hook_reload 块重现,通知不重复注入)
**验证:** 运行该脚本全部场景 PASS;`m07a_skills.sh`、`m05_sessions.sh`、`m06_interaction.sh` 回归通过

## T12: 全量验证收尾

**文件:** —
**依赖:** T11
**步骤:** `gofmt -l internal cmd` 为空;`go vet ./...`;`go test ./...` 全绿;核对每任务已按序提交
**验证:** 三条命令输出干净;`git log --oneline` 含各任务提交

## 执行顺序

```
B1(并行): T1  T3  T4  T5
B2(并行): T2(T1)          T10(T4)
B3:       T6(T1,T2,T4)
B4:       T7(T6)
B5(并行): T8(T7)   T9(T7)
B6:       T11(T8,T9,T10) → T12
```

并行冲突协调:T1/T3/T4/T5 分属四个互不相交的包,零文件交叉可全程并行;T10 与 T2 不同包可并行;T6/T7 同包串行(hooks.go 与 run/service/protocol 分文件,但 T7 编译依赖 T6 类型);T8(cmd+runtime)与 T9(tui)无共享文件可并行;T6 是 B2–B5 的汇合点,sessionlog(T4)与 hooks 引擎(T1/T2)零交叉。
