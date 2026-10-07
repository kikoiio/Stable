# M10-A 本地 Print 与 Provider 选择 Plan

> 依据已批准的 [spec.md](spec.md)，包含只读 print 修订。

## 架构概览

```text
stable --print / stable provider ...
          │
          ├── Print CLI：读取输入、准备 runtime、创建 ephemeral session
          │       └── run_start → 输出回答 → session_discard
          ├── Provider CLI：查询来源；runtime 关闭时原子更新配置
          ▼
conversation service
  ├── sessionlog 标记 ephemeral；列表不显示临时项
  ├── 从 session 标记生成可信的只读 run authority
  └── 复用现有 runner 和工具执行路径
          ▼
execution
  ├── 仅允许 OpRead 工具
  └── host 分派之前拒绝其他操作类型
```

临时标记由 service 从 session log 读取，不能由 print 客户端自行声明 run 权限。`BuildAuthority` 将该标记转为可信的只读 authority；tool executor 在任何 host 工具或普通工具执行前应用限制。非只读调用以明确执行错误终止 run，不创建 candidate。session 销毁限定在 ephemeral session，清理日志、临时权限记录和 service 运行态。

`cmd/stable` 增加两个入口。`internal/appconfig` 提供 provider 来源读取和私有配置更新。`internal/conversation`、`internal/sessionlog`、`internal/store` 管理临时会话生命周期。`internal/permission` 与 `internal/execution` 从 service 生成的 authority 执行只读限制。`internal/agent`、`internal/llm` 和 runtime 启动机制沿用现有实现。

## 核心数据结构与接口

### Session 元信息与协议

```go
type SessionInfo struct {
    ID        string
    Title     string
    CreatedAt time.Time
    UpdatedAt time.Time
    Ephemeral bool
}
```

`conversation.ClientMsg` 增加 `Ephemeral bool`，用于 `session_create`；新增 `session_discard` 操作，接收 `ProjectRoot` 与 `SessionID`。discard 只允许销毁带 ephemeral 标记且没有活动 run 的 session。

`sessionlog.Create` 保持普通 session 语义，新增 `CreateEphemeral`。`sessionlog.List` 不返回 ephemeral session。新增 `sessionlog.DeleteEphemeral(root, id)`：读取并验证 session 元信息标记后，在 session 文件锁下删除其 JSONL 文件。

### 可信只读 authority

```go
type Authority struct {
    // 现有 run、session、路径、模式及能力字段
    ReadOnly bool
}
```

`conversation.BuildAuthority` 从受信 session log 读取 `Ephemeral` 并设置 `ReadOnly`。run 请求不接受客户端提供的 `ReadOnly`。`permission.Policy.Decide` 在精确规则匹配前拒绝 `ReadOnly` authority 上除 `OpRead` 外的操作。只读标记不进入 scope digest，使既有只读规则继续匹配；它只收紧权限，不放宽任何操作。

`execution.toolRunExecutor.Execute` 在 host 工具分派前检查只读工具白名单。白名单对应现有 executor 分类为 `OpRead` 的工具（`read_file`、`glob`、`grep`）；其他 host 工具、MCP 和普通工具均拒绝。该检查防止不经过 `PermissionGate` 的 host 分支绕过只读约束。

### Provider 配置来源

```go
type ConfigSource string // file | env | default

type ModelSelection struct {
    Provider       string
    Model          string
    BaseURL        string
    ProviderSource ConfigSource
    ModelSource    ConfigSource
    BaseURLSource  ConfigSource
}
```

`appconfig.LoadWithModelSources() (AppConfig, ModelSelection, error)` 返回有效配置及 provider/model/base URL 字段来源，不返回凭据值。`appconfig.UpdateModelSelection(provider, model, baseURL string) error` 仅更新模型选择字段；不接受密钥参数。

### CLI 入口

```go
func runPrint(args []string, stdout, stderr io.Writer) error
func runProvider(args []string, stdout, stderr io.Writer) error
```

`runPrint` 负责输入、runtime、临时 session、run stream、输出、取消和销毁。`runProvider` 分派 `list`、`show`、`use`，在切换前检查 runtime 状态。

