# M07-A 技能(Skills)与命令关联 Tasks

> 状态:已批准(2026-10-06)。基于已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。每步完成即运行该任务「验证」;每任务完成即提交。

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `internal/skills/skills.go`、`parser.go`(+测试) | 元数据模型、两种布局解析、默认回填、界限 |
| 新建 | `internal/skills/catalog.go`(+测试) | 两阶段 catalog、mtime 热更新、拒绝报告 |
| 修改 | `internal/appconfig/config.go`(+测试) | `UserSkillsDir()` |
| 修改 | `internal/sessionlog/events.go`、`log.go`、`validate.go`、`projection.go`(+测试) | 3 新事件+白名单+校验+投影 |
| 修改 | `internal/execution/executor_factory.go`、`tools_schema.go`(+测试) | SkillProvider 接口、option、schema |
| 修改 | `internal/execution/tool_executor.go`(+测试) | `load_skill` 主机分支 |
| 新建 | `internal/conversation/skills.go`(+测试) | SkillGate:激活、快照、diff、重建 |
| 修改 | `internal/conversation/service.go`、`protocol.go`(+测试) | 持有 SkillGate、op 分发、协议字段 |
| 修改 | `internal/conversation/run.go`(+测试) | startRun 技能段注入 |
| 修改 | `cmd/stable/chatserve.go`、`internal/runtime/supervisor.go` | nameMap 白名单 + provider 注入 |
| 修改 | `internal/tui/model.go`(+测试) | TUI catalog、技能命令、`/skills`、报告 |
| 修改 | `internal/tui/transcript.go`(+测试) | 3 类事件渲染 |
| 新建 | `tests/e2e/m07a_skills.sh` | 端到端场景 |

## T1: skills 元数据与解析器

**文件:** `internal/skills/skills.go`、`parser.go`、`parser_test.go`、`skills_test.go`;`go.mod`(yaml.v3 转正)
**依赖:** 无
**步骤:**
1. `SkillMeta`(name/description/when_to_use/tags/mode/context/fork_context)+ `IsFork()`;`Skill{Meta, PromptBody, SourceDir, Source, BodyLoaded}`
2. `parseFrontmatterOnly(dir)`:优先 `skill.yaml`,其次 `SKILL.md` frontmatter(`---` 分隔,yaml.v3 解析);返回未读正文的 phase-1 Skill
3. `loadSkillBody(skill)`:按布局读 `prompt.md` 或 SKILL.md 正文段;失败保留旧正文并返回错误
4. `applyMetaDefaults`:缺 name→目录名小写连字符、缺 description→正文首个非空非标题行、fork 时缺 fork_context→"none"
5. 界限:单文件 >256KB 拒绝(带原因)
**验证:** `go test ./internal/skills/...` — 两种布局、默认回填、fork 字段、超限拒绝

## T2: skills catalog 与热更新

**文件:** `internal/skills/catalog.go`、`catalog_test.go`
**依赖:** T1
**步骤:**
1. `LoadCatalog(userDir, projectDir)`:两层各扫直接子目录(非目录跳过、symlink 目录拒绝并记录原因、不可读/非法 frontmatter/正文缺失跳过并记录);按 name 合并,项目覆盖用户,两种布局并存 SKILL.md 优先;超 `MaxSkills=200` 停止并记录
2. `Get/GetFull(现读)/List(按名排序)/Source`;`Rejections() []string`(路径:原因,去重)
3. `NeedsReload`(目录 mtime 快照对比,含目录消失与新增)+ `Reload`(重建+重拍快照)
**验证:** `go test ./internal/skills/...` — 覆盖优先级、布局并存、phase-1 不读正文、GetFull 热更新(改盘后读到新正文)、增删检测、symlink/超限/坏文件拒绝且报告完整

## T3: appconfig 用户技能目录

**文件:** `internal/appconfig/config.go`、`config_test.go`
**依赖:** 无
**步骤:** `UserSkillsDir() (string, error)`:`XDG_CONFIG_HOME`/`$HOME/.config` + `stable/skills`,逻辑与 `ConfigPath()` 同源;测试覆盖 XDG 与 HOME 两分支
**验证:** `go test ./internal/appconfig/...`

## T4: sessionlog 三新事件

