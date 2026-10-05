# M07-A 技能(Skills)与命令关联 Plan

> 状态:已批准(2026-10-06)。依据已批准的 [spec.md](spec.md)。spec 六条 F 的架构归属:F1→internal/skills,F2→TUI+命令注册表,F3→execution 主机工具+两处白名单,F4→startRun 注入+事件,F5→激活单一入口+清单注入,F6→fork 检查。task/checklist 待批。

## 架构概览

- **独立 skills 模块(`internal/skills`,零内部依赖)**:移植 mewcode 的两阶段 catalog——phase-1 只读 frontmatter(轻量重扫),`GetFull` 每次现读正文(正文级热更新),`NeedsReload` 以目录 mtime 快照检测增删。裁剪:去内置技能 embed、去 eager 遗留加载器、去 fork 执行与网络安装;目录路径改为 Stable 惯例(用户级 `~/.config/stable/skills`、项目级 `.stable/skills`);YAML 解析用 `gopkg.in/yaml.v3`(已在依赖树,转正为直接依赖)。
- **激活逻辑下沉服务侧(conversation)**:TUI 与 conversation 服务是两个进程,技能目录各持一份 catalog 实例,靠 mtime 检测各自收敛。斜杠命令入口(TUI)发 `skill_invoke` op,工具入口(execution 主机工具)经 `SkillProvider` 接口——两条入口汇聚到服务侧同一激活函数:fork 检查 → `GetFull` 现读 → 渲染 → 落 `skill_invoked` 事件 → 更新会话激活清单;斜杠命令路径把渲染正文作为用户消息走现有 `startRun`。
- **会话暴露(conversation/run)**:仿 M06 计划提醒模式,在 `startRun` 的 prefix 组装处注入技能清单段(快照稳定)与新增技能提醒;首次运行落 `skill_inventory` 快照事件,新增时落 `skill_delta` 事件。重启后从事件重放重建快照与已通知集合,注入内容一致。
- **LoadSkill 主机工具(execution)**:完整复制 M06 六件套接线(schema → 两处 nameMap 白名单 → `executeHostTool` 分支 → provider 接口 + option → conversation adapter)。read 类、免权限审批(同 `ask_user` 先例——技能文件是用户显式放置的受信内容,且沙箱内无法访问用户级目录,必须是主机侧读取)。
- **会话事件(sessionlog)**:新增 `skill_inventory` / `skill_delta` / `skill_invoked` 三种事件,走白名单 4 处 + owned-append 校验 + 投影 + transcript 渲染的既有扩展流程。
- **TUI 集成**:技能命令注册进 M06 命令注册表(`KindLocal`,handler 发 `skill_invoke` op,保证 fork/未知/读失败错误由服务统一返回);新增 `/skills` 内置命令(列表 + reload);补全描述加「(技能)」后缀;冲突与异常文件报告复用现有状态栏报告通道。

## 核心数据结构

### internal/skills/skills.go

```go
type SkillMeta struct {
    Name        string   `yaml:"name"`
    Description string   `yaml:"description"`
    WhenToUse   string   `yaml:"when_to_use"`
    Tags        []string `yaml:"tags"`
    Mode        string   `yaml:"mode"`          // "" → inline;"fork" 仅解析
    Context     string   `yaml:"context"`       // 遗留:=="fork" 视同 fork
    ForkContext string   `yaml:"fork_context"`  // 解析保留,M09 前无行为
}
func (m SkillMeta) IsFork() bool

type Skill struct {
    Meta       SkillMeta
    PromptBody string // phase-1 为空,GetFull 现读
    SourceDir  string
    Source     string // "user" | "project"
    BodyLoaded bool
}
```

### internal/skills/catalog.go

