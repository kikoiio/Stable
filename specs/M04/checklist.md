# M04 基础工具 Checklist

> 状态：已批准（2026-10-04）。基于已批准的 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md)。每项必须通过运行代码或观察行为验证后才能勾选。真实隔离正例和 S01 案例必须在能够创建 Linux namespace 的环境运行；受限环境的拒绝结果不能替代正例证据。

## 工具循环与基础工具

- [x] **C01 / AC1,F1**：普通任务中模型发出工具调用后，工具被统一执行、结果回填下一轮模型请求，模型继续调用并最终收敛为 `completed`；消息序列包含 assistant 工具调用和对应 user 工具结果。（验证：fake provider + FakeExecutor 集成测试；保存运行事件序列。）
- [x] **C02 / AC1,F1**：`tool_exec_start`、`tool_exec_result`、`awaiting_approval` 等事件按运行序号单调发布，TUI 显示工具名、状态、耗时和授权等待；新循环不以 `awaiting_tools` 作为终态。（验证：集成测试断言序号与终态，e2e 观察 transcript。）
- [x] **C03 / AC1,F2f**：普通任务至少执行一次受控命令，stdout/stderr 或规范化退出信息回到对话，后续模型输入或运行记录可观察到该结果。（验证：e2e 断言命令输出出现在下一轮输入或事件记录。）
- [x] **C04 / F2a**：读文件从正式工程只读视图返回 1-based 行号文本，支持起始行和行数分段；文件不存在、目录和非法参数返回可读错误。（验证：tools 单测。）
- [x] **C05 / F2b**：glob 按模式匹配正式工程文件和目录，跳过 SkipDirs，按修改时间倒序返回。（验证：tools 单测。）
- [x] **C06 / F2c**：grep 在正式工程只读视图按正则搜索并输出 `路径:行号:内容`；include 过滤、无匹配和非法正则行为正确。（验证：tools 单测。）
- [x] **C07 / F2d,AC2**：写文件只能在候选区创建或覆盖文件；覆盖已有文件附带准确 diff；候选接收前正式工程内容和 manifest 不变。（验证：临时工程 digest 对比和工具单测。）
- [x] **C08 / F2e**：精确编辑只能作用于候选区；未读先改、old_string 不存在或不唯一时拒绝，并说明原因。（验证：tools 单测和执行器场景各触发一次。）
- [x] **C09 / F2f**：命令在隔离沙箱内执行，stdout/stderr 合并返回；超时可观测且进程组清理；grep/diff/test 等约定的非零退出按源端语义处理。（验证：Go 场景套件。）
- [x] **C10 / F2g,AC8**：写入和编辑结果包含 diff 文本、新增行数和删除行数，且与候选 review 的实际变更一致。（验证：对同一候选比较 `ToolOutcome.Diff` 与 review 的变更摘要。）

## 权限与授权边界

- [x] **C11 / AC3,F3**：四种权限模式下读、写、命令的允许、询问和拒绝结果符合 M03 询问表：默认模式读允许且写/命令询问，acceptEdits 允许写，plan 对写询问，bypass 仍受硬边界约束。（验证：executor 权限矩阵单测；e2e 至少覆盖 default 与 acceptEdits。）
- [x] **C12 / AC3,F3**：保存的精确规则仅对相同操作、参数和授权范围复用；参数、路径或范围改变后重新询问；deny 规则跨模式仍生效。（验证：单测和 e2e 各验证一次。）
- [x] **C13 / AC2,F3**：`../`、绝对路径和符号链接路径不能访问授权正式工程之外的 sentinel，越界写入也被拒绝，错误原因可见。（验证：真实沙箱三连场景。）
- [x] **C14 / AC2,N6**：候选接收前正式工程逐字节不变；受信 accept 后仅应用预览绑定的候选差异一次。（验证：运行前后完整 manifest/digest 对比和重复接收请求。）
- [x] **C15 / F3,N5**：等待授权时 TUI 显示待授权请求，用户取消后工具未执行、运行可终止；过期或拒绝不产生候选写入。（验证：e2e 触发 ask 后取消并检查执行记录。）

## 隔离执行与清理

