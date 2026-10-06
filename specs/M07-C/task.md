# M07-C MCP Tasks

> 状态:待批准。依据已批准的 spec.md 与 plan.md(specs/M07-C/)。
> 执行约定:并行子代理须遵守文件所有权边界(不同任务不同文件);启动批量编译/全量测试等重型操作前检查内存(free -h、/proc/pressure/memory)并与主代理协调;每个任务完成即运行其验证,先有证据再标记完成;每组逻辑相关任务完成后提交一次。执行环境:Ubuntu linux/amd64,go1.26.0;开发在 /home/neo/Projects/stable-m07c worktree(m07c 分支)。
> 与 plan 的偏差记录:MCPCaller 接口增加 ResolveTarget 方法(目标解析与「未知工具」错误收敛到 Manager),使执行包无需引入 mcp 包(从而不引入 SDK),与 plan「执行包不见 SDK」决策一致。

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 修改 | `go.mod`、`go.sum` | go-sdk 依赖 |
| 修改 | `internal/appconfig/config.go` | MCPServerConfig 类型 + mcp_servers 解析 |
| 新建 | `internal/mcp/sanitize.go` | 命名归一化与前缀构造 |
| 新建 | `internal/mcp/coerce.go` | 参数按 schema 强转 |
| 新建 | `internal/mcp/config.go` | 两级加载合并与校验 |
| 新建 | `internal/mcp/discover_timeout.go` | stdio 探测超时包装 |
| 新建 | `internal/mcp/strategy.go` | eager/dispatch 分档 |
| 新建 | `internal/mcp/mcp.go` | Manager 主体 |
| 新建 | `internal/mcp/mcptest/` | 假 stdio 服务器 fixture(正常/探测迟钝/秒退) |
| 新建 | `internal/execution/mcp.go` | MCPCaller 接口 + MCPToolSchema 类型 |
| 修改 | `internal/execution/executor_factory.go`、`tool_executor.go` | WithMCPCaller + host 分支三入口 |
| 修改 | `internal/execution/tools_schema.go` | mcp_call/tool_search schema 常量 |
| 修改 | `internal/permission/model.go`、`policy.go` | OpMCPTool Kind 与判定 |
| 修改 | `internal/sessionlog/events.go` | mcp_reload/mcp_server 事件族 |
| 新建 | `internal/hooks/http_action.go`;修改 `internal/hooks/hooks.go` | http 动作 |
| 修改 | `internal/conversation/protocol.go`、`run.go`、deps 接线处;新建 `internal/conversation/mcp.go` | 服务端接线 |
| 修改 | `cmd/stable/chatserve.go`、`internal/runtime/supervisor.go` | Manager 构造 + schema 组装 |
| 修改 | `internal/tui/model.go` | /mcp 命令与渲染 |
| 新建 | `tests/e2e/m07c_mcp_test.go`、`m07c_mcp.sh` | e2e 场景组 |
| 新建 | `specs/M07-C/checklist.md` | 验收清单(阶段四) |

## T1: 引入 go-sdk 依赖

**文件:** `go.mod`、`go.sum`
**依赖:** 无
**步骤:** `go get github.com/modelcontextprotocol/go-sdk`(取不低于 v1.8.0 的最新兼容版)后 `go mod tidy`;确认不引入与现有依赖的版本冲突。
**验证:** `go build ./...` 退出码 0;`go mod tidy` 后 `git diff --stat go.mod go.sum` 无二次变化。

## T2: appconfig 类型与主配置解析

**文件:** `internal/appconfig/config.go`
**依赖:** 无
**步骤:** 1) 定义 `MCPServerConfig{Name, Command string; Args []string; URL, Transport string; Headers, Env map[string]string}`(JSON tag: name/command/args/url/transport/headers/env)。2) `AppConfig` 增 `MCPServers []MCPServerConfig \`json:"mcp_servers"\``。3) Load 正常解析;此层不做语义校验(校验在 mcp 包)。
**验证:** `go test ./internal/appconfig/ -count=1` 退出码 0;新增断言:含 mcp_servers 的 JSON 正确解析、缺键得 nil、类型错不 panic;原有断言未修改全过。

## T3: mcp 包骨架与命名归一化

**文件:** 新建 `internal/mcp/sanitize.go`、`sanitize_test.go`
**依赖:** 无
**步骤:** 1) `SanitizeName(s string) string`:非 `[A-Za-z0-9]` 字符归一为 `_`。2) `MCPToolNamePrefix(server) = "mcp__" + SanitizeName(server) + "__"`。3) `ToolName(server, tool) = prefix + SanitizeName(tool)`。
**验证:** `go test ./internal/mcp/ -count=1`;归一化矩阵含 `chrome-devtools → chrome_devtools`、空串、全特殊字符(用例对齐源端 strategy_test)。

