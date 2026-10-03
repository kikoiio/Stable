# M01 共用 TUI 外壳 Plan

> 基于已批准的 [spec.md](spec.md)。M01 改造终端界面和基础交互，沿用 Stable 的服务请求、领域数据与持久化事实；不实现后续 agent 运行能力。

## 架构概览

TUI 使用单一对话主视图，消息和输入常驻；会话与目标通过导航视图进入，不再同时占用三栏。根 Bubble Tea 模型协调导航、输入、消息视窗、布局、状态和补全组件。会话及目标数据继续由 Stable conversation 服务通过现有 Unix socket 协议提供。消息视窗消费已加载的 session events；本阶段不增加流式事件协议。

```text
用户键盘输入 / 终端尺寸
          ↓
    根 Bubble Tea Model ─────→ 会话/目标导航
       │       │                    │
       │       ├→ Composer → 补全/弹层承载
       │       ├→ Transcript Viewport ← Markdown Renderer
       │       ├→ Layout Calculator
       │       └→ Status Bar
       │
       └→ 现有 Unix socket 请求 → Stable conversation 服务
                              └→ 完整 transcript / session / goal 响应
```

组件依赖从根模型指向表现组件；组件不反向调用根模型或服务。根模型把用户意图转成现有服务请求。只在视图状态中新增字段，不修改目标、事件、会话存储 schema。

## 核心数据结构与接口

### 视图状态

```go
type ViewMode uint8

const (
    ChatView ViewMode = iota
    SessionPickerView
    GoalPickerView
)

type NavigationState struct {
    Mode     ViewMode
    Cursor   int
    ReturnTo ViewMode
}

type LayoutMetrics struct {
    Width, Height     int
    TranscriptWidth   int
    TranscriptHeight  int
    ComposerHeight    int
    Compact            bool
    TooSmall           bool
}
```

`NavigationState` 只记录当前选择与返回位置；`LayoutMetrics` 是当前终端尺寸计算出的瞬时几何信息，不持久化。根模型继续引用现有 `sessionlog.SessionInfo`、`sessionlog.Event` 和 `core.Goal`，输入草稿留在当前 TUI 会话状态中。

### 状态与补全

```go
type StatusPhase uint8

const (
    StatusIdle StatusPhase = iota
    StatusLoading
    StatusError
)

type StatusState struct {
    Phase StatusPhase
    Text  string
}

type CompletionKind uint8

const (
    CommandCompletion CompletionKind = iota
    PathCompletion
)

type CompletionItem struct {
    Kind      CompletionKind
    Label     string
    Detail    string
    InsertText string
}
```

`CompletionItem` 只提供候选呈现与插入内容。斜杠候选须关联到当前已注册的操作；文件候选只插入项目根目录下的相对路径，不读取文件内容。

### 组件接口

```go
type Requester interface {
    Request(context.Context, conversation.ClientMsg) ([]conversation.ServerMsg, error)
}

type TranscriptRenderer interface {
    Render([]sessionlog.Event, width int) (string, error)
}

type PathCompleter interface {
    Complete(projectRoot, prefix string) ([]CompletionItem, error)
}

type CommandSource interface {
    List(prefix string) []CompletionItem
}

func ComputeLayout(width, height, composerHeight int) LayoutMetrics
func FilterCompletions(items []CompletionItem, prefix string) []CompletionItem
```

`Requester` 的 socket 实现继续调用现有 `conversation.Request`；不扩展 `ClientMsg`/`ServerMsg`。`TranscriptRenderer` 根据可用宽度渲染已有事件，不更改原始事件。布局和候选过滤保持纯函数，方便无终端的单元验证。

## 模块设计

### 根 TUI 模型

**职责：** 保存当前会话、选中目标、事件、输入草稿、视图状态与服务状态；路由 `tea.Msg` 和子组件意图；构造主视图。
**对外接口：** 现有 `New`、`Init`、`Update`、`View` 与 `Run` 生命周期。
**依赖：** 导航、输入、消息视窗、布局、状态栏和现有 conversation 客户端。

### 会话与目标导航

