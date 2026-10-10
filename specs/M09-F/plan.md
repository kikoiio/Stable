# M09-F 受控工作树与并行写入 Plan

> 状态：已批准，实施中（2026-10-07）。用户批准 M09-F 四份规格文档及其定义的实现范围；按任务 DAG 实施，重型验证按 checklist 执行。

## 架构决策

新增 `internal/workspace` 管理可信 ownership、状态、私有 Git、writer lease、文件配额与导出。conversation 提供 session 操作，execution 只依赖窄接口；runtime 提供正式根、state area、candidate parent、sandbox 与 store。新管理器不拥有正式项目写权限，只有既有 candidate acceptance coordinator 可改正式文件。

project identity由既有正式路径注册得到的稳定项目ID标识，不能用会随候选目录交换改变的inode作为永久ID；root identity另用于每次操作的重验证。

每工作树单独 `repo.git`，不复用正式仓库或其它工作树 Git common directory。初始化从经安全 manifest 验证的项目数据建立合成基线 commit；Git history 仅代表本隔离项，不能宣传为正式分支或正式提交历史。linked checkout 的 `.git` 由服务生成和验证，但在 child 文件工具与 command 中不可见/不可改；状态/diff/export 由服务接口完成。保留完整当前文件基线，使已有用户未提交内容不会被忽略。

管理区用 `appconfig.StateDir()` 受信配置，不接受客户端根路径；与正式根不能包含/重叠，全部祖先无链接且归属受信用户。工作树可与正式工程不同文件系统，导出时流式复制到既有 same-volume candidate parent；不得因 stateDir 不同盘而改变候选交换要求。

## 共享契约

```go
// internal/workspace；字段 JSON 键由主集成任务定稿。
type Scope struct {
    ProjectID, SessionID, OriginRunID, OriginTaskID string
    Work agent.WorkRef
    Authority permission.Authority // 仅受信内部调用，公开协议无此字段
}
type Snapshot struct {
    ID, Label, SessionID, State, WriterRunID, CandidateID string
    Generation, Cursor uint64
    BaselineDigest, WorkspaceDigest string
    ChangedFiles, ConflictCount int
    Conflicts []string
    Summary, Error string
}
type WriterLease struct {
    WorkspaceID, RunID string
    Generation uint64
    Authority permission.Authority
    // 物理根仅用于受信 executor/profile，不能从 tool args 构造。
}
type Service interface {
    Create(context.Context, Scope, string) (Snapshot, error)
    Get(context.Context, Scope, string) (Snapshot, error)
    List(context.Context, Scope, uint64, int) ([]Snapshot, error)
    Enter(context.Context, Scope, string) (Snapshot, error)
    Exit(context.Context, Scope) (Snapshot, error) // keep 为默认
    Keep(context.Context, Scope, string) (Snapshot, error)
    AcquireWriter(context.Context, Scope, string, string) (WriterLease, error)
    StopWriter(context.Context, Scope, string) (Snapshot, error)
    Export(context.Context, Scope, string) (Snapshot, error)
    RemoveClean(context.Context, Scope, string) (Snapshot, error)
}
```

冲突 resolve 与 dirty discard 是单独用户专用服务操作，不在上方模型可用 Service 工具接口中。resolve决策包含 user/session/workspace/generation、完整 B/F/W digests、精确冲突路径和每路径 `use_workspace/use_formal` 选择，使用服务生成预览与受信用户决策记录。dirty discard 要求不可伪造的确认记录，绑定 user/session/workspace/generation/digest，并在实际删除前重新验证。它不加入 child/parent 模型工具；受信 UI 必须先显示完整保留/导出选择及将丢弃的变更摘要。复用权限、tool call/result、snapshot、session cursor，避免 execution 导入 conversation。

公开 Snapshot 不返回 authority、private Git path、definition body 或原始 diff。所有操作幂等键包含 workspace ID、generation、operation ID；重复操作不能创建第二项、重复导出或复用过期 writer。

## 生命周期与持久事实

工作树持久态 `creating → ready → writing → stopping → kept`；可从 ready/kept 进入 `exporting → exported` 或 `removing → removed`，失败进入 `blocked`，运行中断进入 `interrupted`。binding 是单独 session 事件，writer generation 是单独单写者租约；不以“当前 UI 目录”代替 authority。exported 保留工作树，不能自动回写正式根；再次申请writer要递增generation，已导出candidate保持冻结、不能随工作树继续编辑而变化。

`RunStarted` 增可选 `WorkspaceID/WorkspaceGeneration`，工作树事件包含受信 WorkRef、origin、ID、generation、阶段、cursor、candidate引用和受限结果。不能同时拥有不同 session/goal 的 writer。任务 terminal 及 workspace keep/export 状态分别持久，模型成功可以留下 dirty 工作树，不自动表示候选创建/接受成功。

