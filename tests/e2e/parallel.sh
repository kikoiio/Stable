#!/usr/bin/env bash
# Source-level e2e for parallel goals: two explicitly created goals run
# concurrently on isolated design copies and independent KiCad sessions, both
# reach verified, and neither touches the other's files.
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
mkdir -p "$project_root/run"
run_root=${E2E_PARALLEL_ROOT:-$(mktemp -d "$project_root/run/parallel-XXXXXXXX")}
mkdir -p "$run_root"
port=${E2E_PARALLEL_PORT:-17339}
address="localhost:$port"
export STABLE_STATE_DIR="$run_root"
goal_a="par-a-$(date +%s)"
goal_b="par-b-$(date +%s)"
fixture_digest=$(sha256sum "$project_root/fixtures/sensor_board/sensor.kicad_sch" | cut -d' ' -f1)
runner_pid=
mock_pid=

runtime_status() {
  STABLE_STATE_DIR="$run_root" "$run_root/dev-install/bin/stable" runtime status
}

runtime_is_running() {
  local status
  status=$(runtime_status 2>/dev/null) || return 1
  python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin).get("running") else 1)' <<<"$status"
}

cleanup() {
  if [[ -x "$run_root/dev-install/bin/stable" ]]; then
    STABLE_STATE_DIR="$run_root" "$run_root/dev-install/bin/stable" down >/dev/null 2>&1 || true
  fi
  if [[ -n "$runner_pid" ]]; then kill "$runner_pid" 2>/dev/null || true; wait "$runner_pid" 2>/dev/null || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$run_root" 2>/dev/null || true
}
trap cleanup EXIT

source "$project_root/tests/e2e/lib.sh"
source "$project_root/tests/e2e/mock_model_env.sh"
source "$project_root/tests/e2e/candidate_accept.sh"
mkdir -p "$run_root/bin"
GOMAXPROCS=1 go build -buildvcs=false -o "$run_root/bin/agentctl" ./cmd/agentctl

STABLE_RUN_LOCAL_UP=1 STABLE_TEMPORAL_PORT="$port" bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
runner_pid=$!
for _ in $(seq 1 120); do
  if [[ -x "$run_root/bin/agentctl" ]] && grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null; then break; fi
  if ! kill -0 "$runner_pid" 2>/dev/null; then
    if [[ ! -x "$run_root/dev-install/bin/stable" ]] || ! runtime_is_running; then
      cat "$run_root/runner.log" >&2
      exit 1
    fi
  fi
  sleep 0.5
done
e2e_wait_for_chat_socket "$run_root/chat.sock"

session_a=$(e2e_new_session "$run_root/session-a.jsonl")
session_b=$(e2e_new_session "$run_root/session-b.jsonl")
create_goal() {
  local session=$1 goal=$2 output="$run_root/proposal-$2.jsonl"
  e2e_chat create_goal --session "$session" --goal "$goal" --text "修复传感器连接并确保 ERC 违规为零" >"$output"
  local proposal
  proposal=$(python3 - "$output" <<'PY'
import json,sys
messages=[json.loads(line) for line in open(sys.argv[1]) if line.strip()]
print(next(m['proposal']['id'] for m in messages if m.get('type')=='proposal'))
PY
)
  e2e_chat confirm --session "$session" --goal "$goal" --proposal "$proposal" >/dev/null
}
create_goal "$session_a" "$goal_a"
create_goal "$session_b" "$goal_b"

read_status() {
  local goal=$1 status_file=$2 error_file="$2.error"
  for _ in $(seq 1 20); do
    if "$run_root/bin/agentctl" status --run-root "$run_root" --goal "$goal" >"$status_file" 2>"$error_file"; then
      return 0
    fi
    if ! grep -q 'database is locked' "$error_file"; then
      cat "$error_file" >&2
      return 1
    fi
    sleep 0.1
  done
  cat "$error_file" >&2
  return 1
}

wait_and_accept() {
  local goal=$1 session=$2 status_file="$run_root/status-$1.json"
  ready=0
  for _ in $(seq 1 900); do
    e2e_allow_pending_approvals "$session" || true
    read_status "$goal" "$status_file" || true
    if [[ -s "$status_file" ]] && python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["snapshot"]; sys.exit(0 if any(a["status"] in ("candidate_ready","awaiting_accept") for a in (s.get("actions") or [])) else 1)' "$status_file"; then ready=1; break; fi
    if ! runtime_is_running; then
      cat "$run_root/worker.log" >&2
      exit 1
    fi
    sleep 0.5
  done
  if [[ "$ready" != 1 ]]; then echo "goal $goal did not produce a candidate" >&2; exit 1; fi
  e2e_accept_ready_candidate "$session" "accept-$goal"
  for _ in $(seq 1 900); do
    e2e_allow_pending_approvals "$session"
    read_status "$goal" "$status_file" || true
    if [[ -s "$status_file" ]] && python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["verified"] else 1)' "$status_file"; then return 0; fi
    sleep 0.5
  done
  echo "goal $goal did not verify" >&2
  exit 1
}

wait_and_accept "$goal_a" "$session_a" &
wait_a=$!
wait_and_accept "$goal_b" "$session_b" &
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
    assert hashlib.sha256((root/'goals'/goal/'sensor.kicad_sch').read_bytes()).hexdigest() == status['actual_artifact_id']
# Each goal got its own repaired copy digest; fixture untouched.
assert hashlib.sha256((project/'fixtures/sensor_board/sensor.kicad_sch').read_bytes()).hexdigest() == fixture_digest
assert digests[goal_a] and digests[goal_b]
print('PARALLEL E2E PASS', goal_a, goal_b)
PY