## T4: 参数强转 CoerceBySchema

**文件:** 新建 `internal/mcp/coerce.go`、`coerce_test.go`
**依赖:** 无
**步骤:** 自源端 `mcp_call.go` 移植:string←数字、integer/number←数字形字符串、boolean←"true"/"false"、array←单键对象/逗号串、object 递归 items;转不动原样保留。
**验证:** `go test ./internal/mcp/ -run Coerce -count=1`;契约矩阵用例自源端 mcp_call_test.go 移植。

## T5: 两级配置合并与校验

**文件:** 新建 `internal/mcp/config.go`、`config_test.go`
**依赖:** T2
**步骤:** 1) `LoadMerge(user []appconfig.MCPServerConfig, projectPath string) ([]appconfig.MCPServerConfig, Rejections)`。2) 项目 `mcp.yaml` 用 yaml.v3 解析为同型数组;`Lstat` 拒符号链接;文件大小上限 256KiB、条目上限 100(对齐 M06/M07 惯例);超限文件/条目跳过进 rejections。3) 逐条 `Validate`:name 非空、command 与 url 恰有其一、transport ∈ {sse, http, streamable, 空}(空默认 streamable);非法整条跳过。4) 合并:用户条目在前项目在后,同名时项目条目覆盖(用户位置移除、项目位置保留)。
**验证:** `go test ./internal/mcp/ -run LoadMerge -count=1`;矩阵:同名覆盖+位置保留、非法跳过含原因、符号链接拒绝、大小/条目超限、空配置、两级并存。

## T6: stdio 探测超时包装

**文件:** 新建 `internal/mcp/discover_timeout.go`、`discover_timeout_test.go`
**依赖:** T1
**步骤:** 自源端 `discover_timeout.go` 移植:`discoverTimeout = 10s`;Connection 包装在 Write 时识别探测请求并起 `time.AfterFunc`;超时伪造 JSON-RPC InternalError 响应触发 SDK 回退;迟到的真实探测响应丢弃。
**验证:** `go test ./internal/mcp/ -run DiscoverTimeout -count=1`;用 net.Pipe 模拟:及时响应直通、超时收到伪造错误、迟到响应不产生第二次输出。

## T7: execution 侧 MCPCaller 接口

**文件:** 新建 `internal/execution/mcp.go`
**依赖:** 无
**步骤:** 定义 `MCPToolSchema{Name, Description string; InputSchema map[string]any}` 与 `MCPCaller` 接口:`CallTool(ctx, server, tool string, args map[string]any) (output string, isError bool, err error)`、`EagerSchemas() []MCPToolSchema`、`DispatchTools() []MCPToolSchema`、`InputSchema(server, tool string) (map[string]any, bool)`、`ResolveTarget(query string) (server, tool string, err error)`(全名→归一前缀唯一匹配→歧义/未知报含可用清单的错误)、`Instructions() string`。纯类型与接口,无行为。
**验证:** `go build ./internal/execution/` 退出码 0。

## T8: 加载策略

**文件:** 新建 `internal/mcp/strategy.go`、`strategy_test.go`
**依赖:** T3、T7
**步骤:** 1) `MeasureSchemaChars(tools []execution.MCPToolSchema) int`(只统计 `mcp__` 前缀)。2) `DecideMode`:估算 token = 字符/2.5,eager ⇔ 估token < 上下文窗口×10%(窗口取模型元数据,缺省 200k;实现时确认元数据来源,不可得用常量)。3) `STABLE_MCP_LOADING` ∈ {eager, dispatch} 强制覆盖全部工具档位。4) `Apply`:产出 eager/dispatch 两个清单,清单按名排序。
**验证:** `go test ./internal/mcp/ -run Strategy -count=1`;矩阵:小 schema→eager、大→dispatch、env 覆盖双向、只统计 mcp__ 工具、排序稳定。

## T9: sessionlog 事件族

**文件:** `internal/sessionlog/events.go`
**依赖:** 无
**步骤:** 照 HookFired 模式新增:`EventMCPReload{Before, After int; Rejections []string; Trigger string}`、`EventMCPServer{Name, Source, State string; Error string; ToolCount int}`(State ∈ connected/disconnected/reload-failed);输出/错误截断上限常量(对齐 MaxHookOutput 量级);append 路径复用既有 journal 写入。
**验证:** `go test ./internal/sessionlog/ -count=1`;新增断言:两类事件落盘、读取往返、截断生效;既有事件校验全过。

## T10: 权限 OpMCPTool

**文件:** `internal/permission/model.go`、`policy.go`
**依赖:** 无
**步骤:** 1) `OperationKind` 增 `OpMCPTool`。2) `Policy.Decide` 新增 case:无匹配规则时 ask;ExactRule 按 `Operation.Target`(归一化 `server__tool`)走既有匹配,deny > ask > allow 顺序不变;其他 Kind 行为零变化。
**验证:** `go test ./internal/permission/ -count=1`;新增断言:MCP 默认 ask、allow/deny/ask 规则命中与优先级、非 MCP Kind 判定结果与迁移前一致(存量断言不改)。