## 模块设计

| 模块 | 职责 | 主要改动 |
|---|---|---|
| `cmd/stable` CLI | print/provider 命令编排；runtime 检查与启动；回答输出 | `main.go` 分派；新增 print 客户端和 provider 子命令 |
| `internal/appconfig` | 读取有效 provider 与字段来源；持久修改 provider/model/base URL | 新增 provider selection API；保留配置其他字段并原子私有写回 |
| `internal/conversation` | 临时 session 生命周期；run authority 构造 | `session_create` 支持 ephemeral；新增受限 `session_discard`；`BuildAuthority` 从 session 元信息生成可信只读 authority |
| `internal/sessionlog` | 临时标记、过滤及销毁日志 | `SessionInfo.Ephemeral`；列表过滤；仅允许删除带标记的临时 session |
| `internal/permission` | 保持权限策略对临时 run 的只读硬边界 | `Authority.ReadOnly` 下除 `OpRead` 外全部拒绝，优先于精确 allow 规则 |
| `internal/execution` | 阻止 host 工具绕过只读边界 | tool executor 在 host 工具分派前拒绝所有非只读工具；普通写/命令/MCP/network 等也由 authority 策略拒绝 |
| `internal/store` | 清除临时 session 的审批和权限决定 | 新增按 session ID 清除审批/决定的事务；保留持久精确规则 |
| `README.md` | 展示 print/provider 用法和能力边界 | 新增命令示例、Gemini 限制与只读说明 |

依赖方向保持单向：CLI → appconfig/runtime/conversation client；conversation → sessionlog/store/permission；execution → permission。`agent` 与 `llm` 不增加第二种 runner 或 provider。

## 模块交互

### Print

1. CLI 读取参数或 stdin，加载有效配置并确认 streaming provider 可用；解析当前工作目录和 state 路径。
2. runtime 未运行时执行配置校验并调用现有 `runtime.Up`。
3. CLI 通过 conversation socket 请求创建 ephemeral session；service 将标记写入 session log，列表查询过滤该 session。
4. CLI 构造 `WorkSession` 请求并通过现有 `run_start` 启动 agent run。service 重读 session 元信息，由 `BuildAuthority` 生成可信 `ReadOnly` authority。
5. tool executor 允许 `OpRead` 工具；在 host 工具分派及权限门前阻止其他工具。print 客户端收到非只读工具执行事件后发送 `run_cancel`。
6. 文本增量写入 stdout；非文本事件不进入回答。provider 错误、交互请求、非只读工具调用和运行失败写入 stderr，并返回非零状态。
7. 收到 run 终态后，CLI 请求 `session_discard`。service 验证 session 为 ephemeral、run 已终止，清除临时审批/决定记录、session log 和 session 运行态。

### Provider

1. `list` 显示四类 provider 与能力；`show` 显示 provider/model/base URL 的有效值及来源，不显示凭据。
2. `use` 解析 provider、model 与可选 base URL；runtime 运行时拒绝更新。
3. `appconfig` 读取原始 JSON，仅更新模型选择字段；在同目录私有临时文件中写入，再原子替换原配置。
4. 成功后说明保存值及仍生效的环境变量覆盖；配置失败时报告错误，保留原配置。

## 文件组织

