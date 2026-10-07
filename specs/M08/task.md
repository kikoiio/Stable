# M08 记忆与指令发现 Tasks

> 状态：全部任务完成（2026-10-07）。任务验证、全仓测试与静态检查通过；逐项验收记录见 [checklist.md](checklist.md)。
> 执行约定：每个任务独立验证后再标记完成；共享文件按下方依赖串行修改。启动重型构建/全量测试前检查 `free -h`、`vmstat 1 5`、`cat /proc/pressure/memory`，由主 agent 协调并发；不终止其他任务进程。云端运行需先说明服务、用途和权限并取得许可。
> 实施偏差：存储需要安全枚举记忆目录；plan 漏列了该能力，因此在 `secfile.Root` 增加 no-follow `ReadDir`，并复用同一根身份校验。实现还把 session ID 加入 `PrepareRun` 参数，以读取该 session 的提取游标，并在 worker state 中记录上次整理后的活动 session 集合供 F8 门槛使用；`MemoryActionRecord` 增加可选 `run_id`，用于判断本 run 是否已主动保存；后台事件带 session 归属以便安全写入对应 session log 并实时推送；运行上下文设置 128 KiB 总上限。以上均为已批准安全、审计和资源要求所需数据，不扩大功能范围。

## 文件清单

| 操作 | 文件 | 职责 |
|---|---|---|
| 修改 | `internal/appconfig/config.go`、`config_test.go` | 用户记忆根目录 helper 及路径规则验证 |
| 修改 | `internal/platform/secfile/secfile.go`、`secfile_linux.go`、`secfile_darwin.go`、`secfile_windows.go`、`secfile_other.go` | 根目录约束下安全枚举、建目录、原子写入、删除 API 与平台实现 |
| 新建 | `internal/platform/secfile/root_write_test.go` | Linux 下越界、链接替换、非普通文件和原子写入契约 |
| 修改 | `internal/platform/secfile/secfile_darwin_test.go`、`secfile_windows_test.go` | Darwin/Windows rooted 写入能力验证 |
| 新建 | `internal/memory/types.go`、`store.go`、`instructions.go`、`selector.go`、`processor.go`、`manager.go` | M08 记忆、指令发现、选择、后台处理与生命周期 |
| 新建 | `internal/memory/types_store_test.go`、`instructions_test.go`、`selector_test.go`、`processor_test.go`、`manager_test.go` | memory 包定向行为验证 |
| 修改 | `internal/conversation/service.go`、`run.go`、`protocol.go`；新建 `internal/conversation/memory.go` | 运行上下文、后台完成事件、管理请求和服务绑定 |
| 新建 | `internal/conversation/run_memory_test.go`、`memory_test.go` | 普通/目标运行过滤、上下文注入和管理 op 验证 |
| 修改 | `internal/execution/executor_factory.go`、`tool_executor.go`、`tools_schema.go`；新建 `internal/execution/memory.go` | 受限记忆 host tools 与 manager provider 接线 |
| 新建 | `internal/execution/memory_test.go` | scope、参数边界、工具路由与审计验证 |
| 修改 | `internal/sessionlog/events.go`、`log.go`、`validate.go`、`projection.go` | 记忆操作/后台状态事件与正文隔离 |
| 新建 | `internal/sessionlog/memory_test.go` | 记忆事件校验、持久化和投影验证 |
| 修改 | `internal/tui/model.go`；新建 `internal/tui/m08_memory_test.go` | `/memory` 命令及结果呈现 |
| 修改 | `cmd/stable/chatserve.go` | 记忆 manager、模型和工具依赖组装 |
| 新建 | `tests/e2e/m08_memory_test.go` | 临时用户目录/项目中的端到端场景 |

## T1：添加用户记忆目录解析

