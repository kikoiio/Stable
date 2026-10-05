# M06 计划与任务交互 Checklist

> 状态:已批准(2026-10-05)。基于已批准的 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md)。每项须有运行代码或观察行为的证据才能勾选。

## 斜杠命令体系

- [ ] **C01 / AC1**:补全列表来自注册表,含内置与自定义命令。(验证:TUI 补全单测断言 + 运行观察。)
- [ ] **C02 / AC1**:会话运行中新增/修改/删除 `.md` 命令文件后,无需重启,补全与执行即可感知。(验证:运行中改文件,观察补全与执行结果。)
- [ ] **C03 / AC1**:`$ARGUMENTS` 替换与无占位符附加两种形态正确;子目录命名空间命令(如 `git:log`)可补全并执行。(验证:commands 单测 + 运行观察。)
- [ ] **C04 / AC1**:自定义命令与内置同名时内置优先且可观察;`/help` 输出与实际可用命令一致。(验证:注册表单测 + /help 运行观察。)
- [ ] **C05 / N5**:超限(深度/大小/数量)与非法(不可读、frontmatter 坏)命令文件被跳过并出报告,TUI 不崩溃、输入不被阻塞。(验证:构造越界与坏文件后运行。)

## 计划模式与审批

- [ ] **C06 / AC2**:`/plan` 切换计划模式,`plan_mode` 事件落会话日志,状态栏可见;重启后回到默认模式。(验证:运行观察 + sessionlog 断言 + 重启。)
- [ ] **C07 / AC2**:计划模式下读放行、候选区写与命令仍询问、仅当前计划文件免询问写;越出该路径的写仍走审批。(验证:permission 判定矩阵单测 + executor 集成测试。)
- [ ] **C08 / AC2**:非计划模式下调用 `exit_plan_mode` 返回明确错误。(验证:execution 单测。)
- [ ] **C09 / AC3**:计划审批三选项分别产生:后续运行 acceptEdits 语义 / 后续运行保持 default / 保持计划模式且反馈文本进入下次运行上下文。(验证:conversation 集成测试三种决策路径。)
- [ ] **C10 / AC3**:计划提交、批准、取消、反馈落 `plan_approval` 事件,重启投影一致;审批标识与目标提案 prop-XXX 独立,互不混用。(验证:回放比较 + ID 前缀断言。)

## 提问

- [ ] **C11 / AC4**:`ask_user` 校验 1–4 题、每题 2–4 选项,超限返回明确错误结果;multiSelect 与「其他」自由输入可用。(验证:execution 单测 + TUI model 测试。)
- [ ] **C12 / AC4**:弹层答复与 `/reply`(带 question id)答复都使问题置为已答且 agent 收到结构化答案继续;重复答复被 M05 校验拒绝。(验证:conversation 集成测试 + TUI model 测试。)
- [ ] **C13 / AC4**:运行结束(取消/崩溃/完成)后遗留的 pending 问题经 `/reply` 答复,产生排队用户消息,下次运行进入上下文。(验证:conversation 集成测试。)
- [ ] **C14 / N2**:提问答复、todo、计划文件落盘前应用凭据脱敏;无法安全脱敏时拒绝写入,不落明文。(验证:注入模拟凭据后检查文件与会话日志。)

## todo

- [ ] **C15 / AC5**:task_create/get/list/update 增改查与依赖(Blocks/BlockedBy)正确;deleted 移除任务并清理悬空引用。(验证:todo 单测。)
- [ ] **C16 / AC5**:重启后当前会话清单恢复;每次变更在 transcript 可见;todo 文件不进候选区、rewind 后仍存在。(验证:重启测试 + rewind 后文件断言。)

## 提案弹窗

- [ ] **C17 / AC6**:提案弹层确认/拒绝与 `/confirm` `/reject` 文本命令到达同一状态机结果(状态、消息一致);「稍后」不改变提案状态;pending 提案在 transcript 可见。(验证:TUI model 测试 + conversation 测试。)

## 队列与审计

- [ ] **C18 / AC7**:权限审批 > 提问 > 计划审批 > review > 提案 同时待决策时按序呈现;退出当前弹层可处理下一个。(验证:TUI model 测试构造多类待决策。)
- [ ] **C19 / N1**:命令执行、模式切换、计划审批、提问答复、提案确认全部落会话日志,重启后投影一致;损坏日志明确失败。(验证:回放比较 + 故障注入沿用 M05 用例。)

## 集成与编译

- [ ] **C20**:六个新工具(ask_user、exit_plan_mode、task_create/get/list/update)在 chatserve 与 runtime 两处白名单可见;未注入对应 sink 时调用返回明确错误而非 panic。(验证:白名单断言 + execution 单测。)
- [ ] **C21**:计划文件写入不进沙箱、不进候选区(候选 manifest 无变化);plan/todo 文件权限 0600、目录 0700。(验证:executor 集成测试 + 权限位断言。)
- [ ] **C22**:agentworker 沙箱 helper 不感知新工具,既有 unknown tool 行为不变。(验证:agentworker 既有测试通过。)
- [ ] **C23 / 编译**:`gofmt -l` 无输出,`go vet ./...` 干净。(验证:命令输出。)
- [ ] **C24 / 测试**:`go test ./...` 全部通过。(验证:命令输出。)

## 端到端场景

- [ ] **C25 / 全链路**:`tests/e2e/m06_interaction.sh`:自定义命令新增→补全→执行;修改→无需重启生效;`/plan` 进入→agent 写计划文件(候选 manifest 无变化)→`exit_plan_mode`→auto 批准→后续运行 acceptEdits 语义;`ask_user`→弹层答复→agent 继续;运行取消→遗留问题 `/reply`→排队消息下次运行可见;task 变更→transcript 可见→重启恢复;`/goal` 提案→弹层确认→与文本命令状态一致。(验证:运行脚本全部断言通过。)
