# M09-F 受控工作树与并行写入 Spec

> 状态：已批准，实施中（2026-10-07）。用户批准 M09-F 四份规格文档及其定义的实现范围；验收项取得实际证据后更新 checklist。

## 背景与目标

[M09 总范围](../M09/README.md) 要求补齐源端工作树与并行写入。源参考为 `mewcode-golang/internal/worktree/`、`internal/tools/{enter_worktree,exit_worktree}.go` 和 `internal/agents/agent_tool.go`；源码仅供只读对照。Stable 的 child 当前只读，正式文件修改必须经过候选预览、明确用户接收和长期目标独立复检。

用户可创建、进入、退出和保留所属工作树，父 agent 可将具名后台任务分派到独立写入空间；多个写入任务互不覆盖。完成结果是可审阅的候选引用及摘要，不直接合并、提交、推送或改变目标成功/证据。

## 具体需求

- **F1 归属与入口：** 工作树 ID 由服务生成，名称只是标签。每项关联 project identity、session、WorkRef、origin run/task 和 generation。创建、列表、进入、退出、保留、删除与导出均先验证归属；客户端不能传物理路径、Git 目录、authority 或候选根。一个 session 至多有一个默认进入项；每项至多一个 writer lease，独立 child 不能借用父/兄弟工作树。
- **F2 私有 Git：** 每项在受信服务 state area 建立独立裸仓库及 linked checkout。初始化来源是正式工程的受限文件快照，包括现有未提交文件，排除 `.git`、`.stable`、legacy `.mewcode` runtime/config 区；生成内部基线提交，不复制正式仓库历史、配置、hooks、remotes、credentials、alternates 或 hardlinks。正式 Git metadata 不作为工作树 common directory，永不运行正式根的 `git worktree add/prune/reset/checkout`。快照前后 project manifest 必须一致，变化则拒绝接受创建结果。
- **F3 进入与退出：** 进入只改变受信 session workspace binding 及后续 run 的作用域，不调用进程级 `os.Chdir`，不修改已运行 run 的 authority。默认退出为 keep，等待 writer 真实停止后清除 binding；原始工作树和变更保留。退出不自动导出、接收或删除。创建与进入分开可观察；`enter_worktree` 未给 ID 时创建后进入，给 ID 时仅进入本 session 已有项。活跃父/child 执行期间禁止切换其 binding。
- **F4 受控写 child：** `run_agent` 新增 `isolation: worktree`，定义可声明同名模式；缺省仍为只读。显式 isolation 选择不能扩大父权限。工作树写入工具集为 `read_file/glob/grep/write_file/edit_file/command` 与角色 tools/disallowedTools、父许可的交集；`explore`、`plan` 内建始终只读，`general-purpose` 仅在工作树模式提供受控写工具。M09-A/B/C 默认仍只读。child 无递归分派、网络/MCP、接收、删除或正式 Git 管理工具。团队写成员使用同一 F writer lease；M09-E 单独获批前不启用团队写入口。
- **F5 硬边界：** 权限门与安全打开在运行时再次检查：只能修改本项 checkout，读到的是去除私有目录的基线/project view；正式工程、兄弟 checkout、service state 和私有 bare repo 均不可访问。command 必须进入现有 Linux sandbox，挂载本 checkout 可写、基线只读和私有 run temp，隐藏 checkout `.git`、`.stable` 与 legacy `.mewcode`。无法提供文件/进程硬隔离即拒绝相应写能力，禁止本机裸执行兜底。command 还要求已配置的 workspace volume/project quota 可实际限制磁盘；无法保证时返回 unavailable，受控文件工具仍可使用。不擅自配置宿主 quota、挂载或内存文件系统。权限模式、plan 审批不能绕过该边界。
- **F6 导出与冲突：** 冻结 writer 后以创建基线 B、当前正式项目 F、当前工作树 W 逐路径比较 bytes/mode/existence。仅一方变动采用其版本；双方相同采用相同版本；双方不同或删除/修改冲突拒绝导出，列出受限路径摘要。F 中工作树未改的文件保留，避免回退其它已接收候选。用户可在保留工作树中编辑出合并结果，再通过用户专用 `worktree_resolve` 查看当前 B/F/W 差异并逐冲突路径确认采用当前 W 或 F。决策绑定 workspace ID、generation、完整 B/F/W digests、精确路径及每路径选择；仅当前快照匹配时下一次导出使用已确认版本，任一方变化要求新预览/确认。模型不能创建或确认该决策，不自动文本合并、不用 force 绕过 baseline。无未解决冲突时导出新候选，走 `FreezeCandidate`、既有 `BuildReview` 与显式 `review_accept`。export success 仅表示候选已冻结并可预览。
- **F7 正式 metadata 保全：** 当前 `candidate.BuildManifest` 排除 `.stable` 但包含 `.git`，`AcceptCandidate` 的目录交换仅恢复 `.stable`。F 上线前必须扩展受保护 metadata 的清单、版本化 manifest policy 与接受/恢复 journal：候选与工作树不携带正式或私有 Git metadata；接受后正式 `.git` 原目录/指针文件及 `.stable`、已有 legacy `.mewcode` 归位，内容与归属不被候选替换。每次保全、交换、归位阶段可恢复；未知状态保留双方且阻断，不能用 force 覆盖。旧候选/旧事务不静默重算 digest，需保留版本契约或明确阻断并重新导出/预览。
- **F8 持久恢复与清理：** 创建、binding、lease、冻结、导出、keep 与删除意图/结果先持久后广播。重启不重跑模型，不自动重启 command；通过 generation 与 sandbox identity 核对并停止本任务残留进程，标记 interrupted、保留 checkout。创建未完成按 journal 补偿；删除中断按真实 ownership 重试或标 blocked。自动清理默认关闭，不能根据名称/mtime 推定归属；保留项、dirty/untracked/ignored 文件、提交变化或状态未知不自动删除。
- **F9 展示与追溯：** TUI/工具能看到工作树 ID、标签、所属 session/work、状态、writer、基线/当前摘要、保留原因、冲突及 candidate ID。session cursor 可恢复，独立工作树任务不覆盖父 ActiveRunID。日志不保存角色正文、thinking、原始 transcript、凭据或无限 diff；任务成功不等同于目标验收。源对照差异逐项记录。

