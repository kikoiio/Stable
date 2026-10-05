# S00 平台支持范围与能力契约 Tasks

> 状态:已批准(2026-10-06)。依据已批准的 spec.md 与 plan.md(docs/spec_docs/S00/)。

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 已建 | `docs/spec_docs/S00/spec.md`、`plan.md`、`task.md` | 流程四件套(checklist.md 由阶段四创建) |
| 新建 | `docs/spec_docs/S00/platform-capability-matrix.md` | 唯一交付物:平台能力表(六区块) |
| 修改 | `.gitignore` | 追加 `docs/spec_docs/S00/` 白名单条目 |

## T1: 能力表骨架

**文件:** `docs/spec_docs/S00/platform-capability-matrix.md`
**依赖:** 无
**步骤:**
1. 创建文件,写入文档标题与说明段(交付物定位、与路线图的关系)。
2. 写入六区块空结构标题:①支持范围声明 ②矩阵总览 ③逐行能力详述 ④发布门槛清单 ⑤完成标志对照 ⑥审计差异记录。
3. 在区块 3 下建立 C01–C10 十个小节空壳(编号+路线图术语名称):C01 路径与安装布局、C02 本机 IPC、C03 单实例锁、C04 进程树托管、C05 私密文件与安全存储、C06 安全根目录访问、C07 候选目录事务(原子验收)、C08 网络隔离(沙箱执行与网络授权)、C09 无头 KiCad 检查、C10 交互式 GUI 会话。
4. 写入状态取值定义(支持/降级/不支持/未评估;Linux 列禁用未评估)与证据类型定义(可运行验证/仅代码审阅)。
5. 写入验收条件域缩写表:PATH、IPC、LOCK、PROC、SECRET、ROOT、CAND、NET、KCAD、GUI,共 10 域对应 10 行。

**验证:** 文件存在,六区块标题、C01–C10 小节、状态/证据定义、10 域缩写表齐全。

## T2: 审计 C01 路径与安装布局

**文件:** 读取 `internal/appconfig/config.go`、`internal/appconfig/owner_unix.go`、`internal/runtime/paths.go`、`internal/tui/model.go`;写入 `platform-capability-matrix.md` 的 C01 小节。
**依赖:** T1
**步骤:**
1. 核实上述文件与路线图盘点描述相符(XDG 默认、`$HOME/.config`/`$HOME/.local/state`、Unix 文件模式检查、TUI 硬编码 `~/.config`);不符之处记入差异候选。
2. 定位机制细节:环境变量覆盖、bin/libexec/share 布局推导、socket 文件名与日志路径拼接。
3. 尝试映射可运行验证:优先定向 `go test ./internal/appconfig/...` 等;不可运行才标「仅代码审阅」并说明原因。
4. 按 plan 能力行模板写入 C01 小节全部字段;macOS/Windows 状态填「未评估(计划评估)」并附等价机制线索(标注未经验证)。
5. 顺带记录审计中发现的矩阵外路径类平台依赖点(如有),供 T10 裁决增补行。

**验证:** C01 小节字段齐全(状态/≥1 文件级引用/证据类型/安全不变量/能力不可用行为/验收条件/线索);可运行验证命令执行成功并记录结论。

## T3: 审计 C02 本机 IPC + C03 单实例锁

**文件:** 读取 `internal/runtime/supervisor.go`、`internal/conversation/service.go`、`internal/conversation/client.go`、`internal/sandbox/network.go`;写入 C02、C03 小节。
**依赖:** T1
**步骤:**
1. 核实 Unix domain socket 使用与 `chmod` 权限设置位置;核实 `syscall.Flock` 单实例锁位置。
2. 定位访问边界语义:当前用户保护如何达成、锁的持有与释放路径。
3. 映射可运行验证(优先 `go test ./internal/runtime/...`、`./internal/conversation/...` 中相关包)。
4. 按 plan 模板写入 C02、C03 小节(C02 用 IPC 域,C03 用 LOCK 域编号)。

**验证:** C02、C03 小节字段均齐全;可运行验证命令执行成功并记录结论。

## T4: 审计 C04 进程树托管

