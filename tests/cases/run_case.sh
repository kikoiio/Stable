#!/usr/bin/env bash
# 用法: tests/cases/run_case.sh <case_id>   例如 S01_missing_wire
set -euo pipefail
case_id=${1:?usage: run_case.sh <case_id>}
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
src="$project_root/tests/cases/cases/$case_id"
[[ -d $src ]] || { echo "no such case: $case_id" >&2; exit 2; }
expect=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["expect"])' "$src/case.json")
mkdir -p "$project_root/run"
run_root=$(mktemp -d "$project_root/run/mytest-$case_id-XXXXXX")
port=${MYTEST_PORT:-17340}
goal_id="mytest-$(date +%s)"
mkdir -p "$run_root/$goal_id"
cp "$src/sensor.kicad_sch" "$src/sensor.kicad_pro" "$run_root/$goal_id/"
initial=$(sha256sum "$run_root/$goal_id/sensor.kicad_sch" | cut -d' ' -f1)
if ! command -v codex >/dev/null 2>&1; then source "$project_root/tests/e2e/mock_model_env.sh"; fi
STABLE_TEMPORAL_PORT="$port" bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
runner=$!
trap 'kill $runner 2>/dev/null || true; wait $runner 2>/dev/null || true; python3 "$project_root/tests/e2e/stop_sessions.py" "$run_root" 2>/dev/null || true' EXIT
for _ in $(seq 1 120); do
  grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null && break
  kill -0 $runner 2>/dev/null || { cat "$run_root/runner.log" >&2; exit 1; }
  sleep 0.5
done
ctl="$run_root/bin/agentctl"
"$ctl" start --run-root "$run_root" --temporal "localhost:$port" --project-root "$project_root" --goal "$goal_id" --interval 2 >/dev/null
want=needs_human; [[ $expect != needs_human ]] && want=verified
final=timeout
for _ in $(seq 1 600); do
  "$ctl" status --run-root "$run_root" --goal "$goal_id" >"$run_root/status.json"
  final=$(python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); g=x["snapshot"]["goal"]["status"]; print("verified" if x["verified"] else g)' "$run_root/status.json")
  [[ $final == verified || $final == needs_human || $final == failed ]] && break
  sleep 0.5
done
"$ctl" export --run-root "$run_root" --goal "$goal_id" --out "$run_root/delivery" >/dev/null 2>&1 || true
after=$(sha256sum "$run_root/delivery/sensor.kicad_sch" 2>/dev/null | cut -d' ' -f1 || true)
echo "case=$case_id expect=$expect final=$final design_changed=$([[ $after != "$initial" ]] && echo yes || echo no)"
echo "evidence: $run_root"
ok=0
[[ $final == "$want" ]] || ok=1
[[ $expect == needs_human && $after != "$initial" ]] && ok=1
[[ $expect == no_change_verified && $after != "$initial" ]] && ok=1
[[ $ok == 0 ]] && echo PASS || { echo FAIL; exit 1; }