## 拟审批的默认值与协议

| 项 | 本轮默认值 |
|---|---|
| 管理区 | runtime 注入的 `appconfig.StateDir()/workspaces/<project-identity>/<workspace-id>`，0700；必须在正式根之外且无符号链接祖先 |
| 每项目录 | `baseline/`（只读项目数据）、`repo.git/`（服务私有）、`checkout/`、`run/`、原子 journal；不创建正式仓库内的工作树 |
| 身份/名称 | ID 为不透明随机值；label 为 1–64 UTF-8 字节、拒绝控制字符；label 不参与路径/ref 拼接 |
| 并行 | 复用 3 child workers、32 queue；创建/导出大文件复制共用一个 materializer，最多 8 项待处理；不另建模型池 |
| 基线快照 | 最多 20,000 文件、128 MiB 总数据、16 MiB 单文件；流式复制并复查 digest；不偷偷截断 |
| 磁盘配额 | 每工作树含 bare/基线/checkout/run 512 MiB；每项目全部工作树 2 GiB，至多 16 未移除项（含 kept/interrupted）；失败保留可恢复记录并报告配额；文件工具写前预算，command 需硬 quota |
| 执行预算 | child 最多 8 轮、3 分钟、50,000 bytes 工具输出、8 KiB 摘要、64 KiB 输入，定义/请求只能缩短；command 每次最多 90 秒且受 child deadline 限制 |
| 生命周期 | create/materialize 最多 3 分钟；query wait 最多 30 秒；停止清理最多 10 秒，未确认退出不得释放 lease 或冻结 |
| 清理 | 默认无定期自动删除；clean remove 要求当前内容等于基线且无 writer/untracked/ignored 新文件或私有提交变化；dirty discard 仅用户专用操作确认具体 ID+generation+digest |
| 列表/摘要 | 默认 20 项、最多 100，session cursor 分页；错误最多 1 KiB、摘要 8 KiB；冲突最多显示 100 路径并报告总数 |

用户入口：`/worktrees`、`/worktree create [label]`、`/worktree enter <id>`、`/worktree exit`、`/worktree keep <id>`、`/worktree export <id>`、`/worktree resolve <id>`（用户冲突预览/逐路径决策）、`/worktree remove <id>`；dirty 删除另用用户确认对话，不把 `discard_changes: true` 当作用户授权。`/agent --worktree <角色> <任务>` 显式创建独立项；已有 `/agent <角色> <任务>` 行为保持 D 的只读默认。命令解析不得把普通任务正文中的 `--worktree` 当控制参数。

父工具新增 `enter_worktree`、`exit_worktree`、`worktree_export`，`run_agent` 扩展 `isolation`；child 不提供这些 lifecycle 工具。协议新增 `worktree_create/list/get/enter/exit/keep/export/resolve/remove`，只收 ID、label、游标、有界 wait、用户确认引用及受限冲突路径选择。list/get 为只读；create/enter/export/clean remove 仍过既有权限审计；冲突 resolve、候选 accept 与 dirty discard 只由用户入口产生受信决策，模型不能自行调用。

定义只新增严格 `isolation` 字段，接受缺省/`none`/`worktree`，不接受 remote 或 permissionMode 提权。已批准 D 的 unsupported field 报错在 F 上线前仍有效；effective tools 列表显示工作树条件，不能仅因解析字段就宣称已支持写能力。