服务 journal 是物理资源的 ownership/reconciliation 记录，session log 是公开状态事实；不构建第二套随意可写的状态源。每操作先写 intent，完成后写 outcome；广播失败可重放，持久失败保留可恢复 intent且不报告完成。创建期间 root身份、quota 预留与输出均记录，不能仅根据目录名接管碰巧存在的路径。

同一 session 禁止 active run 中切换 binding；未来 run 从服务器 binding 构造新 authority。`exit/keep` 对 writer 先发取消，退出/冻结只在 sandbox 子进程确认结束后完成。child 显式父取消、service.Close 等沿 D 取消路径连接 lease；父正常完成和断线不改变已接受后台任务语义。E 团队服务只通过接口传受信成员 WorkRef，不拥有裸目录。

## 安全文件快照与私有 Git

1. 获取已验证 project root handle，扫描 project-v2 manifest；根 `.git/.stable/.mewcode` 不复制，嵌套 Git、submodule、链接和特殊文件拒绝。检查文件/总字节/项目配额，预留工作树预算；扫描/复制都受取消与 materializer 队列限制。
2. 安全 handle 流式复制到 baseline，复制后重算 source 与 baseline digest；任何变化失败，保留状态原因并仅清理本次确认归属的未接受临时文件。
3. 在 private bare 使用固定 argv Git plumbing 建合成树/commit及本项 checkout。Git env 清空 `GIT_*`、全局/system/user config和credentials，禁用 config include、hooks/template、签名、外部 filter/fsmonitor/diff/textconv 和网络协议；只用服务显式 argv，不拼 shell或用户 ref。不打开正式 Git config 或解析其 commondir/alternates。
4. 每项私有 repo 禁用 alternates、object hardlinks、remotes、外部 hooks、replace refs 和 symlink；验证 `.git` pointer 始终指向本项内部。基线 data root 不带 metadata，child看不到历史秘密或其它工作树对象。
5. 服务只执行创建/检查/冻结私有 Git 所需操作，进程有统一超时/输出限制。`.git` 指针/裸仓库写仅服务所有；agent `command` 在 checkout `.git` 只读 mask 下不能运行修改私有 Git 的命令。

`.gitmodules`、`.gitattributes` 不被解释为执行入口：非空 submodule 声明拒绝；树生成避免 filters/checkout转换，使用安全逐文件 materialization而非受项目属性影响的 Git checkout。既有 `.worktreeinclude` 不作为复制规则。

## 权限与 command sandbox

受信 writer authority 绑定独立 child run、session/goal/workitem，AllowedRoot 指向 sanitized baseline，CandidateRoot 指向 checkout，FormalRoot 保留真实正式根防越界，mode 仅继承/收窄，network grant为空。角色/调用者不能传 permissionMode、Git path、additional mounts或环境凭据。plan 状态硬拒绝项目写与 command，只有父既有 plan-file路径例外，不把该例外传给 child。

工作树 executor 与 read-only factory 分开明确能力：普通 D 使用原只读 factory；F factory 不设置 `WithReadOnlyTools`，但提供固定六工具 allowlist、workspace lease/generation检查、受保护路径拒绝、gate和快照。写工具预算预留在单writer锁下完成，失败释放预留；删除/重命名/二进制及chmod也计入候选变化。

现有 `sandbox.SandboxProfile` 挂 project RO/candidate RW/run RW，F 扩展只允许服务构造的 protected mask，不允许客户端设置 mask/mount；profile validator验证根不重叠、lease identity、没有其它workspace/正式metadata。`.git/.stable/.mewcode` 为只读 empty file/dir mask，整个根不能改名/替换，formal/state/bare 根不挂载。网络unshare、无凭据环境、取消整组进程沿用既有接口。

command需可验证的磁盘hard quota（预先配置 volume/project quota）；现有 bwrap 不提供该保证，不能把周期scan当hard上限。无quota时该能力 unavailable，写文件工具仍严格计量可用；不得自动改宿主quota/挂载/tmpfs。Actions专用fixture可配置临时独立quota设备并清理。测试负载遵守云端优先，运行时不将任意用户 command 自动上传云端。

## 导出与候选连接

导出先确认 writer退出、冻结 checkout identity+digest、禁止修改 lease。三方比较统一 versioned project-v2 manifest：

| 基线 B / 正式 F / 工作树 W | 输出 |
|---|---|
| W=B | 使用 F，包括正式中新文件 |
| F=B | 使用 W，包括工作树删除/模式变化 |
| F=W | 使用相同版本 |
| 其它 | 冲突，未创建可接受候选 |

比较包括路径存在性、bytes digest、mode，路径rename按delete/add处理；导出数据同样受128 MiB/20,000文件/16 MiB单文件限制；构建产物不能因被gitignore就自动丢弃，超限需用户在工作树中明确处理。