```go
type Catalog struct { /* skills map + dirModTimes 快照 */ }
func LoadCatalog(userDir, projectDir string) *Catalog  // 项目覆盖用户
func (c *Catalog) Get(name string) (*Skill, bool)
func (c *Catalog) GetFull(name string) (*Skill, error) // 每次现读正文;读失败保留旧正文并返回错误
func (c *Catalog) List() []Skill                       // 按名排序
func (c *Catalog) NeedsReload() bool                   // 目录 mtime 对比
func (c *Catalog) Reload()                             // 重建 + 重拍快照
func (c *Catalog) Rejections() []string                // 被跳过技能的「路径:原因」报告
```

### internal/sessionlog/events.go 新增

```go
type SkillInfo struct { Name, Description, WhenToUse, Source string }
type SkillInventory struct { Skills []SkillInfo }            // 每会话至多 1 条(owned-append)
type SkillDelta     struct { Added []SkillInfo }             // 数量上限校验
type SkillInvoked   struct { Name, Source, Entry, Args string } // Entry: "slash"|"tool"
// 事件常量:EventSkillInventory / EventSkillDelta / EventSkillInvoked
```

### internal/execution/executor_factory.go 新增

```go
type SkillProvider interface {
    // 激活并返回渲染后正文;fork/未知/读失败返回错误
    LoadSkill(ctx context.Context, sessionID, name, args string) (string, error)
    // 返回注入用清单快照;检测新增技能时落 skill_delta 并返回提醒文本
    SkillInventory(ctx context.Context, sessionID string) (snapshotText, deltaReminder string, err error)
}
func WithSkillProvider(p SkillProvider) ToolExecutorOption
```

## 模块设计

### 模块 1:internal/skills(新建,零内部依赖)
**职责:** 技能目录扫描、两种布局解析、两阶段加载、热更新检测、界限与拒绝报告。
**对外接口:** `LoadCatalog(userDir, projectDir)`、`Catalog.Get/GetFull/List/NeedsReload/Reload/Rejections`。
**依赖:** 仅标准库 + `gopkg.in/yaml.v3`。
**界限常量:** `MaxSkills=200`、`MaxFileBytes=256KB`(对齐 M06 commands loader 量级);技能目录仅一层直接子目录,不递归;子目录为符号链接→拒绝并报告;附属文件(references/ 等)不读取。

### 模块 2:internal/appconfig(修改一处)
**职责:** 新增 `UserSkillsDir() (string, error)`——`XDG_CONFIG_HOME`/`$HOME/.config` 拼 `stable/skills`,与 `ConfigPath()` 目录逻辑同源;仅供生产接线调用,skills 包与测试显式传目录。

### 模块 3:internal/execution(修改)
**职责:** `load_skill` 主机工具。
**改动点:**
- `tools_schema.go`:新增 `loadSkillSchema`(`name` 必填参数,静态描述)与 `SkillToolSchemas()` 聚合函数(仿 `M06ToolSchemas()`)。
- `tool_executor.go`:`executeHostTool` 增加 `case "load_skill"`——nil provider 返回明确错误;调用 `SkillProvider.LoadSkill(ctx, sessionID, name, args)`,成功返回 `# Skill: <name>\n\n<body>`。免 Gate(位于 `Gate.Authorize` 之前的 M06 主机分支,与 `ask_user` 同先例)。
- `executor_factory.go`:`SkillProvider` 接口 + `WithSkillProvider` option。
**依赖:** `internal/agent`(既有)。