## 数据与平台边界

根 `.git`、`.stable` 和 legacy `.mewcode` 为受保护 metadata；项目数据中出现嵌套 `.git`、submodule/gitlink、symlink、hardlink（link count >1）、socket、device/FIFO、越界路径等均 fail closed。首版不执行 `.worktreeinclude`、本地 settings 复制、hooks 或依赖安装；`.gitmodules` 非空 submodule 声明明确不支持，不能悄悄漏内容。普通 regular 项目文件（含 gitignored/untracked）以快照为准，不自动判断其是“可丢弃构建产物”。不上传代码到新云服务，不调用真实 provider。

Linux 为可运行验收平台；其它平台可查看持久记录，硬 sandbox 不可用时写执行返回 unavailable。F 不加入远程隔离、tmux/iTerm 后端、正式分支管理、共享 writable checkout、自动提交/推送或目标证据提交能力。

## 源行为对照

| 源事实 | Stable F 处理 |
|---|---|
| `create.go` 用正式 `.git` 的 `worktree add -B`，默认分支不可用会 fetch | 适配：私有独立 bare + 当前文件基线，无 fetch、共享 refs 或分支重置 |
| `session.go` 全局 singleton、`os.Chdir`、单一 JSON session 文件 | 适配：按 session binding、每 run 不变 authority、持久事件与 generation |
| `setup.go` 复制 local settings、共享 hooks、symlink 大目录、`.worktreeinclude` | 明确不沿用：可能复制凭据、执行外部 hooks或写回源目录；首版均关闭并显示差异；child Git metadata 被隐藏，Git 变更管理仅服务执行 |
| `ExitWorktree` keep/remove，`discard_changes` 可由工具参数传入 | 适配：默认 keep；dirty discard 绑定真实用户确认；child 无删除入口 |
| sync Agent 的调用参数 isolation 创建工作树，dirty keep/clean remove | 适配：每 child 独立 writer、持久可查询候选；默认不自动删除 |
| `runAsync` 分支未传调用 isolation；definition `Isolation` 解析并装入 spec，但检索到的创建路径只检查调用 isolation | 源缺口：不能宣称 async/definition 已完整运行；Stable F 对同步/后台/definition 三入口各做实际验证 |
| teammate 调用 isolation 创建工作树及 Workdir | 条件适配：复用 F 服务，写团队成员要等 E/F 均验收 |
| stale cleanup 以名称/mtime筛选，`status -uno` 跳过 untracked | 适配：持久 ownership+完整 manifest；dirty/未知保留，默认禁用自动清理 |

## 验收标准

- **AC1（F1/F3）：** 跨 session/project/goal 拒绝，label 不影响路径，两个 session binding 互不干扰；进入/退出不改变进程 cwd、活跃 authority 或父 stream。
- **AC2（F2）：** 正式 Git refs/index/config/hooks与 `.stable` 不被创建或执行触碰；私有仓库无 alternates/hardlinks/remote；基线包含已有 dirty/untracked 项目文件，源变化拒绝快照。
- **AC3（F4/F5）：** fake child 的写入/command 只影响本 checkout；兄弟/正式/state/Git metadata 攻击失败，缺 sandbox 即 unavailable；同步、后台、definition isolation 各可观察，readonly/plan 权限不扩大。
- **AC4（F6）：** 多个工作树对不同文件导出不会回退先接收变更；同路径 bytes/mode/delete 冲突阻断；用户对当前 W 手工合并后可通过 digest-bound 逐路径决策继续导出，过期/不完整/模型确认拒绝；导出候选的 review digest 绑定真实版本，接受只走真实用户决策。
- **AC5（F7）：** 正式 `.git` 目录及 linked `.git` 指针文件分别验收；保护 metadata 注入/替换被拒绝；每个交换/归位 crash 点恢复幂等，旧候选/事务版本不被静默解释。
- **AC6（F8）：** queued/running/creating/exporting/removing 崩溃和重复恢复不重跑模型、不误杀他人、不丢 dirty/untracked/ignored 工作；失败清理保留 blocked 状态与 lease。
- **AC7（F8/默认配额）：** worker/queue/materializer/文件字节数/磁盘/保留项/时间/输出上限实际生效；stop 实际退出才释放 lease，dirty 删除不能由模型 bool 或过期确认触发。
- **AC8（F9）：** slash、父工具、独立任务、候选 UI、重连游标形成闭环；stage/摘要/冲突可见，角色正文/凭据/thinking不进入日志，目标状态/证据不被任务结果改写。
- **AC9（兼容/对照）：** A/B/C/D、已实施 E、候选交换/rewind、权限/计划、Session/Goal 回归及源差异表有证据；GitHub Actions 记录确切代码 SHA、run/job 与失败历史。未实现的 E 或其它平台不得写成已通过。
