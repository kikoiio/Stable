# M02 模型与流式对话 Spec

## 背景

mewcode 的模型层支持 Anthropic、OpenAI 和 OpenAI-compatible 流式 API，将文本、思考、工具调用、用量、结束、重试和错误转换为 agent 事件；TUI 可以实时消费这些事件，并取消当前运行。Stable 当前会话聊天使用完整请求/响应，长期目标由结构化模型决策和持久化状态驱动，还没有统一的流式 agent 运行入口。

M00 已确定普通任务和长期目标工作项由同一套 agent 执行能力处理，Stable 负责目标调度、验收事实和后续复核。M02 将建立模型 provider 流和共用 run/stream 接口，将有序事件送到 Stable TUI，并保存可重载的会话事实。Stable 现有 Gemini 结构化目标决策继续可用；新增 agent streaming 首发覆盖 mewcode 的三种 provider 协议。

## 目标

- 接入 Anthropic、OpenAI 和 OpenAI-compatible provider 的流式模型响应。
- 让普通任务和目标工作项使用同一 agent run/stream 执行入口，并保留可区分的任务归属。
- 在共用 TUI 中实时呈现文本、思考、工具调用事件、用量、错误和取消状态。
- 保留 Stable 的目标、验收和证据事实来源；模型事件不得改变目标验证结论。
- 取消运行可贯穿 agent 和 provider，且运行事件与会话事实可追踪、可重载。

## 功能需求

- **F1 Provider 配置**：可从配置选择 Anthropic、OpenAI 或 OpenAI-compatible provider，并配置 model 与 base URL；provider 凭据不得写入会话或日志。Stable 现有 Gemini 结构化目标决策继续受支持，但 M02 不增加 Gemini agent streaming。
- **F2 共用执行入口**：普通任务与长期目标工作项均进入同一个 agent run/stream 执行入口；每次运行可标识 session、普通/目标归属及可选目标/工作项 ID。长期目标状态、验收条件和证据仍以 Stable 持久事实为准。
- **F3 流式事件**：provider 流转换为有序且可关联 run/session 的事件，至少包括文本增量、思考增量/完成、工具调用开始/参数片段/完成、token 用量、运行结束、重试和错误。
- **F4 TUI 呈现**：回答文本实时追加；思考与最终回答分区显示；工具调用事件显示名称、ID、参数和生命周期；用量和运行状态可见。未知事件安全忽略，不混入其他 session/run。
- **F5 取消**：用户可取消当前运行；取消信号传到 agent 和 provider 并关闭流。取消不是成功或验证结论，长期目标的后续责任由 M00 契约及 Stable 调度处理。
- **F6 错误与重试**：至少区分认证、限流、网络、上下文超限和普通 provider 错误。允许重试的错误采用有界重试并显示状态；已经输出的部分文本在错误后保留。
- **F7 工具事件边界**：M02 解码并呈现模型返回的 tool-call 事件，但不执行工具。实际工具执行、授权、隔离及结果回填由 M03/M04 定义。
- **F8 用量**：记录并显示 provider 报告的输入、输出和缓存 token 用量；provider 未报告的字段明确显示为不可用，不伪造为零。
- **F9 持久会话事实**：流式文本和 run 结束状态进入现有 Stable 会话事实；重载会话后，呈现与已记录内容/状态一致。普通任务与目标工作项不另建互不兼容的运行协议。

## 非功能需求

- **N1 平台与配置诊断**：首发验收只要求 Linux；provider 配置、协议和密钥解析失败可诊断，认证信息不能进入日志、会话或错误正文。
- **N2 事件顺序与关联**：每个 run 的事件有稳定 ID、单调序号和确定顺序。断连或慢消费不能造成静默错序、丢失归属或跨 session 混流；后续重连可按 Stable 事件游标恢复，完整恢复 UI 在 M05 深化。
- **N3 取消资源释放**：取消上下文贯穿 agent/provider，及时关闭网络流并释放 goroutine 与资源；完成、错误和取消结果互斥，重复取消安全。
- **N4 重试边界**：限流/网络重试次数与总等待时间有界，尊重可用的 Retry-After；认证或参数错误不盲目重试。重试期间已接收的文本不丢失。
- **N5 Provider 隔离及既有能力**：三种新增 streaming 协议适配相互隔离；Stable 现有 Gemini 结构化目标决策继续通过，不要求 Gemini 在本阶段具备 agent streaming。
- **N6 可重复验证**：使用 fake provider/SSE fixture 覆盖流式事件、错误、用量和取消，不依赖真实 API key 或外网；既有目标决策、会话持久化和 Linux TUI 回归继续通过。

## 不做的事

- 不实现文件读写、目录搜索、命令执行等工具的实际运行及权限/沙箱策略。M02 只承载 tool-call 事件；工具与安全边界由 M03/M04 实现。
- 不迁入 MCP、skills、hooks、memory、子 agent、团队或远程会话；这些能力按迁移地图归入 M07–M10。
- 不实现会话搜索/恢复、上下文压缩、rewind 与工作区恢复；归入 M05。
- 不实现 plan/todo/ask-user 审批界面或自定义命令；归入 M06。
- 不让模型直接修改 Stable 正式文件或目标、验收和证据事实。隔离候选、接收和独立复核按 M00 契约及后续子项目交付。
- 不增加 Gemini agent streaming，也不把非 Linux 平台列为本阶段验收目标。

## 验收标准

- **AC1（F1）**：Anthropic、OpenAI 和 OpenAI-compatible 均可从配置创建 streaming client，并通过对应协议 fixture；Stable 现有 Gemini 结构化目标决策回归通过。
- **AC2（F2）**：普通任务与目标工作项生成不同归属的 `WorkRef`，但调用同一个 run/stream 执行入口；模型事件不能替代 Stable 目标状态或验收证据。
- **AC3（F3/F4/F9）**：文本增量按序实时显示，多个 session/run 不串流；结束时形成完整会话消息，失败或取消时保留已收到文本；重载后内容和结束状态与持久记录一致。
- **AC4（F3/F4/F7）**：思考流与最终回答分区呈现，不混入可提交文本；tool-call 名称、ID、参数片段和完成事件可见，M02 不执行该调用。
- **AC5（F4/F8）**：输入、输出和缓存 token 按 provider 报告展示；未报告字段明确显示不可用。
- **AC6（F5）**：取消从 TUI 传到 provider，关闭流且最终状态唯一为 cancelled；长期目标不会因模型事件或取消被标记 verified。
- **AC7（F6）**：认证、限流、网络、上下文超限及普通 provider 错误可区分；重试次数/等待有界且可见，认证/参数错误不重试，错误前已输出文本保留。
- **AC8（F2/F3/F9/N2）**：事件具有关联的 run/session ID 和单调序号；重载和并发运行测试不丢失归属、不覆盖其他运行数据。
- **AC9（F1/N1）**：API key 不出现在日志、持久消息、错误正文或 TUI transcript。
- **AC10（N6）**：fake provider/SSE 测试覆盖正常结束、tool-call、部分流、格式错误/截断、用量、限流、取消和会话重载；`go test ./...` 通过。
- **AC11（N1/N6）**：验收无需外部 API key 或网络，并在 Linux 环境完成。
