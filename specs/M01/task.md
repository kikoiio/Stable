# M01 共用 TUI 外壳 Tasks

> 基于已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。任务按单一 UI 组件或集成步骤拆分；只实现共用 TUI 外壳和基础交互，不接入完整 agent 流、权限控制或工具循环。

## 文件清单

| 操作 | 文件 | 职责 |
| --- | --- | --- |
| 修改 | `go.mod`、`go.sum` | 加入 Glamour Markdown renderer 依赖 |
| 修改 | `internal/tui/model.go` | 根状态机、socket 请求、对话主视图及组件协调 |
| 新建 | `internal/tui/layout.go` | 响应式几何计算与紧凑/小窗口状态 |
| 新建 | `internal/tui/composer.go` | 多行 textarea、草稿与输入意图 |
| 新建 | `internal/tui/transcript.go` | viewport、滚动和事件投影 |
| 新建 | `internal/tui/markdown.go` | Glamour 渲染与无色回退 |
| 新建 | `internal/tui/navigation.go` | 会话/目标导航与 picker |
| 新建 | `internal/tui/completion.go` | `/sessions`、`/goals`、项目相对路径补全及弹层承载 |
| 新建 | `internal/tui/status.go` | 状态栏及焦点/快捷键提示 |
| 修改 | `internal/tui/model_test.go` | 根模型和导航交互回归 |
| 新建 | `internal/tui/layout_test.go` | 全尺寸、紧凑和小窗口布局检查 |
| 新建 | `internal/tui/composer_test.go` | 多行输入、Enter/Ctrl+J 与草稿保留 |
| 新建 | `internal/tui/transcript_test.go` | 长消息、滚动、Markdown 和无色样式 |
| 新建 | `internal/tui/completion_test.go` | 内建命令与根目录相对路径候选 |

不修改 `internal/conversation`、`internal/core`、持久化结构、服务启动方式和 mewcode 源仓库。

## T1：加入 Glamour 依赖

**文件：** `go.mod`、`go.sum`
**依赖：** 无

**步骤：**
1. 将 `github.com/charmbracelet/glamour` 以 `v1.0.0` 加为直接依赖。
2. 更新校验和，不升级现有 Bubble Tea/Bubbles/Lipgloss 版本。

**验证：** `go list -m github.com/charmbracelet/glamour` 返回 `v1.0.0`；`go mod verify` 成功。

## T2：定义布局结果并实现几何计算

**文件：** `internal/tui/layout.go`、`internal/tui/layout_test.go`
**依赖：** 无

**步骤：**
1. 定义 `LayoutMetrics` 字段。
2. 实现纯函数 `ComputeLayout(width, height, composerHeight int)`。
3. 计算消息区/输入区尺寸、紧凑模式和不可用尺寸标志；加入普通尺寸的基础用例。

**验证：** `go test ./internal/tui -run TestComputeLayout` 通过；普通尺寸返回非负消息区和输入区。

## T3：覆盖布局边界用例

**文件：** `internal/tui/layout_test.go`
**依赖：** T2

**步骤：**
1. 添加刚高于与低于紧凑宽度阈值的测试用例。
2. 添加最低可用宽高边界和多行 composer 高度用例。
3. 检查不可用尺寸产生 `TooSmall`，而不是负视窗尺寸。

**验证：** `go test ./internal/tui -run TestComputeLayout` 通过，所有边界断言成立。

## T4：实现 Markdown renderer

**文件：** `internal/tui/markdown.go`、`internal/tui/transcript_test.go`
**依赖：** T1

**步骤：**
1. 封装按终端宽度创建/复用 Glamour renderer 的逻辑。
2. 遵循终端颜色能力，提供无色/纯文本 fallback。
3. 不强制 TrueColor，按消息可用宽度换行；加入单段文本的 smoke test。

**验证：** `go test ./internal/tui -run TestRenderMarkdown` 通过；渲染不返回错误且保留正文。

## T5：验证 Markdown 结构和无色输出

**文件：** `internal/tui/transcript_test.go`
**依赖：** T4

**步骤：**
1. 添加标题、列表、代码块和长段落渲染用例。
2. 添加颜色关闭时渲染用例。
3. 断言内容可读且无色输出不含 ANSI 颜色序列。

**验证：** `go test ./internal/tui -run TestRenderMarkdown` 通过。

## T6：将会话事件投影为完整消息

**文件：** `internal/tui/transcript.go`、`internal/tui/transcript_test.go`
**依赖：** T4

**步骤：**
1. 将现有 `sessionlog.Event` 中的消息事件格式化为角色与消息内容。
2. 保留消息内换行和完整文本，不压缩成长摘要。
3. 对未知事件类型保持安全忽略或显示中性占位；加入投影基础测试。

