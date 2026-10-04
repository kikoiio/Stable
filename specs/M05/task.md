# M05 会话与上下文 Tasks

> 状态：已批准（2026-10-04）。依据已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。文件路径相对仓库根目录。

## 范围和依赖

- 前置：M03 隔离与受信接收、M04 工具执行协议；实现以本工作树的已批准 M04 基线为准。
- M05 的日志、输入历史、上下文压缩、候选快照/rewind 和消息协议各有独立接口；M06 可复用 PendingQuestion/Reply 数据协议，但 M05 不交付通用提问 UI。
- 修改 `internal/conversation`、`internal/sessionlog`、`internal/agent` 的共享协议时，由单一负责人集成；先通过接口与 fixture 并行开发。
- 运行真实沙箱、e2e 和全量 Go 检查前，按 AGENTS.md 检查 `MemAvailable`、后续 `vmstat` 采样与 memory PSI；错开并发重型操作。
- 所有新持久化内容（输入历史、摘要、快照元数据、恢复上下文）沿用既有凭据脱敏约束；脱敏失败时拒绝写入，不保存明文。

## 文件清单

| 操作 | 文件 | 职责 |
| --- | --- | --- |
| 修改/新增 | `internal/sessionlog/events.go`、`log.go`、`sessions.go`、`projection.go`、`search.go` | 边界、快照、rewind、question/reply 事件，统一回放投影和有界搜索 |
| 新增 | `internal/sessioncontext/context.go`、`compact.go`、`projection.go` | token 估算、压缩触发、完整工具往返保护、压缩后上下文投影 |
| 修改 | `internal/agent/runner.go`、`events.go` | provider 请求前准备上下文并发布持久边界事件 |
| 修改 | `internal/prompt/`、`internal/appconfig/config.go` | 摘要提示、上下文窗口与快照配额配置 |
| 新增 | `internal/inputhistory/store.go` | 私有、有界、去重、脱敏的输入历史 |
| 新增/修改 | `internal/candidate/snapshot.go`、`internal/store/schema.sql`、migrations、候选 store | 内容寻址快照、候选 rewind 和崩溃恢复 journal |
| 修改 | `internal/conversation/protocol.go`、`session.go`、`run.go`、`service.go`；新增 `snapshots.go`、`questions.go` | 搜索、快照、rewind、say/reply 操作及唯一日志写入者 |
| 修改 | `internal/tui/composer.go`、`model.go`、`navigation.go`、`transcript.go` | 会话搜索/恢复、输入历史、边界与快照呈现、say/reply 状态 |
| 新增 | 各模块定向测试、`tests/e2e/m05_sessions.sh`、Makefile 入口 | 故障恢复和完整用户流程验证 |

---

## T1：SessionLog 事件与边界校验

**文件：** `internal/sessionlog/events.go`、`log.go`、相关测试

**依赖：** 无

**步骤：**
1. 扩展压缩边界事件，记录 scope、run/session 序号范围和摘要；旧版缺少 scope 的边界仍按 session scope 读取。
2. 增加 snapshot、rewind、pending question、reply 事件的字段和归属校验；question 稳定字段为 `QuestionID`、`WorkRef`、`SessionID`、`RunID`、`Prompt`、`CreatedAt`、`Status`，reply 保存 `QuestionID`、`ReplyText`、`RepliedAt`。
3. 校验边界范围属于当前 session/run，且不切断工具调用与结果配对。
4. 保留 JSONL 序号、校验和、权限及 append-only 特性。

**验证：** `go test ./internal/sessionlog/...`；覆盖非法范围、旧边界回放、坏日志 fail-closed。

## T2：统一会话投影与搜索

**文件：** `internal/sessionlog/projection.go`、`search.go`、`sessions.go`

**依赖：** T1

**步骤：**
1. 按日志序号生成一致的 transcript 与 agent 消息投影。
2. 边界摘要替代其覆盖范围，尾部原文与完整工具往返按序保留；原始事件仍可审计。
3. 提供逐会话流式文本搜索（无二级索引），返回有界片段、会话元数据与明确损坏错误。
4. 禁止未完成工具调用作为可恢复普通对话尾部。

