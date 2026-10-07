# M08 记忆与指令发现 Plan

> 状态：已批准（2026-10-07）。完整 plan、task 和 checklist 均已获用户批准；进入按 task 实施与 checklist 验收阶段。

## 架构概览

M08 以独立 `internal/memory` 包实现指令发现、记忆文件存储、相关记忆选择和受限后台提取/整理。`conversation.Service` 是统一接入点：普通会话和目标工作项都在 run 启动时通过同一 manager 获取上下文，在 run 完成后触发同一记忆生命周期。`execution` 通过专用主机工具访问 memory manager，记忆写入不经过候选文件 helper，也不改变普通工程写入的候选边界。

`cmd/stable/chatserve` 负责构造用户/项目目录、复用已配置的 `decision.ChatProvider` 并注入 conversation 与 execution。`sessionlog` 记录不含正文的管理及后台结果事件，TUI 负责 `/memory` 命令与报告呈现。后台 selector、extractor 和 consolidator 都是单次 JSON 模型调用；模型仅返回条目引用或受限变更集，Go 端校验后执行，不构造通用子 agent。

## 核心数据结构

### `MemoryScope` 与 `MemoryType`

```go
type MemoryScope string

const (
    ScopeUser    MemoryScope = "user"
    ScopeProject MemoryScope = "project"
)

type MemoryType string

const (
    TypeUser      MemoryType = "user"
    TypeFeedback  MemoryType = "feedback"
    TypeProject   MemoryType = "project"
    TypeReference MemoryType = "reference"
)
```

`user`/`feedback` 只允许进入用户目录；`project`/`reference` 只允许进入当前项目目录。scope 由可信调用上下文决定，模型输出不能把条目路由到任意路径。

### `InstructionSource` 与 `LoadIssue`

```go
type InstructionSource struct {
    Path     string
    Priority int
    Content  string
}

type LoadIssue struct {
    Path   string
    Reason string
}
```

`DiscoverInstructions(projectRoot, workDir, userConfigDir)` 按 spec 顺序扫描并展开 include；相同规范化绝对路径只处理一次。加载结果按优先级升序拼接，问题项可用于 run 报告。

### `MemoryHeader`、`MemoryEntry` 与 `MemoryRef`

```go
type MemoryHeader struct {
    Scope       MemoryScope
    Type        MemoryType
    Filename    string // 相对所属 memory 根目录的文件名
    Name        string
    Description string
    UpdatedAt   time.Time
}

type MemoryEntry struct {
    MemoryHeader
    Body string
}

type MemoryRef struct {
    Scope    MemoryScope
    Filename string
}
```

selector 返回 `MemoryRef` 而非绝对路径；manager 重新按 scope 和 filename 解析并校验后才读取正文。

### `MemoryChange`

```go
type MemoryAction string

const (
    ActionUpsert MemoryAction = "upsert"
    ActionDelete MemoryAction = "delete"
)

type MemoryChange struct {
    Action      MemoryAction
    Scope       MemoryScope
    Type        MemoryType
    Name        string
    Description string
    Body        string
}
```

模型不能指定路径、索引文件名或任意 frontmatter；存储层根据已校验 scope 与安全文件名生成路径及 frontmatter。索引由有效条目元数据重建。

### `RunMemoryContext` 与 `RunCompletion`

```go
type RunMemoryContext struct {
    InstructionText string
    UserIndex       string
    ProjectIndex    string
    Selected        []MemoryEntry
    Issues          []LoadIssue
    UserTruncated   bool
    ProjectTruncated bool
    ExtractedThrough uint64
}

type ConversationText struct {
    Seq  uint64
    Kind string // session_text 或 goal_reply
    Text string
}

type RunCompletion struct {
    ProjectRoot          string
    SessionID            string
    RunID                string
    WorkKind             string
    Messages             []ConversationText
    ThroughSeq           uint64
    MainAgentWroteMemory bool
}
```

