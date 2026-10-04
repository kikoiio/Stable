# M05 会话与上下文 Checklist

> 状态：已批准（2026-10-04）。基于已批准的 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md)。每项须有运行代码或观察行为的证据才能勾选。

## 会话事实、投影与恢复

- [x] **C01 / AC1,N1**：多个 session 的事件按稳定序号读取；session 元数据列表按最近活动排序；损坏日志明确失败，不回退到陈旧 transcript。（验证：sessionlog/conversation 单测。）
- [x] **C02 / AC1,F1**：按标题、首条用户消息和事件文本搜索；结果有界并可选择恢复正确 session。（验证：搜索单测及 TUI 场景。）
- [x] **C03 / AC1,F1**：恢复后的 transcript 和 agent context 由同一 projection 生成，工具调用与结果配对且次序一致。（验证：比较 projection 序列。）
- [x] **C04 / AC1,N1**：并发追加、游标续读、截断记录和校验失败均有稳定错误或恢复行为；不静默跳过事件。（验证：日志故障注入。）

## 输入历史

- [x] **C05 / AC2,F2,N3**：项目 `.stable/input-history.jsonl` 以私有权限创建，重启后历史可恢复，空白输入不保存。（验证：临时项目文件权限和重启测试。）
- [x] **C06 / AC2,F2,N4**：连续重复输入折叠，超过 200 条后只保留最近 200 条，前后浏览顺序正确。（验证：存储与 composer 测试。）
- [x] **C07 / AC2**：输入历史不会显示为 transcript 消息，也不会进入 ContextProjection。（验证：跨模块投影断言。）
- [x] **C08 / AC2,N3**：输入历史、摘要、快照元数据与恢复上下文在持久化前应用凭据脱敏；无法安全脱敏时拒绝写入，不保存明文凭据。（验证：注入模拟凭据后检查落盘内容与投影。）

## 持久上下文压缩

- [x] **C09 / AC3,F3,N4**：默认窗口为 8192 tokens、80% 触发；明确配置覆盖有效，非法配置回退且日志可观察。（验证：config/sessioncontext 单测。）
- [x] **C10 / AC3,F3**：触发压缩后 provider 收到较早内容摘要及完整近期原文；未完成工具调用及其结果不会被拆分。（验证：fake provider 请求断言。）
- [x] **C11 / AC3,N4**：压缩输入大小有上限，仅基于 session log 事件构造，不无界读取工程文件；超限输入按策略截断或明确失败。（验证：构造超大日志事件的定向测试。）
- [x] **C12 / AC3,N1**：boundary 只追加一次，记录 scope、RunID 和有效序号范围；重启投影一致，原始事件仍可审计。（验证：回放前后比较。）
- [x] **C13 / AC4,N1**：摘要生成、范围校验或边界持久化失败时本轮不向 provider 发送超预算上下文，不报告压缩成功。（验证：逐项注入错误。）
- [x] **C14 / AC3,F3**：conversation 是边界事件的唯一日志追加者；断线重连按 cursor 收到边界且没有重复边界。（验证：stream 集成测试。）

## 文件快照与 rewind