## T11: hooks http 动作

**文件:** 新建 `internal/hooks/http_action.go`;修改 `internal/hooks/hooks.go`
**依赖:** 无
**步骤:** 1) 新建共享 HTTP 客户端(合理超时上限)。2) `runHTTPAction(a Action, timeout)`:URL 与 headers 值 `os.ExpandEnv`;构造请求(Method 默认 GET,Body 原样);响应 2xx:读体截断至输出上限作为输出;非 2xx:错误含状态码与截断体;超时错误文案与一般失败可区分。3) `hooks.go` FireOne 的 `case "http"` 桩替换为调用;`on_error` 语义由既有 FireOne 流程自然适用。
**验证:** `go test ./internal/hooks/ -count=1`;httptest 断言:方法/头(含 env 展开)/体到达、2xx 回流、非 2xx 失败、超时可区分、输出截断;既有 hook 测试全过。

## T12: Manager 主体与测试 fixture

**文件:** 新建 `internal/mcp/mcp.go`、`mcp_test.go`、`internal/mcp/mcptest/`(子包)
**依赖:** T1、T5、T6、T8
**步骤:** 1) `mcptest` 子包:用 SDK server 侧实现假 stdio MCP 服务器(binary 经 `go build` 临时产出或 go test TestMain 构建到临时目录),三种模式:正常回显工具 echo、探测迟钝(>10s 应答)、启动即退出;`Instructions` 字段可配。2) `Manager`:持有配置与连接;`ConnectAll`(逐台连接+ListTools+包装注册,失败记 `MCP server '<名>': <原因>` 继续);实现 `MCPCaller` 全部方法(含 `ResolveTarget`);`Reload()` 差分(名字+配置等价):删除/变更断开+杀子进程、新增/变更重连、重新分档;`EnsureFresh()` 比对用户主配置与项目 mcp.yaml 的 mtime;`Shutdown()` 有序断开;`Status()` 逐台状态。3) 事件写入经回调注入(避免 mcp 包依赖 sessionlog,由 conversation 接线时注入写函数)。
**验证:** `go test ./internal/mcp/ -count=1`;用 fixture 断言:连接成功与失败跳过、CallTool 文本 join/空占位/IsError 透传、ResolveTarget 全名/唯一后缀/歧义、Reload 增删改差分、EnsureFresh 的 mtime 触发、Shutdown 后子进程退出;双 GOOS(windows/darwin)交叉编译 `go build ./internal/mcp/` 退出码 0。

## T13: 执行 host 分支三入口

**文件:** 修改 `internal/execution/executor_factory.go`、`tool_executor.go`、`tools_schema.go`
**依赖:** T7、T10
**步骤:** 1) `tools_schema.go` 增 `mcp_call`/`tool_search` schema 常量。2) `ToolExecutorDeps` 增 `MCPCaller MCPCaller`;`WithMCPCaller` Option。3) `toolRunExecutor.Execute` host 分支新增三入口(均在 pre_tool_use hook 之后、与既有 host 工具同级):a) `mcp__` 前缀直调:构造 `Operation{Kind: OpMCPTool, Name: server, Target: server__tool}` → `Gate.Authorize` → `CallTool`;b) `mcp_call`:参数 {server, tool, arguments} → `ResolveTarget` → 按目标 `InputSchema` 强转 → 以目标构造 Operation 过门 → `CallTool`;c) `tool_search`:按名/模式过滤 `DispatchTools()`,返回名+描述+截断 schema,条数上限 20,无权限操作。4) `MCPCaller == nil` 时三入口按未知工具处理(向后兼容未接线的测试)。
**验证:** `go test ./internal/execution/ -count=1`;用桩 caller 断言:hook 先于门、门先于调用、ask 拒绝路径、强转后调用参数、未知工具指引错误、tool_search 上限;既有 host 工具测试全过。

## T14: conversation 服务端接线

**文件:** 修改 `internal/conversation/protocol.go`、`run.go`、deps 接线处;新建 `internal/conversation/mcp.go`
**依赖:** T9、T12
**步骤:** 1) `Deps` 增 `MCP mcp.ManagerView`(或直接 `*mcp.Manager`,含 EnsureFresh/Status/Reload/Instructions/事件写回调注入点)。2) `protocol.go` op 白名单加 `mcp_list`、`mcp_reload`。3) 新建 `mcp.go`:`listMCP`(EnsureFresh → Status + rejections 渲染文本)、`reloadMCP`(Reload → 落 EventMCPReload 与逐台 EventMCPServer → 返回 before/after 报告),照 hooks.go 的 handlers 模式。4) `run.go` 启动序列:前缀组装前调 `EnsureFresh`;instructions 非空且本会话未注入时,以系统提醒拼进首轮前缀(与技能 delta 同位置),会话级 once 标记。
**验证:** `go test ./internal/conversation/ -count=1`;单测:mcp_list/mcp_reload op 走通、instructions 只注入一次、EnsureFresh 在 run 前触发;既有 hooks/skills 测试全过。

