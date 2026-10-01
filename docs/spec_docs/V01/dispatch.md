# V01 并行开发下发说明（T06）

> 本文档给三条实现线的 session 各自阅读。四份已批准文档在**你自己 worktree** 的 `docs/spec_docs/V01/`（spec.md / plan.md / task.md / checklist.md），数据契约在 `docs/spec_docs/V01/contracts/`。开工前先通读 task.md 中你负责线的小节。

## 共同基线

- 你的 worktree 从快照 `db5d78253b50304cfeb22fa2c7fa09377600adf8` 创建（见 `docs/spec_docs/V01/baseline/snapshot.sha`）。
- worktree 里已有的两个未跟踪文件 `cmd/stable/chat_test.go`、`internal/decision/chat.go` 是 V01 之前的既有工作：**保持不动、不要提交**，你的改动也不得依赖它们。
- Go 测试统一用离线模式：`GOPROXY=off GOSUMDB=off go test <包>`。每完成一个任务先跑 task.md 写明的验证，拿到真实输出再标记完成。
- 提交规则：只提交你负责文件的 V01 增量；`git add` 逐个文件核对，绝不提交其他线文件或上述既有未跟踪文件。每组任务完成后 commit 并在汇报里给出提交 SHA。

## 文件所有权（越界即返工）

| 线 | 可写文件 | 禁止写入 |
| --- | --- | --- |
| A 存储 | `internal/store/**`（schema.sql、sqlite.go 及三个 _test.go）、T12 交接后的 `internal/core/types.go`、两份 `contracts/*.md` 定稿 | 其余一切 |
| B 验证 | `internal/core/activities.go`、`internal/core/activities_test.go`、`internal/core/current_evidence.go`（新建）、`internal/execution/**`、`workers/kicad/**` | `internal/core/types.go`、`internal/store/**` 等 |
| C 会话 | `internal/conversation/session.go`、`internal/conversation/service_test.go`、`internal/goalrun/**`、`cmd/agentworker/**` | 其余一切 |

`internal/core/types.go` 已由主任务加入共享类型（`GoalPendingReverification`、`EvidenceProvenance`、`VerificationToken`、`VerificationResult`、Evidence/Decision 版本字段）；B、C 只读使用，**不要改它**。`StateStore` 接口扩展留给 A（T12/T14）。

## 各线任务与暂停点

### A 线（worktree：`/home/neo/orca/workspaces/Stable/v01-a-storage`，分支 `v01-a-storage`）
- 任务：T07 → T16（见 task.md「A：存储与迁移」）。现在即可开始。
- 你独占 `contracts/storage-v3.md` 与 `contracts/evidence-v1.md` 的定稿（T16），初稿已在 contracts/ 下，按实现核对修订。
- T12 完成后你获得 `internal/core/types.go` 的独占写权（加 `StateStore` 新方法签名，T14 里做）。
- **完成后必须报告**：`ConfirmGoalCriteria` / `CommitVerification` / `UpdateStatusForToken` 的最终签名与语义、未处理事件查询的行为、提交 SHA。B、C 两条线在等这些接口。

### B 线（worktree：`/home/neo/orca/workspaces/Stable/v01-b-verify`，分支 `v01-b-verify`）
- 无依赖任务现在开始：T17、T18、T19（KiCad 适配器）、T20（`EvidenceCurrent`）。
- **暂停点**：T21、T22 依赖 A 的 T13/T14；T25 依赖 A 的 T10。到这些任务时**停下来报告**「等待主任务同步 A 线差异」，不要照猜测的接口写。主任务会把 A 的完成差异同步进你的 worktree（不纳入你的提交），再让你继续。
- T23、T24 在 T22 之后继续。

### C 线（worktree：`/home/neo/orca/workspaces/Stable/v01-c-session`，分支 `v01-c-session`）
- **先不要开工**：你的全部任务（T27–T30）依赖 A 的确认事务接口（T12）与事件查询（T15）。
- 启动条件：主任务通知「A 已完成 T12+T15 并已同步差异」。
- 测试环境注意：`internal/conversation` 的 httptest 需要允许本机 loopback；受限沙箱跑不了时如实记录，换可监听 loopback 的环境再跑。

## 汇报格式（每组任务完成后）

```
线 X / 任务 Tnn：完成
验证：<实际命令与输出摘要（通过/失败原样贴）>
提交：<SHA 或「未提交」>
阻塞：<无 / 等待主任务同步 A 的 Txx 差异>
```

遇到与 task.md/plan.md 冲突或验证不过的情况：停下报告，不要自由发挥。