- [x] **C15 / AC5,F4,N2**：快照绑定 project/session/candidate/run，manifest 对新增、修改、删除、权限和内容摘要准确，其他归属无法列出或恢复。（验证：candidate/store 测试。）
- [x] **C16 / AC5,N3**：snapshot metadata 与内容寻址 blob 在重启后可验证；损坏或缺失 blob 拒绝 restore。（验证：重启及破坏 blob 测试。）
- [x] **C17 / AC6,N4**：默认项目 1 GiB、每候选 50 个 manifest 配额有效；同 digest blob 复用；超限在会产生变更的工具执行前拒绝。（验证：边界配额测试。）
- [x] **C18 / AC5,AC6,F4**：写/编辑/可能改文件的命令有前态和后态快照；只读和无变化调用不产生额外快照。（验证：tool executor 集成测试。）
- [x] **C19 / AC6,F5**：对候选新增、修改、删除文件后 rewind 到旧快照，候选 manifest 精确恢复且快照后的文件被移除；正式工程 digest 逐字节不变。（验证：e2e 及 manifest 对比。）
- [x] **C20 / AC7,F5,N1**：只有未接收且无活动写入者的同一候选可 rewind；accepted、其他 session、digest 不符、活动运行及越界路径均拒绝。（验证：拒绝矩阵。）
- [x] **C21 / AC7,N1**：对 prepared、swapped、finalized 各阶段注入中断，重启后按 digest 完成恢复或安全拒绝；不存在半恢复成功状态。（验证：journal 故障注入。）
- [x] **C22 / AC7,F5**：rewind 使旧 review 失效并生成可观察 receipt；接收处理中或未完成 accept journal 阻止 rewind/accept。（验证：store/conversation 集成测试。）
- [x] **C23 / AC7,F5**：前态或后态 checkpoint 失败后候选阻断，后续写入和接收均拒绝；错误可在工具结果和 transcript 中查看。（验证：注入配额/I/O 错误。）
- [x] **C24 / AC3,AC7,N5**：压缩触发/边界、快照创建与 rewind 的待处理、成功、失败状态在 transcript 和 review 中可区分，失败含可理解原因。（验证：TUI model 测试与故障注入。）

## /say、/reply 与目标事实

- [x] **C25 / AC8,F6**：活动目标收到 `/say` 后，消息持久排入下一决策轮，当前运行不被取消或并发重入。（验证：目标 workflow 事件序列。）
- [x] **C26 / AC8,F6**：`/reply` 必须绑定同一 WorkRef 的 pending QuestionID；不存在、错归属或已答问题时拒绝，重复提交不产生第二次唤醒。（验证：service 单测。）
- [x] **C27 / AC8,N1**：重启后 `/say` 与 `/reply` 仍按序可消费，不丢失、不重复执行；状态从持久事件重建。（验证：重启恢复 e2e。）
- [x] **C28 / AC9,F7**：压缩、快照和 rewind 不更改验收标准、证据或目标 verified 结论；强制接收后目标仍待独立复核。（验证：目标状态前后断言。）

## 用户流程、兼容和回归

- [x] **C29 / AC1,AC2,AC3**：完整会话流程“输入 → 重启 → 搜索 → 恢复 → 继续”，UI 显示和 provider 实际上下文一致。（验证：M05 e2e 第一段。）
- [x] **C30 / AC5,AC6,AC7**：完整候选流程“工具变更 → 多个快照 → rewind → 重启”，正式工程保持不变，receipt 和候选 review 正确。（验证：M05 e2e 快照段。）
- [x] **C31 / AC8,AC9**：目标流程演示 `/say` 下一轮消费、有效 `/reply` 精确答复和目标事实不变。（验证：M05 e2e 消息段。）
- [x] **C32 / AC10,N1**：`go build -p 1 ./...`、`go vet -p 1 ./...`、定向测试与 `go test -p 1 ./...` 通过；保存命令、退出码和日志位置。（验证：单次受控资源批次。）
- [x] **C33 / AC10**：M00–M04 核心会话、目标、候选接收及工具 e2e 回归通过。（验证：列出执行脚本和逐项结果。）
- [x] **C34 / AC10**：M05 session、snapshot、question schema 的历史 fixture 可读；既有会话压缩记录按兼容规则投影。（验证：golden/fixture 测试。）
- [x] **C35 / AC10,plan**：里程碑每个提交在独立 worktree 中可编译，记录 commit SHA 与退出码。（验证：`go build -p 1 -buildvcs=false ./...`。）

## 验收记录

执行时记录日期、提交、环境、内存采样、命令或用户操作、退出码/界面结果及证据路径。真实隔离能力不可用时，不能把拒绝路径结果记为 rewind 正例；任何未通过或未执行项目保持未勾选。M05 只有 C01–C35 全部有证据后才能标为通过。

