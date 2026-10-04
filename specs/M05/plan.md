# M05 会话与上下文 Plan（草案）

> 状态：已批准（2026-10-04）。依据已批准的 [spec.md](spec.md)；用户于 2026-10-04 批准本方案。

## 架构概览

M05 沿用 append-only session log 作为会话事实来源。恢复、搜索、UI 展示和 agent 上下文都由同一套事件投影构造，避免“屏幕上看见的历史”和模型拿到的历史分叉。输入历史单独保存在项目本地状态目录，不并入 transcript。

Context 项目负责估算上下文大小、决定何时压缩、调用现有模型提供方生成摘要，并把压缩边界发回 agent 流。agent Runner 在每次模型请求前调用这一能力；conversation service 按流顺序将边界持久化到 session log。边界记录摘要、所属 session 或 run 以及被压缩的序号范围。近期原文仍留在原日志位置，投影时按范围排除已摘要事件并保留截止点之后的完整尾部。原始事件不删除；一次边界追加即可提交压缩，不存在摘要和尾部分多次写入的半完成状态。

候选快照在受信执行器的工具边界创建，并由 candidate 模块保存到项目的 .stable 服务目录。快照记录绑定项目、会话、候选、运行和完整 manifest digest。rewind 请求由 conversation service 验证归属与生命周期；只有没有活动写入者、候选尚未接收且当前 digest 可核对时，candidate 模块才能通过 staging 目录和原子目录交换恢复快照。SQLite rewind journal 负责崩溃恢复，正式工程目录不参与 rewind。

现有 /say 持续使用 Stable 的持久消息和事件唤醒链路，按消息 ID 在目标下一决策轮消费。/reply 增加待答问题 ID 关联；M05 建立事件和协议契约，M06 的提问入口负责产生待答问题。没有有效待答问题时由受信 service 拒绝答复。

## 核心数据结构与接口

### 压缩边界

Boundary 保存摘要、scope、可选 RunID、被摘要覆盖的起止 session 序号或 run 序号，以及创建时间。压缩边界作为单个 EventBoundary 追加；验证序号范围、summary 非空、范围属于该 session/run，且截止点不切断工具调用/结果配对。历史投影使用最近有效边界：只用摘要代表其覆盖范围，保留截止点之后仍在原日志中的事件。旧版 Boundary 缺少 scope 时按 session 级边界解释。

### ContextManager

Runner 调用 ContextManager.Prepare(ctx, request, messages)，获得可发送给 provider 的消息及可选 ContextBoundary。ContextManager 依赖模型上下文窗口配置、保留尾部策略和摘要生成器。若摘要失败、边界校验失败或写入失败，本轮返回明确的 context preparation error，不发送未经压缩的超限请求，也不报告压缩成功。

ContextBoundary 是 agent event 的一种持久事件，包含 RunID、被摘要覆盖的 run 序号范围及 summary。Runner 只发布边界事件；conversation 的单一事件消费者负责按流顺序将其变为带 run scope 的 EventBoundary，防止 Runner 与 run event consumer 并发追加日志。重连订阅仍能从 session cursor 看到压缩状态。

### SessionProjection 与搜索

SessionProjection 按稳定序号生成 transcript 项和 agent 消息，包含用户/assistant 文本、已配对工具调用与结果、目标消息、运行终态及压缩边界。工具调用尚无结果时不能进入可恢复的普通对话尾部；损坏日志保持现有 fail-closed 行为。会话搜索以 session log 文件为准逐条扫描，返回 session 元数据和有界匹配片段；首版不引入二级全文索引，避免索引与 append-only 日志不同步。

### InputHistory

InputHistoryEntry 保存输入文本和时间戳。InputHistoryStore 提供 Append、List 和有界浏览游标；存储在项目 .stable/input-history.jsonl，目录和文件权限与 session log 一致。连续重复项只保存一次，最多保留 200 条。输入历史不参与 ContextProjection。

### FileSnapshot 与 SnapshotStore