## T15: 装配与 schema 组装接线

**文件:** 修改 `cmd/stable/chatserve.go`、`internal/runtime/supervisor.go`
**依赖:** T12、T13、T14
**步骤:** 1) `chatserve.go`:构造 Manager(用户配置取 `appconfig.Load().MCPServers`,项目路径取会话工作目录)→ 注入 `conversation.Deps` 与 `WithMCPCaller`;后台启动 ConnectAll;服务退出时 Shutdown。2) `chatserveToolSchemas` 与 `runtimeToolSchemas`:配置了 ≥1 台服务器时追加 `mcp_call`+`tool_search` 常量 schema 与 `caller.EagerSchemas()`(经注入的 caller 接口,不直接 import mcp 包于 runtime);未配置时保持原清单。3) supervisor 侧 caller 经装配传入(实现时确认注入路径,不新增 runtime→mcp import)。
**验证:** `go build ./...` 退出码 0;单测驱动两个组装函数:无配置清单不变(存量断言不改)、有配置含两常量与 eager 工具;不运行 stable CLI(遵守配置文件禁令)。

## T16: TUI /mcp 命令

**文件:** `internal/tui/model.go`
**依赖:** T14
**步骤:** 照 /hooks 模式:内置命令 `/mcp` 无参发 op `mcp_list`、`/mcp reload` 发 `mcp_reload`;结果处理:列表渲染 transcript 系统消息,报告(重载前后/连接错误)进状态栏;op 回复走 protocol 既有分发。
**验证:** `go build ./...` 退出码 0;`go test ./internal/tui/... -count=1`(如可测则 Bubble Tea 模型测试照 /hooks;不可测则 build+代码审阅如实记录)。

## T17: e2e 场景组

**文件:** 新建 `tests/e2e/m07c_mcp_test.go`、`m07c_mcp.sh`
**依赖:** T12、T13、T14
**步骤:** 照 m07b_hooks_test.go 模式:`m07cNewService` 注入 WithMCPCaller(真 Manager + mcptest fixture 服务器)与 Deps;场景:① 两级配置合并与同名覆盖(AC1);② fixture 正常/秒退并存,失败跳过报告(AC2/AC5);③ eager 直调经 hook reject 与权限 ask/allow(AC4);④ dispatch:大 schema 工具不在清单、tool_search 查到、mcp_call 调用成功、未知工具指引(AC3/AC4/AC5);⑤ instructions 首轮注入不重复(AC6);⑥ 修改 mcp.yaml 后 run 前自动重载 + mcp_reload 事件投影(AC7);⑦ httptest hook http 动作(AC8);⑧ 重启后事件投影一致(AC9)。`m07c_mcp.sh` 为薄包装命令。
**验证:** `bash tests/e2e/m07c_mcp.sh` 退出码 0,场景断言全部通过;不触真实用户配置(临时 HOME/临时项目目录)。

## T18: 汇合全量验证

**文件:** 无(验证任务)
**依赖:** T15、T16、T17
**步骤:** 运行 `make test` 与 `bash tests/e2e/m07c_mcp.sh`;`go mod tidy` 复验;资源紧张时先查 free -h 与 memory PSI,失败定位修复后重跑。
**验证:** 两命令退出码 0,输出留存 `/tmp/m07c-final-*.log`。

## 执行顺序

```
T1 ──→ T6 ──────────────┐
T2 ──→ T5 ──────────────┤
T3 ──┬→ T8 ─────────────┼──→ T12 ──→ T14 ──┬→ T15 ─┐
T7 ──┴→ T8              │                   ├→ T16 ─┤
T10 ──→ T13(依赖 T7,T10)─┘                   └→ T17 ─┼→ T18
T9 ────────────────────────→ T14             T12,T13 ┘
T11(独立)
```

**可执行的最大并行批次**(批次内任务互不改同一文件):

1. 批次 1:T1、T2、T3、T4、T7、T9、T10、T11(八条独立线)
2. 批次 2:T5(依赖 T2)、T6(依赖 T1)、T8(依赖 T3、T7)、T13(依赖 T7、T10)
3. 批次 3:T12(依赖 T1、T5、T6、T8)
4. 批次 4:T14(依赖 T9、T12)
5. 批次 5:T15(依赖 T12、T13、T14)、T16(依赖 T14)、T17(依赖 T12、T13、T14)
6. 批次 6:T18(汇合门禁)