**文件:** `internal/sessionlog/events.go`、`log.go`、`validate.go`、`projection.go` 及各自测试
**依赖:** 无
**步骤:**
1. `EventSkillInventory/EventSkillDelta/EventSkillInvoked` 常量;`SkillInfo/SkillInventory/SkillDelta/SkillInvoked` Data 类型;数量上限常量(单次 delta/inventory 条目数 ≤200)
2. Append 与 replay 两处白名单追加;owned-append:`skill_inventory` 每会话至多 1 条(重复拒绝)、`skill_delta`/`skill_invoked` 校验必填字段与数量
3. 投影:`ItemSkillInventory/ItemSkillDelta/ItemSkillInvoked`;`prompt.MessagesFromItems` 不消费三类(不进模型上下文)
**验证:** `go test ./internal/sessionlog/...` — inventory 重复拒绝、字段缺失拒绝、投影 Item 断言

## T5: execution SkillProvider 与 schema

**文件:** `internal/execution/executor_factory.go`、`tools_schema.go` 及测试
**依赖:** 无
**步骤:**
1. `tools_schema.go`:`loadSkillSchema`(参数 `name` 必填;静态描述说明按清单中的名称调用)+ `SkillToolSchemas() []map[string]any`
2. `executor_factory.go`:`SkillProvider` 接口(LoadSkill/SkillInventory 两方法)+ `WithSkillProvider` option,透传至 toolRunExecutor
**验证:** `go test ./internal/execution/...` — schema 结构断言;option 注入断言

## T6: execution load_skill 分支

**文件:** `internal/execution/tool_executor.go` 及测试
**依赖:** T5
**步骤:**
1. `executeHostTool` 增加 `case "load_skill"`:参数校验(name 缺失→明确错误结果);provider 为 nil→「技能通道不可用」;调用 `LoadSkill(ctx, sessionID, name, args)`,成功→`# Skill: <name>\n\n<body>`,错误→IsError 结果
2. 确认分支位于 Gate 之前(与 M06 主机工具同位),读类不产生候选快照
**验证:** `go test ./internal/execution/...` — 正常/nil/未知技能/fork 错误路径;候选 manifest 无变化

## T7: conversation SkillGate

**文件:** `internal/conversation/skills.go`、`skills_test.go`
**依赖:** T2, T4, T5
**步骤:**
1. `SkillGate`:持有 `*skills.Catalog`、用户/项目目录、store 引用;`sessionSkillState{inventorySent bool, snapshot []SkillInfo, notified map[string]bool, activated []string}`
2. `activate(sessionID, name, args, entry)`:IsFork→错误「子 agent 能力未启用(fork 模式留待 M09)」;`GetFull` 现读;失败错误含可用技能名列表;渲染经 `commands.ExpandPrompt`;`eventMu` 下落 `skill_invoked`;更新激活清单
3. `ensureInventory`:restore 后未落过→落 `skill_inventory`(当前 catalog 快照)
4. `refreshAndDiff`:`NeedsReload`→`Reload`→与 snapshot diff 新增(不在 notified 的)→落 `skill_delta`+标记 notified+返回提醒文本
5. `restore(sessionID)`:惰性重放三类事件重建状态(inventory 已存在则 inventorySent=true,快照取事件值)
6. 实现 `execution.SkillProvider`(LoadSkill→activate("tool");SkillInventory→ensureInventory+refreshAndDiff 组装注入文本:清单段+已激活段「正文已在会话早期提供,如已被压缩可 LoadSkill 重新加载」+delta 提醒)
**验证:** `go test ./internal/conversation/...` — 激活闭环与事件断言、fork/未知错误、delta 一次性、重启 restore 后注入一致

## T8: conversation op 协议与接线

**文件:** `internal/conversation/service.go`、`protocol.go` 及测试
**依赖:** T7
**步骤:**
1. `ClientMsg` 增加 `SkillName/SkillArgs`;`validOp` 增加 `skill_invoke/skill_reload`;`ServerMsg` 增加 `SkillReport`(kind: error/reload/delta)
2. `Service` 持有 `skills *SkillGate`(nil 时 op 返回明确错误);op `skill_invoke`→activate→以渲染正文为 Intent/Messages 走 `startRun`;错误经 SkillReport 返回且不产生会话消息
3. op `skill_reload`→`Reload`+清除快照缓存(下次 run 重算)→SkillReport 返回前后数量
4. `skill_delta` 落盘时同时广播 SkillReport(delta)
**验证:** `go test ./internal/conversation/...` — op 状态机、激活错误路径不落消息、reload 报告

## T9: conversation startRun 注入

