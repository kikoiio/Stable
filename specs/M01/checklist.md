# M01 共用 TUI 外壳 Checklist

> M01 验收共用 TUI 外壳和基础交互。每项通过真实测试输出或 Linux 终端中的可观察行为记录证据；没有实现的后续功能不能记为通过。

## 行为验收

- [x] **AC1 / F1：对话主视图**。启动后对话消息区、输入区和状态栏是主界面；不再同时显示固定的会话/聊天/目标三栏。（证据：`TestChatPrimaryViewAndDraftSurvivesNavigation` 检查主视图包含 composer，且不包含旧三栏标题。）
- [x] **AC1 / F1：同一 TUI 的导航入口**。分别打开会话和目标导航，再返回对话，进程和主界面持续使用同一 TUI。（证据：`TestSlashCommandsOnlySwitchLocalNavigation`、`TestChatPrimaryViewAndDraftSurvivesNavigation`；根模型直接切换 ViewMode。）
- [x] **AC2 / F2：多行输入**。输入两行文本时 composer 高度随内容增长；Ctrl+J 插入换行，Enter 提交完整文本。（证据：`TestComposerHeightAndDraft`、`TestComposerCtrlJAndSubmitIntent`、`TestComposerSubmitClearsOnlyAfterSubmit`。）
- [x] **AC2 / N2：草稿保留**。输入草稿后调整终端尺寸并打开/关闭导航，文本不丢失；明确提交后 composer 清空。（证据：`TestModelResizeAndServiceErrorKeepDraftAndExposeError`、`TestChatPrimaryViewAndDraftSurvivesNavigation`、`TestComposerSubmitClearsOnlyAfterSubmit`。）
- [x] **AC3 / F3：Markdown 消息**。标题、列表、代码块、换行和长段落按窗口宽度呈现，未被折叠成摘要。（证据：`TestRenderMarkdownStructureAndNoColor`、`TestTranscriptProjectionAndSessionReset`。）
- [x] **AC3 / F3：长对话滚动**。可从底部滚动到较早消息，再回到最新消息；切换会话后不混入前一会话事件。（证据：`TestTranscriptViewportScrollAndResize`、`TestTranscriptProjectionAndSessionReset`。）
- [x] **AC4 / F4：会话导航**。会话列表显示当前项；↑/↓移动、Enter 加载选中会话、Esc 返回。（证据：`TestSessionNavigationAndResizeStayAvailable`、`TestSessionPickerUsesExistingSessionLoadOperation`；操作名仍为 `session_load`。）
- [x] **AC4 / F4：目标状态与证据入口**。目标视图能显示已有目标名称、状态、摘要和证据入口；没有目标时显示空状态。（证据：`TestGoalNavigationShowsStatusAndEvidence`；视图读取 `core.Goal` 现有字段。）
- [x] **AC5 / F5：状态栏**。会话/目标上下文、焦点提示和快捷键可见；Idle、Loading、Error 三种状态文案不同。（证据：`TestStatusPhasesAndActionsAreVisible`。）
- [x] **AC6 / F6：slash 命令补全**。输入 `/s`、`/g` 可筛选已实现的 `/sessions`、`/goals`；选择后只切换界面，未知命令不作为可用项显示。（证据：`TestCommandCompletion`、`TestTabCompletionInsertsOnlyCommandOrRelativePath`、`TestSlashCommandsOnlySwitchLocalNavigation`。）
- [x] **AC6 / F6：文件路径补全**。输入 `@` 前缀可筛选项目根目录下的相对路径；选择后只插入相对路径文本，不读取文件内容；逃出项目根目录的路径不可选。（证据：`TestPathCompletionStaysWithinRoot`、`TestTabCompletionInsertsOnlyCommandOrRelativePath`；实现只枚举路径，不打开文件。）
- [x] **AC7 / F7：弹层布局承载**。候选/弹层内容能在窄宽度中裁剪并保持可见区域；无处理器的权限、计划和提问操作没有菜单项。（证据：`TestCompletionOverlayClipsToAvailableArea`；`builtinCommands` 仅登记 `/sessions`、`/goals`。）
- [x] **AC8 / N1–N3：Linux 尺寸适配**。Linux 终端 resize 后 transcript/composer 尺寸更新；紧凑尺寸仍可导航和输入，低于最小尺寸时显示放大提示。（证据：`TestComputeLayout`、`TestSessionNavigationAndResizeStayAvailable`、`TestModelResizeAndServiceErrorKeepDraftAndExposeError`。）
- [x] **AC9 / N4：无颜色模式**。关闭颜色时，消息、焦点、加载和错误仍可辨认，Markdown 不输出 ANSI 颜色序列。（证据：`TestRenderMarkdownStructureAndNoColor`；主 transcript 使用 Glamour `notty` 样式，状态使用文字标识。）
- [x] **AC9 / N5–N6：操作与状态可信**。提示的快捷键均能触发对应动作；未实现命令、权限操作、流式工具事件不显示成已可用功能。（证据：`TestTabCompletionInsertsOnlyCommandOrRelativePath`、`TestSlashCommandsOnlySwitchLocalNavigation`、`TestStatusPhasesAndActionsAreVisible`；无权限/计划/提问命令注册。）
- [x] **AC10：范围边界**。M01 不增加 agent 流、模型后端、权限判定、工具循环、目标执行或存储 schema。（证据：实现只改 `internal/tui`、`go.mod`、`go.sum`；`internal/conversation`、`internal/core`、持久化代码无 diff。）