普通会话只提供游标之后的新 `text` 消息，排除 `goal_request`、工具事件和已消费内容。目标工作项只提供游标之后由用户提交的 `/say` 或 `/reply` 文本；不传 goal intent、assistant 执行输出、工具结果、证据或 Stable 目标事件。`ThroughSeq` 是构建输入时 session log 的可信上界；提取完成或有意跳过后才推进 StateDir 中的每会话游标，失败则保留游标供下轮重试。`MainAgentWroteMemory` 由本 run 的 memory 工具审计结果确定。

### Worker 输入与状态

```go
type ExtractionInput struct {
    WorkKind string // session 或 goal
    Messages []ConversationText
    Existing []MemoryHeader
}

type ConsolidationInput struct {
    ActiveSessions int
    UserEntries     []MemoryEntry
    ProjectEntries  []MemoryEntry
}

type WorkerState struct {
    SessionCursors     map[string]uint64
    LastConsolidatedAt time.Time
}
```

WorkerState 按规范项目根保存于私有 Stable StateDir；锁和状态不写入项目记忆目录。

## 核心接口

```go
type ChatModel interface {
    GenerateChat(context.Context, []decision.ChatMessage) (string, error)
}

type Selector interface {
    Select(context.Context, string, []MemoryHeader) ([]MemoryRef, error)
}

type Processor interface {
    Extract(context.Context, ExtractionInput) ([]MemoryChange, error)
    Consolidate(context.Context, ConsolidationInput) ([]MemoryChange, error)
}

type Manager interface {
    PrepareRun(ctx context.Context, projectRoot, workDir, query string) (RunMemoryContext, error)
    List(ctx context.Context, projectRoot string) ([]MemoryHeader, error)
    Read(ctx context.Context, projectRoot string, scope MemoryScope, filename string) (MemoryEntry, error)
    Save(ctx context.Context, projectRoot string, change MemoryChange) error
    Delete(ctx context.Context, projectRoot string, scope MemoryScope, filename string) error
    Clear(ctx context.Context, projectRoot string, scope MemoryScope) (int, error)
    CompleteRun(ctx context.Context, completion RunCompletion)
    MaybeConsolidate(ctx context.Context, projectRoot string) error
}
```

`List` 列出两级元数据，不读取或返回记忆正文；正文只能通过指定 scope 与 filename 的 `Read` 获取。`PrepareRun` 返回的 `ExtractedThrough` 是本 session 已消费的可信游标，用于过滤新消息；`RunCompletion.ThroughSeq` 是本轮输入快照的可信上界。成功处理或有意跳过后将游标推进到该上界，模型或存储失败则保留旧游标以便重试。

`ChatModel` 由现有 `decision.ChatProvider` 实现并从组合入口注入。`Selector` 只解析至多 5 个合法、候选清单内的引用；`Processor` 只返回受限 JSON 变更。`Manager`/文件存储负责路径解析、frontmatter 校验、容量限制、索引重建、原子写入和事件回调。

`ExtractionInput` 包含运行类型、已过滤的会话文本与现有记忆清单；goal 类型还会在 prompt 和 Go 结果校验中应用偏好、项目背景与参考的允许类别。`ConsolidationInput` 包含当前项目自上次成功整理后有新活动的不同会话数量、两级记忆条目和索引。二者都不暴露项目源代码或任意文件读取接口。

## 模块设计

### `internal/memory`

**职责：** 指令发现与 include 安全边界；用户/项目 memory roots；Markdown/frontmatter 解析与生成；有界扫描、索引截断与刷新；记忆读取、写入、删除和清理；selector、extractor、consolidator 的 prompt/JSON 解析；每项目游标、整理时间和互斥状态。用户级 memory 目录和新建文件分别使用仅当前用户可访问的目录/文件权限；项目级目录遵循项目自身权限。

