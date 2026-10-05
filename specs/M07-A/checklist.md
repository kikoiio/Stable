# M07-A 技能(Skills)与命令关联 Checklist

> 状态:验收完成(2026-10-06)。基于已批准的 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md)。全部 23 项以运行代码或观察行为的证据勾选;开发中发现并修复 1 项集成缺陷(skill_invoke 流式语义,见 C22 证据),修复后复验通过。

## 技能发现与解析

- [x] **C01 / AC1**:两种布局技能均被发现与解析;缺 name 回退目录名、缺 description 回退正文首行。(证据:`go test ./internal/skills/` 26 用例全 PASS——两种布局、默认回填、fork 双路径;e2e 场景 1 中 SKILL.md 与 skill.yaml+prompt.md 技能均出现在 catalog 与补全。)
- [x] **C02 / AC1**:项目级覆盖用户级;两种布局并存 SKILL.md 优先;来源与覆盖可观察。(证据:skills 包合并优先级与并存提示测试;e2e 断言 user 层技能 Source="user"、project 层 Source="project",并存信息性写入 Rejections。)

## 斜杠命令关联

- [x] **C03 / AC2**:技能注册同名斜杠命令,`$ARGUMENTS` 两形态正确。(证据:e2e 场景 1——`/code-review 走线宽度` 激活后会话首条消息为「1. 逐文件审查 走线宽度\n2. 输出结论」;TUI m07a 测试断言补全项 Detail 带技能后缀。)
- [x] **C04 / AC2**:同名冲突按「内置>命令文件>技能」跳过且可观察。(证据:TUI `TestSkillCommandsCompleteBelowCommandsAndBuiltins`——/alpha 归命令文件、/search 归内置、/beta 技能命令带「（技能）」标记;状态栏一次性报告 skillConflictShown。)
- [x] **C05 / AC2**:`/skills` 列表与实际一致含已激活;`/skills reload` 报告数量变化。(证据:TUI `TestSkillsCommandListsAndReloadSendsOps`——列表文本含「/beta — 项目技能」、reload 发 skill_reload op 且状态栏「技能已重载：1 → 1」;e2e reload 报告 Before=3 After=3;skill_list op 返回已激活清单。)

## LoadSkill 工具

- [x] **C06 / AC3**:已知技能返回「# Skill: name」+正文并激活;未知技能报错并列出可用名。(证据:execution `TestLoadSkillReturnsBodyWithHeader`/`TestLoadSkillProviderErrorSurfaces`(错误含 available 列表);e2e 场景 1 load_skill 工具结果实测。)
- [x] **C07 / AC3**:`load_skill` 在 chatserve 与 runtime 两处白名单可见。(证据:`TestChatserveToolSchemas` 与 `TestRuntimeToolSchemas` 白名单含 load_skill,13 项按名排序;两处 nameMap 均追加。)

## 会话暴露

- [x] **C08 / AC4**:首次运行的模型请求含技能清单段。(证据:e2e 场景 2 run 1——fake provider 实收消息含「可用技能…alpha — 基线技能」;conversation `TestStartRunSkillInjectionPlacementAndPersistence` 断言位置在 system 之后、历史之前。)
- [x] **C09 / AC4**:会话中新增技能下一轮提醒,且同一技能不重复提醒。(证据:e2e 场景 2 run 2 实收 delta 提醒「会话期间新增可用: beta」、run 3 无提醒;conversation `TestSkillGateInventoryAndDeltaOnce` 断言 delta 只落一次。)
- [x] **C10 / AC4**:三类事件落会话日志,重启后投影与渲染一致。(证据:e2e 场景 2 重启前后 inventory=1/delta=1 不变、清单注入内容一致;TUI `TestSkillReportStatusAndTranscriptRendering` 三渲染块断言;sessionlog 投影 round-trip 测试。)

## 激活语义