## 集成与回归

- [x] 所有 TUI 组件单元测试通过。（证据：`GOCACHE=/tmp/stable-gocache go test ./internal/tui` → `ok stable/internal/tui`。）
- [x] 项目构建与已有包测试通过。（证据：`GOCACHE=/tmp/stable-gocache go test ./...`；所有包通过，runtime 包无测试文件。）
- [x] 既有 session list/load、chat、goal 数据视图行为保留，socket 操作名与服务协议未变化。（证据：session list/load 根模型测试通过；选会话仍发出 `session_load`；`internal/conversation`、`internal/core` 无 diff。）
- [x] 依赖版本固定且工作区格式干净。（证据：`go list -m` 返回 Glamour `v1.0.0`；`go mod verify` 为 `all modules verified`；`git diff --check` 通过。）

## 端到端场景

- [x] **打开会话、编辑、切换目标并返回**：Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771) 的 `M01 production TTY acceptance` 通过 Python PTY 启动正式 `stable` 无参数入口，以隔离 runtime 和 loopback fake provider 完成 `/sess` Tab 补全、会话加载、多行输入/回复及 Ctrl+G 目标导航往返。
- [x] **长回复、resize 与错误提示**：同一 PTY 验收以 360 条 fake-provider 输出验证超视口 Page Up/Page Down、终端 resize 和受控 provider 错误状态；Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771) 通过。
- [x] **补全不越界且不冒充功能**：同一 PTY 验收覆盖命令补全、runtime trusted project root 内文件补全和指向其外的 symlink 不进入候选；Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771) 通过。

## 验收记录

T1–T22 已实现并通过既有 TUI 与全仓测试。自动化组件/模型验收 20 项已勾选；三个真实终端场景现有 PTY/fake-provider 自动验收脚本并接入 Go workflow，但此提交尚未执行该云端 workflow，因此仍保留未勾选。M01 交付不包含后续 agent 流、权限或工具循环。

## Linux 收尾复核（M11）

- [x] 真实 Linux PTY 的三个端到端场景由 `bash tests/e2e/m01_tty.sh` 自动执行并记录可观察输出；脚本使用隔离 HOME/state、loopback fake provider 和正式 `stable` 无参数入口。验证：Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771)，job `build-and-test` 通过；组件测试作为补充证据。
