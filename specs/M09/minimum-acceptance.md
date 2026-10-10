# M09 最小验收记录

> 状态：按用户批准的最小验收集完成。E/F checklist 中未勾选的全量交叉矩阵保留为扩展验证记录，不作为本次完成判据；未验证的组合仍明确保持未勾选。

## 范围与证据

| 范围 | 验收证据 |
|---|---|
| E-AC1 身份与授权 | `TestTeamQueriesBindRunToPersistedWorkRef`、`TestGoalTeamsAreIsolatedAcrossPersistedWorkItemQueries`、`TestGoalTeamMessageSendRejectsAnotherWorkItemWithoutFacts`、`TestGoalTeamSpawnRejectsSiblingWorkItemAndGoalOwnerWithoutFacts`、`TestTeamStopRejectsValidSiblingRunIdentityThenAllowsSessionUser`；并包含 team/member 容量和重名拒绝无副作用用例。 |
| E-AC8 限额、恢复与隐私 | team/member/turn、消息 pending 与批次、task/request/query、delegation input/output 的精确边界及超限拒绝由 E checklist 所列定向测试覆盖。`TestTeamMessageWriteFailureDoesNotReportSuccess` 验证写失败不报告成功、恢复后同 token 重试只产生一条 durable message；`TestTeamWatcherRetriesTerminalAppendAndRecoveryIsIdempotent` 验证终态重试与重启收敛。role、thinking、credential 与 child transcript canary 覆盖 raw log、replay/projection、socket stream 和 TUI；`TestTeamMessageTextCannotCreateControlAndCredentialIsRedacted` 额外验证消息投影脱敏。 |
| E-AC9 TUI/service 纵向流程 | `TestTeamTUITwoMembersTwoTurnsRoutePrivateHandoffAndKeepParentRun`、`TestTeamPlanTUIRejectReviseApproveAndAutoReadOnlyFollowUp`、`TestTeamTUIFactsSurviveParentCompactionAndSocketReconnect`、`TestTeamTUIShutdownTextIsOrdinaryUntilTypedShutdownCommand`、`TestGoalTeamTaskBoardKeepsGoalStatusAndSessionTodoIndependent` 与 `TestAcceptedGoalWorkspaceCandidateDoesNotCreateGoalEvidence` 覆盖多轮消息/任务、plan 请求、compaction 重连、控制消息区分、Goal 与 session todo 隔离和 candidate/evidence 边界。 |
| F-AC1/2/4 ownership、Git 与接收 | `TestWorktreeTUITwoSessionsKeepBindingsAndQueriesIsolated`、`TestWorkspaceSocketScopesGoalAndWorkItemOwnership`、`TestPrivateGitMaterializePreservesActualFormalRepository`、`TestWorktreeTUIResolutionExportAndAcceptanceUsesConversationService` 与 `TestWorktreeTeamMemberFlowsThroughExportReviewAndAcceptance` 覆盖隔离 binding、受控 Git metadata、冲突处理、候选 review/accept，以及 team child 写入到显式接收。 |
| F-AC3 Linux sandbox | M09 Workspace Linux workflow 的 `writer-sandbox-volume` job 运行 `TestWorkspaceRealVolumeQuotaAndMetadataAttacks`，使用 disposable volume 验证真实 quota、metadata/mount 攻击与受限 writer 路径。平台所需隔离能力不可用时按 unavailable 处理。 |
| F-AC5/6 持久化与恢复 | E/F checklist 中记录的 acceptance/export/remove/rewind crash cuts、legacy metadata fail-closed、root identity replacement 和重启幂等回归均纳入 Go 与 Workspace Linux CI。这里只据实际记录关闭这些列出的恢复切片，不外推未测试的故障组合。 |
| F-AC7 资源边界与期限 | `TestPoolDelegatorUsesDefaultWorkerAndQueueCapacity`、`TestMaterializerSingleWorkerAndEightPendingSlots`、三项 `TestManifestDefault*LimitAtBoundary`、workspace storage/workspace-count 与 child output/budget tests 覆盖 production caps 和 +1 拒绝。`TestMaterializerProductionDeadlineAndParentDeadline`、`TestPrivateGitMaterializeUsesConfiguredDeadline`、既有 command/query/stop deadline tests 覆盖 3m/90s/30s/10s 生命周期期限与 parent deadline 收窄。 |
| F-AC8/9 TUI 与源行为范围 | `TestWorktreeTUIResolutionExportAndAcceptanceUsesConversationService` 覆盖 TUI 确认与 resolution；上述 privacy canary 覆盖用户可见面；E→F writer/acceptance 场景已列于 E/F checklist。源端行为与明确差异见 [E spec](../M09-E/spec.md) 和 [F spec](../M09-F/spec.md) 的源端行为对照章节。 |

## 云端验证

当前组合代码及本文新增测试完成后，推送 SHA 的以下 workflow 全部通过，且均指向同一代码 SHA：

- 验证代码 SHA：`8aa1e45bd058272ab21bd45740fcb05473cfb6b8`（提交 `test: complete M09 minimum acceptance`）。
- Go `build-and-test` 与 `test-package`：[run 38018937676](https://github.com/kikoiio/Stable/actions/runs/38018937676)，两个 job 均通过。
- M09 Workspace Linux `writer-sandbox-volume`：[run 38018937587](https://github.com/kikoiio/Stable/actions/runs/38018937587)，通过。
- E2E：[run 38018937619](https://github.com/kikoiio/Stable/actions/runs/38018937619)，`unit`、`cases`、`e2e-sessions`、`e2e-m04`、`e2e-core`、`e2e-m03` 六个 jobs 均通过。E2E 中的 M03 job 是既有全仓回归；本轮没有修改 M03。

历史 M09 过程中曾出现的失败及修复留在 [M09 总进度记录](README.md)；最终状态只依据本节所列同 SHA 的成功结果。

## 完成边界

完成声明仅覆盖上表的用户批准最小验收集和明确列出的证据。E/F checklist 未勾选的完整排列组合、额外故障注入和未纳入本表的源端差异仍保持开放，不在本次声明中表示通过。
