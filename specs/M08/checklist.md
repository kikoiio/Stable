# M08 记忆与指令发现 Checklist

> 状态：Linux 功能与集成验收完成（2026-10-10）。原验收完成于 2026-10-07；追加真实 goal work item 与持久性端到端场景后，AC7、AC11、AC12 已由最新 SHA 的云端 Go 全量测试及 E2E core 复验。未在开发机运行测试/build。

## 功能行为

- [x] **AC1 / F1：指令分层和优先级** — 在临时用户配置、项目根、嵌套目录和工作目录分别创建指令文件，启动 run 后观察上下文按规定顺序包含各文件，较具体文件覆盖优先级更高；修改文件后下一次 run 读到新内容；非 Git 目录以工作目录为项目根。（验证：`go test ./internal/memory/ -run 'Instruction' -count=1`，再运行临时项目场景并检查捕获的模型输入。）
- [x] **AC2 / F2：include 解析与边界** — 分别检查 `@./`、`@../`、`@~/` 和允许根内的绝对路径可展开；代码围栏不展开；重复、循环、深度超过 5、越界和 symlink 逃逸均不读取目标文件，原行保留且 issue 可观察。（验证：`go test ./internal/memory/ -run 'Include' -count=1`，断言模型输入与 issue。）
- [x] **AC3 / F3：双范围存储和索引** — 保存四种类型后，观察 user/feedback 在用户记忆根、project/reference 在项目记忆根；`MEMORY.md` 更新，list 返回正确名称、类型、描述和 scope；项目记忆未被自动暂存或提交。（验证：`go test ./internal/memory/ -run 'Store|Index' -count=1`，在临时仓库检查文件和 Git index。）
- [x] **AC4 / F4–F5：每次运行加载索引并召回** — 普通 run 和 goal run 的模型请求均包含两级受限索引；桩 selector 最多选 5 条，只有被选中文件正文进入上下文，并附 scope、更新时间及超过一天的过期提示；selector 失败时 run 继续且不注入记忆。（验证：`go test ./internal/memory/ ./internal/conversation/ -run 'PrepareRun|Selector|Memory' -count=1`，检查捕获的请求。）
- [x] **AC5 / F6、F9：agent 读写及管理命令** — agent 可在两个 scope 读取、保存、更新、删除条目；尝试写项目其他文件仍被拒绝。`/memory list`、指定 scope 删除、默认项目清理、显式 user/all 清理均显示正确结果并重建索引。（验证：`go test ./internal/execution/ ./internal/conversation/ ./internal/tui/ -run 'Memory' -count=1`，核对文件树、索引和 TUI 结果。）
- [x] **AC6 / F7：普通 run 后提取** — run 终态返回给用户后，后台提取能够保存符合类别的偏好或项目上下文；agent 本轮已成功保存时 extractor 跳过；后台失败显示失败状态且不阻塞已完成 run。（验证：`go test ./internal/memory/ ./internal/conversation/ -run 'CompleteRun|Extract|Memory' -count=1`，用阻塞桩验证 run 已返回。）
- [x] **AC7 / F7：goal 事实隔离** — goal run 的后台输入不含 goal intent、Stable 目标事件、assistant 执行输出、工具结果和证据；提供含目标标准、进度、证据或结果的交互时，只产生允许的偏好、项目背景或参考类别记忆，目标持久状态保持一致。（验证：最新 SHA `b61226f` 的 [Go run 38038695866](https://github.com/kikoiio/Stable/actions/runs/38038695866) `build-and-test` 全量 `go test ./...` 通过；`TestM08GoalWorkItemMemoryIsolation` 通过。）
- [x] **AC8 / F8：整理门槛和互斥** — 小于 24 小时或少于 5 个不同活动 session 时整理不运行；两个条件满足后才运行；并发触发最多一个整理任务，成功/跳过/失败状态可观察。（验证：`go test ./internal/memory/ -run 'Consolidat|Manager' -count=1`，使用可控时钟与并发调用并检查调用计数和状态。）
- [x] **AC9 / N1–N2：安全和信息使用边界** — traversal、越界 include、symlink 逃逸、非普通文件及伪造 scope/filename 写入均被拒绝；用户记忆目录和新文件仅当前用户可访问；模型仅接收本次请求/允许的记忆输入，不获得任意文件读取接口；目标与验收事实没有因记忆变更而改变。（验证：`go test ./internal/platform/secfile/ ./internal/memory/ ./internal/execution/ -run 'Root|Path|Memory|Scope' -count=1`，在临时目录检查权限、模型请求和目标记录。）
- [x] **AC10 / N3–N4、N6：资源上限与失败隔离** — 超限指令/记忆文件、每级最多 200 个条目、30 行 frontmatter、每份索引 200 行/25 KB、1 MiB 单文件和最多 5 条召回均按规定截断或跳过并给出提示；损坏文件、无效 JSON 和后台故障不使普通或 goal run 失败。（验证：`go test ./internal/memory/ ./internal/conversation/ -run 'Limit|Truncat|Failure|Memory' -count=1`，观察 issue 与 run 结果。）
- [x] **AC11 / N7：记忆持久性** — 压缩、会话恢复与候选回滚后，未显式删除的记忆仍可读和召回；显式清理记忆不会更改 session log 中的目标事实。（`TestM08MemorySurvivesCompactionRestoreAndCandidateRewind`：真实 compaction boundary、conversation service + memory manager 重启及 transcript 恢复、真实 `snapshot_rewind`；Go run 38038695866 的全量 Go 测试通过。）
- [x] **AC12：完整端到端流程** — 在嵌套临时项目中依次运行 agent，观察用户/项目指令加载、记忆召回、run 后提取及下一会话恢复；再运行 goal work，确认相关记忆可读且目标标准、证据和验收状态未变化。（`TestM08GoalWorkItemMemoryIsolation` 经真实 `conversation.OpenRun` WorkSession 与 `GoalSocketClient.RunGoal` 验收指令加载、记忆上下文、run 后提取、goal_reply 过滤及目标事实隔离；Go run 38038695866 的全量 Go 测试、E2E run 38038701842 的 `e2e-core` 均通过。）

## 集成检查

- [x] conversation 普通 run 与 goal run 经过同一 memory manager 接入，且注入前缀未保存为聊天消息。（验证：`go test ./internal/conversation/ -run 'Run|Memory' -count=1`，检查 session log 不含 memory context 正文。）
- [x] `memory_list/read/save/delete` 由专用 host 分支调用已绑定 provider；普通 `write_file`/`edit_file` 仍遵守候选写入边界。（验证：`go test ./internal/execution/ -run 'Memory|Write|Edit' -count=1`。）
- [x] `memory_action` 与 `memory_background` 事件可追加、重放和投影；事件 payload 不包含记忆正文或模型原始响应。（验证：`go test ./internal/sessionlog/ -run 'Memory' -count=1`，检查序列化日志。）
- [x] 用户记忆根由 `UserMemoryDir()` 解析；chatserve 注入同一已配置模型，服务退出时回收本服务启动的后台工作。（验证：`go test ./internal/appconfig/ ./cmd/stable/ -run 'UserMemoryDir|Chatserve|Memory' -count=1`。）
- [x] TUI 命令、conversation 协议及服务端操作范围一致；无参数 clear 不会清除用户级记忆。（验证：`go test ./internal/tui/ ./internal/conversation/ -run 'Memory' -count=1`，检查 user memory 文件仍存在。）
- [x] secfile 的 Linux、Darwin、Windows 实现均构建；无法保证安全的平台明确返回 `ErrUnsupported`，不回退为普通路径写入。（验证：运行当前平台 `go test ./internal/platform/secfile/ -count=1`，并执行 task 中的 Darwin/Windows `go test -c` 交叉编译。）

## 编译与测试

- [x] M08 相关包的定向测试通过。（验证：按 task T20 运行 appconfig、secfile、memory、sessionlog、execution、conversation、tui、chatserve 和 e2e 定向测试，记录各命令退出码。）
- [x] 全仓 Go 测试和静态检查通过。（验证：`go test ./...`、`go vet ./...`、`git diff --check` 均退出码为 0。）

## 端到端边界场景

- [x] selector 或后台模型返回错误/非法 JSON 时，当前 run 已完成，未出现虚假保存或整理成功提示，游标可按规则重试。（验证：端到端测试注入失败模型并检查返回消息、事件和游标。）
- [x] 恢复运行时更换工作目录或项目根，memory manager 仍使用服务绑定的规范根；工具参数无法切换到其他项目或用户目录。（验证：端到端场景传入伪造根路径并确认只触及绑定项目。）
- [x] 项目记忆默认仍留在项目目录；完成创建、更新和清理后 `git status` 只反映记忆文件本身，没有自动 add/commit。（验证：临时 Git 仓库执行流程并检查 Git index 与工作树。）
