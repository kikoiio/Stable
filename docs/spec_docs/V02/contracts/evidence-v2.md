# 证据出处 v2 契约

## 类别与快照

检查类别仅为 `kicad.erc` 和 `sensor.connection`。每个当前依赖快照包含：

- `schema_version`：整数 `1`。
- `family`：所属检查类别。
- `sources`：按 `kind`、`identity` 排序的输入记录；每条包含 `kind`、`identity`、`digest`、`state`、`reason`。
- `checker_id`、`checker_version`：本类别实际检查器的身份和版本；版本无法确认时为空且快照不可用。
- `fingerprint`：由类别、来源状态/摘要及检查器身份/版本规范化计算的 SHA-256 标识，不包含文件时间戳。
- `available`、`reason`：类别输入和检查器均可验证时为可用；否则说明缺失或解析/版本错误。

来源类型：`project_erc`、`project_symbol_table`、`symbol_library`、`global_symbol_table`、`path_variable`。来源状态：`present`、`absent_optional`、`missing_required`、`unreadable`、`malformed`。`absent_optional` 是稳定且可验证的输入状态；其他非 `present` 状态都应带可读原因。路径变量记录键名与值摘要，不保存值原文。

ERC 快照包括项目 ERC 有效设置、被检查环境选用的项目和全局符号库表、当前表解析选中的符号库文件及解析所需路径变量，以及真实 KiCad CLI 版本。连线快照只包括连线检查器 ID 和实际版本。不得扫描未被本次检查环境选用的宿主配置。

## v2 证据 provenance

新的 `EvidenceProvenance` 必须含以下字段：

| 字段 | 要求 |
| --- | --- |
| `schema_version` | 固定为 `2` |
| `claim` | 本条证据验证的验收主张 |
| `coverage` | 检查覆盖范围；不能留空 |
| `checker_id` / `checker_version` | 实际独立检查器身份和版本 |
| `source_level` | `tool_check`；观察、模型陈述与截图不能证明达标 |
| `invalidation_rule` | 说明原理图、标准和依赖类别变化时如何失效 |
| `family` | `kicad.erc` 或 `sensor.connection` |
| `dependency` | 该证据生成时冻结的完整 `DependencySnapshot` |

检查器字段必须与冻结快照一致；快照不可用时不生成通过证据。可用性变化本身改变指纹。ERC 报告可以由多条标准证据共用，但每条证据均声明自己的 claim/coverage 并引用同一检查时快照。

## 当前证据判定

一条证据只有同时满足以下条件才是当前证据：

1. 原始结果为通过，且 `invalidated_reason` 为空。
2. 证据的原理图摘要和验收标准版本与目标一致。
3. provenance 为完整 v2，`source_level=tool_check`。
4. 证据类别存在当前快照；证据冻结快照可用且 fingerprint、类别、检查器 ID/版本与当前快照一致。
5. 覆盖范围满足对应验收项；ERC 违规数同时满足该项的当前阈值。

每条验收项独立寻找当前证据。只复核 ERC 时可复用匹配的连线证据，反之亦然；目标仅当所有现行验收项均有当前通过证据时才达标。模型文本、对话、截图及未知版本结果永不作为当前通过证据。

## 旧数据与报告路径

- V01 的 `schema_version=1` provenance、没有 provenance 的旧证据，以及缺少依赖快照的记录均按 `unknown` 显示；迁移不补写当前版本。
- 旧证据保留原始结果、报告路径和 ID，作为历史追溯；它们不能满足 v2 当前证据判定。
- `absent_optional` 与 `unknown` 不同：前者是当前采集器确认的稳定状态，后者是缺少历史出处。
- 报告文件路径可以留在受控存储用于读取历史报告，但状态与导出中的路径变量不得泄露原值。交付目录只复制由当前 ERC 证据引用的报告；其余可读取报告位于历史部分并附失效原因及原出处。