FileSnapshot 保存 SnapshotID、ProjectRootDigest、SessionID、CandidateID、RunID、CreatedAt、Label 及候选 manifest。SnapshotStore 提供 Create、List、ValidateRestore 和 Restore。文件内容用 SHA-256 内容寻址存入 .stable/candidate-snapshots；manifest 对每条路径记载存在状态、模式、大小和 digest。写入 blob 使用临时文件、fsync 和原子 rename；超出快照配额时在工具结果报告失败，不能伪报 checkpoint 成功。

Restore 接受 expected candidate digest 和 snapshot ID。candidate 模块在非活动候选上重建 staging 目录，校验结果 manifest 后通过同文件系统的 rename exchange 交换候选目录。SQLite rewind journal 记录 prepared、swapped、finalized 阶段；启动恢复按阶段核对 digest。接收记录、接受处理中 journal 或项目正式目录永不作为 Restore 目标。

### PendingQuestion 与 Reply

PendingQuestion 保存 QuestionID、WorkRef、SessionID、提示文本、创建运行和 pending/replied 状态。session log 持久记录提问和答复事件；reply 必须携带 QuestionID，QuestionStore 在同一受信更新中确认问题仍 pending 并将其置为 replied。M05 提供数据与 service 校验，不添加 M06 的提问界面。

## 模块设计

### internal/sessionlog

保留现有 JSONL 信封、序号、文件权限及旧记录读取。扩展 Boundary 保留尾部事件，新增 snapshot、rewind、question、reply 事件校验；增加统一投影和流式搜索。原始事件只追加不删除。

### internal/sessioncontext 与 internal/prompt

sessioncontext 估算 agent request 的消息大小，调用摘要器，并产出有界 ContextBoundary；prompt 复用其摘要提示与保留尾部策略。模型配置增加 context_window_tokens；未设置时使用保守默认值，明确留出 max output 与 safety margin。普通 chat 和 agent run 使用同一压缩边界投影。

### internal/agent

RunnerOptions 注入 ContextManager。Runner 在每次 provider 请求前检查上下文，必要时发布压缩边界事件，再发送投影后的消息。已有 tool call/result 往返作为不可拆分组；Runner 的预算、取消、重试和终态语义保持不变。

### internal/inputhistory

封装项目级历史文件的安全创建、追加、去重、裁剪、加载与私有权限；TUI 不直接读写文件。

### internal/candidate 与 internal/store

candidate/snapshot.go 封装受信 manifest、内容寻址快照、staging restore 和原子交换。store 增加候选 digest 更新、review 失效和 rewind journal；数据库迁移新增 journal 表。rewind 完成后候选回到 ready，旧 review 不能继续接受；接受流程遇到活动或未完成 rewind journal 时拒绝并等待恢复。

### internal/conversation

增加 session_search、snapshot_list、snapshot_rewind 和 question/reply 的 protocol 字段与操作。service 检查 SessionID、项目根、候选 owner、目标工作项、候选状态、activeRuns、expected digest 与 pending question。工具执行器在写/编辑/命令返回后通过 SnapshotStore 创建检查点；checkpoint 错误进入可观察的 tool result。conversation 是 event log 的唯一追加者。

### internal/tui

Session picker 接入搜索，composer 支持最近输入历史导航；transcript 展示压缩边界、检查点和 rewind 结果。候选 review 页面列出该会话候选的快照并提供 rewind 确认。活动运行期间 rewind 入口显示不可用原因。普通 /say 与 /reply 按已批准语义提交并显示排队或拒绝状态。

## 模块交互