**对外接口：** `Manager`；`ChatModel`、`Selector`、`Processor` 为可替换依赖。`NewManager(Options) (*Service, error)` 接受规范项目根、用户配置目录、私有 StateDir、模型和结果回调。

**依赖：** 标准库、`decision.ChatProvider` 接口与 `platform/secfile`；不得依赖 `conversation`、`execution` 或通用 agent runner。

现有 `secfile.Root.Open` 只提供安全读取；M08 还需在同一可信根下安全枚举目录、创建目录、原子写入、替换和删除条目。因此扩展 `Root` 的相对路径 `ReadDir`、`MkdirAll`、`WriteFileAtomic`、`RemoveFile` 能力。manager 分别以项目根和用户配置基目录为可信锚点，再在根内定位 `.stable/memory` 或 `stable/memory`；不得先信任并打开任意 memory 子目录作为锚点。平台实现拒绝 symlink/non-regular-file，无法保证根边界时 fail closed。原子写入只替换根内已验证的普通文件，不能跟随目标路径中的链接。

### `internal/appconfig`

**职责：** 在现有 `UserConfigHome` 路径规则上提供用户 memory 根解析，与 skills 使用相同的 `XDG_CONFIG_HOME`/home 语义；`STABLE_CONFIG` 仍只移动主配置文件，不移动配置目录树。

**对外接口：** 新增 `UserMemoryDir() (string, error)`。

该 helper 内部调用 `platform/paths.UserConfigHome()`，与 `UserSkillsDir()` 使用相同的配置基目录规则。

### `internal/conversation`

**职责：** 将 manager 绑定到受服务配置约束的项目根；在 `startRun` 为 session/goal run 构造临时上下文；处理 `/memory` op；在 `consumeRun` 终态后触发异步提取与整理；将结果写入同一 session event log。

**对外接口：** `Deps.Memory memory.Manager`；协议操作 `memory_list`、`memory_delete`、`memory_clear`；通过 `ServerMsg` 的 memory list/report 字段返回结果。

**依赖：** `sessionlog`、`agent`、`memory` 与现有服务组件；manager 不信任 client 提交的 project root，使用 chatserve 绑定的 root。

### `internal/execution`

**职责：** 定义 `MemoryProvider` 并注册 `memory_list`、`memory_read`、`memory_save`、`memory_delete` 主机工具。Provider 被绑定到服务的规范项目根与用户 memory 根；参数只接收 scope、filename/name、type、description、body，不接收绝对路径或根目录。

**对外接口：** `WithMemoryProvider(provider)`；schema 加入 `chatserveToolSchemas`。写入调用不会创建候选或快照，但每次调用仍通过统一工具执行记录路径并返回明确结果。

**依赖：** `agent`、`llm` 与被注入的 memory provider；文件安全和 scope 归属由 provider/manager 负责，普通 `write_file`/`edit_file` 分派不变。

```go
type MemoryProvider interface {
    List(ctx context.Context, sessionID string) ([]memory.MemoryHeader, error)
    Read(ctx context.Context, sessionID string, scope memory.MemoryScope, filename string) (memory.MemoryEntry, error)
    Save(ctx context.Context, sessionID string, change memory.MemoryChange) error
    Delete(ctx context.Context, sessionID string, scope memory.MemoryScope, filename string) error
}
```

Provider 由 chatserve 绑定到规范项目根与用户 memory 根；工具参数不包含根路径。`sessionID` 仅用于操作归属及审计事件。

### `internal/sessionlog` 与 `internal/tui`

`sessionlog` 新增 `memory_action`、`memory_background` 事件及严格 payload 校验；保存范围、条目标识、操作、状态和时间，不把后台模型原始响应写入事件。投影保留用户可读的记忆状态摘要，不把运行时注入上下文复制成聊天消息。

TUI 注册 `/memory`、`/memory list`、`/memory delete <scope> <entry>`、`/memory clear [user|all]`，通过既有 request/result 模式调用服务；后台报告显示新增/更新/删除数量或安全错误摘要。

