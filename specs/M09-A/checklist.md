# M09-A 协作事件与只读协调原型 Checklist

> 状态：实现与验收完成（2026-10-07）。所有条目均有运行验证；fake provider 和临时项目目录覆盖委派流程，不依赖真实模型费用或外部网络。

## 实现完整性

- [x] **AC1：委派入口** `/delegate <任务>` 启动普通父 run；空参数只显示用法且没有创建 run。（验证：TUI 测试断言有效输入进入现有 session run 路径、空参数不启动 run。）
- [x] **AC2：并行与有界队列** 一次 `delegate_tasks` 调用接收多个子任务，超过活动 worker 容量的项目排队；同时运行数不超过 3，队列最多 32 项，并最终各自进入终态。（验证：fake runner 屏障测试覆盖并行上限、共享池、FIFO 启动和队列满时继续排队后完成。）
- [x] **AC3：只读与权限边界** 子 agent 能在授权根内读取、搜索和列目录；越权路径、写文件、命令、MCP、递归委派和外部网络调用不可用或被拒绝。（验证：e2e child 实际运行 glob；工具清单限于读搜列，执行器拒绝写、命令、MCP、网络和递归委派，并确认文件内容不变。）
- [x] **AC4：provider 与上下文隔离** child 使用父 run 的 provider/model 与授权项目根，只收到自己的显式任务和必要项目上下文，不含完整历史或兄弟任务信息。（验证：fake provider 测试确认模型与显式任务上下文；e2e 确认父 run 收到逐项结果。）
- [x] **AC5：逐项结果与部分失败** 混合成功、失败的批次保留每项状态和原因，成功摘要仍返回父 agent；全失败批次不报告成功。（验证：fake runner 测试覆盖混合成功/失败、顺序、摘要和错误字段。）
- [x] **AC6：进度展示与隐私** TUI 显示任务名、排队/运行状态、阶段摘要和终态，不呈现思考流或 child 原始 transcript；事件符合现有脱敏和大小限制。（验证：TUI 投影测试覆盖状态聚合与不显示思考流；reporter 测试覆盖脱敏和大小上限。）
- [x] **AC7：事件续读与状态顺序** 断线重连后按事件游标恢复每个子任务生命周期；每项状态顺序合法且终态仅一次。（验证：run_subscribe 测试从游标续读包含协作事件的父 run 序列；runner 测试验证注入事件后序号单调，sessionlog 回放和 e2e 验证持久顺序。）
- [x] **AC8：重启中断恢复** 在 queued 和 running 阶段模拟服务重启；未完成子项及父 run 变为 `interrupted`，未闭合委派工具调用获得明确结果并保持 call/result 配对；恢复幂等且不重跑。（验证：持久化日志恢复测试覆盖 queued/running、中断结果配对、父 run 终态及二次恢复幂等。）
- [x] **AC9：父 run 取消** 取消父 run 后活动 child 停止、排队项转为 `canceled`，工具返回逐项终态且不遗留活动 child。（验证：阻塞 fake runner 测试取消父 context 并确认所有批次结果进入 canceled。）
- [x] **AC10：单项预算与资源约束** 单项超过 8 轮、3 分钟、50,000 字节工具输出或 8 KiB 摘要上限时停止或截断并说明原因；高负载时并发与队列内存保持有界。（验证：runner 预算测试覆盖轮次上限；委派测试覆盖父剩余时长、聚合工具输出和流式摘要截断；池测试覆盖 worker/队列边界。）
- [x] **AC11：既有流程回归** 普通 run、权限门、候选接收和目标验证仍按原行为工作；子 agent 只读结果不改变候选、正式工程或目标验证状态。（验证：全量 Go 测试通过，覆盖既有候选和目标验证包。）
- [x] **AC12：Linux 本机行为覆盖** fake provider 下可重复验证真实 runner 并行、取消、重启恢复和 TUI 投影，整个验证不依赖真实 provider 或网络。（验证：Linux 本机 fake-provider 单测和 e2e 均通过；子工具白名单不含网络工具。）

## 集成

- [x] 父工具调用和结果完整配对。（验证：fake-provider e2e 运行含 `delegate_tasks` 的父 run，重放 sessionlog，确认委派 tool call 对应结果。）
- [x] 多父 run 共用单个服务级 FIFO 资源池。（验证：两个父 run 共用一个 pool 测试，确认全局活动数受 worker 上限约束。）
- [x] 协作事件通过现有 run 事件序列和 `run_subscribe` 游标消费。（验证：runner 单测验证共享单调序列，fake-provider e2e 验证事件落入 sessionlog 且父 run 接收结果。）
- [x] 普通工具、权限审批与会话取消语义未被委派功能绕过。（验证：只读拒绝测试、现有权限流程回归和父取消单测通过。）
- [x] 用户入口与父 agent 工具组成完整闭环。（验证：fake-provider e2e 驱动委派工具，父 run 接收逐项结果并完成汇总。）

## 端到端场景

- [x] **并行只读调查：** fake provider 驱动父 agent 生成两个子任务；观察 queued/running/terminal 事件、child 在授权目录运行只读 glob、逐项结果和父 run 汇总。（验证：Linux fake-provider 端到端测试断言两项均执行、工具清单只读且结果回到父 run。）
- [x] **部分失败仍汇总：** 父 agent 委派两个任务，其中一个 child 遇到受控 provider 错误；观察成功项保留、失败项带原因且父 agent仍能总结。（验证：fake-provider e2e 检查 sessionlog 中逐项结果并确认父 run 完成。）
- [x] **资源排队和取消：** 多个父 run 提交超过 worker 数的任务后取消其中一个父 run；观察其它父 run 的队列继续推进，被取消批次的运行/排队项均进入 canceled。（验证：共享池并发单测覆盖队列等待、单父取消和另一父任务继续推进。）
- [x] **重启恢复与续读：** 运行中断开服务后执行启动恢复；queued/running 子任务与父 run 显示 interrupted，工具结果闭合；重连游标测试可读取协作事件，恢复不会重新提交 child。（验证：启动扫描恢复测试运行两次并核对事件数量不变；run_subscribe 游标测试覆盖协作事件续读。）

## 构建与测试

- [x] 受影响 Go 包定向测试通过。（验证：`GOMAXPROCS=2 go test -p 2 ./internal/agent/... ./internal/execution/... ./internal/sessionlog/... ./internal/conversation/... ./internal/runtime/... ./internal/tui/... ./cmd/stable/... -count=1` 退出码为 0。）
- [x] 全量测试通过。（验证：`GOMAXPROCS=2 go test -p 2 ./...` 退出码为 0。）
- [x] 全量构建通过。（验证：`GOMAXPROCS=2 go build -p 2 ./...` 退出码为 0。）
- [x] Linux 命令入口与服务端端到端场景通过。（验证：TUI 命令单测和 fake-provider service e2e 分别验证普通父 run 入口、委派两项子任务、收取结果并输出汇总，不访问真实模型 provider。）