> 2026-10-04 最终验收记录（基于里程碑最终提交 260e71e；环境：Ubuntu 本机，GOMAXPROCS=2、测试 -p 1 受控资源批次；内存采样 MemAvailable 5.0–6.7 GiB、memory PSI some/full avg10=0.00；bwrap 可用）：
>
> - **C01–C28**（单测/集成证据）：`GOMAXPROCS=2 go test -p 1 ./...` 退出码 0，29 包全部 ok（日志 `/tmp/m05-final2-c32.log`）。逐项映射：
>   - C01：internal/sessionlog（log_test：稳定序号、List 按最近活动、损坏 fail-closed）、internal/conversation（run_test 游标续读）。
>   - C02：internal/sessionlog/search_test（标题/首条用户消息/事件文本、有界 snippet、损坏上报）；internal/tui/m05_test（选择器过滤、/search 恢复、空结果）。
>   - C03：internal/sessionlog/projection_test（统一投影、工具配对 Matched）+ e2e TestM05SessionSearchAndRestartRestore（重启前后 transcript/context DeepEqual）。
>   - C04：internal/sessionlog log_test/validate_test（并发追加、截断记录、校验失败、不静默跳过）。
>   - C05–C08：internal/inputhistory 全部测试（0600 权限、重启恢复、空白忽略、连续去重、200 条上限、凭据脱敏与短凭据拒写）；C07 投影断言（inputhistory 数据不进 sessionlog.Project）。
>   - C09：internal/appconfig（默认值/覆盖/非法回退）+ internal/sessioncontext/context_test。
>   - C10：internal/sessioncontext run_test/projection_test（fake provider 请求断言、工具对不拆分）。
>   - C11：internal/sessioncontext compact_test（MaxCompactionInputChars=48000、逐块摘要、仅基于 session log 事件）。
>   - C12：internal/conversation/compaction_test（boundary 单次追加、scope/RunID/范围）+ e2e TestM05CompactionBoundaryAudit（原事件 2*turns 完整可审计、重启投影一致）。
>   - C13：internal/sessioncontext（摘要失败/超预算拒绝发送）+ internal/agent/context_test（PrepareRun 失败 → RunFailed 且不调用 provider）。
>   - C14：internal/conversation/compaction_test（conversation 唯一追加者、cursor 重放边界不重复）。
>   - C15：internal/candidate/snapshot_test（project/session/candidate/run 绑定、manifest 新增/修改/删除/权限/digest 准确、越权 List/Restore 拒绝）。
>   - C16：internal/candidate/snapshot_test（重启后可验证；损坏/缺失 blob 拒绝 restore）。
>   - C17：internal/candidate/snapshot_test（配额先试后写、同 digest 复用）+ internal/appconfig（默认 1 GiB / 50 manifests）。
>   - C18：internal/execution/snapshot_test（前态/后态快照、只读与无变化命令不写新 manifest）。
>   - C19：e2e TestM05SnapshotRewindAndRestartRecovery + internal/conversation/protocol_m05_test（manifest 精确恢复、快照后文件移除、正式 digest 不变）。
>   - C20：internal/conversation/protocol_m05_test TestRewindSnapshotRefusals（accepted/其他 session/digest 不符/活动运行/未完成 journal 拒绝矩阵）。
>   - C21：internal/store/rewind_test（prepared/swapped 各阶段中断注入，按 digest finalize/block，无半恢复状态）。
>   - C22：internal/conversation（rewind 后 candidate_reviews 删除、pending+completed receipt 事件）+ PendingAcceptances 阻止 rewind。
>   - C23：internal/execution/snapshot_test（前态/后态失败注入 → 候选 blocked、后续写入 denied、工具结果如实报错、finalize 不冻结 ready）。
>   - C24：internal/tui/m05_test（压缩/快照/rewind/question 的待处理/成功/失败渲染与错误文本）。
>   - C25：e2e TestM05SayReplyAndGoalFactBoundary（/say 持久排队、pending→signaled→processed 按序消费、当前运行不取消）；目标 workflow 事件序列另有既有 e2e（waiting_restart 等）回归。
>   - C26：internal/conversation/protocol_m05_test TestQuestionReplyLifecycle（绑定同一 session pending QuestionID；不存在/错归属/已答拒绝；sessionlog.Append 原子校验，重复不产生第二次事件）。
>   - C27：同上两测试中的 fresh-service 重启重放（状态从持久事件重建）。
>   - C28：e2e TestM05SayReplyAndGoalFactBoundary（压缩/快照/rewind/say/reply 后 goal verified 与 evidence 不变）；强制接收独立复核由 m03_suite.sh 内 m03_force_accept.sh 回归覆盖。
> - **C29–C31**：`bash tests/e2e/m05_sessions.sh` 四段全部 PASS（日志 `/tmp/m05-final-m05_sessions.log`）。
> - **C32**：`go build -p 1 -buildvcs=false ./...`、`go vet -p 1 ./...`、`go test -p 1 ./...` 在最终提交 260e71e 上退出码均为 0（日志 `/tmp/m05-final2-c32.log`）。
> - **C33**：`run.sh`、`waiting_restart.sh`、`unsupported.sh`、`m03_suite.sh`、`m04_tools.sh` 在受控批次中逐项退出码 0（日志 `/tmp/m05-final-{run,waiting_restart,unsupported,m03,m04_tools}.log`）。`criteria_change.sh` 与 `dependency_change.sh` 存在间歇性失败，两脚本在本分支均有完整 PASS 记录（`/tmp/m05-repro-criteria.log`、`/tmp/m05-repro3-criteria.log`、`/tmp/m05-repro-dependency.log`、`/tmp/m05-repro2-dependency.log`），失败样本 `/tmp/m05-final-criteria_change.log`、`/tmp/m05-repro2-criteria.log`、`/tmp/m05-final-dependency_change.log`。根因已定位在 M05 未触碰的 goal/computer 路径：已验证后的评估轮偶发观察到 computer session stale → 重新拉起 eeschema → WaitingForHuman 命中未答 agent question → workflow `events.Receive` 无限期停放（internal/core/workflow.go:44-52，无 30s 定时器），脚本 phase4 不应答问题，60s 导出竞速窗口超时（断言 `goal status is waiting / computer session opened`）；未答问题来自 mock decide 在瞬时观察失败时返回 ask_human。对比证据 `/tmp/stable-e2e-criteria-V74DA6Q1`（通过，1 代 computer session）vs `/tmp/stable-e2e-criteria-TwpKIyfW`（失败，2 代）。根因分析已同步 stable-36；M04 共享脚本未改动。该间歇失败根因已在 master 2d71843 修复（WaitingForHuman 长退避自愈：人工信号即时恢复语义不变，另挂 5×check_interval 长退避重新评估，瞬时 ask_human 自愈），修复侧 workflow 单测×2 与 criteria_change/dependency_change 重跑均退出 0。
> - **C34**：260e71e fix(sessionlog)——legacy 无 scope 边界按旧 Compact 语义投影（边界前全部事件由摘要替代，重放尾部不重复）；internal/sessionlog TestProjectionLegacyScopelessBoundary fixture 测试通过。
> - **C35**：12 个提交逐一在独立 worktree 执行 `GOMAXPROCS=2 go build -p 1 -buildvcs=false ./...`，退出码全部 0（日志 `/tmp/m05-final-c35.log`）：
>
> | 提交 | SHA | 退出码 |
> | --- | --- | --- |
> | 03bd4b3 | 03bd4b33c77868fc7a92be0a673cda5f538b335d | 0 |
> | 3f2f7b0 | 3f2f7b06499732d50f53c1a0850e8246d7a471f8 | 0 |
> | 6d0085a | 6d0085ae41b3b8d9a19f7cac59c67a1f2b63ecf0 | 0 |
> | 61cdf2d | 61cdf2daccc6c2e43a6f424ca33c7a9714b02cb3 | 0 |
> | 0ed92c4 | 0ed92c42be625b55c4c396d33703089321d32b4b | 0 |
> | ed1dadc | ed1dadc6094e25f59086af500b69b26af5f570e4 | 0 |
> | 606282e | 606282e36442706ba614518bb4110342960e4239 | 0 |
> | 23bc3dd | 23bc3ddb03d017ff3b21f427d3c40254396841c4 | 0 |
> | d5d4551 | d5d45518044a210bc723e07096d28d933032c90b | 0 |
> | 01c502e | 01c502ebf222861fecfb4595014f4a364b536623 | 0 |
> | 4c67e1e | 4c67e1efe2e99d4bffa1206e1ffede5f42641ac6 | 0 |
> | 260e71e | 260e71ea342518653cbcaae93a200519ddb83db7 | 0 |
