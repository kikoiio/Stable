#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
mkdir -p "$project_root/run"
run_root=${E2E_WAITING_ROOT:-$(mktemp -d "$project_root/run/waiting-XXXXXXXX")}
port=${E2E_WAITING_PORT:-17337}
goal_id="waiting-$(date +%s)"
runner_pid=
mock_pid=
cleanup() {
  if [[ -n "$runner_pid" ]]; then kill "$runner_pid" 2>/dev/null || true; wait "$runner_pid" 2>/dev/null || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$run_root" 2>/dev/null || true
}
trap cleanup EXIT

if ! command -v codex >/dev/null 2>&1; then source "$project_root/tests/e2e/mock_model_env.sh"; fi

start_runner() {
  PROACTIVE_TEMPORAL_PORT="$port" bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
  runner_pid=$!
  for _ in $(seq 1 120); do
    if [[ -x "$run_root/bin/agentctl" ]] && grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null; then return; fi
    if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/runner.log" >&2; return 1; fi
    sleep 0.5
  done
  return 1
}

start_runner
"$run_root/bin/agentctl" start --run-root "$run_root" --temporal "localhost:$port" \
  --project-root "$project_root" --goal "$goal_id" --interval 60 >/dev/null
status_file="$run_root/waiting-status.json"
for _ in $(seq 1 360); do
  "$run_root/bin/agentctl" status --run-root "$run_root" --goal "$goal_id" >"$status_file"
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); s=x["snapshot"]; sys.exit(0 if s["goal"]["status"]=="waiting" and s["session"]["generation"]>=1 else 1)' "$status_file"; then break; fi
  sleep 0.5
done
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); assert x["snapshot"]["goal"]["status"]=="waiting"' "$status_file"
worker_pid=$(sed -n 's/^Worker PID: //p' "$run_root/runner.log" | tail -n 1)
[[ -n "$worker_pid" ]]
kill -TERM "$worker_pid"
wait "$runner_pid" 2>/dev/null || true
runner_pid=

"$run_root/bin/agentctl" notify --run-root "$run_root" --temporal "localhost:$port" \
  --goal "$goal_id" --event resume-event --kind design_changed >/dev/null
start_runner
for _ in $(seq 1 600); do
  "$run_root/bin/agentctl" status --run-root "$run_root" --goal "$goal_id" >"$status_file"
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["verified"] and any(e["id"]=="resume-event" and e["status"]=="processed" for e in x["snapshot"]["events"]) else 1)' "$status_file"; then break; fi
  if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/worker.log" >&2; exit 1; fi
  sleep 0.5
done
python3 - "$status_file" "$goal_id" <<'PY'
import json,sys
x=json.load(open(sys.argv[1]));g=sys.argv[2];s=x['snapshot']
assert x['verified'] and s['goal']['status']=='verified'
assert s['goal']['id']==g and s['agent']['id']=='agent-'+g
assert s['session']['id']=='computer-'+g
assert sum(e['id']=='resume-event' for e in s['events'])==1
assert any(e['id']=='resume-event' and e['status']=='processed' for e in s['events'])
print('WAITING RESTART PASS',g,s['agent']['id'],s['session']['id'])
PY