**文件:** `internal/conversation/run.go` 及测试
**依赖:** T7
**步骤:**
1. `startRun` prefix 组装:system prefix 之后、计划提醒之前调用 `SkillProvider.SkillInventory`(skills 非 nil 时);snapshotText 非空则作为 user 角色消息插入;deltaReminder 有值则再插一条
2. 技能段不落 sessionlog(与计划提醒同模式);技能功能关闭(skills nil)时零改动
**验证:** `go test ./internal/conversation/...` — 注入次数与位置、无技能时行为不变、压缩 boundary 后清单仍在

## T10: 进程接线与工具白名单

**文件:** `cmd/stable/chatserve.go`、`internal/runtime/supervisor.go`
**依赖:** T6, T8
**步骤:**
1. 两处 nameMap 追加 `"load_skill"`,schema sources 追加 `execution.SkillToolSchemas()`,保持按名排序
2. 两处构造 `conversation.SkillGate`(appconfig.UserSkillsDir + 项目 `.stable/skills`)并 `WithSkillProvider` 注入
**验证:** `go build ./...`;两处工具 schema 测试数量断言更新并通过

## T11: TUI 技能命令与 /skills

**文件:** `internal/tui/model.go` 及测试
**依赖:** T2, T8
**步骤:**
1. Model 构造 TUI 侧 `*skills.Catalog`;`refreshCommands` 顺序:内置→命令文件→技能(`RegisterOptional`,冲突跳过计数进状态栏报告);技能命令 `KindLocal`、Description 加「(技能)」、handler 发 `skill_invoke` op
2. `/skills` 内置命令:无参列出本地 catalog(名称、描述、来源、「/name 调用」提示)与已激活清单;`reload` 子命令:本地 Reload+发 `skill_reload`
3. `ServerMsg` SkillReport 处理:error/reload/delta → 状态栏一次性提示
4. 补全前 `NeedsReload`→`Reload`(与命令 loader 并列)
**验证:** `go test ./internal/tui/...` — 冲突优先级(内置>命令>技能)、补全含技能命令、/skills 列表、reload 发 op、SkillReport 展示

## T12: TUI transcript 渲染

**文件:** `internal/tui/transcript.go` 及测试
**依赖:** T4
**步骤:** 三类事件渲染块:`skill_invoked`→「技能激活:name(来源·入口)」;`skill_inventory`→「可用技能 N 项」摘要块;`skill_delta`→「新增技能:...」块;重启重放后渲染一致
**验证:** `go test ./internal/tui/...` — 三块渲染与重放一致性断言

## T13: 端到端

**文件:** `tests/e2e/m07a_skills.sh`(新)
**依赖:** T9, T10, T11, T12
**步骤:**(fake provider 脚本,风格对齐 m06_interaction.sh)
1. 放置两种布局技能→补全可见→`/<name>` 激活→transcript 技能激活块→agent 收到正文
2. 会话运行中修改正文→LoadSkill 工具调用读到新正文;新增技能目录→delta 提醒与状态栏提示
3. fork 技能:列表可见,slash 与 LoadSkill 调用均报「子 agent 能力未启用」
4. 重启服务→清单注入与激活块投影一致、inventory 不重复、delta 不重复提醒
5. `/skills reload` 数量报告;同名冲突(内置/命令文件)技能命令被跳过
**验证:** 运行该脚本全部场景 PASS;`m05_sessions.sh`、`m06_interaction.sh` 回归通过

## T14: 全量验证收尾

**文件:** —
**依赖:** T13
**步骤:** `gofmt -l internal cmd` 为空;`go vet ./...`;`go test ./...` 全绿;核对每任务已按序提交
**验证:** 三条命令输出干净;`git log --oneline` 含各任务提交

## 执行顺序

```
B1(并行): T1  T3  T4  T5
B2(并行): T2(T1)          T6(T5)
B3(并行): T7(T2,T4,T5)    T12(T4)
B4(并行): T8(T7)   T9(T7)
B5(并行): T10(T6,T8)   T11(T2,T8)
B6:       T13(T9,T10,T11,T12) → T14
```

并行冲突协调:T8/T9 同包不同文件(service.go+protocol.go vs run.go)可并行;T10/T11 无共享文件可并行;T11 与 T12 同包不同文件、依赖不同,T12 在 B3 提前消化;T7 是 B3–B5 的汇合点。sessionlog(T4)与 skills(T1/T2)零交叉可全程并行。