```text
cmd/stable/
├── main.go                     — CLI 分派、usage
├── print.go                    — print 输入、runtime、session/run stream、输出与清理
├── print_test.go               — 参数/stdin、输出/失败、取消与清理
├── provider.go                 — provider list/show/use
└── provider_test.go            — provider 查询、切换和 runtime 限制

internal/appconfig/
├── provider.go                 — 有效值/来源读取；原子私有配置更新
└── provider_test.go            — 来源、字段保留、权限与失败完整性

internal/conversation/
├── protocol.go                 — ephemeral 创建字段与 session_discard 协议
├── session.go                  — 临时 session 创建与列表过滤接线
├── service.go                  — session_discard 分派
├── authority.go                — 从 session 元信息派生 ReadOnly authority
├── ephemeral.go                — 活动 run 检查、临时状态与存储清理
└── ephemeral_test.go           — 生命周期、活动 run 和普通 session 拒绝

internal/sessionlog/
├── events.go                   — SessionInfo 增加 Ephemeral
├── sessions.go                 — 创建临时 session、列表过滤
├── ephemeral.go                — 校验标记后删除临时日志
└── ephemeral_test.go           — 过滤、删除校验、普通 session 保留

internal/permission/
├── model.go                    — Authority 增加服务端 ReadOnly 字段
└── policy.go                   — ReadOnly 下拒绝非 OpRead 操作

internal/execution/
├── tool_executor.go            — host 分派前实施只读工具限制
└── tool_executor_m10_test.go   — host/普通工具非只读拒绝回归

internal/store/
├── ephemeral.go                — 按 session 清理临时审批和权限决定
└── ephemeral_test.go           — 清理范围、持久规则保留

README.md                       — print/provider 命令和只读边界
```

测试使用 fake provider、临时配置和隔离的 session/state 路径；不访问真实密钥或外网。

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| Print 执行路径 | 复用现有 session service、`run_start`、agent runner 和执行器 | 保持 provider、事件及工具调用路径一致 |
| 临时 session 标记 | `SessionInfo.Ephemeral` 持久保存；列表过滤；销毁只接受带标记且无活动 run 的 session | 防止客户端删除普通 session，也避免临时 session 进入历史列表 |
| 只读 authority 来源 | `BuildAuthority` 从 session log 派生 `Authority.ReadOnly`，客户端不能设置 | 确保只读约束由 service 控制 |
| 只读工具限制 | permission policy 对 `ReadOnly` authority 拒绝非 `OpRead`；executor 在 host 分派前再拒绝非只读工具 | 覆盖权限门和绕过权限门的 host 工具路径 |
| 非只读工具请求 | executor 在任何副作用前拒绝；CLI 收到工具执行事件后发送 `run_cancel` 并返回非零状态 | 避免 candidate 改动，同时使 print 在非只读请求时终止 |
| 临时数据清理 | 等待 run 终态；事务删除 session 的审批/决定记录，再校验并删除 session log，最后清除 service 内存状态 | 不触碰全局精确规则；部分失败会返回错误且不会报告成功 |
| Provider 来源 | provider/model/base URL 分别记录 file/env/default 来源 | `show` 可说明有效值来源而不披露密钥 |
| Provider 配置更新 | 校验字段；保留 JSON 的其他键；同目录私有临时文件写入并原子替换 | 避免丢失 MCP 或未来配置字段，也避免半写配置 |
| Runtime 切换边界 | 运行中拒绝 `provider use`；不自动重启 | 避免中断活动目标或会话 |
| Gemini 处理 | `list/show` 保留 Gemini 目标决策能力说明；print 启动 run 前拒绝 Gemini | 与当前 streaming provider 能力一致 |

## Spec 覆盖自检

| Spec 条目 | Plan 归属 |
|---|---|
| F1 输入与 runtime | CLI 模块、Print 交互 1–2 |
| F2 临时 session 与 read-only run | conversation/sessionlog/permission/execution；Print 交互 3–7 |
| F3 输出、失败和 Gemini 限制 | CLI Print 输出适配、Provider 校验 |
| F4 非交互边界 | permission policy、tool executor、CLI run 取消 |
| F5 provider list/show | `cmd/stable/provider.go`、appconfig 来源读取 |
| F6 provider use 与配置保留 | appconfig 原子更新、Provider 交互 |
| F7 runtime 与环境覆盖 | Provider runtime 检查、appconfig 来源解析 |
| N1–N4 | 凭据不输出、原子配置写入、临时数据清理、fake provider/临时配置验证 |

## 设计自检

- Spec 的 F1–F7 均在模块表和交互时序中有归属。
- 权限策略与 executor 双重落实 ReadOnly；客户端不能扩大 authority。
- 依赖方向无环，session discard 在活动 run 检查之后执行。
- Provider 更新只修改模型选择字段，并保留其他 JSON 字段与私有权限。
- Gemini 流式不支持与 remote 未纳入范围均与 spec 一致。
