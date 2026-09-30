#!/usr/bin/env python3
"""Generate test-case designs from fixtures/sensor_board into tests/cases/cases/."""
import json
import pathlib
import shutil
import subprocess
import tempfile

root = pathlib.Path(__file__).resolve().parent
project = root.parent
fixture = project / "fixtures/sensor_board"
out = root / "cases"

TOP_WIRE = "(xy 113.03 100.33) (xy 121.92 100.33)"
MISSING = "(xy 114.3 102.87) (xy 121.92 102.87)"
INSERT_BEFORE = '\t(symbol (lib_id "Device:Thermistor_NTC")'
WIRE = (
    "\t(wire (pts " + MISSING + ") (stroke (width 0) (type solid)) "
    '(uuid "81110618-6579-58c6-8f1f-78d9234e76d6"))\n'
)


def sub(text, old, new):
    assert text.count(old) == 1, old
    return text.replace(old, new, 1)


base = (fixture / "sensor.kicad_sch").read_text()

# id: (title, expected behaviour, transform)
CASES = {
    "S01_missing_wire": (
        "原始夹具：J1.2 与 RT1.2 之间缺一根连线",
        "repair",
        lambda t: t,
    ),
    "S02_already_connected": (
        "连线已存在：不应再修改设计，直接以 ERC 验证",
        "no_change_verified",
        lambda t: sub(t, INSERT_BEFORE, WIRE + INSERT_BEFORE),
    ),
    "U01_top_wire_shifted": (
        "上方连线端点被移动：超出支持的故障类型，应 needs_human 且设计不变",
        "needs_human",
        lambda t: sub(t, TOP_WIRE, "(xy 113.03 100.33) (xy 119.38 100.33)"),
    ),
    "U02_connector_replaced": (
        "接头引用了未定义的库符号（KiCad 无法加载，ERC 无输出）：应 needs_human",
        "needs_human",
        lambda t: t.replace(
            '(symbol (lib_id "Connector:Conn_01x02_Socket") (at 127',
            '(symbol (lib_id "Connector:Conn_01x03_Socket") (at 127',
        ),
    ),
    "U03_bottom_lead_removed": (
        "下方引线被删除：缺的不是已知那一根线，应 needs_human",
        "needs_human",
        lambda t: sub(
            t,
            '\t(wire (pts (xy 114.3 105.41) (xy 114.3 102.87))\n'
            '        (stroke (width 0) (type solid)) (uuid "ddd57af5-2125-566a-bcf5-c11ca6ff8a52"))\n',
            "",
        ),
    ),
    "U04_corrupt_schematic": (
        "原理图文件被截断：应给出可读错误/needs_human，不得崩溃或写坏文件",
        "needs_human",
        lambda t: t[: len(t) // 2],
    ),
}


def erc(path):
    with tempfile.TemporaryDirectory() as d:
        report = pathlib.Path(d) / "erc.json"
        subprocess.run(
            ["kicad-cli", "sch", "erc", "--format", "json", "--severity-all",
             "-o", str(report), str(path)],
            capture_output=True,
        )
        if not report.exists():
            return None
        data = json.loads(report.read_text())
        return sorted(v["type"] for s in data.get("sheets", []) for v in s["violations"])


shutil.rmtree(out, ignore_errors=True)
index = []
for cid, (title, expect, fn) in CASES.items():
    d = out / cid
    d.mkdir(parents=True)
    shutil.copy(fixture / "sensor.kicad_pro", d / "sensor.kicad_pro")
    (d / "sensor.kicad_sch").write_text(fn(base))
    meta = {"id": cid, "title": title, "expect": expect, "initial_erc": erc(d / "sensor.kicad_sch")}
    (d / "case.json").write_text(json.dumps(meta, ensure_ascii=False, indent=2) + "\n")
    index.append(meta)
    print(cid, expect, meta["initial_erc"])