### 模块 4:internal/conversation(修改,激活与暴露核心)
**职责:** 技能激活单一入口、会话技能状态、清单注入、delta 检测、事件落盘、op 协议扩展。
**改动点:**
- 新建 `internal/conversation/skills.go`:`SkillGate` 类型——持有 `*skills.Catalog`、项目/用户目录、`s.eventMu` 下落事件;核心方法:
  - `activate(sessionID, name, args, entry) (body string, err error)`:fork 检查(IsFork→「子 agent 能力未启用(fork 模式留待 M09)」)→ `GetFull` 现读 → `commands.ExpandPrompt(body, args)` 渲染 → 落 `skill_invoked` → 更新会话激活清单;
  - `ensureInventory(sessionID)`:首次 run 落 `skill_inventory` 快照事件(已落过则跳过);
  - `refreshAndDiff(sessionID)`:`NeedsReload`→`Reload`→与快照 diff→新增集非空落 `skill_delta` 并返回提醒文本;
  - `restore(sessionID)`:从 sessionlog 重放三类事件,重建快照/已通知集合/激活清单(惰性,首次触碰会话时)。
  - 实现 execution 的 `SkillProvider` 接口(LoadSkill→activate(entry="tool");SkillInventory→ensureInventory+refreshAndDiff)。
- `service.go`:`Service` 持有 `skills *SkillGate`(可 nil——未接线时功能明确关闭);op 分发新增 `skill_invoke`、`skill_reload`。
- `run.go`(`startRun`):prefix 组装处、system prefix 之后、计划提醒之前注入——①`ensureInventory`;②`refreshAndDiff` 产生的 delta 提醒(如有);③清单快照段(「可用技能」名称+描述+when_to_use+调用提示;已激活技能另起一行注明「正文已在会话早期提供,如已被压缩可 LoadSkill 重新加载」)。清单文本不落日志(与计划提醒同模式),事件已审计。
- `protocol.go`:`ClientMsg` 增加 `SkillName/SkillArgs` 字段;`validOp` 增加 `skill_invoke`/`skill_reload`;`ServerMsg` 增加 `SkillReport`(激活错误、reload 数量报告、delta 通知推送)。
**依赖:** `internal/skills`、`internal/commands`(ExpandPrompt)、`internal/sessionlog`、经 `SkillProvider` 接口与 execution 反转协作。

### 模块 5:internal/sessionlog(修改)
**改动点:** `events.go`(3 个事件常量 + 3 个 Data 类型 + 数量上限常量);`log.go`(Append/replay 两处白名单);`validate.go`(owned-append:`skill_inventory` 每会话至多 1 条,`skill_delta`/`skill_invoked` 格式与数量校验);`projection.go`(ItemKind + 3 个 case)。

### 模块 6:internal/tui(修改)
**改动点:**
- `model.go`:构造 TUI 侧 `*skills.Catalog`(appconfig.UserSkillsDir + 项目 `.stable/skills`);`refreshCommands` 在自定义命令之后注册技能命令(`RegisterOptional`,冲突被跳过→状态栏报告,实现「内置>命令文件>技能」);技能命令 `KindLocal`,handler 发 `skill_invoke` op;`registerBuiltins` 新增 `/skills`(无参列表 + `reload` 子命令:本地 Reload + 发 `skill_reload` op);`ServerMsg` 处理 SkillReport(状态栏/通知)。
- `transcript.go`:3 类事件的渲染块(`skill_invoked`→「技能激活:name(来源)」;`skill_inventory`→清单摘要块;`skill_delta`→「新增技能」块)。
- `completion.go`:无需改动(后缀加在 Command.Description 上)。

### 模块 7:进程接线(修改)
- `cmd/stable/chatserve.go`:`chatserveToolSchemas` nameMap 加 `"load_skill"`;构造 `SkillGate` 并 `WithSkillProvider` 注入。
- `internal/runtime/supervisor.go`:`runtimeToolSchemas` nameMap 加 `"load_skill"`;同上注入。

## 模块交互

### 链路 1:斜杠命令激活(用户入口)
1. 用户输入 `/code-review 性能` → TUI `refreshCommands` 命中技能命令(`KindLocal`)→ handler 发 `ClientMsg{Op:"skill_invoke", SkillName:"code-review", SkillArgs:"性能"}`。
2. 服务侧 op 分发 → `SkillGate.activate(session, "code-review", "性能", entry="slash")`:IsFork 检查 → `GetFull` 现读正文 → `commands.ExpandPrompt` 渲染 → 落 `skill_invoked` 事件 → 激活清单追加。
3. 服务以渲染正文为 Intent 走既有 `startRun`:正文作为最后一条 user 消息落 `EventMessage` 并进入本次 run 上下文(agent 下一轮可见)。
4. 失败(fork/未知/读失败)经 `ServerMsg.SkillReport` 返回,TUI 状态栏显示,不产生会话消息。

