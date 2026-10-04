# M05 会话与上下文 Checklist

> 状态：已批准（2026-10-04）。基于已批准的 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md)。每项须有运行代码或观察行为的证据才能勾选。

## 会话事实、投影与恢复

- [ ] **C01 / AC1,N1**：多个 session 的事件按稳定序号读取；session 元数据列表按最近活动排序；损坏日志明确失败，不回退到陈旧 transcript。（验证：sessionlog/conversation 单测。）
- [ ] **C02 / AC1,F1**：按标题、首条用户消息和事件文本搜索；结果有界并可选择恢复正确 session。（验证：搜索单测及 TUI 场景。）
- [ ] **C03 / AC1,F1**：恢复后的 transcript 和 agent context 由同一 projection 生成，工具调用与结果配对且次序一致。（验证：比较 projection 序列。）
- [ ] **C04 / AC1,N1**：并发追加、游标续读、截断记录和校验失败均有稳定错误或恢复行为；不静默跳过事件。（验证：日志故障注入。）

## 输入历史

- [ ] **C05 / AC2,F2,N3**：项目 `.stable/input-history.jsonl` 以私有权限创建，重启后历史可恢复，空白输入不保存。（验证：临时项目文件权限和重启测试。）
- [ ] **C06 / AC2,F2,N4**：连续重复输入折叠，超过 200 条后只保留最近 200 条，前后浏览顺序正确。（验证：存储与 composer 测试。）
- [ ] **C07 / AC2**：输入历史不会显示为 transcript 消息，也不会进入 ContextProjection。（验证：跨模块投影断言。）
- [ ] **C08 / AC2,N3**：输入历史、摘要、快照元数据与恢复上下文在持久化前应用凭据脱敏；无法安全脱敏时拒绝写入，不保存明文凭据。（验证：注入模拟凭据后检查落盘内容与投影。）

## 持久上下文压缩

- [ ] **C09 / AC3,F3,N4**：默认窗口为 8192 tokens、80% 触发；明确配置覆盖有效，非法配置回退且日志可观察。（验证：config/sessioncontext 单测。）
- [ ] **C10 / AC3,F3**：触发压缩后 provider 收到较早内容摘要及完整近期原文；未完成工具调用及其结果不会被拆分。（验证：fake provider 请求断言。）
- [ ] **C11 / AC3,N4**：压缩输入大小有上限，仅基于 session log 事件构造，不无界读取工程文件；超限输入按策略截断或明确失败。（验证：构造超大日志事件的定向测试。）
- [ ] **C12 / AC3,N1**：boundary 只追加一次，记录 scope、RunID 和有效序号范围；重启投影一致，原始事件仍可审计。（验证：回放前后比较。）
- [ ] **C13 / AC4,N1**：摘要生成、范围校验或边界持久化失败时本轮不向 provider 发送超预算上下文，不报告压缩成功。（验证：逐项注入错误。）
- [ ] **C14 / AC3,F3**：conversation 是边界事件的唯一日志追加者；断线重连按 cursor 收到边界且没有重复边界。（验证：stream 集成测试。）

## 文件快照与 rewind

