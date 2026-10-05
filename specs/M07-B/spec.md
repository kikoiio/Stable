# M07-B Hooks Spec

> 状态:已批准(2026-10-06)。依据迁移地图 M07 行及三子项拆分决定(M07-A skills+命令关联 → M07-B hooks → M07-C MCP);用户已确认:配置采用两级独立文件(项目 `.stable/hooks.yaml` + 用户 `~/.config/stable/hooks.yaml`,显式 id 冲突项目覆盖用户)、事件集为 4 事件(run_start/run_end/pre_tool_use/post_tool_use,per-round 事件与 shutdown 不迁)、动作类型迁 command+prompt(http/agent 解析保留但调用报「未启用」,分别留 M07-C/M09)、hook 输出回流模型(下一轮 run 前缀注入)、pre_tool_use hook 先于权限门(hook 只能收紧不能放宽)、两类 run(会话与目标)都触发、mtime 热更新 + `/hooks` 命令。

## 背景

Stable 经 M00–M07-A 已具备统一 agent 执行(runner、工具执行、权限审批)、持久会话与压缩、统一命令注册表与技能目录发现。hooks 是用户在配置文件中定义的生命周期自动化:在执行流程的特定事件点(run 开始/结束、工具调用前后)自动执行预设动作(运行命令、产出提醒文本),并可拒绝特定工具调用。mewcode 源端 `internal/hooks` 提供行为参照:hooks 数组内嵌主配置、9 种事件、4 种动作类型、条件表达式、`once`/`async`/`on_error`、pre_tool_use 拦截、输出经系统提醒回流;但其配置无热更新(启动加载一次),`shutdown` 事件从未被触发(死枚举),session_start/end 实际按每次执行触发(≈ Stable 的 run 边界)。

本项为 M07 三个子项中的第二个(M07-B),后续 M07-C(MCP)独立走规格。

## 目标

- 两级 hooks 配置文件(项目 `<项目根>/.stable/hooks.yaml` + 用户 `~/.config/stable/hooks.yaml`),合并生效,显式 id 冲突项目覆盖用户,独立于含凭据的主配置。
- 4 种事件:run_start、run_end、pre_tool_use、post_tool_use;会话 run 与目标工作项 run 都触发。
- command(受控 shell 执行,事件上下文经环境变量传入)与 prompt(提醒文本)两种动作完整迁移;http/agent 解析保留但调用时报「未启用」。
- pre_tool_use hook 先于权限门执行,拒绝即拦截;hook 只能收紧、不能放宽权限。
- hook 输出经系统提醒回流,下一轮 run 对模型可见;触发与结果落会话事件审计。
- mtime 热更新 + `/hooks` 命令;非法配置跳过且原因可见。

## 功能需求

- F1 配置发现与解析:扫描项目级 `<项目根>/.stable/hooks.yaml` 与用户级 `~/.config/stable/hooks.yaml`,两文件合并生效,合并结果可观察:执行顺序为用户文件条目在前、项目文件条目在后;显式 id 冲突时项目条目覆盖用户条目(保留项目条目位置);同一文件内显式 id 重复时后者跳过且报告;缺 id 的 hook 自动按「来源:序号」生成稳定 id。字段:`id`、`event`、`if`、`action`(type/command/message/timeout)、`reject`、`once`、`async`、`on_error`。加载校验:事件名限 4 事件白名单;按动作类型校验必填字段(command 需非空 command;prompt 需非空 message;http/agent 字段要求解析保留);timeout 非负。非法 hook 整条跳过并在加载报告可见,不阻塞其他 hook、不阻塞服务。
- F2 事件触发:① run_start——每次 run 启动时触发;② run_end——run 到达终态时触发(完成/失败/取消/预算耗尽均触发),条件变量 message 绑定终态摘要(最终回复文本或错误信息);③ pre_tool_use——每次工具调用执行前触发;④ post_tool_use——工具执行完成后触发,message 绑定工具结果内容。同事件多 hook 按合并列表顺序执行;async hook 立即返回、后台执行。
- F3 动作执行:command 动作经 bash 执行,注入 `STABLE_EVENT`/`STABLE_TOOL`/`STABLE_FILE_PATH` 环境变量(只含事件上下文);超时上限可按 hook 配置(默认量级对齐源端 10 分钟),超时与一般失败区分可观察。prompt 动作产出配置的提醒文本。http/agent 动作调用时返回明确「未启用」错误,指明归属(http 留 M07-C、agent 留 M09)。
- F4 拦截:pre_tool_use hook 配置 `reject: true`、或动作失败且 `on_error: reject` 时,该工具调用被拒绝——不进入权限门、不执行工具,模型收到的工具结果为含 hook id 与输出的明确拒绝消息。未拒绝的调用照常经权限门判断,权限门语义不变。拒绝事实落会话事件。
- F5 条件与控制字段:`if` 条件支持 `==`、`!=`、`=~`(正则)、`=*`(glob)及 `&&`、`||`、`!` 组合,变量:`tool`、`event`、`file_path`、`message`、`args.<字段名>`。`once` hook 每会话至多触发一次(按会话隔离,新会话重新可触发)。`on_error` 取 `fail`(默认,失败按错误处理)/`ignore`(记录继续)/`reject`(仅 pre_tool_use 有拦截意义)。
- F6 输出回流:hook 执行输出(非空)进入按会话隔离的通知队列;下一次 run 启动时,该会话队列中的通知以系统提醒注入 run 前缀(与技能 delta 提醒同位置),注入后清空不重复。注入文本不落会话日志,hook 触发与输出已由事件落盘。每会话通知条数与单条长度有上限,超限截断并标注。
- F7 热更新与 /hooks:hooks 配置文件 mtime 变化后自动重载,下一次事件触发即生效;`/hooks` 内置命令无参列出全部 hook(id、事件、动作类型、来源层级、reject/once/async 标记)与加载报告(被跳过的 hook 及原因);`/hooks reload` 手动重载并报告数量变化。
- F8 审计:每次 hook 实际执行(通过条件)落会话事件(hook id、事件名、动作类型、成功与否、输出截断);拒绝、通知注入、reload 结果可观察(transcript 渲染 + 状态栏)。服务重启后已落盘事件投影一致;未注入的内存通知队列不跨重启保留(丢失但事件可查)。