**验证：** sessionlog/conversation 定向测试；同一日志的 UI 与 agent projection 对比序列完全匹配。

## T3：项目输入历史

**文件：** 新增 `internal/inputhistory/store.go` 与测试

**依赖：** 无

**步骤：**
1. 安全创建 `.stable/input-history.jsonl`，继承 session log 的目录/文件私有权限。
2. 实现追加、倒序浏览和游标；忽略空白输入，连续相同项去重，最多 200 项。
3. 追加前应用凭据脱敏；无法安全脱敏时拒绝写入并返回可见错误，不保存明文凭据。
4. 并发追加与文件截断保持有效 JSONL；历史不进入 transcript/context projection。

**验证：** `go test ./internal/inputhistory/...`；权限、重启、裁剪、重复边界与脱敏用例测试。

## T4：上下文预算与摘要器

**文件：** 新增 `internal/sessioncontext/`；修改 `internal/prompt/`、`internal/appconfig/config.go`

**依赖：** T1、T2

**步骤：**
1. 配置默认窗口 8192 tokens，80% 触发压缩，保留 20% 输出与 10% 安全余量；支持显式覆盖与非法配置回退，回退有日志可观察。
2. 对较早的已完成消息摘要，保护近期尾部和完整工具调用/结果组。
3. 摘要失败、边界无效或持久化失败时返回可见错误，不发出未经处理的超预算请求。
4. 以确定的 session/run 序号范围生成单个 boundary event；不删除或搬移原始日志事件。
5. 普通 chat 与 agent run 使用同一压缩边界投影；压缩输入设大小上限，仅基于 session log 事件构造，不无界读取工程文件。

**验证：** `go test ./internal/sessioncontext/... ./internal/prompt/... ./internal/appconfig/...`；覆盖阈值、工具配对、摘要失败、边界写入失败与压缩输入上限。

## T5：Runner 压缩事件接入

**文件：** `internal/agent/runner.go`、`events.go`、runner 测试

**依赖：** T4

**步骤：**
1. 注入 ContextManager，在每次 provider 请求前生成投影消息。
2. 需要压缩时发布顺序化 boundary event，再发送摘要加尾部消息。
3. conversation 的单一流消费者持久化 boundary；重连游标可读取该事件。
4. 保留现有预算、取消、重试与终态语义。

**验证：** `go test ./internal/agent/...`；fake provider 验证压缩后的请求和事件顺序。

## T6：候选快照存储

**文件：** 新增 `internal/candidate/snapshot.go` 与测试，修改配置

**依赖：** M04 候选工具执行器接口

**步骤：**
1. 生成绑定项目、session、candidate、run 的 manifest，记录路径存在状态、模式、大小及 SHA-256；元数据写入前应用凭据脱敏。
2. 在 `.stable/candidate-snapshots` 写内容寻址 blobs；临时文件、fsync、原子 rename；拒绝路径穿越及符号链接逃逸。
3. 默认上限为每项目 1 GiB、每候选 50 个 manifest，配额可配置；超限在工具变更之前失败，同 digest blob 不重复计费。
4. 提供 Create/List/ValidateRestore/Restore，复原前校验完整快照。

**验证：** `go test ./internal/candidate/...`；覆盖新增、修改、删除、损坏 blob、配额、越界路径与重启可见性。

## T7：快照钩入工具写入边界

**文件：** `internal/execution/tool_executor.go`、`internal/candidate/` 及集成测试

**依赖：** T6

**步骤：**
1. 所有写、编辑和可能改文件的命令执行前创建前态快照，成功后对比 manifest 并创建后态快照。
2. 只读操作和确认无变化的命令不新建快照。
3. 任一必要快照失败时将候选阻断，禁止后续写入和接收，并返回可观察工具结果，不伪报 checkpoint 成功。
4. 快照 metadata event 由 conversation 追加到对应 session，事件绑定运行和候选，不泄漏正式工作树内容。

**验证：** execution/candidate 集成测试；注入前态及后态快照失败并确认候选不可写、不可接收。

## T8：Rewind journal 与候选恢复

**文件：** store schema/migrations、candidate store、`internal/candidate/snapshot.go`

**依赖：** T6