**验证：** `go test ./internal/tui -run TestTranscriptProjection` 通过；多行正文逐字保留。

## T7：为 transcript 添加 viewport 滚动与尺寸更新

**文件：** `internal/tui/transcript.go`、`internal/tui/transcript_test.go`
**依赖：** T2、T6

**步骤：**
1. 封装 Bubbles `viewport.Model` 并将 T6 输出作为内容。
2. 支持滚动输入，载入新会话时重置位置。
3. 在窗口 resize 时更新宽高但保留事件内容；加入滚动和尺寸测试。

**验证：** `go test ./internal/tui -run TestTranscriptViewport` 通过；缩放后内容仍在，向上/下滚动改变视窗位置。

## T8：验证长消息与 viewport

**文件：** `internal/tui/transcript_test.go`
**依赖：** T5、T7

**步骤：**
1. 组合测试长 Markdown 消息在窄宽度下换行。
2. 验证滚动可以到达长消息开头和末尾。
3. 验证不同会话载入不会混留旧事件。

**验证：** `go test ./internal/tui -run TestTranscript` 通过。

## T9：配置 textarea 与多行高度

**文件：** `internal/tui/composer.go`、`internal/tui/composer_test.go`
**依赖：** T2

**步骤：**
1. 用现有 Bubbles `textarea.Model` 封装输入器。
2. 根据内容行数计算高度并限制在可用区域内。
3. 提供草稿读取/设置和当前焦点状态；加入高度基础测试。

**验证：** `go test ./internal/tui -run TestComposerHeight` 通过；多行内容增加输入区高度且不超过布局可用高度。

## T10：实现 Enter/Ctrl+J 输入映射和草稿保留

**文件：** `internal/tui/composer.go`、`internal/tui/composer_test.go`
**依赖：** T9

**步骤：**
1. 将 Enter 映射为提交意图，Ctrl+J 映射为插入换行。
2. 暂时失去焦点或进入/退出导航时保留草稿。
3. 成功提交后清空草稿；取消输入时保留草稿。

**验证：** `go test ./internal/tui -run TestComposerInput` 通过；按键意图和草稿生命周期符合预期。

## T11：覆盖 composer 键盘和状态测试

**文件：** `internal/tui/composer_test.go`
**依赖：** T10

**步骤：**
1. 测试 Enter 提交非空文本及忽略空文本。
2. 测试 Ctrl+J 插入换行。
3. 测试导航和 resize 往返后草稿仍在。

**验证：** `go test ./internal/tui -run TestComposer` 通过。

## T12：实现会话选择视图

**文件：** `internal/tui/navigation.go`、`internal/tui/model_test.go`
**依赖：** T2

**步骤：**
1. 按现有 `sessionlog.SessionInfo` 展示会话名称和当前选中项。
2. 支持上下移动、Enter 选择和 Esc 返回意图。
3. 空列表显示明确的空状态；加入 picker 边界与选择用例。

**验证：** `go test ./internal/tui -run TestSessionNavigation` 通过；上下边界、选择和空列表符合预期。

## T13：实现目标状态视图

**文件：** `internal/tui/navigation.go`、`internal/tui/model_test.go`
**依赖：** T2

**步骤：**
1. 按现有 `core.Goal` 展示名称、状态、摘要和证据入口。
2. 支持目标选择与空状态。
3. 不新增服务请求或持久化字段；加入目标摘要呈现用例。

**验证：** `go test ./internal/tui -run TestGoalNavigation` 通过；视图内容对应输入目标字段。

## T14：实现内建 slash 导航命令

**文件：** `internal/tui/completion.go`、`internal/tui/completion_test.go`
**依赖：** T12、T13

**步骤：**
1. 定义 `CompletionItem` 和命令候选前缀过滤。
2. 只注册 `/sessions`、`/goals` 两个 UI 导航动作。
3. 选择时向根模型发送本地导航意图，不发起 agent/service 请求；加入导航命令基础测试。

**验证：** `go test ./internal/tui -run TestCommandCompletion` 通过；两命令分别进入对应视图，未知命令无候选。

## T15：实现项目相对路径补全与通用弹层

**文件：** `internal/tui/completion.go`、`internal/tui/completion_test.go`
**依赖：** T2

**步骤：**
1. 从 `@` 前缀生成项目根目录内相对路径候选。
2. 排除解析后逃逸项目根目录的路径；确认时只插入路径字符串。
3. 实现候选/弹层的宽度裁剪和可视区域布局，不注册未实现操作；加入根内/根外路径测试。