## 模块交互

### run 上下文

1. TUI 提交普通 run，或 Temporal worker 经 `GoalSocketClient` 提交 goal work；二者均到达同一个 `conversation.Service.startRun`。
2. Service 使用其绑定的 project root、由 `BuildAuthority` 得到的可信 allowed work root 和当前用户配置目录调用 `Manager.PrepareRun`；不使用 client 任意路径作为 workdir。
3. Manager 先发现并展开指令，再读取用户与项目 `MEMORY.md`（各自受行数/字节上限限制），扫描最多 200 条/范围的 frontmatter 清单，并用请求文本调用 selector。
4. selector 结果必须是候选清单内、最多 5 条的 `MemoryRef`；manager 重新安全打开文件并附上 scope、更新时间和过期提示。
5. Service 将 `STABLE instructions`、memory indexes、selected memory 作为 run-only 的上下文前缀，置于历史和新输入之前，然后交给现有 `StreamingRunner`。这些前缀不另存为 session message。

### 主动记忆工具与管理命令

Agent 的 `memory_*` 调用由 tool executor 主机分支路由至绑定的 `MemoryProvider`。Provider 将请求转换成 scope + filename 形式调用 manager；manager 执行根目录 containment、symlink/non-regular-file 检查、类型映射和大小限制，成功后重建对应 scope 的索引。TUI 的 list/delete/clear op 走 conversation 服务直接调用同一 manager。两条路径都写入 metadata-only 的 `memory_action` 事件；现有 tool call/result 记录继续沿用通用工具审计。

### 自动提取与整理

1. `consumeRun` 收到终态并对客户端广播结果后，构建 `RunCompletion`；session 文本游标避免重复处理，普通 run 与 goal run 分别按已批准的消息过滤规则取输入。
2. `CompleteRun` 立即返回并调度有界后台单次模型调用。若本 run 的 agent 已成功 `memory_save`，跳过 extractor；否则把过滤后的文本和记忆清单发给现有模型服务。
3. Manager 校验每个 `MemoryChange` 的 action/type/scope/name/body；拒绝不匹配的类别、越界引用、重复/超限变更。通过后在对应 memory root 原子更新主题文件并重建索引。
4. 每个 run 完成后检查整理门槛。门槛为距成功整理至少 24 小时、且成功整理后已有至少 5 个不同 session 出现新活动；同项目已有整理任务时跳过。通过门槛后，consolidator 读取两级记忆与活动计数，模型返回受限变更集，manager 校验并应用。
5. 提取或整理完成、跳过或失败时写 `memory_background` 事件；状态只含 action/count/reason，不包含正文。后台失败不回滚已完成 run，也不改变 goal records。

## 文件组织