**步骤：**
1. 添加 prepared、swapped、finalized rewind journal 和候选版本/digest 更新。
2. 只对 ready 且未接收候选使用 staging 重建及同文件系统原子目录交换。
3. 每阶段写入前后校验 manifest；旧 review 失效，候选回到 ready。
4. 启动时按 journal 阶段和 digest 恢复或拒绝；活动或未完成 acceptance journal 时拒绝 rewind/accept。
5. 正式工程目录永不成为恢复目标。

**验证：** store/candidate 定向测试；在每个 journal 阶段注入中断后重启恢复，并对比正式工程完整 digest。

## T9：Conversation 服务协议

**文件：** conversation protocol、session/run/service；新增 `snapshots.go`、`questions.go`

**依赖：** T1、T2、T5、T7、T8

**步骤：**
1. 增加 session search、snapshot list/rewind、question/reply 请求和事件。
2. service 验证项目、session、candidate、run、目标归属、生命周期、activeRuns、expected digest 和待答问题状态。
3. conversation service 保持 session log 的唯一追加权，按 agent stream 顺序持久化边界和候选事件。
4. `/say` 使用既有持久消息，下一决策轮按 ID 顺序消费；`/reply` 原子地回答同一 WorkRef 下的 pending QuestionID。
5. 重复/不存在/已完成的问题回复返回明确拒绝，不唤醒错误工作项。

**验证：** `go test ./internal/conversation/...`；服务层测试覆盖重启重放、重复提交、归属越权及事件顺序。

## T10：TUI 搜索、输入历史和恢复显示

**文件：** composer、model、navigation、transcript 与测试

**依赖：** T2、T3、T9

**步骤：**
1. 会话选择器按最近活动列出并可搜索标题、首条用户消息和内容。
2. composer 支持最多 200 条历史的前后导航，不将选择操作提交为新消息；提交时同步追加历史，失败时显示历史已保存/未保存的明确状态。
3. 展示压缩边界、工具配对、快照和 rewind 的待处理/成功/失败状态及可理解的错误。
4. 搜索结果选择后恢复的 transcript 与 agent context 一致。

**验证：** `go test ./internal/tui/...` 与 TUI model 测试；覆盖空结果、长片段、输入导航、历史保存失败提示和恢复错误。

## T11：快照 review、rewind 与消息交互

**文件：** TUI review/transcript/composer 相关实现和测试

**依赖：** T9、T10

**步骤：**
1. 候选 review 展示同一 session/candidate 的快照及时间、运行、标签和 manifest 摘要。
2. rewind 请求须确认目标快照；活动运行时禁用并说明原因，完成时显示 receipt。
3. `/say` 展示 queued/consumed 状态；`/reply` 只在存在明确 pending 问题时启用，无可答问题时明确提示。

**验证：** TUI model 测试和服务集成检查；UI 状态与持久事件/receipt 一致。

## T12：端到端恢复和目标事实边界

**文件：** 新增 `tests/e2e/m05_sessions.sh`、Makefile 入口

**依赖：** T1–T11

**步骤：**
1. 演示多会话创建、搜索、重启及 transcript/context 一致恢复。
2. 演示超阈值压缩、持久边界与原事件审计。
3. 演示候选多次变更、快照、rewind、重启恢复，确认正式工程不变。
4. 演示目标活动期间 `/say` 下一轮消费与 `/reply` 精确答复；目标证据/verified 状态不被这些事件改变。
5. 提供可重复的定向 e2e 命令及清晰失败信息。

**验证：** `bash tests/e2e/m05_sessions.sh` 在可运行所需沙箱能力的 Linux 环境退出 0。

## T13：构建、回归和交付记录

**依赖：** T1–T12

**步骤：**
1. 按 checklist 执行定向包检查、全量 build/vet/test 与既有 M00–M04 场景。
2. 在每次提交的独立 worktree 运行 `go build -p 1 -buildvcs=false ./...`，记录提交 SHA 与退出码。
3. 补齐 checklist 的命令、环境、结果和证据位置；未通过项保持未勾选并说明限制。

**资源安排：** 启动全量 Go 检查、e2e 或沙箱前遵守 AGENTS.md 内存采样要求；不并发运行多个重型批次。
