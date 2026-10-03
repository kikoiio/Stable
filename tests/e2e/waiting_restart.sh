#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
e2e_alloc waiting
cd "$project_root"

run_root=$E2E_ROOT
port=$E2E_PORT
goal_id=$E2E_GOAL
runner_pid=
mock_pid=
cleanup() {
  if [[ -n "$runner_pid" ]]; then kill "$runner_pid" 2>/dev/null || true; wait "$runner_pid" 2>/dev/null || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
}
e2e_on_cleanup cleanup
trap e2e_run_cleanups EXIT

if ! command -v codex >/dev/null 2>&1; then source "$project_root/tests/e2e/mock_model_env.sh"; fi
mkdir -p "$run_root/bin" "$run_root/goals"
go build -buildvcs=false -o "$run_root/bin/agentctl" ./cmd/agentctl
source "$project_root/tests/e2e/candidate_accept.sh"
status_file="$run_root/waiting-status.json"
read_status() {
  "$run_root/bin/agentctl" status --run-root "$run_root/goals" --db "$run_root/state.db" --goal "$goal_id" >"$status_file"
}
wait_candidate() {
  for _ in $(seq 1 900); do
    e2e_allow_pending_approvals "$session_id" || true
    if read_status && python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["snapshot"]; sys.exit(0 if any(a["status"] in ("candidate_ready","awaiting_accept") for a in (s.get("actions") or [])) else 1)' "$status_file"; then return 0; fi
    sleep 0.5
  done
  echo 'candidate did not become ready' >&2
  return 1
}

start_runner() {
  STABLE_RUN_LOCAL_UP=1 STABLE_TEMPORAL_PORT="$port" bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
  runner_pid=$!
  for _ in $(seq 1 480); do
    if grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null; then
      e2e_wait_for_chat_socket "$run_root/chat.sock"
      return 0
    fi
    sleep 0.5
  done
  return 1
}

start_runner
session_id=$(e2e_new_session "$run_root/session.jsonl")
e2e_chat create_goal --session "$session_id" --goal "$goal_id" --text "修复传感器连接并确保 ERC 违规为零" >"$run_root/proposal.jsonl"
proposal_id=$(python3 - "$run_root/proposal.jsonl" <<'PY'
import json,sys
messages=[json.loads(line) for line in open(sys.argv[1]) if line.strip()]
print(next(m['proposal']['id'] for m in messages if m.get('type')=='proposal'))
PY
)
e2e_chat confirm --session "$session_id" --goal "$goal_id" --proposal "$proposal_id" >/dev/null
for _ in $(seq 1 360); do
  "$run_root/bin/agentctl" status --run-root "$run_root/goals" --db "$run_root/state.db" --goal "$goal_id" >"$status_file"
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); s=x["snapshot"]; sys.exit(0 if s["goal"]["status"]=="waiting" and s["session"]["generation"]>=1 else 1)' "$status_file"; then break; fi
  sleep 0.5
done
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); assert x["snapshot"]["goal"]["status"]=="waiting"' "$status_file"
# Simulate a full runtime crash: stop the daemonized stack; the next
# start_runner brings it back and the worker replays the queued event.
STABLE_STATE_DIR="$run_root" "$run_root/dev-install/bin/stable" down >/dev/null 2>&1 || true
wait "$runner_pid" 2>/dev/null || true
runner_pid=

"$run_root/bin/agentctl" notify --run-root "$run_root/goals" --db "$run_root/state.db" --temporal "localhost:$port" \
  --goal "$goal_id" --event resume-event --kind design_changed >/dev/null
start_runner
wait_candidate
read_status
python3 - "$status_file" <<'PY'
import json,sys
x=json.load(open(sys.argv[1])); assert not x['verified'] and x['snapshot']['goal']['status']=='waiting', x
assert any(e['id']=='resume-event' and e['status']=='processed' for e in x['snapshot']['events']), x['snapshot']['events']
PY
e2e_accept_ready_candidate "$session_id" "accept-$goal_id"
for _ in $(seq 1 900); do
  e2e_allow_pending_approvals "$session_id" || true
  if read_status && python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["verified"] and any(e["id"]=="resume-event" and e["status"]=="processed" for e in x["snapshot"]["events"]) else 1)' "$status_file"; then break; fi
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