**文件:** 读取 `internal/runtime/supervisor.go`、`internal/runtime/sessions.go`、`internal/sandbox/session.go`、`internal/sandbox/process_linux.go`;写入 C04 小节。
**依赖:** T1
**步骤:**
1. 核实进程组、`Pdeathsig`、POSIX signal、`/proc` 查询、`kill` 的使用位置。
2. 核实会话清理对 KiCad `eeschema` 与 Xvfb 的特指逻辑。
3. 映射可运行验证(优先 `go test ./internal/sandbox/...` 相关包)。
4. 按 plan 模板写入 C04 小节(PROC 域)。

**验证:** C04 小节字段齐全;可运行验证命令执行成功并记录结论。

## T5: 审计 C05 私密文件与安全存储

**文件:** 读取 `internal/appconfig/owner_unix.go`、`internal/appconfig/config.go`;写入 C05 小节。
**依赖:** T1
**步骤:**
1. 核实 Unix 文件模式检查与当前用户归属检查的具体逻辑(哪些权限位、什么判定、失败时行为)。
2. 核实该检查覆盖哪些私密文件(模型配置、会话数据等)。
3. 映射可运行验证(`go test ./internal/appconfig/...`)。
4. 按 plan 模板写入 C05 小节(SECRET 域);不变量注意覆盖「私密配置不因平台权限模型扩大可读范围」。

**验证:** C05 小节字段齐全;可运行验证命令执行成功并记录结论。

## T6: 审计 C06 安全根目录访问

**文件:** 读取 `internal/candidate/workspace.go`、`internal/candidate/erc_checker.go`;写入 C06 小节。
**依赖:** T1
**步骤:**
1. 核实 `golang.org/x/sys/unix` 直接调用、`openat2`、`O_NOFOLLOW` 的使用位置与防护语义(路径穿越/符号链接防护)。
2. 明确「指定根目录内解析/读写」的现有实现方式与覆盖的调用方。
3. 映射可运行验证(`go test ./internal/candidate/...`)。
4. 按 plan 模板写入 C06 小节(ROOT 域)。

**验证:** C06 小节字段齐全;可运行验证命令执行成功并记录结论。

## T7: 审计 C07 候选目录事务(原子验收)

**文件:** 读取 `internal/candidate/accept.go`、`internal/candidate/snapshot.go`、`internal/candidate/rewind.go`;写入 C07 小节。
**依赖:** T1
**步骤:**
1. 核实 `RENAME_EXCHANGE` 的使用与目录交换承担的候选验收原子性。
2. 核实现有崩溃恢复/journal 机制(如有)与恢复路径。
3. 映射可运行验证(`go test ./internal/candidate/...` 中 accept/snapshot/rewind 相关测试)。
4. 按 plan 模板写入 C07 小节(CAND 域);不变量注意覆盖「验收/恢复不暴露半写入的项目状态」。

**验证:** C07 小节字段齐全;可运行验证命令执行成功并记录结论。

## T8: 审计 C08 网络隔离(沙箱执行与网络授权)

**文件:** 读取 `internal/sandbox/linux.go`、`internal/sandbox/network_linux.go`、`internal/sandbox/network_other.go`、`internal/sandbox/process_linux.go`、`cmd/agentworker/main.go`、`internal/runtime/supervisor.go`;写入 C08 小节。
**依赖:** T1
**步骤:**
1. 核实 bubblewrap、mount/PID/network namespace、Linux guest 路径的使用;核实 `sandbox.LinuxManager{}` 在入口的直接构造位置。
2. 核实 `network_other.go` 对非 Linux loopback 能力的拒绝行为(fail-closed 现状)。
3. 核实网络代理 socket 与授权语义。
4. 映射可运行验证(`go test ./internal/sandbox/...`;注意 build tags 限制,必要时说明为什么某些验证只能在该平台跑)。
5. 按 plan 模板写入 C08 小节(NET 域)。

**验证:** C08 小节字段齐全;可运行验证命令执行成功并记录结论(或标「仅代码审阅」并说明)。

## T9: 审计 C09 无头 KiCad + C10 交互式 GUI 会话

