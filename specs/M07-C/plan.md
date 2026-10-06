# M07-C MCP Plan

> 状态:已批准(2026-10-06)。依据已批准的 spec.md(specs/M07-C/spec.md)。

## 架构概览

MCP Manager 由 conversation 服务单一持有,作为 M04 工具体系的一个「工具来源」接入;模型侧看到的是普通工具(eager 直调或经 `mcp_call` 分发),执行侧统一走 host 分支 + M03 权限门;TUI 只经协议消息查看状态。

```
appconfig(主配置 mcp_servers 键) ──┐
.stable/mcp.yaml(项目级) ─────────┤
                                  ▼
                    internal/mcp.Manager（服务进程内）
                    ├── ConnectAll / Reload / Shutdown
                    ├── 策略: eager schema 直进清单 | dispatch 经 tool_search+mcp_call
                    └── CallTool（SDK 客户端：stdio / streamable-http / sse）
                                  │
        ┌─────────────────────────┼──────────────────────────┐
        ▼                         ▼                          ▼
schema 组装点                执行 host 分支                  审计/状态
chatserve.go +           tool_executor.go：            sessionlog 新事件族
supervisor.go            mcp__* → 门 → CallTool        conversation op:
（拼 provider tools[]）                                 mcp_list / mcp_reload → TUI /mcp
```

## 核心数据结构与接口

**MCPCaller(消费侧接口,定义在 internal/execution,避免执行包依赖 SDK):**

```go
// MCPCaller 由 conversation 服务持有的 Manager 实现,经 Option 注入执行器
type MCPCaller interface {
    // 直调/分发共用:server/tool 为归一化后的名字,args 已按 schema 强转
    CallTool(ctx context.Context, server, tool string, args map[string]any) (output string, isError bool, err error)
    // eager 档工具的 provider schema(连接后固定,运行期不变)
    EagerSchemas() []MCPToolSchema   // Name/Description/InputSchema
    // dispatch 档工具清单(供 tool_search 检索)
    DispatchTools() []MCPToolSchema
    // 服务器原始 schema(强转依据),按归一化名查
    InputSchema(server, tool string) (map[string]any, bool)
    // 服务器握手 instructions(按 server 去重后汇总)
    Instructions() string
}
```

`internal/mcp.Manager` 实现该接口,另含服务侧生命周期方法:`ConnectAll(ctx)`、`Reload() (before, after int, rejections []string, err error)`、`Status() []ServerStatus`、`Shutdown()`、`EnsureFresh()`(mtime 检查,run 启动与 /mcp 前调用)。

**MCPServerConfig(定义在 internal/appconfig):** `Name`(必填)、`Command`+`Args`、`URL`、`Transport`、`Headers`、`Env`(map);主配置 `mcp_servers` 键解析,项目级 `.stable/mcp.yaml` 由 mcp 包解析为同型。

**权限扩展:** `permission.OperationKind` 新增 `OpMCPTool`(`Name=server`、`Target="server__tool"` 归一化匹配串),`Policy.Decide` 新增对应 case:默认 ask,ExactRule 沿用既有 deny > ask > allow 顺序;不改其他 Kind 的任何判定。

## 模块设计

