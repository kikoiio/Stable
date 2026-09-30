#!/usr/bin/env bash
# Source-level e2e for parallel goals: two explicitly created goals run
# concurrently on isolated design copies and independent KiCad sessions, both
# reach verified, and neither touches the other's files.
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
mkdir -p "$project_root/run"
run_root=${E2E_PARALLEL_ROOT:-$(mktemp -d "$project_root/run/parallel-XXXXXXXX")}
port=${E2E_PARALLEL_PORT:-17339}
address="localhost:$port"
goal_a="par-a-$(date +%s)"
goal_b="par-b-$(date +%s)"
fixture_digest=$(sha256sum "$project_root/fixtures/sensor_board/sensor.kicad_sch" | cut -d' ' -f1)
runner_pid=
mock_pid=

cleanup() {
  if [[ -n "$runner_pid" ]]; then kill "$runner_pid" 2>/dev/null || true; wait "$runner_pid" 2>/dev/null || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$run_root" 2>/dev/null || true
}
trap cleanup EXIT

source "$project_root/tests/e2e/mock_model_env.sh"

STABLE_TEMPORAL_PORT="$port" bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
runner_pid=$!
for _ in $(seq 1 120); do
  if [[ -x "$run_root/bin/agentctl" ]] && grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null; then break; fi
  if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/runner.log" >&2; exit 1; fi
  sleep 0.5
done

cat > "$run_root/goal-definition.json" <<'JSON'
{"objective":"Repair the sensor connector and obtain a clean KiCad ERC","criteria":[{"id":"erc-clean","kind":"kicad.erc_clean","payload":{"max_violations":0}}],"check_interval_seconds":2}
JSON
"$run_root/bin/agentctl" create --run-root "$run_root" --temporal "$address" \
  --project-root "$project_root" --goal "$goal_a" --from "$run_root/goal-definition.json" >/dev/null
"$run_root/bin/agentctl" create --run-root "$run_root" --temporal "$address" \
  --project-root "$project_root" --goal "$goal_b" --from "$run_root/goal-definition.json" >/dev/null

wait_verified() {
  local goal_id="$1" status_file="$run_root/status-$1.json"
  for _ in $(seq 1 900); do
    "$run_root/bin/agentctl" status --run-root "$run_root" --goal "$goal_id" >"$status_file"
    if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["verified"] else 1)' "$status_file"; then return 0; fi
    if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/worker.log" >&2; exit 1; fi
    sleep 0.5
  done
  echo "goal $goal_id did not verify" >&2
  exit 1
}

wait_verified "$goal_a" &
wait_a=$!
wait_verified "$goal_b" &
wait_b=$!
wait "$wait_a" "$wait_b"

python3 - "$run_root" "$goal_a" "$goal_b" "$fixture_digest" "$project_root" <<'PY'
import hashlib, json, pathlib, sys
root, goal_a, goal_b, fixture_digest, project = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4], pathlib.Path(sys.argv[5])
digests = {}
for goal in (goal_a, goal_b):
    status = json.loads((root/f'status-{goal}.json').read_text())
    snap = status['snapshot']
    assert status['verified'] and snap['goal']['status'] == 'verified', (goal, snap['goal'])
    assert snap['goal']['allowed_root'].endswith(goal)
    assert snap['session']['id'] == 'computer-' + goal
    handles = json.loads(snap['session']['runtime_handle'])
    assert handles.get('display'), (goal, 'no dedicated display')
    digests[goal] = status['actual_artifact_id']
    assert hashlib.sha256((root/goal/'sensor.kicad_sch').read_bytes()).hexdigest() == status['actual_artifact_id']
# Each goal got its own repaired copy digest; fixture untouched.
assert hashlib.sha256((project/'fixtures/sensor_board/sensor.kicad_sch').read_bytes()).hexdigest() == fixture_digest
assert digests[goal_a] and digests[goal_b]
print('PARALLEL E2E PASS', goal_a, goal_b)
PY