**职责：** 在会话 picker 中列出/选取会话；在目标视图中列出目标及状态、摘要和证据入口；提供返回对话的导航事件。
**对外接口：** 通过 Bubble Tea 消息通知根模型选中会话或目标。
**依赖：** 根模型传入的现有 `SessionInfo`、`Goal` 列表；选中会话时由根模型发出 `session_load` 请求。

### 输入器

**职责：** 封装 Bubbles `textarea.Model`、焦点、自动高度、Enter 提交与 Ctrl+J 换行；维护未提交草稿；显示候选菜单并发出选中意图。
**对外接口：** Bubble Tea `Update`/`View` 与提交、取消、选择候选等输入意图。
**依赖：** 补全组件；不直接请求模型、服务或执行命令。

### 消息视窗与 Markdown 渲染

**职责：** 以 Bubbles `viewport.Model` 呈现 transcript，响应滚动和尺寸变化；按消息宽度渲染 Markdown，保留完整长消息。
**对外接口：** 设置事件/宽度、处理滚动输入、输出视图。
**依赖：** 根模型提供的现有 `sessionlog.Event`；Glamour renderer；不写会话存储。

### 补全与弹层承载

**职责：** 筛选命令与项目文件候选，处理候选游标和接受/取消；提供统一弹层尺寸与遮罩区域。M01 内建 `/sessions`、`/goals` 仅切换本地视图；其他命令只有具备实际处理器时才可列出。
**对外接口：** `CommandSource.List`、`PathCompleter.Complete` 及候选选择事件。
**依赖：** 项目根路径、当前已注册命令元数据、终端布局；不执行文件读取或未来的权限/计划动作。

### 布局与状态栏

**职责：** 依据窗口和输入高度计算 transcript/composer 几何；在足够空间时使用完整对话布局，较窄时采用紧凑布局，低于当前可用阈值时显示放大提示；用文本与符号区分焦点、闲置、加载和错误。
**对外接口：** `ComputeLayout` 和状态视图。
**依赖：** Bubble Tea 窗口尺寸消息、根模型状态；不发起业务操作。

依赖无环：根模型 → 导航/输入/视窗/布局/状态；输入 → 补全；视窗 → renderer。导航、布局、状态及渲染器均不依赖根模型具体实现。

## 模块交互

### 启动与对话

1. Bubble Tea 启动根模型，沿用现有 `session_list` 请求；服务响应后选择最近会话并调用 `session_load`。
2. Transcript renderer 将已加载事件转换为有宽度约束的对话视图，viewport 定位到最新消息；用户可滚动查看较早内容。
3. 输入器接收按键：Enter 产生提交意图，Ctrl+J 在草稿中插入换行。根模型发送现有 `chat` 请求，记录 Loading 状态；结果返回后刷新完整会话。
4. 失败时保留服务返回的现有恢复语义，状态栏显示失败；不会把失败误显示成闲置或成功。

### 导航、命令与文件补全

1. 用户提交 `/sessions` 或 `/goals`，输入器匹配已注册的内建命令并向根模型发出导航意图。
2. 根模型切换到对应选择视图；↑/↓ 移动，Enter 选择，Esc 返回对话。选会话时请求 `session_load`；选目标时只显示已加载的 Stable 事实。
3. 用户输入 `@` 路径前缀时，路径补全器在项目根目录内生成相对路径候选；确认后把路径文本插入输入框。M01 不读取该文件或把内容附加给 agent。
4. 通用弹层区域计算可视范围并覆盖主视图；当前没有处理器的权限、计划、提问操作不注册、不显示。

### 尺寸与状态

1. 收到 `tea.WindowSizeMsg` 后，根模型调用 `ComputeLayout`，更新 viewport、textarea 与候选/弹层宽度。
2. 布局切换期间保留当前会话、导航游标和未提交草稿；无需重新读取服务或丢弃输入。
3. 状态栏从根模型的 `StatusState` 渲染上下文、焦点提示、加载或错误；颜色关闭时仍用文字/符号区分。

## 文件组织

