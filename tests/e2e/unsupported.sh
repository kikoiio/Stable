#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
mkdir -p "$project_root/run"
run_root=${E2E_UNSUPPORTED_ROOT:-$(mktemp -d "$project_root/run/unsupported-XXXXXXXX")}
port=${E2E_UNSUPPORTED_PORT:-17336}
goal_id="unsupported-$(date +%s)"
goal_dir="$run_root/$goal_id"
mkdir -p "$goal_dir"
cp "$project_root/fixtures/sensor_board/sensor.kicad_sch" "$goal_dir/sensor.kicad_sch"
cp "$project_root/fixtures/sensor_board/sensor.kicad_pro" "$goal_dir/sensor.kicad_pro"
python3 - "$goal_dir/sensor.kicad_sch" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
old = '(xy 113.03 100.33) (xy 121.92 100.33)'
new = '(xy 113.03 100.33) (xy 119.38 100.33)'
data = path.read_text()
assert data.count(old) == 1
path.write_text(data.replace(old, new, 1))
PY
initial_digest=$(sha256sum "$goal_dir/sensor.kicad_sch" | cut -d' ' -f1)
fixture_digest=$(sha256sum "$project_root/fixtures/sensor_board/sensor.kicad_sch" | cut -d' ' -f1)

PROACTIVE_TEMPORAL_PORT="$port" bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
runner_pid=$!
cleanup() {
  kill "$runner_pid" 2>/dev/null || true
  wait "$runner_pid" 2>/dev/null || true
  python3 "$project_root/tests/e2e/stop_sessions.py" "$run_root" 2>/dev/null || true
}
trap cleanup EXIT
for _ in $(seq 1 120); do
  if [[ -x "$run_root/bin/agentctl" ]] && rg -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null; then break; fi
  if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/runner.log" >&2; exit 1; fi
  sleep 0.5
done
[[ -x "$run_root/bin/agentctl" ]] && rg -q 'agent worker ready' "$run_root/worker.log"

"$run_root/bin/agentctl" start --run-root "$run_root" --temporal "localhost:$port" \
  --project-root "$project_root" --goal "$goal_id" --interval 2 >/dev/null
status_file="$run_root/unsupported-status.json"
for _ in $(seq 1 600); do
  "$run_root/bin/agentctl" status --run-root "$run_root" --goal "$goal_id" >"$status_file"
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["snapshot"]["goal"]["status"]=="needs_human" else 1)' "$status_file"; then break; fi
  if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/worker.log" >&2; exit 1; fi
  sleep 0.5
done
"$run_root/bin/agentctl" export --run-root "$run_root" --goal "$goal_id" --out "$run_root/delivery" >/dev/null
python3 - "$run_root" "$goal_id" "$initial_digest" "$fixture_digest" "$project_root" <<'PY'
import hashlib, json, pathlib, sys
root, goal, initial, fixture, project = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4], pathlib.Path(sys.argv[5])
status = json.loads((root/'unsupported-status.json').read_text())
snapshot = status['snapshot']
assert snapshot['goal']['status'] == 'needs_human', snapshot['goal']
assert not status['verified'] and status['unverified']
assert snapshot['decisions'][-1]['proposal']['kind'] == 'ask_human'
assert len(snapshot['goal']['reason']) > 20 and snapshot['goal']['reason'] == snapshot['decisions'][-1]['proposal']['reason']
assert not any(a['status'] == 'applied' and a['desired_postcondition'] == {'sensor.connection_present': True} for a in snapshot['actions'])
design = root/'delivery'/'sensor.kicad_sch'
assert hashlib.sha256(design.read_bytes()).hexdigest() == initial == status['actual_artifact_id']
assert hashlib.sha256((project/'fixtures/sensor_board/sensor.kicad_sch').read_bytes()).hexdigest() == fixture
delivery = json.loads((root/'delivery'/'delivery.json').read_text())
assert not delivery['verified'] and delivery['unverified']
assert not (root/'delivery'/'erc.json').exists()
print('UNSUPPORTED PASS', goal, initial)
print('Evidence:', root/'delivery')
PY