| 模块 | 职责 | 关键内容 |
|------|------|----------|
| `internal/mcp`(新包) | Manager + 配置 + 策略 + 协议适配 | 唯一引入官方 SDK 的包;自源端移植 discover 超时包装、schema 强转、命名归一化三个纯函数件 |
| `internal/appconfig` | `MCPServerConfig` 类型 + 主配置 `mcp_servers` 键解析 | 轻量,不知 SDK |
| `internal/execution` | host 分支新增 `mcp__*` 直调、`mcp_call`、`tool_search` 三个入口 | `ToolExecutorDeps` 增 `WithMCPCaller` Option;分支内构造 `OpMCPTool` 过 Gate;`mcp_call` 解析目标后按目标工具过门 |
| `internal/permission` | `OpMCPTool` Kind + 判定 case | 默认 ask;ExactRule 按 `Target`(归一化 `server__tool`)匹配,deny > ask > allow 顺序不变 |
| `internal/sessionlog` | 事件族 `mcp_reload` / `mcp_server` | 照 HookFired 模式:常量 + payload 结构体 + 输出截断上限 |
| `internal/conversation` | 服务端接线与状态 | op 白名单加 `mcp_list`/`mcp_reload`;handlers 照 hooks.go 模式;run.go 启动序列加 `EnsureFresh()` 与 instructions 首轮注入(会话级 once 标记,与技能 delta 同位置) |
| `internal/hooks` | http 动作实现 | 新文件 `http_action.go`;`hooks.go` 的「未启用」桩替换;headers/URL 值 `os.ExpandEnv`;响应体截断进 hook 输出回流 |
| `cmd/stable/chatserve.go` + `internal/runtime/supervisor.go` | schema 组装接线 | 两个组装点在白名单后追加:`mcp_call` + `tool_search` 常量 schema + `caller.EagerSchemas()`;配置了服务器才追加(首轮热更新后下一 run 生效,文档注明缓存预热代价) |
| `internal/tui` | `/mcp` 内置命令 | 照 `/hooks` 模式:无参发 `mcp_list`、`reload` 发 `mcp_reload`;结果渲染 transcript 系统消息 + 状态栏报告 |

## 模块交互(关键时序)

1. **启动**:服务装配 → `appconfig.Load`(含 mcp_servers)→ Manager 构造(合并项目 `mcp.yaml`,校验,rejections 记录)→ 后台 goroutine `ConnectAll`(逐台连接 + ListTools + 注册包装,失败记错误继续)→ 完成后 `DecideMode` 一次固定 eager/dispatch 分档 → TUI 经状态消息看到「Connected to N server(s), M tools」。
2. **eager 直调**:模型发起 `mcp__srv__tool` → toolRunExecutor:落 `EventToolCall` → pre_tool_use hook → host 分支命中 `mcp__` 前缀 → 构造 `OpMCPTool` → `Gate.Authorize`(ask 时走既有审批 UI)→ `MCPCaller.CallTool` → 文本结果/错误标记 → `EventToolResult`。
3. **dispatch**:模型调 `tool_search`(host 只读分支,无权限操作,按名/模式返回 dispatch 档工具的 schema,条数有上限)→ 模型调 `mcp_call{server, tool, arguments}` → host 分支解析目标(全名 → 前缀唯一匹配,歧义报错)→ 按目标 `InputSchema` 强转参数 → 以目标构造 `OpMCPTool` 过门 → `CallTool`。
4. **热更新**:run 启动(前缀组装前)或 `/mcp*` 命令 → `EnsureFresh()` 比对两级配置 mtime → 变更则 `Reload()`:重新读取仅 `mcp_servers`(主配置整文件解析后取键,其余键丢弃不应用)→ 按名字+配置等价性差分:删除/变更的断开并杀子进程,新增/变更的重连 → `DecideMode` 重新分档 → 落 `mcp_reload`(before/after/rejections)与逐台 `mcp_server` 事件。
5. **http hook**:`FireOne` http case → 展开头/URL 环境变量 → 请求(可配超时,默认对齐 command 量级)→ 2xx:响应体截断为输出回流;非 2xx/超时/网络错误:失败信息(含状态码或原因)作为错误输出,`on_error` 语义适用。

**测试基线**:e2e 用 Go 写的假 stdio MCP 服务器 fixture(SDK server 侧实现 initialize/list/call;另带「探测不响应」变体验证 10 秒回退)+ 本地 httptest 服务器验 http hook;全部离线、临时目录,绝不触真实用户配置。

## 文件组织