**验证：** `go test ./internal/tui -run TestPathCompletion` 通过；根内文件可选，根外路径被排除，内容不被读取。

## T16：测试命令、路径和候选视图

**文件：** `internal/tui/completion_test.go`
**依赖：** T14、T15

**步骤：**
1. 测试命令前缀筛选、游标移动、选择和取消。
2. 测试 `@` 文件前缀筛选、相对路径插入与越界路径排除。
3. 测试窄窗口下候选和弹层裁剪。

**验证：** `go test ./internal/tui -run 'Test(CommandCompletion|PathCompletion)'` 通过。

## T17：实现状态栏

**文件：** `internal/tui/status.go`、`internal/tui/model_test.go`
**依赖：** T2

**步骤：**
1. 定义 Idle、Loading、Error 三类状态并渲染会话/目标上下文。
2. 显示焦点操作提示和实际已注册快捷键。
3. 用文字/符号区分焦点、加载和错误，不只依赖颜色；加入三态基础测试。

**验证：** `go test ./internal/tui -run TestStatus` 通过；三态内容不同且错误状态无需颜色可辨识。

## T18：建立对话主视图

**文件：** `internal/tui/model.go`、`internal/tui/model_test.go`
**依赖：** T7、T9、T17

**步骤：**
1. 将默认 View 切换为对话消息视窗、composer 和状态栏组成的主视图。
2. 移除默认常驻三栏布局，不删除会话/目标数据。
3. 接入计算出的布局尺寸；加入默认主视图断言。

**验证：** `go test ./internal/tui -run TestChatPrimaryView` 通过；默认 View 包含对话和输入而不同时渲染三栏。

## T19：接入会话/目标导航与现有 socket 请求

**文件：** `internal/tui/model.go`、`internal/tui/model_test.go`
**依赖：** T12、T13、T14、T18

**步骤：**
1. 将导航意图路由到会话/目标选择视图；Enter/Esc 按规则切换或返回。
2. 选会话时沿用 `session_load`，提交消息沿用 `chat` 请求。
3. 目标视图只读当前 Stable 服务提供的目标事实；不新增请求协议，并验证现有 socket 操作名。

**验证：** `go test ./internal/tui -run 'Test(ModelNavigation|SessionLoad|ChatRequest)'` 通过；记录的 socket 操作仍为现有操作名。

## T20：接入 resize、输入与错误状态

**文件：** `internal/tui/model.go`、`internal/tui/model_test.go`
**依赖：** T10、T15、T17、T19

**步骤：**
1. 路由 `tea.WindowSizeMsg` 更新布局、viewport、textarea、候选和弹层尺寸。
2. 将 composer 提交意图转成现有请求，将 loading/result/error 状态交给状态栏。
3. 保留草稿、当前导航和既有服务错误恢复行为。

**验证：** `go test ./internal/tui -run 'Test(ModelResize|ModelError|DraftPreserved)'` 通过；resize 不丢草稿，失败显示错误且状态非闲置。

## T21：覆盖根模型用户流程

**文件：** `internal/tui/model_test.go`
**依赖：** T19、T20

**步骤：**
1. 测试启动、最近会话加载、普通消息提交与结果刷新。
2. 测试 `/sessions`、`/goals`、选择和返回对话。
3. 测试目标状态展示与服务失败反馈。

**验证：** `go test ./internal/tui -run 'Test(Model|Session|Goal|Navigation)'` 通过；组件状态和 View 与预期一致。

## T22：执行 TUI 包回归

**文件：** `internal/tui` 全部测试文件
**依赖：** T3、T5、T8、T11、T16、T17、T21

**步骤：**
1. 运行整个 TUI 包测试，检查所有新增和既有用例。
2. 修复由对话主视图、键路由或 layout 改造引入的回归。
3. 确认 M01 未新增服务端协议、存储字段或未交付的可操作入口。

**验证：** `go test ./internal/tui` 通过；小尺寸、无色、长消息、补全、导航和既有会话功能均有测试证据。

## 执行顺序

```text
T1 → T4 → T5 ─────────┐
T2 → T3 ───────┐      ├→ T6 → T7 → T8 ─────┐
 ├→ T9 → T10 → T11 ──┤                    │
 ├→ T12 ───────┐     ├→ T14 ──┐            ├→ T18 → T19 → T20 → T21 → T22
 ├→ T13 ───────┴─────┘        └→ T16 ─────┤
 └→ T15 ──────────────────────┘            └→ T17
```

如果实作需要增加服务协议字段、持久化格式或运行能力，先回到对应子项目的 spec 评审，不在 M01 内扩大范围。