1. 应用启动后，sessionlog 列表服务逐项目读取有效 session 元数据；用户输入查询后由流式搜索返回有界结果，选择一项后用统一投影恢复 transcript 和 agent context。
2. 用户提交 composer 文本时，TUI 同步追加 InputHistory，再发起 chat/say/reply。失败要显示历史已保存或未保存的明确状态；历史不会伪装成 transcript。
3. Runner 每轮 provider 请求前调用 ContextManager；需要压缩时先生成完整 summary、确定不拆分工具往返的 cutoff，再发布一个边界事件。conversation 将其顺序持久化后向订阅端广播；之后的 model request 使用相同 scope/range 投影恢复摘要加尾部。
4. ToolExecutor 在候选写入前确认候选运行归属，并在写/编辑/命令前建立可恢复的前态 checkpoint；操作成功后对比 manifest 并建立后态 checkpoint。前态或后态 checkpoint 失败时，停止后续候选写入并将候选转为 blocked，不能接受没有可验证快照的改动。conversation 把 snapshot metadata event 追加到对应 session。只读工具及无变化命令不创建新快照。
5. 用户请求 rewind 时，service 验证候选没有 active run、不是 accepted 且没有 acceptance apply 在途，记录 journal 后调用 SnapshotStore restore。重新计算 digest、更新候选版本并清除旧 review，追加 rewind event，最后广播新候选摘要和 receipt。
6. 目标的 /say 复用现有持久消息和 workflow wake event；workflow 在后续 decision round 按 message ID 消费。/reply 先定位相同 WorkRef 下的 pending QuestionID，原子记录答复后唤醒对应 agent，防止重复答复。

## 文件组织

- 修改：internal/sessionlog/events.go、log.go、sessions.go；新增 projection.go、search.go
- 新增：internal/sessioncontext/context.go、compact.go、projection.go 及定向测试
- 修改：internal/agent/runner.go、events.go；增加 ContextManager 与 boundary event
- 修改：internal/appconfig/config.go、配置校验和用户文档
- 新增：internal/inputhistory/store.go 及测试
- 新增：internal/candidate/snapshot.go 及测试
- 修改：internal/store/schema.sql、migrations、candidate.go 与候选状态迁移测试
- 修改：internal/conversation/protocol.go、session.go、run.go、service.go；新增 snapshots.go、questions.go 和集成测试
- 修改：internal/tui/composer.go、model.go、navigation.go、transcript.go 及相关测试
- 新增：tests/e2e/m05_sessions.sh，覆盖重启恢复、压缩边界、候选 rewind 和目标消息队列

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| transcript 与 context | 一个 append-only session log，加统一投影与压缩边界 | 维持单一事实来源，完整审计可读，恢复一致 |
| 压缩写入 | 单个边界 event 保存 summary 和其覆盖的 session/run 序号范围 | 原始尾部留在日志原位；一次追加可提交压缩，崩溃不会造成重复或丢尾 |
| 输入历史 | 独立的 200 条项目级私有 JSONL | 不混入模型语境；符合源端使用习惯，迁移简单 |
| 会话搜索 | 首版流式扫描 session log，无独立索引 | 避免索引事务和日志写入不一致；规模不足前保持简单 |
| snapshot 保存 | digest manifest + 内容寻址 blob | 同内容复用，能够校验快照完整性，避免重复复制未变化文件 |
| rewind | 仅 ready、无活动写入的候选；staging + rename exchange + SQLite journal | 不触碰正式目录；进程中断后可对账恢复。任何无法建立前态/后态快照的变更将阻止候选接收 |
| 上下文阈值 | 可配置窗口长度，保守缺省值并预留输出空间 | 兼容不同模型与本地兼容 provider，避免将 MaxTokens 误作上下文窗口 |
| /reply | 显式 QuestionID 和单次状态迁移 | 确保答复关联到实际待答项且不会重复消费 |

## 已批准的实现默认值

- `context_window_tokens` 缺省为 8192，达到 80% 时启动压缩；预留 20% 输出空间与 10% 安全余量。provider 明确给出更大窗口时可由配置覆盖。
- 快照默认每项目最多 1 GiB、每候选最多 50 个 manifest。达到任一限制时，在工具变更前拒绝操作并说明原因；同 digest 内容寻址复用不重复计入 blob 配额。配额可由配置调整。
- question event 稳定字段为 `QuestionID`、`WorkRef`、`SessionID`、`RunID`、`Prompt`、`CreatedAt`、`Status`；reply 保存 `QuestionID`、`ReplyText`、`RepliedAt`。字段不绑定 UI，供 M06 提问入口复用。
