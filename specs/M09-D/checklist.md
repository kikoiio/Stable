# M09-D Agent 定义与后台任务 Checklist

> 状态：实现已提交，最终 SHA 云端 Go、package、E2E 验证通过；功能 AC 仍待逐项核对（2026-10-07）。

## 功能验收

- [ ] **AC1 定义：** 内建/用户/项目覆盖正确，来源与只读能力可见；删除/修改重载生效；错误 YAML、未知字段、未知工具、非法名称、超长文件和符号链接明确拒绝，其它定义继续可用。（验证：临时目录 catalog 测试与 service list/reload。）
- [ ] **AC2 分派：** 同步等待终态，后台入队成功即返回稳定 task/run ID；definition background 强制异步；未知角色、空指令、输入超限、queued 记录失败、queue full 均不会报告已接受。（验证：屏障 fake runner 与持久化失败注入。）
- [ ] **AC3 权限：** child 收到正确角色正文/显式任务、provider/model/WorkRef/授权根/permission bounds；不含历史或兄弟结果；只读交集有效；越权与写入/命令/MCP/网络/递归委派均拒绝。（验证：fake provider 输入捕获及受控 executor 实际禁止操作。）
- [ ] **AC4 生命周期：** 父正常结束、TUI 断线后继续；指定任务和父显式取消可停止关联 queued/running，另一个父 run 或 session 不受影响；查询显示实际终态。（验证：双父 run、双 session 屏障与断线 service fixture。）
- [ ] **AC5 事件与通知：** 独立任务 run 和来源关联正确、游标单调、重复订阅去重；结果可交接下一父 run，destination 缺失时恢复交接；重复查询不重跑；完成通知不自动发起新 run。（验证：session replay、重连与 provider 调用计数。）
- [ ] **AC6 预算：** 同步/后台/fork/hook 共用 3 workers/32 queue；8 轮、3 分钟、50,000 字节工具输出、8 KiB 摘要、64 KiB 输入上限生效；definition 与调用只能收窄；查询等待不超过 30 秒且不占 child worker。（验证：共享池竞争、预算边界与等待取消测试。）
- [ ] **AC7 恢复与失败：** provider 错误、timeout、取消、入队/终态写盘失败有明确结果；首条 queued 前、queued、running，以及子项已终态而独立 run 尚未终结的间隙均恢复正确；连续两次恢复不重复终态、不调用 provider、不丢 tool result。（验证：持久 fixture、故障注入和两次恢复对比。）
- [ ] **AC8 隐私和展示：** 列表、TUI 和日志不保存原始角色正文、credential、thinking 或 child 原始 transcript；摘要与错误脱敏截断，任务状态可见且不覆盖同时运行的父 run。（验证：带敏感标记的 fake 输出、sessionlog 投影和 TUI 视图对比。）
- [ ] **AC9 兼容：** M09-A/B/C、todo、普通 Session/Goal run、工具 hook 顺序、权限门、候选受控接收和独立目标验证回归通过。（验证：受影响包 fake 定向与完整 Go/云端集成。）

## 完整场景

- [ ] **用户后台调查：** `/agents` 查看定义，`/agent explore <任务>` 启动，继续普通父 run；`/tasks` 查看状态和摘要，断开重连后按 cursor 恢复。（验证：fake-provider Linux service/client/TUI 集成，无外网。）
- [ ] **父工具交接：** 父 Session/Goal 调用 `run_agent` 后台返回 ID，随后 `task_output` 查询或等待结果；tool call/result 配对，父继续汇总。（验证：fake 父 provider 脚本、run 和 session 回放。）
- [ ] **队列与隔离取消：** 两个父 run 同时提交超过 worker 数的任务，取消其中一个；另一父的 queued/running 继续；超出 queue 上限的提交未接受。（验证：共享池屏障和终态计数。）
- [ ] **角色变更：** 有任务运行时重载同名定义；当前任务使用原快照、新请求使用新定义，删除/无效定义不再可用。（验证：definition hash/input 捕获对比。）
- [ ] **服务重启：** queued/running 任务恢复为 interrupted；已完成结果与通知仍能读到；多次恢复不产生新模型请求。（验证：持久日志 fixture 和 fake 启动计数。）
- [ ] **受信边界：** 构造另一 session 的 ID、路径/工具越权和未知扩权字段；服务/模型工具明确拒绝，无正式文件/候选/目标事实变化。（验证：双 session fixture 及项目 manifest 对比。）

## 验证记录

- [ ] fake 场景使用临时目录，未读取真实用户 definitions 或调用真实 provider/外网。
- [ ] 格式化、diff和协议文档检查通过，无未审阅生成文件。
- [x] 重型构建、Go 全量、E2E 与 package acceptance 在已授权 GitHub Actions 执行；精确 SHA、workflow/run/job 记录如下。所有列出的 job 均成功。
- [x] README 记载只读角色、后台生命周期、查看/取消入口与重启不重跑；M09 总范围状态已更新，E/F 仍单独验收。

- 代码提交 `ccc3fbc7a606f29c07f3d2156fea6736f892d641`：GitHub Actions [Go run 37590568076](https://github.com/kikoiio/Stable/actions/runs/37590568076) 的 `build-and-test`（依赖校验、构建、`go test ./...`）与 `test-package` 均通过。
- 同一 SHA 的 GitHub Actions [E2E run 37590568082](https://github.com/kikoiio/Stable/actions/runs/37590568082)：`unit`、`cases`、`e2e-m03`、`e2e-m04`、`e2e-sessions` 与 `e2e-core`（`make e2e`）全部通过。