**文件：** `internal/appconfig/config.go`、`internal/appconfig/config_test.go`
**依赖：** 无
**步骤：**
1. 新增 `UserMemoryDir() (string, error)`，返回 `platform/paths.UserConfigHome()` 下的 `stable/memory`。
2. 保持 `STABLE_CONFIG` 只影响主配置文件路径；沿用现有 XDG 与 home 解析规则。
3. 添加默认路径、XDG 覆盖和错误传播断言。

**验证：** `go test ./internal/appconfig/ -run 'UserMemoryDir|UserConfigHome' -count=1` 退出码为 0，路径与配置目录规则一致。

## T2：定义 secfile 安全写入契约

**文件：** `internal/platform/secfile/secfile.go`
**依赖：** 无
**步骤：**
1. 在 `Root` 上定义相对路径 `ReadDir`、`MkdirAll`、`WriteFileAtomic`、`RemoveFile` API，拒绝空根、绝对路径、`..` 逃逸和无效权限。
2. 将实际系统调用委托给平台实现；明确枚举不跟随链接、替换目标不跟随符号链接，写入失败不得留下半文件。
3. 保持既有读取、目录交换和私有文件 API 行为不变。

**验证：** `go test ./internal/platform/secfile/ -run 'Root|Write|Remove' -count=1` 退出码为 0；已有 secfile 公开 API 调用仍可编译。

## T3：实现 Linux rooted 原子文件操作

**文件：** `internal/platform/secfile/secfile_linux.go`、`root_write_test.go`
**依赖：** T2
**步骤：**
1. 使用根目录相对 no-follow 操作创建目录、写临时文件并原子替换，以及删除普通文件。
2. 每次提交前重验根身份；拒绝符号链接、非普通目标和根外路径。
3. 覆盖覆盖写、删除、父目录创建、根替换竞态和失败不留临时文件。

**验证：** `go test ./internal/platform/secfile/ -run 'RootWrite|RootRemove|RootMkdir' -count=1` 退出码为 0，危险路径均返回安全错误。

## T4：实现 Darwin rooted 原子文件操作

**文件：** `internal/platform/secfile/secfile_darwin.go`、`secfile_darwin_test.go`
**依赖：** T2
**步骤：**
1. 依据 Darwin 已有的安全打开原语实现根内 mkdir、原子写入和删除。
2. 对无法保证 no-follow/root containment 的操作 fail closed。
3. 在 Darwin 专属测试中验证原子替换和 no-follow 约束，不修改 Linux 共用测试文件。

**验证：** `GOOS=darwin GOARCH=arm64 go test -c -o /tmp/secfile-darwin.test ./internal/platform/secfile/` 编译通过；可用 Darwin 环境时运行 rooted 写入用例。

## T5：实现 Windows 与其他平台安全回退

**文件：** `internal/platform/secfile/secfile_windows.go`、`secfile_other.go`、`secfile_windows_test.go`
**依赖：** T2
**步骤：**
1. Windows 实现遵循现有 root identity 与安全打开约束，创建、替换及删除前检查链接和目标类型。
2. 不能提供等价安全保证的平台返回 `ErrUnsupported`，不得静默退化为不安全的路径拼接写入。
3. 增加平台专属测试或交叉编译覆盖。

**验证：** `GOOS=windows GOARCH=amd64 go test -c -o /tmp/secfile-windows.test.exe ./internal/platform/secfile/` 编译通过；当前平台不支持时契约测试观察到明确的 `ErrUnsupported`。

## T6：定义 memory 类型与路径分类

**文件：** `internal/memory/types.go`、`types_store_test.go`
**依赖：** 无
**步骤：**
1. 定义 plan 中的 scope、type、header、entry、ref、change、run context、completion、worker input/state 类型。
2. 实现并测试 user/feedback 与 project/reference 的 scope 映射，以及拒绝未知类别。
3. 确保模型输入结构不含绝对路径或任意文件访问能力。

**验证：** `go test ./internal/memory/ -run 'MemoryType|MemoryScope|Change' -count=1` 退出码为 0。

## T7：实现受限 memory 存储与索引

