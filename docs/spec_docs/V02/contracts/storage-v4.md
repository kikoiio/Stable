# SQLite 存储 v4 契约

## 版本

`PRAGMA user_version` 为 4。新数据库直接使用 v4 schema；版本 1、2、3 的数据库按实际缺列情况安全迁移到 v4。迁移、旧目标状态更新和待唤醒事件写入在同一事务中完成。迁移可重复打开，不重复创建事件，不删除或改写历史证据。

## 字段与表

### `goals.dependency_revision`

- 类型：`INTEGER NOT NULL DEFAULT 0`，非负单调递增。
- 用途：为目标级检查/状态/动作令牌提供并发守卫。第一次采集只建立基线，不递增；任何已有基线的类别快照发生变化（包含恢复、变不可用或重新可用）时递增一次。
- 旧行：迁移值为 `0`。值本身不证明已有依赖快照或旧证据当前。

### `decisions.dependency_revision`

- 类型：可空 `INTEGER`。
- 用途：记录决策产生时的目标依赖代次；预留动作要求该值非空且等于目标当前代次。
- 旧行：`NULL` 表示未知，不能预留新动作或更新当前状态。

### `goal_dependencies`

```sql
CREATE TABLE goal_dependencies (
    goal_id TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
    family TEXT NOT NULL,
    snapshot_json TEXT NOT NULL,
    PRIMARY KEY (goal_id, family)
);
```

`family` 仅接受 `kicad.erc` 或 `sensor.connection`。`snapshot_json` 是该类别完整的 `DependencySnapshot`；数组来源按 `kind`、`identity` 排序。当前行表示最近一次协调器观察到的快照，包括不可用快照。尚无行表示未建立基线，不能解释为“依赖为空且已验证”。

## 迁移语义

1. 在一个 SQLite 事务内补齐上述列并创建表，再迁移目标状态及事件，最后设置 `user_version=4`。
2. V1–V3 证据行原样保留；不合成 `dependency_revision`、依赖快照或 v2 provenance。读取端按未知处理。
3. 对迁移前状态为 `verified` 且没有依赖快照的目标，置为 `pending_reverification`，原因说明尚无工程依赖证据，并插入唤醒事件 `migrate-v4-<goal_id>`。同一目标只插入一次。
4. 已是其他状态的目标不因为迁移而伪造依赖快照；首次运行时的依赖刷新建立基线。若其旧证据不能证明当前性，目标保持或转入待复核，再由当前证据规则决定结论。

## 协调事务与失效

`ReconcileDependencies(goalID, snapshots)` 将提供的两类快照作为一个观察批次处理。

- 对无历史快照的新目标，只保存两类基线，不生成变化事件、不推进依赖代次。
- 对已有快照，按类别比较指纹、可用性和规范化来源状态。无变化时不改代次、不插入事件。
- 任一类别变化时，在同一事务内保存新快照、令 `dependency_revision` 增加一次、目标和 agent 置为 `pending_reverification`，写明受影响类别及不可用原因；仅将受影响类别的旧证据写入 `invalidated_reason`。其他类别的证据 ID、结果和 provenance 不变。
- 对同一类别的同一快照重复协调是幂等操作。内容恢复到旧指纹仍是一次新变化；先前失效原因不清除，必须由新检查产生证据。
- 变化事件 ID 为 `dependency-change-<goal_id>-<new_dependency_revision>`，其事件写入与快照、状态和证据失效处于同一事务。事件沿用 V01 的 pending/signaled/processed 投递与重送规则。
- 有类别不可用时仍保存该快照和可读原因；不可用快照不能产生通过证据，也不能使目标保持已达标。

## 令牌守卫

复核、状态更新和动作预留都比较 `criteria_revision`、原理图摘要及 `dependency_revision`。任何字段过期或未知时，操作不得改变当前目标状态或执行动作。过期复核结果可作为历史保存，但其证据标记为失效，且不能确认目标达标。

复核提交先写入本轮证据，再将其与其他类别仍有效的当前证据合并，逐项检查全部验收条件；不得单独使用本轮 `Passed` 将目标设为达标。只有所有条件都有匹配当前快照的 v2 通过证据，且三部分令牌仍匹配，目标才能设为 `verified`。