- [x] **C11 / AC5**:slash 激活后渲染正文作为用户消息进入会话,agent 下一轮可见。(证据:e2e 场景 1——会话消息 texts[0] 为渲染正文,agent 实收消息含该正文;skill_invoked 事件 Entry=slash。)
- [x] **C12 / AC5**:激活记录随会话持久;压缩后已激活技能仍可感知。(证据:conversation 注入测试手动落 run-scope boundary 后,run 2 消息仍含「已激活技能…code-review」;e2e 场景 2 重启后快照含「已激活技能」;正文经 load_skill 现读重取。)

## fork 字段

- [x] **C13 / AC6**:fork 技能可发现可列出;两入口调用均报「子 agent 能力未启用」;不产生消息与激活记录。(证据:e2e 场景 1——deep-dive(mode: fork)在 catalog 3 项中可见,skill_invoke 经 socket 返回 error+done,会话消息 0 条、skill_invoked 0 条;conversation `TestSkillGateUnknownAndForkRefused` 同断言。)

## 有界性与异常

- [x] **C14 / AC7**:超限与异常技能跳过且路径原因入报告;不崩溃不阻塞。(证据:skills 包 26 用例含 symlink 拒绝、256KB 超限、frontmatter 非法、正文缺失、MaxSkills=200 上限与 Rejections 排序去重;NeedsReload 为轻量 mtime 对比,不阻塞输入路径。)

## 热更新

- [x] **C15 / AC8**:运行中修改正文,下次调用即新正文。(证据:e2e 场景 1——改写 SKILL.md 后第二次 load_skill 结果含新增「3. 复核修复」;skills 包 `GetFull` 现读热更新测试。)
- [x] **C16 / AC8**:增删技能目录无需重启感知;`/skills reload` 手动生效。(证据:e2e 场景 2 run 2 服务侧自动感知新增 beta;TUI `refreshCommands` 每次补全/派发前 NeedsReload→Reload;conversation `TestSkillReloadOpAndForcedDiff` 与 e2e reload 报告。)

## 集成与编译

- [x] **C17**:`load_skill` 位于权限 Gate 之前且只读——候选区无变化。(证据:execution `TestLoadSkillBypassesCandidate`——候选根目录未创建、无 diff、无快照。)
- [x] **C18**:skills nil 时服务与 TUI 行为与 M06 基线一致。(证据:全量 `go test ./...` 34 包全 ok——M05/M06 既有 harness 均未注入 SkillGate,回归全绿;op/skill_invoke 在 nil 时返回「技能通道不可用」。)
- [x] **C19**:sessionlog 校验——inventory 重复被拒;损坏日志明确失败。(证据:sessionlog `TestSkillEventValidation`(7 种非法形状+重复 inventory)与 `TestReplayRejectsDuplicateSkillInventory`(replay 侧拒绝)。)
- [x] **C20**:三类技能事件不进入模型上下文。(证据:`TestSkillItemsStayOutOfModelContext`(外部测试包)——投影含 3 个 skill Item 但 `prompt.MessagesFromItems` 只产出普通消息。)
- [x] **C21 / 编译**:`gofmt -l` 无输出、`go vet ./...` 干净、`go test ./...` 全部通过。(证据:验收终验——gofmt 修复 model.go 对齐后三命令均干净;34 包 ok、0 FAIL。)

## 端到端场景

- [x] **C22 / 全链路**:`tests/e2e/m07a_skills.sh` 全部场景通过。(证据:验收时亲自复跑两场景组 PASS。开发中发现的集成缺陷——skill_invoke 经 requestCmd 调用会在无 done 的流上挂起——已修复:成功时流绑定 run 直至 outcome,失败时 error+done 终止;新增 `conversation.InvokeSkill` 客户端,TUI 技能命令改走 run 流;修复后复验通过。)
- [x] **C23 / 回归**:`m05_sessions.sh` 与 `m06_interaction.sh` 通过。(证据:验收时亲自复跑——M05 全场景 PASS、M06 全场景 PASS。)