**文件：** `internal/memory/store.go`、`types_store_test.go`
**依赖：** T1、T3、T4、T5、T6
**步骤：**
1. 从项目根与用户配置锚点定位 memory 目录；新建用户目录/文件采用当前用户私有权限。
2. 读取和校验 Markdown frontmatter、字段及大小；每范围最多扫描 200 项，frontmatter 最多 30 行，超大条目跳过并记录问题。
3. 通过 secfile 原子写入、删除及重建 `MEMORY.md`；列表仅返回 header，单条正文只能按 scope 与 filename 读取。
4. 用安全 filename 生成逻辑保存条目，拒绝绝对路径、路径穿越、链接逃逸和类型/范围不匹配。

**验证：** `go test ./internal/memory/ -run 'Store|Index|Frontmatter|MemoryPath' -count=1` 退出码为 0；覆盖读写删、索引重建、限额及路径攻击。

## T8：实现分层指令发现与受限 include

**文件：** `internal/memory/instructions.go`、`instructions_test.go`
**依赖：** T6、T7
**步骤：**
1. 按 spec 的用户级、项目根至工作目录和最终本地文件顺序发现指令。
2. 实现 `@./`、`@../`、`@~/`、`@/` include，限定在项目根或用户配置锚点内；忽略代码围栏中的 include。
3. 限制单文件 1 MiB 与嵌套深度 5，规范化路径去重并阻断循环；错误保留原行并生成可观察 issue。

**验证：** `go test ./internal/memory/ -run 'Instruction|Include' -count=1` 退出码为 0；覆盖优先顺序、所有 include 类型、围栏、循环、重复、超深、越界与链接逃逸。

## T9：实现记忆相关性选择

**文件：** `internal/memory/selector.go`、`selector_test.go`
**依赖：** T7、T8
**步骤：**
1. 只把请求文本和两级 metadata 清单交给已配置模型，不在选择调用中提供记忆正文。
2. 解析结构化引用，拒绝候选集合外或非法 scope/filename 的条目，并将结果限制为最多 5 项。
3. 模型失败、格式错误或无候选时返回空召回和可观察状态，不阻断 run。

**验证：** `go test ./internal/memory/ -run 'Selector|Select' -count=1` 退出码为 0；桩模型断言输入中无记忆正文且结果最多 5 项。

## T10：实现提取与整理处理器

**文件：** `internal/memory/processor.go`、`processor_test.go`
**依赖：** T8、T9
**步骤：**
1. 为提取和整理构造单次 JSON 模型请求及严格解码器。
2. 目标工作项请求只允许偏好、反馈、项目背景和参考类别；不得将 goal intent、assistant 输出、工具结果、证据或 Stable 目标事件放入输入。
3. 对 action、scope/type、名称、描述、正文大小、重复项和每批变更数量做 Go 端校验；模型不能提供路径或任意 frontmatter。
4. 不合规或失败的整批变更不写入，返回可审计的错误摘要。

**验证：** `go test ./internal/memory/ -run 'Extract|Consolidat|Processor|ChangeValidation' -count=1` 退出码为 0；非法 JSON、范围错配和超限输出不产生变更。

## T11：实现 manager CRUD、run 准备与后台状态

**文件：** `internal/memory/manager.go`、`manager_test.go`
**依赖：** T10
**步骤：**
1. 实现 `PrepareRun`：发现指令、读取受限双索引、选择并安全读取至多 5 条记忆，附来源/时间/过期提示。
2. 实现 list/read/save/delete/clear，并确保 clear 默认项目范围由调用方明确传入。
3. 实现 StateDir 下每项目游标、成功整理时间及整理互斥；运行完成后异步提取，主动保存成功时跳过重复提取。
4. 仅在距成功整理 ≥24 小时且其后 ≥5 个不同 session 有新活动时整理；提取/整理失败保留游标或成功时间并报告状态。
5. 后台调用有界且不阻塞 run；冲突整理任务跳过，不改写 goal records。

