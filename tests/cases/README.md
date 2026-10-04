# tests/cases：适合当前能力的测试用例

## 范围说明

系统目前只支持一个固定场景：`fixtures/sensor_board`（NTC 热敏电阻 RT1 + 接头 J1），
唯一可自动修复的故障是 **J1.2 与 RT1.2 之间缺一根连线**，并用 KiCad ERC 独立验证。
公开资料里没有可直接套用的题集（KiCad 官方文档和论坛只描述 ERC 违规类型，如
`pin_not_connected`、`unconnected_wire_endpoint`），任意电路设计、PCB、仿真、采购
均在 spec"不做的事"之内，所以这里的用例都是从该夹具派生的变体，检验"该修的修、
不该动的不动、看不懂的交给人"。

`t01/test01.ppt` 是原有文件，未改动。

## 用例（`cases/<id>/`，含 `sensor.kicad_sch`、`sensor.kicad_pro`、`case.json`）

| ID | 内容 | 预期 | 初始 ERC |
|----|------|------|----------|
| S01_missing_wire | 原始夹具，缺一根连线 | 会话绑定目标，经共享工具写入候选区，评审并接受后进入 pending_reverification | 2×pin_not_connected + 1×unconnected_wire_endpoint |
| S02_already_connected | 连线已存在 | 不修改设计，ERC 通过，verified | 无 |
| U01_top_wire_shifted | 上方连线端点被移动 | needs_human，设计文件不变 | 6 条 |
| U02_connector_replaced | 接头引用未定义的库符号 | needs_human，设计文件不变 | KiCad 无法加载 |
| U03_bottom_lead_removed | 下方引线被删除 | needs_human，设计文件不变 | 3 条 |
| U04_corrupt_schematic | 文件被截断 | needs_human，设计文件不变 | KiCad 无法加载 |

S 开头是应当成功的用例，U 开头是超出能力、应当安全地停下来交给人的用例。

## 使用

```bash
python3 tests/cases/make_cases.py            # 重新生成 cases/（需要 kicad-cli）
tests/cases/run_case.sh S01_missing_wire     # 跑单个用例，输出 PASS/FAIL
```

`run_case.sh` 使用 `tests/e2e/lib.sh` 分配独立端口、HOME、TMPDIR 和运行目录，不影响其他
本地服务；没有 `codex` 时自动使用 `tests/e2e/mock_model_env.sh` 的模拟模型。S01 的流程是：
创建会话，提出并确认目标，使用共享 `read_file`/`edit_file` 工具生成候选，读取正式评审，
正常接受候选，最后断言会话归属、一次正式目录交换和 `pending_reverification`。独立复核由
后续 worker 处理，不在接受命令中提前宣称 verified。
输出请重定向到文件，不要接 `| tail`：残留的子进程会占着管道导致命令不返回。

目前只有 S01 已迁移到 M04 会话/候选流程；S02 和 U01-U04 仍保留用例素材，但旧的
`agentctl start` 入口已移除，因此这些用例会明确报告 legacy runner unavailable，而不是
伪装成已经完成迁移。

## 未覆盖（现在测了也没意义）

通知去重、中途重启恢复已由 `tests/e2e/run.sh`、`waiting_restart.sh` 覆盖；多种模型服务、
配置校验见 `tests/package`。新增故障类型或新电路之前，不建议往这里加用例。