- [x] **C16 / AC4,F4**：所有工具调用均经隔离进程；正式工程在客内只读，候选区可写，授权根外 sentinel 不可读。（验证：真实 bwrap 探针和文件断言。）
- [x] **C17 / AC4,F4**：隔离不可用时文件和命令工具均返回明确“隔离不可用”拒绝结果，正式工程、候选区和宿主均没有回退执行痕迹。（验证：受限环境或注入 sandbox failure。）
- [x] **C18 / AC4,F4,N5**：取消或命令超时后工具进程及子进程全部退出，不留残留进程，也没有继续写入候选的后台进程。（验证：长寿命子进程场景、进程树和候选摘要检查。）
- [x] **C19 / AC4,N3**：注入的模型密钥和宿主秘密标记不出现在工具环境、挂载、授权提示、工具输出、运行事件、sessionlog 或候选文件中。（验证：全链路 marker 扫描。）

## 记录、回放与预算

- [x] **C20 / AC6,F5**：每次工具调用均成对记录 `EventToolCall/EventToolResult`，名称、脱敏参数摘要、状态、耗时和结果规模与实际一致；按运行查询和游标回放顺序一致。（验证：sessionlog 单测和 ReplayAfter 比对。）
- [x] **C21 / AC6,N2**：超过 50000 字符的工具结果在对话回填和持久记录中均被截断并明确注明，未产生未授权的磁盘溢写。（验证：构造超长工具输出并扫描记录。）
- [x] **C22 / AC5,F6**：模型工具回合数或总时长超限后运行以 `budget_exhausted` 终态停止，事件和状态说明原因；同一响应中的多个工具调用只消耗一轮；正常任务不误停。（验证：runner 单测和注入预算场景。）
- [x] **C23 / N5**：断线重连后按游标恢复工具执行、等待授权和预算事件，顺序正确；取消不会复用旧授权或重复执行工具。（验证：集成测试模拟 subscribeRun/AfterSeq。）
- [x] **C24 / plan**：跨运行历史重建包含上一运行的 assistant 工具调用和 user 工具结果投影，并遵守既有消息数量和截断限制。（验证：conversation 单测断言重建序列。）

## 两类任务一致与候选生命周期

- [x] **C25 / AC7,F7**：目标工作项使用与普通任务相同的 runner、executor、权限、隔离和预算语义；同类操作结果一致；候选按目标/工作项归属隔离。（验证：goal e2e 与普通任务对比。）
- [x] **C26 / plan**：纯对话和只读调查运行不产生候选目录；首次写入、编辑或可能产生变更的命令调用前创建候选并进入 `prepared → running`。（验证：conversation 集成测试断言两条路径。）
- [x] **C27 / plan**：运行结束时无变更候选被清理；有变更候选被冻结并登记 `ready`；候选与会话/目标归属一致。（验证：conversation 集成测试和 store 查询。）
- [x] **C28 / AC1,AC2,N6**：候选经既有 review/accept 流程接收，review 显示完整差异与检查发现；接收后正式工程仅改变一次，重复决定返回同一回执且不重复应用。（验证：e2e 全链和重复提交。）

## 编译、回归与完整用户流程

- [x] **C29 / AC9**：`go build ./...`、`go vet ./...`、`go test ./...` 均退出 0。（验证：记录退出码和日志位置。）
- [x] **C30 / AC9**：既有 e2e `run.sh`、`waiting_restart.sh`、`unsupported.sh`、`criteria_change.sh`、`dependency_change.sh` 全部退出 0。（验证：逐脚本记录退出码。）
- [x] **C31 / AC9**：`make m03-e2e`、两个桥接目录的 `python3 -m unittest discover`、`make package` 和 `make test-package` 全部通过。（验证：记录退出码。）
- [x] **C32 / AC9,F8**：fake 执行器驱动的循环、预算、候选登记和审批等待集成场景全部通过。（验证：运行指定 Go 包测试并保存日志。）
- [x] **C33 / AC1,F8**：普通任务完整流程为“提交 → 读/搜/列调查 → 写/编辑修改 → 受控命令 → TUI 事件 → 候选 ready → review → accept”，正式工程改变一次。（验证：`bash tests/e2e/m04_tools.sh` 普通任务段。）
- [x] **C34 / AC7,F7**：目标工作项完整流程为“目标绑定会话 → 工具执行 → 候选 ready → review → accept”，接收后目标进入待独立复核。（验证：`bash tests/e2e/m04_tools.sh` 目标段。）
- [x] **C35 / AC10,F9**：S01 案例运行器完成“会话绑定目标 → 工具调查/修复 → review → 受控接收”，退出码 0，正式工程仅改变一次，目标未被错误标记为已验证。（验证：可创建 namespace 的 Linux 环境运行案例。）
- [ ] **C36 / 回归**：本里程碑每个提交均可独立编译。（验证：临时 worktree 中逐提交运行 `go build -buildvcs=false ./...`。）