**验证：** `go test ./internal/memory/ -run 'Manager|PrepareRun|CompleteRun|Consolidat' -count=1` 退出码为 0；覆盖游标重试、跳过、成功推进、24 小时/5 session 门槛和并发互斥。

## T12：添加 sessionlog 记忆事件

**文件：** `internal/sessionlog/events.go`、`log.go`、`validate.go`、`projection.go`、`memory_test.go`
**依赖：** 无
**步骤：**
1. 定义 `memory_action` 与 `memory_background` 事件及严格 payload 校验。
2. 事件只存 scope、条目标识、操作、状态、计数、时间和安全错误摘要；不存记忆正文、模型原始响应或运行时注入上下文。
3. 接入 append/replay 与用户可读投影，保持目标事实和原有事件投影不变。

**验证：** `go test ./internal/sessionlog/ -run 'Memory|Projection|Validate' -count=1` 退出码为 0；事件往返一致且正文不会出现在序列化记录中。

## T13：定义 execution provider 与工具 schema

**文件：** 新建 `internal/execution/memory.go`；修改 `tools_schema.go`、`executor_factory.go`
**依赖：** T6、T7
**步骤：**
1. 定义 plan 中的 `MemoryProvider`，仅接收 session id、scope、filename/name、type、description、body。
2. 注册 memory_list/read/save/delete schema；不提供根路径参数，也不扩展普通文件写工具。
3. 将 provider 注入 executor 工厂并保持未配置时的既有构造兼容性。

**验证：** `go test ./internal/execution/ -run 'Memory.*Schema|ExecutorFactory' -count=1` 退出码为 0；schema 不含任意路径字段。

## T14：实现 execution 记忆工具主机分派

**文件：** `internal/execution/tool_executor.go`、`internal/execution/memory_test.go`
**依赖：** T12、T13
**步骤：**
1. 在既有 host tool 分派中实现 list/read/save/delete，调用已绑定 provider。
2. 保持普通 write/edit 候选边界不变；scope/type 不匹配、越权 session 或无效 filename 返回明确错误。
3. 记录工具调用及结果到通用审计路径，并由 provider 回调产生 metadata-only `memory_action` 事件。

**验证：** `go test ./internal/execution/ -run 'Memory' -count=1` 退出码为 0；桩 provider 断言参数、调用结果和失败边界。

## T15：接入 conversation 服务配置与管理协议

**文件：** `internal/conversation/service.go`、`protocol.go`、新建 `memory.go`、`memory_test.go`
**依赖：** T11、T12
**步骤：**
1. 在服务依赖中注入受规范项目根约束的 `memory.Manager`；拒绝客户端提供的任意根目录。
2. 增加 memory list/delete/clear 请求与结果消息校验，清理范围只接受当前项目默认或显式 user/all。
3. 实现 handlers，调用 manager 后写 metadata-only 事件，并返回条目列表、数量或安全错误。
4. 验证删除和 clear 后索引同步更新，goal records 不被修改。

**验证：** `go test ./internal/conversation/ -run 'Memory|Protocol' -count=1` 退出码为 0；非法 op/范围被拒绝且有效操作可往返。

## T16：接入 run 上下文与 completion 触发

**文件：** `internal/conversation/run.go`、`run_memory_test.go`
**依赖：** T11、T15
**步骤：**
1. 在 `startRun` 获取可信 work root 与请求文本，注入不持久化的指令、索引和召回正文前缀。
2. 将 memory 故障作为可观察 issue 处理，不阻断普通或 goal run；不将注入内容写入 session message。
3. 在 run 终态广播后构造带 `ThroughSeq` 的 completion：普通 run 只取新 text；goal run 只取允许的用户 `/say`、`/reply` 文本，不读取目标事件/证据/agent 输出/工具结果。
4. 记录本 run 是否成功主动保存记忆，再异步交给 manager；不改变 goal 状态。

**验证：** `go test ./internal/conversation/ -run 'Memory|Run' -count=1` 退出码为 0；断言上下文临时性、消息过滤、游标和终态触发顺序。