### 链路 2:LoadSkill 工具激活(agent 入口)
1. agent 发起 `load_skill{name}` 工具调用 → runner → executor `Execute` → `executeHostTool` 命中(Gate 之前)→ `SkillProvider.LoadSkill`。
2. 同一 `activate`(entry="tool");正文作为工具结果 `# Skill: <name>\n\n<body>` 返回,经 `EventToolCall/EventToolExecResult` 落盘(50000 字符上限内截断由既有工具输出限制处理)。
3. 会话激活清单更新,后续每轮注入清单时含该技能。

### 链路 3:清单注入与 delta 检测(每次 startRun)
1. `startRun` prefix 组装:system prefix → **技能段**(`ensureInventory` 首次落 `skill_inventory`;`refreshAndDiff` 检测目录变化,新增集落 `skill_delta` 并生成提醒文本)→ 计划提醒 → 历史 → 本次消息。
2. 技能源于服务侧 catalog 快照(会话内稳定);提醒文本仅当本轮有新增时出现。
3. `skill_delta` 落盘同时 `ServerMsg` 推送,TUI 状态栏一次性提示。

### 链路 4:重启恢复
1. 服务重启后会话首次触碰时 `restore`:重放该会话日志中三类事件 → 重建清单快照、已通知集合(全部视为已通知,不重复提醒)、激活清单 → 后续 run 注入内容与重启前一致,`skill_inventory` 不重复落(owned-append 校验保证)。

### 链路 5:热更新与 /skills reload
1. TUI 侧:`refreshCompletions`/`dispatchCommand` 每次触发时 `NeedsReload`(mtime 对比)→ 变化则 `Reload` + 重注册技能命令 → 补全即时感知;`Rejections` 复用状态栏报告通道。
2. 服务侧:随链路 3 每次 startRun 检测;正文级变化无需检测(`GetFull` 现读)。
3. `/skills reload`:TUI 本地 `Reload` + 发 `skill_reload` op → 服务 `Reload` + 快照重算(下次 run 生效)→ `ServerMsg.SkillReport` 返回数量变化。
4. `/skills` 无参:TUI 本地 catalog 列表(名称、描述、来源、`/name` 调用提示)+ 当前会话已激活清单(经 ServerMsg 查询或事件投影)。

## 文件组织