## 覆盖与证据记录

> 2026-10-04 最终验收记录（全部基于里程碑最终代码，环境：Ubuntu,AppArmor 限制 unprivileged userns 但 bwrap 可用，真实 namespace 正例已执行）：
>
> - **C03/C14/C15/C33/C34/C35**：`bash tests/e2e/m04_tools.sh` 整体退出码 0（日志 `/tmp/m04-final-m04_tools.log`）。普通任务段证据 `/tmp/stable-e2e-m04-tools-KjE4NMjC`（run1 23 事件 completed 且事件流与 mock.log 均含 `m04-probe-` 命令输出；候选 ready；接收前正式工程 manifest 逐字节不变；幂等 accept 返回同一回执 `receipt-accept-m04-…`，journal finalized 计数 1；run2 审批等待中取消，写工具无 `tool_exec_result`，终态 cancelled；run3 拒绝后无新候选、正式工程不变；C19 密钥/哨兵扫描无命中）。目标段证据 `/tmp/stable-e2e-case-S01_missing_wire-j6AVaYF6`（退出码 0，goal `pending_reverification`，正式工程仅改变一次）。
> - **C09/C13/C16/C18/C19（Go 场景）**：`STABLE_M04_HELPER=… go test -p 1 ./tests/e2e -run '^TestM04(SandboxPositive|SandboxEscapeTrio|CommandTimeoutAndExitSemantics|ToolFlowScrubsSecrets)$'` 全 PASS（真实 bwrap；超时进程组清理经 pgrep 断言；`../`/绝对路径/符号链接三连逃逸均拒绝；密钥脱敏与 sessionlog 扫描通过）。
> - **C23**：`go test ./internal/conversation -run TestSubscribeRunRestoresToolEventsByCursor` PASS（AfterSeq=4 游标回放 awaiting_approval/tool_exec_result/terminal，终态运行取消为 no-op）。
> - **C29**：`go build ./...`、`go vet ./...`、`go test -p 1 ./...` 均退出 0（日志 `/tmp/m04-final-go-test.log`）。
> - **C30**：`run.sh`、`waiting_restart.sh`、`unsupported.sh`、`criteria_change.sh`、`dependency_change.sh` 全部退出 0（日志 `/tmp/m04-final-{run,waiting_restart,unsupported,criteria_change,dependency_change}.log`）。其中三个 goal 脚本为适配 M04「goal 评估走工具循环」补充了审批驱动与稳定收敛判定。
> - **C31**：`make m03-e2e`、`make package`、`make test-package` 均退出 0（日志 `/tmp/m04-final-{m03,package,test-package}.log`；test-package 证据 `/tmp/stable-package-{cli-Iy9Bsslx,e2e-0K5GzclT,restart-ntkCkcTq}`）。
> - **C36**：b6279bc、febbf63 已在独立 worktree 逐提交 `GOMAXPROCS=2 go build -p 1 -buildvcs=false ./...` 退出 0（Codex 侧执行）；本记录所属新增提交见下文提交清单，同法验证后勾选。
> - 验收中修复的缺陷：执行器写/命令在权限门前创建候选（首次写不再被误拒）；取消审批中的运行补写配对 tool_result（修复 sessionlog 悬空 pending 导致的会话中毒）；runner 终态错误携带底层原因；候选 manifest/接收排除并回迁 `.stable` 服务目录。

| 验收标准 | 对应检查 |
| --- | --- |
| AC1 | C01–C03、C28、C33 |
| AC2 | C07、C13–C14、C33 |
| AC3 | C11–C12 |
| AC4 | C16–C19 |
| AC5 | C22 |
| AC6 | C20–C21、C23 |
| AC7 | C25、C34 |
| AC8 | C10 |
| AC9 | C29–C32、C36 |
| AC10 | C35 |

验收时对每项记录执行日期、环境、命令或用户操作、退出码或界面结果、摘要或回执及证据位置。真实 Linux 隔离正例（C16、C33、C34）和 S01 运行器（C35）未完成时，M04 不能标记通过。
