# 契约：证据出处格式 v1（evidence-v1）

> 状态：P0 起草（T03），A 线定稿（T16）。对应 `core.EvidenceProvenance`，存储在 `evidence.provenance` 列（JSON 字符串）。

## 必填字段

```json
{
  "schema_version": 1,
  "claim": "ERC 违规数不超过当前标准允许的 0",
  "coverage": "kicad.erc_clean / RT1.2-J1.2 连线",
  "checker_id": "kicad-cli-erc",
  "checker_version": "8.0.7",
  "source_level": "tool_check",
  "invalidation_rule": "准则变更或产物摘要变化时失效"
}
```

| 字段 | 约束 |
| --- | --- |
| `schema_version` | 本格式为 `1`；读取方遇到更大版本按 `unknown` 处理，不猜测语义 |
| `claim` | 非空；本证据支持的具体断言 |
| `coverage` | 非空；覆盖的准则/范围 |
| `checker_id` | 非空；检查器身份（如 `kicad-cli-erc`、`sensor-connection-check`） |
| `checker_version` | 非空；**真实**检查器版本。无法取得真实版本时检查不可作为通过证据，不得填猜测值 |
| `source_level` | `tool_check` \| `observation` \| `unknown`；只有 `tool_check` 可作为验收证据 |
| `invalidation_rule` | 非空；何时本证据失效 |

## 旧记录（unknown）读法

- `provenance` 为 `NULL` 或 `schema_version` 缺失 → 出处整体视为 `unknown`。
- `criteria_revision` 为 `NULL` → 标准版本未知。
- 状态与导出把上述缺失**显式显示为 `unknown`**，不得推断、不得补成当前版本。
- 含义：出处或版本未知的证据永远不满足「当前证据」条件。

## 当前证据条件（`EvidenceCurrent`，通用筛选器）

一条证据支持目标当前结论，当且仅当全部成立：

1. `result == "pass"` 且 `invalidated_reason` 为空（未失效）；
2. `criteria_revision` 非空且等于目标当前 `criteria_revision`；
3. `artifact_id` 等于目标当前产物 ID（产物摘要一致由存储令牌核对保证）;
4. `provenance` 完整（必填字段全非空）且 `source_level == "tool_check"`；
5. 截图、模型陈述类证据（`source_level != "tool_check"`）**永不**进入验收证据集合。

该筛选器不包含任何 KiCad 专有判断；ERC 违规数与当前阈值是否匹配由检查适配器在产生证据时判定。

## 示例：失效记录

标准从 v3 确认为 v4 后，v3 时代的证据行：

```json
{
  "id": "ev_01…",
  "criterion_id": "erc_clean",
  "result": "pass",
  "criteria_revision": 3,
  "provenance": { "…": "同上，v3 时期产生" },
  "invalidated_reason": "标准于确认提案 cp_… 后升级到 v4，原结论需完整复核"
}
```

`result` 保持当时的原始 `pass`；失效原因另存，供历史导出展示。
