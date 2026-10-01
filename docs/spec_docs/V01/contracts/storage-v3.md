# 契约：SQLite 存储 v3（storage-v3）

> 状态：P0 起草（T03），A 线定稿（T16）。字段定义以 `internal/core/types.go` 与 plan.md 为准。

## 版本

- 数据库自 `PRAGMA user_version = 2`（现有）升至 **3**。
- 迁移在**单个事务**内执行；任何一列添加失败即整体回滚，不留下半迁移状态。
- 旧库（v1/v2）与新库都必须可被同一版本代码打开；迁移幂等——重复打开不重复执行、不重复生成事件。

## 新增列

### `evidence` 表

| 列 | 类型 | 空值语义 |
| --- | --- | --- |
| `criteria_revision` | INTEGER NULL | `NULL` = 旧记录，标准版本未知；**不得**回填为当前版本 |
| `provenance` | TEXT NULL（JSON，见 evidence-v1 契约） | `NULL`/空 = 出处未知，展示层显示 `unknown` |
| `invalidated_reason` | TEXT NOT NULL DEFAULT '' | 空串 = 未失效；失效原因独立保存，不改写 `result` |

### `decisions` 表

| 列 | 类型 | 空值语义 |
| --- | --- | --- |
| `criteria_revision` | INTEGER NULL | `NULL` = 旧决策，无法证明适用于当前标准；动作预留时**拒绝** |

## 迁移与旧数据恢复规则

1. 逐列 `ALTER TABLE ... ADD COLUMN`（SQLite 支持加列，无需重建表）；用 `PRAGMA table_info` 判列是否已存在，已存在则跳过。
2. 旧行新列保持 `NULL`/空串，不做任何推断回填。
3. 迁移收尾扫描：对 `status='verified'` 的目标，若其缺少**可判定为当前**的证据（按 evidence-v1 的当前证据条件评估），则：
   - 目标状态改为 `pending_reverification`，`reason` 写明"旧版达标结论缺少可核对的当前证据（标准版本/出处未知），已转入待复核"；
   - 插入一条 `criteria_updated` 唤醒事件（`status='pending'`），供 worker 补送触发完整复核。
4. 事件插入以目标 ID + 迁移批次为幂等键：同一数据库重复打开、重复迁移**不得**产生第二条事件。

## 事件读取

- 「未处理事件」= `status IN ('pending','signaled')`；`processed` 不再返回。
- 补送与去重一律以事件 `id` 为准；投递成功后状态推进为 `processed`，失败保持原状态留待重试。

## 事务语义（A 线实现，此处固定返回契约）

- `ConfirmGoalCriteria(proposalID)`：校验提案有效且未被确认 → 单事务内：确认提案、目标 `criteria_revision+1`、状态置 `pending_reverification` 并写入 reason、给该目标当前未失效证据写 `invalidated_reason`、插入 `criteria_updated` 事件。返回 `CriteriaConfirmation{Proposal, Goal, Event}`；无效或重复确认返回错误且**不改目标**、不返回第二个事件。
- `CommitVerification(token, result)`：始终保存本轮全部证据（可追溯）；仅当 token 的 `criteria_revision` 与目标当前版本、`artifact_id` 与当前产物摘要都匹配时才允许改变当前结论（全部通过 → `verified`；未通过 → `active` + 未满足项）。不匹配时本轮证据标为历史（写失效原因），返回 `current=false`，目标状态不变。
- `UpdateStatusForToken(token, status, reason)`：token 匹配才更新状态；产物摘要变化而目标处于 `pending_reverification` 时，保持待复核并更新原因；旧令牌（版本或摘要不匹配）不得覆盖待复核状态，返回 `current=false`。
- `ReserveAction`：核对决策的 `criteria_revision` 与目标当前版本；`NULL` 或不匹配均拒绝新动作预留。