- [ ] **C15 / AC5,F4,N2**：快照绑定 project/session/candidate/run，manifest 对新增、修改、删除、权限和内容摘要准确，其他归属无法列出或恢复。（验证：candidate/store 测试。）
- [ ] **C16 / AC5,N3**：snapshot metadata 与内容寻址 blob 在重启后可验证；损坏或缺失 blob 拒绝 restore。（验证：重启及破坏 blob 测试。）
- [ ] **C17 / AC6,N4**：默认项目 1 GiB、每候选 50 个 manifest 配额有效；同 digest blob 复用；超限在会产生变更的工具执行前拒绝。（验证：边界配额测试。）
- [ ] **C18 / AC5,AC6,F4**：写/编辑/可能改文件的命令有前态和后态快照；只读和无变化调用不产生额外快照。（验证：tool executor 集成测试。）
- [ ] **C19 / AC6,F5**：对候选新增、修改、删除文件后 rewind 到旧快照，候选 manifest 精确恢复且快照后的文件被移除；正式工程 digest 逐字节不变。（验证：e2e 及 manifest 对比。）
- [ ] **C20 / AC7,F5,N1**：只有未接收且无活动写入者的同一候选可 rewind；accepted、其他 session、digest 不符、活动运行及越界路径均拒绝。（验证：拒绝矩阵。）
- [ ] **C21 / AC7,N1**：对 prepared、swapped、finalized 各阶段注入中断，重启后按 digest 完成恢复或安全拒绝；不存在半恢复成功状态。（验证：journal 故障注入。）
- [ ] **C22 / AC7,F5**：rewind 使旧 review 失效并生成可观察 receipt；接收处理中或未完成 accept journal 阻止 rewind/accept。（验证：store/conversation 集成测试。）
- [ ] **C23 / AC7,F5**：前态或后态 checkpoint 失败后候选阻断，后续写入和接收均拒绝；错误可在工具结果和 transcript 中查看。（验证：注入配额/I/O 错误。）
- [ ] **C24 / AC3,AC7,N5**：压缩触发/边界、快照创建与 rewind 的待处理、成功、失败状态在 transcript 和 review 中可区分，失败含可理解原因。（验证：TUI model 测试与故障注入。）

## /say、/reply 与目标事实

- [ ] **C25 / AC8,F6**：活动目标收到 `/say` 后，消息持久排入下一决策轮，当前运行不被取消或并发重入。（验证：目标 workflow 事件序列。）
- [ ] **C26 / AC8,F6**：`/reply` 必须绑定同一 WorkRef 的 pending QuestionID；不存在、错归属或已答问题时拒绝，重复提交不产生第二次唤醒。（验证：service 单测。）
- [ ] **C27 / AC8,N1**：重启后 `/say` 与 `/reply` 仍按序可消费，不丢失、不重复执行；状态从持久事件重建。（验证：重启恢复 e2e。）
- [ ] **C28 / AC9,F7**：压缩、快照和 rewind 不更改验收标准、证据或目标 verified 结论；强制接收后目标仍待独立复核。（验证：目标状态前后断言。）

## 用户流程、兼容和回归

- [ ] **C29 / AC1,AC2,AC3**：完整会话流程“输入 → 重启 → 搜索 → 恢复 → 继续”，UI 显示和 provider 实际上下文一致。（验证：M05 e2e 第一段。）
- [ ] **C30 / AC5,AC6,AC7**：完整候选流程“工具变更 → 多个快照 → rewind → 重启”，正式工程保持不变，receipt 和候选 review 正确。（验证：M05 e2e 快照段。）
- [ ] **C31 / AC8,AC9**：目标流程演示 `/say` 下一轮消费、有效 `/reply` 精确答复和目标事实不变。（验证：M05 e2e 消息段。）
- [ ] **C32 / AC10,N1**：`go build -p 1 ./...`、`go vet -p 1 ./...`、定向测试与 `go test -p 1 ./...` 通过；保存命令、退出码和日志位置。（验证：单次受控资源批次。）
- [ ] **C33 / AC10**：M00–M04 核心会话、目标、候选接收及工具 e2e 回归通过。（验证：列出执行脚本和逐项结果。）
- [ ] **C34 / AC10**：M05 session、snapshot、question schema 的历史 fixture 可读；既有会话压缩记录按兼容规则投影。（验证：golden/fixture 测试。）
- [ ] **C35 / AC10,plan**：里程碑每个提交在独立 worktree 中可编译，记录 commit SHA 与退出码。（验证：`go build -p 1 -buildvcs=false ./...`。）

## 验收记录

执行时记录日期、提交、环境、内存采样、命令或用户操作、退出码/界面结果及证据路径。真实隔离能力不可用时，不能把拒绝路径结果记为 rewind 正例；任何未通过或未执行项目保持未勾选。M05 只有 C01–C35 全部有证据后才能标为通过。