```text
Stable/
├── go.mod                          加入 Glamour 直接依赖
├── go.sum                          锁定其依赖校验和
├── internal/tui/
│   ├── model.go                    根状态机，替换三栏为对话主视图
│   ├── layout.go                   响应式布局计算
│   ├── composer.go                 textarea、多行草稿及焦点
│   ├── transcript.go               viewport 和消息事件呈现
│   ├── markdown.go                 Glamour 与无色回退
│   ├── navigation.go               会话/目标选择视图
│   ├── completion.go               命令/路径候选与通用弹层承载
│   ├── status.go                   状态栏、焦点和提示
│   ├── model_test.go               更新现有根模型交互测试
│   ├── layout_test.go               紧凑/小窗口布局测试
│   ├── composer_test.go             Enter、Ctrl+J、草稿保留测试
│   ├── transcript_test.go           滚动、换行、Markdown/无色测试
│   └── completion_test.go           命令和项目相对路径补全测试
└── specs/M01/
    ├── spec.md                      已批准的需求
    ├── plan.md                      本文
    ├── task.md                      下一阶段的有序实施任务
    └── checklist.md                 实施验收场景
```

M01 不修改 `internal/conversation`、`internal/core`、存储 schema 或服务启动协议。`go.sum` 由新增依赖解析生成。具体测试用例在 task/checklist 中细化，不在 M01 新增大范围集成设施。

## 技术决策

| 决策点 | 选择 | 理由 |
| --- | --- | --- |
| UI 框架 | 保留 Stable 现有 Bubble Tea、Bubbles、Lipgloss 版本 | 降低迁移面，现有 TUI 与目标视图可渐进重构。 |
| 主视图 | 对话常驻，会话/目标以导航视图进入 | 采用 mewcode 的对话优先体验，同时保留 Stable 目标入口。 |
| 导航动作 | `/sessions`、`/goals`，列表用 ↑/↓、Enter、Esc | 在常驻输入区也能找到入口，不占固定侧栏；内建命令只切换视图。 |
| 输入提交 | Enter 提交，Ctrl+J 换行 | 与 mewcode 现有行为一致，保留明确的多行输入方式。 |
| 文件补全边界 | 只在项目根目录内补全相对路径并插入文本 | 提供 `@` 交互而不引入文件读取/上下文注入能力。 |
| Markdown | Glamour `v1.0.0`，按终端能力选样式并提供无色/纯文本回退 | 复用 mewcode 已采用的 renderer，同时满足无颜色可辨识；不复制强制 TrueColor 配置。 |
| 服务通信 | 沿用 Unix socket 与完整请求/响应 | M01 不改服务端协议；流式事件归 M02。 |
| 弹层 | 先提供尺寸、遮罩和内容容器，动作 handler 由后续模块注入 | 建立可复用外壳，不伪造尚未交付的权限/计划/提问功能。 |
| TUI 组织 | 按职责拆分现有 `internal/tui` 包，不复制 mewcode 主模型 | mewcode TUI 依赖 agent、权限、MCP、memory、teams 等大量内部模块，整文件迁入会绕开 Stable 契约。 |

## Spec 覆盖

| Spec 需求 | 设计落点 | 可观察实现证据 |
| --- | --- | --- |
| F1 | 根模型、`ViewMode`、导航 | 对话主视图及 session/goal 选择视图。 |
| F2 | composer 与键路由 | 多行自动高度、Enter 提交、Ctrl+J 换行、草稿保留。 |
| F3 | viewport、transcript、Markdown renderer | 滚动完整事件与格式化长消息。 |
| F4 | navigation 与 Stable 既有类型 | 选择会话、目标状态/摘要/证据入口。 |
| F5 | `StatusState`、status bar | 焦点提示、闲置/加载/错误区分。 |
| F6 | `CommandSource`、`PathCompleter` | slash 命令和项目相对路径候选及插入。 |
| F7 | completion/overlay container | 可复用弹层尺寸；未实现动作不显示。 |
| N1–N6 | 技术栈、布局、状态、无色样式约束 | Linux Bubble Tea、resize/compact、纯文本状态标识与范围检查。 |