```
internal/
├── skills/                        [新建]
│   ├── skills.go                  — SkillMeta/Skill/IsFork
│   ├── parser.go                  — 两种布局解析、默认回填、界限校验
│   ├── catalog.go                 — 两阶段 Catalog、NeedsReload/Reload、Rejections
│   ├── parser_test.go
│   ├── catalog_test.go
│   └── skills_test.go
├── sessionlog/
│   ├── events.go                  [修改] 3 事件 + Data 类型
│   ├── log.go                     [修改] 白名单 ×2
│   ├── validate.go                [修改] owned-append 校验
│   ├── projection.go              [修改] ItemKind + case ×3
│   └── (各自 *_test.go 追加)
├── execution/
│   ├── tools_schema.go            [修改] loadSkillSchema + SkillToolSchemas
│   ├── tool_executor.go           [修改] executeHostTool 分支
│   ├── executor_factory.go        [修改] SkillProvider + option
│   └── tool_executor_test.go      [修改]
├── conversation/
│   ├── skills.go                  [新建] SkillGate、激活、快照/diff/重建
│   ├── skills_test.go             [新建]
│   ├── service.go                 [修改] 持有 SkillGate、op 分发
│   ├── run.go                     [修改] startRun prefix 注入
│   └── protocol.go                [修改] op 与消息字段
├── tui/
│   ├── model.go                   [修改] catalog 接线、技能命令、/skills、报告
│   ├── transcript.go              [修改] 3 类事件渲染
│   └── model_test.go / transcript_test.go [修改]
├── appconfig/config.go            [修改] UserSkillsDir
cmd/stable/chatserve.go            [修改] nameMap + 注入
internal/runtime/supervisor.go     [修改] nameMap + 注入
tests/e2e/m07a_skills.sh           [新建] 端到端场景
```

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| D1 catalog 实例归属 | TUI 与服务各持一份,靠 mtime 检测收敛 | 两进程边界(chatserve/supervise);与 M06 commands loader 仅在 TUI 不同——技能服务侧需要(清单注入/LoadSkill),TUI 侧也需要(补全//skills);mtime 检测轻量且幂等 |
| D2 技能命令用 KindLocal 发 op,而非 KindPrompt 直发正文 | 服务侧统一激活 | fork/未知/读失败错误需服务侧判定;`KindPrompt` 的 Body 在注册时固定,违背正文现读;单一激活路径保证审计与清单一致 |
| D3 LoadSkill 免权限审批(主机工具,Gate 之前) | 同 `ask_user` 先例 | 技能文件是用户显式放置的受信内容,与自定义命令同级;沙箱内无法访问用户级目录,必须主机侧读取;只读、无写路径、无网络 |
| D4 事件只记轻量元数据,正文由既有消息/工具结果承载 | `skill_invoked` 不含正文 | 256KB 正文进 sessionlog 会膨胀;正文已分别经 `EventMessage`(slash)与 `EventToolResult`(tool)落盘,事件与正文可按时间序对应 |
| D5 SkillProvider 接口定义在 execution,conversation 实现 | M06 TodoProvider 同构 | execution 不反向依赖 conversation;与既有 QuestionSink/TodoProvider/PlanSink 模式一致 |
| D6 渲染复用 `commands.ExpandPrompt` | 单一实现 | 语义与 M06 自定义命令完全一致($ARGUMENTS 两形态),避免双实现漂移;conversation 新增对 commands 的依赖(commands 零内部依赖,无环) |
| D7 YAML 解析用 `gopkg.in/yaml.v3` | 转正既有 indirect 依赖 | frontmatter 字段多且 tags 为数组,手写解析脆弱;mewcode 同款,已在依赖树 |
| D8 清单注入文本不落日志,审计走事件 | 与计划提醒同模式 | per-run 注入是上下文行为而非会话事实;`skill_inventory`/`skill_delta` 事件已完整审计,重启投影一致由事件重放保证 |
| D9 压缩后保留机制 | 每轮轻量注入已激活技能**清单**(名称+描述),正文经 LoadSkill 现读重取 | 对齐源端 compaction 语义(SOP 指令不丢)而正文不重复注入(上下文成本);正文永远新鲜(现读);spec 已将机制留 plan 决定 |
| D10 不做 /clear | 新会话自然隔离 | 调研确认 Stable 无 /clear 语义;激活清单按 sessionID 隔离,`session_create` 即彻底清空 |
| D11 符号链接技能目录拒绝 | 对齐 M06 plan/todo 先例 | 技能目录边界之外的内容不因技能通道进入上下文 |
| D12 owned-append 校验强度 | inventory 每会话严格 1 条;delta/invoked 格式+数量校验,不做唯一性 | 技能可删后重装,同名 delta 重复合法;严格唯一会造成误拒;inventory 重复必然是缺陷 |
| D13 会话内清单快照固定 | 首次 run 落 inventory,后续不变;新增走 delta | spec F4「清单按快照稳定」;删除技能不影响已注入快照,LoadSkill 调用时自然报未知错误 |