## T17：实现 TUI `/memory` 命令

**文件：** `internal/tui/model.go`、`internal/tui/m08_memory_test.go`
**依赖：** T15
**步骤：**
1. 注册 `/memory list`、`/memory delete <scope> <entry>`、`/memory clear [user|all]`。
2. 无参数 `/memory` 显示用法；clear 默认不传范围并由服务端解释为当前项目。
3. 按既有 request/result 流程呈现名称、类型、描述、scope、操作结果和后台状态摘要。

**验证：** `go test ./internal/tui/ -run 'Memory' -count=1` 退出码为 0；覆盖命令解析、默认清理范围和错误结果呈现。

## T18：组装 chatserve 依赖

**文件：** `cmd/stable/chatserve.go`
**依赖：** T1、T11、T14、T15、T16
**步骤：**
1. 以 appconfig 用户配置目录、规范项目根和 StateDir 创建 manager。
2. 将现有 `decision.ChatProvider` 适配为 memory 的单次 JSON 模型调用，注入 conversation 与 execution。
3. 绑定结果事件到对应 session log；服务退出时停止/等待本服务启动的后台任务。
4. 不创建额外 provider，不将项目根暴露给工具参数。

**验证：** `go test ./cmd/stable/ -run 'Chatserve|Memory' -count=1` 退出码为 0；构造依赖可启动，原有配置与工具 schema 仍成立。

## T19：添加 M08 集成与端到端场景

**文件：** 新建 `tests/e2e/m08_memory_test.go`
**依赖：** T16、T17、T18
**步骤：**
1. 在临时 HOME、配置目录和嵌套项目中验证分层指令、受限 include、双范围索引及相关召回。
2. 验证主动记忆、普通 run 后提取、goal run 允许类别过滤及管理命令；检查 Stable goal facts 未改变。
3. 验证 24 小时与 5 session 门槛、并发整理、失败可见、恢复/压缩后记忆仍在。
4. 验证路径穿越、链接逃逸、超限输入和 selector/worker 失败时的隔离行为。

**验证：** `go test ./tests/e2e/ -run 'M08|Memory' -count=1` 退出码为 0；场景只写入临时目录。

## T20：汇合后执行定向与全量验证

**文件：** 无（验证任务）
**依赖：** T3–T19
**步骤：**
1. 按改动包运行 appconfig、secfile、memory、sessionlog、execution、conversation、tui 和 chatserve 定向测试。
2. 运行 M08 端到端场景，再运行 `go test ./...`。
3. 检查 `git diff --check` 和 `go vet ./...`；若内存持续下降或压力升高，停止增加并发，按 AGENTS.md 降低并行度后继续。
4. 记录实际命令和结果；不得把未执行或失败的检查标为通过。

**验证：** 所有命令退出码为 0；失败项先修复并重跑，不能以部分测试替代全量验证。

## 执行顺序

```text
批次 A（可并行）：T1、T2、T6、T12
批次 B：T3、T4、T5（均依赖 T2，可分文件并行）
批次 C：T7（依赖 T1、T3、T4、T5、T6）
批次 D：T8 → T9 → T10 → T11（memory 包共用构建/测试边界，按序完成）
批次 E：T13、T15（分别依赖 T6/T7 与 T11/T12，可并行）
批次 F：T14、T16、T17（分别依赖 T13、T15；无共享源码文件）
批次 G：T18（依赖 T1、T11、T14–T16）
批次 H：T19（依赖 T16–T18）
批次 I：T20（全部实现与集成任务完成后）
```

**并行边界：** T3/T4/T5 分别只修改 Linux、Darwin、Windows/other 平台文件与各自测试文件；共享 `secfile.go` API 先由 T2 固定。T1、T2、T6、T12 互不改同一文件。memory 包任务按序执行，避免定向测试与同包源码并发变动；其余任务按表中依赖推进。实际并发由当前 MemAvailable、换页速率与 memory PSI 决定，测试汇合时避免同时启动多个全量测试进程。