```
internal/mcp/                    [新包]
├── mcp.go                       Manager：ConnectAll/CallTool/Reload/Status/Shutdown/EnsureFresh
├── config.go                    两级加载合并 LoadMerge(user, projectPath)、校验、rejections
├── strategy.go                  DecideMode/ApplyMode：eager/dispatch 分档、阈值、STABLE_MCP_LOADING 覆盖
├── discover_timeout.go          stdio 探测 10s 超时包装（自源端移植）
├── coerce.go                    CoerceBySchema 参数强转（自源端移植）
├── sanitize.go                  命名归一化 + mcp__ 前缀构造
├── mcptest/                     [新子包] 假 stdio MCP 服务器 fixture（正常/探测迟钝/秒退三种），供单测与 e2e 复用
└── *_test.go                    单测（合并矩阵/策略/强转/归一/超时回退/Manager 行为）

internal/appconfig/config.go     [修改] MCPServerConfig 类型 + AppConfig.MCPServers 解析
internal/execution/
├── executor_factory.go          [修改] ToolExecutorDeps.MCPCaller + WithMCPCaller Option
├── tool_executor.go             [修改] host 分支：mcp__* 直调、mcp_call、tool_search 三入口
└── tools_schema.go              [修改] mcp_call/tool_search schema 常量
internal/permission/
├── model.go                     [修改] OperationKind 增 OpMCPTool
└── policy.go                    [修改] 新 Kind 判定 case（默认 ask，Target 匹配）
internal/sessionlog/events.go    [修改] mcp_reload/mcp_server 事件常量 + payload + 截断上限
internal/conversation/
├── protocol.go                  [修改] op 白名单加 mcp_list/mcp_reload
├── mcp.go                       [新] 服务端 handlers + instructions 首轮注入状态
├── run.go                       [修改] 启动序列加 EnsureFresh + instructions 注入
└── deps.go 等接线处             [修改] Deps 增 MCP 字段
internal/hooks/
├── http_action.go               [新] http 动作实现
└── hooks.go                     [修改] 替换「未启用」桩
cmd/stable/chatserve.go          [修改] Manager 构造接线 + chatserveToolSchemas 追加
internal/runtime/supervisor.go   [修改] runtimeToolSchemas 追加（经理由依赖注入取得 caller）
internal/tui/model.go            [修改] /mcp 内置命令 + 结果渲染
go.mod / go.sum                  [修改] github.com/modelcontextprotocol/go-sdk
tests/e2e/
├── m07c_mcp_test.go             [新] 场景组（AC1–AC9 可测子集）
└── m07c_mcp.sh                  [新] 薄包装（照 m07b_hooks.sh）
specs/M07-C/                     spec.md 已落盘;plan.md、task.md、checklist.md 随阶段追加
```

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| SDK 及版本 | 官方 `github.com/modelcontextprotocol/go-sdk`,取不低于源端 v1.8.0 的最新兼容版 | 握手/协商/三传输现成;与源端行为对齐成本最低 |
| Manager 归属 | conversation 服务单点持有,执行包只见 `MCPCaller` 接口 | 客户端-服务端架构的唯一事实源;执行包不引入 SDK 依赖 |
| 配置类型位置 | `MCPServerConfig` 在 appconfig,合并/校验在 mcp 包 | 主配置解析方持有类型;appconfig 不被 SDK 污染 |
| 阈值估算 | 字符数/2.5 估 token,预算 = 上下文窗口 × 10%;窗口值取模型元数据,缺失时回退 200k 常量 | 与源端系数一致;实现时确认 Stable 模型元数据是否携带窗口字段 |
| dispatch 工具进清单的条件 | 配置了至少一台服务器时 `mcp_call`+`tool_search` 恒进清单;运行中首次配置则下一 run 生效 | 无服务器时不占上下文;代价是热更新后首 run 缓存重预热,可接受并文档化 |
| 热更新触发点 | run 启动前 + /mcp 命令前调 `EnsureFresh()`(mtime stat),不设后台 watcher | 满足 AC7「下一次触发即生效」;无常驻开销 |
| 主配置热读取边界 | 整文件解析后仅取 `mcp_servers` 键应用,其余键丢弃 | 服务进程本就读取主配置(受信控制面);agent/沙箱永不接触 |
| 权限匹配 | `OpMCPTool.Target = "server__tool"` 归一化串,沿用 ExactRule 既有匹配语法 | 规则可写 `mcp_call(linear__*)` 同款形态;M03 判定顺序不动 |
| http hook 信任模型 | 用户配置即受信,不经权限门,与 command 动作同级 | hooks 本就是用户显式自动化;机制对齐 M07-B spec 预留语义 |
| 重连策略 | 无自动重连/守护(对齐源端);失败本会话跳过 + 报告 | 缩小状态机;重连留给真实需要时另立小项 |
| 测试形态 | 假 stdio 服务器 fixture(SDK server 侧)+ httptest,全离线临时目录 | 遵守用户安全规则:绝不读真实用户配置;不依赖外网 |