## 非功能需求

- N1 一致性与审计:hook 触发、拒绝、通知、reload 结果全部落会话日志,重启后投影一致;损坏日志明确失败,不回退陈旧状态(延续 M05 语义)。通知注入文本不落日志,与 M07-A 清单注入同模式(per-run 上下文行为,审计走 hook 事件)。
- N2 安全边界:hooks 配置只来自用户编辑的两级文件——不提供任何供模型创建或修改 hook 的工具/op,模型无法改动 hook 配置;hook 动作属控制面用户自动化(等同用户本人执行),不经沙箱执行,但其输出进入会话上下文时沿用既有脱敏管线,不因 hook 通道绕过;hook 拒绝只能收紧不能放宽——权限门语义完全不变;hook 通道不构成验收证据,不触碰候选区接收与正式工程只读边界;hooks 配置文件为符号链接(指向边界之外)时跳过并报告(对齐 M06 loader 惯例)。
- N3 归属隔离:hooks 按来源层级(项目/用户)区分并在列表可见;项目级 hooks 不跨项目生效;once 状态与通知队列按会话隔离,跨会话不共享。
- N4 可观察性:id 冲突覆盖、同文件重复跳过、非法 hook 跳过、http/agent 未启用报错、hook 拒绝、命令超时、热更新结果均有明确可见反馈或日志,不静默吞掉。
- N5 有界性:hooks 配置单文件大小、hook 数量上限(量级对齐 M06 commands loader 与 M07-A 技能常量);hook 输出落盘有截断上限;每会话通知队列条数与单条长度有上限,超限截断并标注;command 动作有超时上限;热更新检查轻量(mtime 对比),不在事件触发路径上做全量重扫;非法配置、不可读文件不阻塞输入、不使 TUI 或服务崩溃。

## 不做的事

- 不做 http 动作执行(网络出口语义与 M07-C MCP 一起定;字段解析保留,调用报「未启用」)。
- 不做 agent 动作执行(单轮 LLM 调用的预算与费用归属语义与 M09 子 agent 一起定;字段解析保留,调用报「未启用」)。
- 不做 per-round 事件(turn_start/turn_end/pre_send/post_receive)与 shutdown 事件(前者与 run 边界语义重复,后者源端从未触发)。
- 不做 hooks 配置的编辑 UI、管理界面与字段级 lint(超出必填校验之外)。
- 不做 hook 的跨项目共享、导入导出机制。
- 不做独立的 hook 执行历史时间线 UI(审计走会话事件投影,复用 transcript 渲染)。
- 不改 M03 权限门与 M04 工具协议的任何语义——hook 是新增的前置否决点,不是权限规则的新形态。
- 不做 hooks 的密钥注入 / secret 管理机制(hook 命令继承服务进程环境,等同用户本人执行;注入的环境变量只含事件上下文)。

## 验收标准

- AC1(对应 F1):两个配置文件的 hook 均被发现与解析;合并规则生效——用户+项目条目都生效、显式 id 冲突项目覆盖、同文件重复跳过;非法 hook 跳过且来源/原因在加载报告可见;`/hooks` 列表与实际加载一致。
- AC2(对应 F2):run_start/run_end 在会话 run 与目标 run 中各触发一次,run 终态各分支(完成/失败/取消/预算耗尽)均触发 run_end;pre_tool_use/post_tool_use 在每次工具调用前后触发;同事件多 hook 按合并列表顺序执行;async hook 不阻塞主流程且输出仍回流。
- AC3(对应 F3):command hook 进程收到 `STABLE_EVENT`/`STABLE_TOOL`/`STABLE_FILE_PATH` 并产出输出;超时有明确报告且与一般失败可区分;prompt hook 产出提醒文本;http/agent 调用返回指明归属(M07-C/M09)的「未启用」错误。
- AC4(对应 F4):`reject` hook 使工具调用被拦截——模型收到含 hook id 的拒绝结果,权限门未被调用,拒绝事实落会话事件;`on_error: reject` 的失败动作同样拦截;`fail`/`ignore` 语义正确。
- AC5(对应 F5):条件表达式四种比较与 `&&`/`||`/`!` 组合正确(以工具名/文件路径/参数为变量验证);once hook 同会话第二次不触发、新会话重新可触发。
- AC6(对应 F6):hook 输出在下一次 run 中以系统提醒对模型可见,注入后不重复;注入文本不落会话日志而 hook 事件落盘;通知超限截断可观察。
- AC7(对应 F7):修改 hooks 配置文件后无需重启,下一次事件触发即生效;新增/删除 hook 可感知;`/hooks reload` 报告数量变化。
- AC8(对应 F8/N1):hook 触发/拒绝/reload 落会话事件,重启后 transcript 投影一致。
- AC9(对应 N5):超限(文件大小/hook 数量/输出长度/通知条数)与异常(非法配置、不可读文件、符号链接)被跳过或截断且有报告;TUI 与会话服务不崩溃,输入不被阻塞。