**文件:** 读取 `internal/runtime/doctor.go`、`internal/runtime/sessions.go`、`internal/candidate/erc_checker.go`;写入 C09、C10 小节。
**依赖:** T1
**步骤:**
1. 核实 doctor 固定查找的 Linux 工具集(Xvfb、`xprop`、`xwininfo`、`import` 等)与 KiCad 检测逻辑。
2. 核实 GUI 启动、无头检查、资源路径中的 Linux 假设;区分 headless checker 与交互 GUI 会话两条能力线。
3. 映射可运行验证(如 `make cases` 需 kicad-cli,视本机可用性决定运行或标注)。
4. 按 plan 模板写入 C09(KCAD 域)、C10(GUI 域)小节;C10 注明允许「降级」状态。

**验证:** C09、C10 小节字段均齐全;可运行验证执行或说明不可运行原因。

## T10: 汇总矩阵总览与差异记录

**文件:** 写入 `platform-capability-matrix.md` 区块 2、区块 6。
**依赖:** T2–T9
**步骤:**
1. 汇总 10 行 × 3 平台状态总览表(一屏可读),与小节逐一核对一致。
2. 裁决 T2–T9 上报的增补行候选:确属重要平台依赖点的,按模板增补并标「补充」;否则不增补。
3. 汇总差异记录(路线图盘点 vs 代码实况),每条含出处(文件/路线图行)。
4. 检查 Linux 列无「未评估」,每行状态唯一。

**验证:** 总览表 10 行 × 3 列状态齐全且与 C 小节相符;差异记录条目含出处;状态取值合法。

## T11: 支持范围声明、发布门槛与完成标志对照

**文件:** 写入 `platform-capability-matrix.md` 区块 1、区块 4、区块 5。
**依赖:** T10
**步骤:**
1. 写支持范围声明:仅 Linux(x86_64 CLI/TUI)受支持;macOS/Windows 为「计划评估」不承诺顺序时限;判定依据为能力表状态+发布门槛清单;与总览核对无矛盾。
2. 写发布门槛清单:逐平台安全设计审查、隔离强度证明、事务崩溃一致性故障注入等;明确 KiCad 规则(无头 ERC 检查必须可用,GUI 会话允许降级)。
3. 写完成标志对照:逐条对照路线图「阶段 0 完成标志」,给出达成位置与证据。

**验证:** 声明与总览/矩阵无矛盾;门槛清单含 KiCad 规则;对照表逐条有位置与证据。

## T12: git 纳入跟踪

**文件:** 修改 `.gitignore`。
**依赖:** T11
**步骤:**
1. 在白名单区追加条目使 `docs/spec_docs/S00/` 全部文档被跟踪(注意需逐级取消忽略,且不影响 `docs/` 其他文件的忽略状态)。
2. 运行 `git ls-files docs/` 与 `git check-ignore` 验证;确认 spec_docs 两份既有文档仍被忽略。

**验证:** `git ls-files` 列出 S00 五份文档;`git check-ignore docs/spec_docs/design-principles.md docs/spec_docs/vision-roadmap.md` 仍命中忽略规则。

## T13: 全文一致性自检

**文件:** 只读 `platform-capability-matrix.md`、`spec.md`。
**依赖:** T11(可与 T12 并行)
**步骤:**
1. 对照 spec AC1–AC8 逐条初验并记录结果。
2. 扫描验收条件编号:无重复、无悬空引用、域缩写与行对应正确。
3. 检查声明/总览/小节/不变量/验收条件五处对同一能力的描述一致(N1),术语与路线图一致。

**验证:** AC1–AC8 初验记录齐全,发现问题回写修复后复验。

## 执行顺序

```
T1
└→ T2 ∥ T3 ∥ T4 ∥ T5 ∥ T6 ∥ T7 ∥ T8 ∥ T9   (取证可并行,写入按小节串行提交)
        └──────────────────┬──────────────────┘
                          T10 → T11 → T12
                                    ∥
                                   T13
```

**并行边界:**
- T2–T9 共享交付物文件,但每行小节唯一归属对应任务,写入动作按小节串行提交,不并发改写同一小节;取证(读代码、跑验证)完全并行。
- T2–T9 的可运行验证优先定向 `go test <包>`;同一并行批次内不同时启动多个大包测试,由主 agent 按本机内存余量错峰调度。
- T12 与 T13 无共享写入,可并行;T13 若发现问题,修复后需重跑 T11 受影响验证。