```text
internal/memory/
├── types.go             — scope/type、entry、change、context、worker input 类型
├── instructions.go      — 指令发现、优先级、受限 include
├── store.go             — memory roots、安全 Markdown/frontmatter 读写、索引
├── selector.go          — 清单扫描与相关记忆选择
├── processor.go         — 提取/整理 JSON prompt、解析与变更校验
└── manager.go           — run context、CRUD、后台任务与门槛编排

internal/appconfig/config.go
  — UserMemoryDir 路径 helper

internal/platform/secfile/
  — Root 内安全枚举、原子写入/删除的文件能力及平台实现

internal/conversation/
├── memory.go            — memory op handlers、结果消息、事件回调
├── protocol.go          — memory op / ServerMsg 字段与协议校验
├── service.go           — manager 注入与绑定
└── run.go               — run context 注入、completion 后台触发

internal/execution/
├── memory.go            — MemoryProvider 与 memory_* host dispatch
├── tools_schema.go      — 记忆工具 schemas
├── executor_factory.go  — MemoryProvider 注入
└── tool_executor.go     — memory_* 主机分派

internal/platform/secfile/
├── secfile.go           — Root 相对 mkdir/write/delete 的公开 API
├── secfile_linux.go     — Linux rooted no-follow 原子操作
├── secfile_darwin.go    — Darwin rooted no-follow 原子操作
├── secfile_windows.go   — Windows 根内安全写入/删除
├── secfile_other.go     — 无法提供保证的平台 fail-closed
└── root_write_test.go   — 越界、链接替换和原子更新契约

internal/sessionlog/
├── events.go            — memory event 与 payload 类型
├── log.go               — 事件白名单和 append/replay 支持
├── validate.go          — payload 校验
└── projection.go        — memory event 投影和上下文排除

internal/tui/model.go    — /memory 命令路由、结果处理与呈现
cmd/stable/chatserve.go  — manager/model/tool 依赖组装

internal/memory/*_test.go
internal/conversation/memory_test.go
internal/execution/memory_test.go
internal/sessionlog/memory_test.go
internal/platform/secfile/root_write_test.go
internal/tui/m08_memory_test.go
tests/e2e/m08_memory_test.go
```

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 记忆存储 | Markdown 主题文件 + YAML frontmatter；`MEMORY.md` 由文件元数据生成 | 可直接查看/提交；索引由程序维护，避免索引与条目分叉 |
| 用户级路径 | `appconfig.UserMemoryDir()`（内部复用 `platform/paths.UserConfigHome()`）；项目级路径 `<projectRoot>/.stable/memory` | 与现有 skills 配置目录、项目会话目录约定一致 |
| 文件安全 | 扩展 `secfile.Root` 的安全相对枚举/写入/删除；用户级新目录/文件设为当前用户私有；模型和工具只传 scope/filename | 限制在可信根下读写，拒绝路径穿越、符号链接和任意绝对路径 |
| 模型调用 | 复用已配置 `decision.ChatProvider.GenerateChat`，采用单次 JSON 选择/变更输出 | 不增加 provider、依赖或通用 agent 工具循环；模型无文件系统能力 |
| agent 记忆工具 | 专用 memory host tools + 绑定根的 `MemoryProvider` | 普通文件写仍只作用候选区；用户记忆写入拥有独立且狭窄的边界 |
| 上下文注入 | `conversation.Service.startRun` 生成非持久化前缀，session/goal 共用 | 延续 M07 每轮上下文模式，避免污染 transcript 和 M05 压缩边界 |
| 后台提取 | Run 终态广播后异步执行；按消息类型过滤，已主动保存时跳过 | 不延迟交互；goal 执行输出、工具结果和目标事件不进入 extractor |
| 整理状态 | 私有 Stable state 下按规范项目根保存成功时间、session 游标和互斥状态 | 不向可能提交的项目记忆目录写运行时锁/时间戳；可检测重复处理与并发整理 |
| 失败策略 | selector 失败为空召回；后台 JSON/文件失败丢弃该批变更并写失败状态 | 记忆为补充上下文，错误不得阻断普通运行或目标运行 |

## Spec 覆盖关系

| Spec | Plan 归属 |
|---|---|
| F1–F2 指令发现/include | `internal/memory/instructions.go` + conversation run 前缀 |
| F3–F5 双范围存储、索引与召回 | `internal/memory/store.go`、`selector.go`、`manager.go` |
| F6 agent 受限记忆操作 | `internal/execution/memory.go` + manager/store 根边界 |
| F7 run 后自动提取与 goal 事实隔离 | `internal/conversation/run.go` + `internal/memory/processor.go` |
| F8 时间/会话门槛整理 | `internal/memory/manager.go` + 私有 StateDir 状态 |
| F9 管理命令 | `internal/conversation/memory.go`、`protocol.go`、`internal/tui/model.go` |
| N1–N7 安全、资源、失败与持久性 | `secfile` 接入、限额校验、sessionlog 事件和 M05 回归验收 |