不做自动文本merge。冲突输出绑定 B/F/W digests；用户可通过正常受控文件编辑产生手工合并的W，然后重新生成B/F/W冲突预览，逐路径确认采用当前W或F；服务持久化resolution ID及完整快照digests/每路径选择。Export仅在B/F/W与generation完全匹配、所有冲突路径均被明确覆盖时采用该决策；预览后任一方变化、额外/缺失路径、复用其它session决策或模型提供确认都拒绝。

resolution不自动改baseline B，不启动模型、不写正式文件；它只授权此次合成候选的冲突路径版本。相同ExportID和快照的重复请求返回同一候选；下一代写入、重新冻结或新的F使旧resolution失效。用户选择经过手工合并的W可实际消除三方冲突阻断，而不是要求W恰好等于B/F。候选生成后仍需正常review和单独用户accept。不存在“模型force接受冲突”。

候选从当前正式F建立 same-volume项目数据，然后应用已验证合成manifest；导出全程前后复核F/W。保存 `WorkspaceID/Generation/ExportID/BaselinePolicy` 关联，再 `FreezeCandidate`→既有review/checkers→用户review_accept。Export操作ID与digests用于去重，同一冻结版本重复返回相同候选；改动后须新generation/新候选。接受后再次导出先前基线会按三方规则识别冲突，不自动rebase。

## 正式 metadata 与版本迁移

F不能仅给manifest跳过`.git`：现有directory exchange会删掉未复制的正式metadata。新增versioned protected metadata policy，v2根`.git`（目录或regular指针文件）和`.stable`、legacy `.mewcode`只由受信transaction保全，所有候选必须不存在这些实体。目录交换journal记录其root identity、保全位置、每个移动phase；复制/替换的伪metadata始终硬拒绝。

保全/交换/归位持有同一project acceptance锁，协调Stable writer和session append。metadata只移动原实体，不从workspace导入、不追随formal `.git`指针去改外部Git。对外部并发修改重查identity与目标存在性，冲突保留双方、标blocked，绝不删除新metadata来让事务看起来成功。正式内容交换不能向用户声称metadata归位前已完成。

`Candidate/Review/transaction`保存manifest policy版本；缺省旧版本按原digest读取，不能重算旧review后继续使用旧确认。非Git旧候选可以原契约验证；含Git metadata的旧candidate需要安全重新导出和新preview，旧未完成Git事务必须显式reconcile，不运行猜测性的自动清理。accept/rewind/recovery都使用同一保护契约，不能只修accept入口。

## 恢复与清理策略

重启按持久ownership/operation journal和真实root身份协调：creating未完成只补偿自己临时项；queued/running模型不重启，writer lease进入interrupted并核对sandbox PID+启动身份+generation；无法确认退出保留blocked lease。export intent缺candidate完整事实时拒绝发布ready candidate；存在完整冻结candidate时补公开outcome，不重复创建。removing失败保留journal和仍存在的路径，不回退到名称模式扫描删除。

clean removal验证完整data manifest等于baseline、没有ignored/untracked新文件、没有新增private commits、无writer/binding；未知状态等同dirty。dirty user discard确认必须绑定最新digest和generation，删除前重新核对；remove只涉及本项private目录，无formal worktree prune或正式branch删除。默认无stale loop，保留项占用配额，UI提供明确保留/导出/删除选择。

## 文件边界与验证

| 所有者 | 文件范围 | 职责 |
|---|---|---|
| workspace核心 | `internal/workspace/{contract,ownership,manifest,git,lease,export,recovery}.go` 与fixtures | 安全快照、私有Git、配额、三方比较、物理journal |
| metadata与候选 | `internal/candidate`、store candidate/transaction/recovery契约 | v2 policy、保护metadata、accept/rewind迁移与crash恢复 |
| sessionlog/conversation | workspace events/projection、新workspace ops、run/agent task接口 | 持久归属/binding、工具/用户决策、独立通知 |
| execution/sandbox | workspace factory、path gate、profile/masks/quota probe | child受控写、进程/磁盘hard边界 |
| catalog/team/runtime | definition isolation、D/E接入、supervisor lifecycle | 显式能力、单pool、受信state root与关闭 |
| TUI/client | slash、工作树/冲突/候选视图、用户conflict resolution/discard confirmation | 独立订阅、恢复、明确用户接收 |

验证用临时项目和fake provider，覆盖真实private Git和Linux sandbox攻击fixture；不读真实用户配置，不运行源项目。metadata事务每个intent/移动点故障注入；并行兄弟workspace与正式候选接受fixture验证三方语义。重型构建、完整Go/E2E/package、quota/sandbox集成用已授权GitHub Actions并记录精确SHA/job；任何新增云服务另行明确告知并授权。
