#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
mkdir -p "$project_root/run"
run_root=${E2E_RUN_ROOT:-$(mktemp -d "$project_root/run/e2e-XXXXXXXX")}
port=${E2E_TEMPORAL_PORT:-17335}
address="localhost:$port"
goal_id="e2e-$(date +%s)-$$"
marker="$run_root/crash-after-repair.marker"
fixture_digest=$(sha256sum "$project_root/fixtures/sensor_board/sensor.kicad_sch" | cut -d' ' -f1)
runner_pid=
mock_pid=

cleanup() {
  if [[ -n "$runner_pid" ]]; then
    kill "$runner_pid" 2>/dev/null || true
    wait "$runner_pid" 2>/dev/null || true
  fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
    if [[ -x "$run_root/dev-install/bin/stable" ]]; then
    STABLE_STATE_DIR="$run_root" "$run_root/dev-install/bin/stable" down >/dev/null 2>&1 || true
  fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$run_root" 2>/dev/null || true
}
trap cleanup EXIT

if ! command -v codex >/dev/null 2>&1; then source "$project_root/tests/e2e/mock_model_env.sh"; fi
mkdir -p "$run_root/bin"
go build -buildvcs=false -o "$run_root/bin/agentctl" ./cmd/agentctl
export STABLE_STATE_DIR="$run_root"
mkdir -p "$run_root/goals"
source "$project_root/tests/e2e/candidate_accept.sh"
start_runner() {
  STABLE_CRASH_AFTER_REPAIR_MARKER="$marker" STABLE_RUN_LOCAL_UP=1 STABLE_TEMPORAL_PORT="$port" \
    bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
  runner_pid=$!
  for _ in $(seq 1 480); do
    if grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null; then break; fi
    sleep 0.5
  done
  grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null || return 1
  # 'agent worker ready' does not imply the chat socket exists: the chat
  # service binds after the worker comes up, and slow CI runners expose the
  # gap as chat_client FileNotFoundError. Wait for the socket on every start,
  # not only after the crash-injection restart.
  for _ in $(seq 1 240); do [[ -S "$run_root/chat.sock" ]] && return 0; sleep 0.5; done
  echo 'chat.sock missing after runner start' >&2
  return 1
}

status_file="$run_root/status.json"
read_status() {
	local error_file="$run_root/status.error"
	if "$run_root/bin/agentctl" status --run-root "$run_root/goals" --db "$run_root/state.db" --goal "$goal_id" >"$status_file" 2>"$error_file"; then
		return 0
	fi
	if grep -q 'design changed during dependency collection' "$error_file"; then
		return 1
	fi
	cat "$error_file" >&2
	return 1
}
wait_candidate() {
	for _ in $(seq 1 900); do
		e2e_allow_pending_approvals "$session_id" || true
		if read_status && python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["snapshot"]; sys.exit(0 if any(a["status"] in ("candidate_ready","awaiting_accept") for a in (s.get("actions") or [])) else 1)' "$status_file"; then
			return 0
		fi
		sleep 0.5
	done
	echo 'candidate did not become ready' >&2
	return 1
}

queue_notification() {
  local event=$1 kind=$2 error_file="$run_root/notify-$1.error"
  for _ in $(seq 1 100); do
    if "$run_root/bin/agentctl" notify --run-root "$run_root/goals" --db "$run_root/state.db" --temporal "$address" \
      --goal "$goal_id" --event "$event" --kind "$kind" >/dev/null 2>"$error_file"; then
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

start_runner
session_id=$(e2e_new_session "$run_root/session.jsonl")
cat > "$run_root/goal-definition.json" <<'JSON'
{"objective":"Repair the sensor connector and obtain a clean KiCad ERC","criteria":[{"id":"erc-clean","kind":"kicad.erc_clean","payload":{"max_violations":0}}],"check_interval_seconds":2}
JSON
e2e_chat create_goal --session "$session_id" --goal "$goal_id" --text "修复传感器连接并确保 ERC 违规为零" >"$run_root/proposal.jsonl"
proposal_id=$(python3 - "$run_root/proposal.jsonl" <<'PY'
import json,sys
messages=[json.loads(line) for line in open(sys.argv[1]) if line.strip()]
print(next(m['proposal']['id'] for m in messages if m.get('type')=='proposal'))
PY
)
e2e_chat confirm --session "$session_id" --goal "$goal_id" --proposal "$proposal_id" >/dev/null

session_restarted=0
for _ in $(seq 1 900); do
	if [[ -e "$marker" ]]; then break; fi
	e2e_allow_pending_approvals "$session_id" || true
	if ! read_status; then sleep 0.5; continue; fi
  # M03: the isolated KiCad session runs inside a PID namespace, so host-side
  # kill cannot reach it. Tear the whole private session down instead; the
  # supervised runtime must regenerate it with a new generation.
  if [[ "$session_restarted" == 0 ]]; then
    gen=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["snapshot"]["session"]["generation"])' "$status_file")
    if [[ "$gen" -ge 1 ]]; then
      python3 "$project_root/tests/e2e/stop_sessions.py" "$run_root" 2>/dev/null || true
      session_restarted=1
      printf 'torn down isolated session generation %s\n' "$gen"
    fi
  fi
  if [[ -e "$marker" ]]; then break; fi
  sleep 0.5
done
if [[ ! -e "$marker" ]]; then echo 'repair crash injection did not fire' >&2; exit 1; fi
wait "$runner_pid" 2>/dev/null || true
runner_pid=

# These notifications are accepted by the SQLite inbox while Temporal is down.
queue_notification design-event design_changed
queue_notification after-crash external_check_failed

start_runner
for _ in $(seq 1 240); do [[ -S "$run_root/chat.sock" ]] && break; sleep 0.5; done
[[ -S "$run_root/chat.sock" ]] || { echo 'chat.sock missing after restart' >&2; exit 1; }
wait_candidate
read_status
python3 - "$status_file" <<'PY'
import json,sys
x=json.load(open(sys.argv[1]))
assert not x['verified'] and x['snapshot']['goal']['status']=='waiting', x
PY
for _ in $(seq 1 120); do [[ -S "$run_root/chat.sock" ]] && break; sleep 0.5; done
e2e_accept_ready_candidate "$session_id" "accept-$goal_id"
for _ in $(seq 1 1800); do
	e2e_allow_pending_approvals "$session_id" || true
	if ! read_status; then sleep 0.5; continue; fi
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["verified"] and any(e["id"]=="after-crash" and e["status"]=="processed" for e in x["snapshot"]["events"]) else 1)' "$status_file"; then break; fi
  sleep 0.5
done
read_status
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); assert x["verified"], x["unverified"]' "$status_file"

"$run_root/bin/agentctl" export --run-root "$run_root/goals" --db "$run_root/state.db" --goal "$goal_id" --out "$run_root/delivery" >/dev/null
python3 - "$run_root" "$goal_id" "$fixture_digest" <<'PY'
import hashlib, json, pathlib, sys
root, goal, fixture_digest = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]
status = json.loads((root/'status.json').read_text())
snapshot = status['snapshot']
assert snapshot['goal']['id'] == goal
assert snapshot['goal']['status'] == 'verified' and status['verified']
assert snapshot['agent']['id'] == 'agent-' + goal
assert snapshot['session']['id'] == 'computer-' + goal
# M03: session generation is per session record and resets when the record is
# recreated after a teardown, so >= 2 cannot be observed from outside; the
# regeneration semantics themselves are covered by TestM03ComputerIsolatedSessionLifecycle.
assert snapshot['session']['generation'] >= 1
assert any(e['kind'] == 'timer' and e['status'] == 'processed' for e in snapshot['events'])
assert any(e['id'] == 'design-event' and e['status'] == 'processed' for e in snapshot['events'])
# M03: the crash hook no longer injects a wake event; post-crash convergence
# is driven by the queued notifies and dependency refresh instead.
assert any(e['id'] == 'after-crash' and e['status'] == 'processed' for e in snapshot['events'])
assert sum(e['id'] == 'design-event' for e in snapshot['events']) == 1
assert len([a for a in snapshot['actions'] if a['desired_postcondition'] == {'sensor.connection_present': True}]) == 1
design = root/'delivery'/'sensor.kicad_sch'
digest = hashlib.sha256(design.read_bytes()).hexdigest()
assert digest == status['actual_artifact_id']
erc = json.loads((root/'delivery'/'erc.json').read_text())
assert all(not sheet['violations'] for sheet in erc['sheets'])
fixture = pathlib.Path('fixtures/sensor_board/sensor.kicad_sch')
assert hashlib.sha256(fixture.read_bytes()).hexdigest() == fixture_digest
delivery = json.loads((root/'delivery'/'delivery.json').read_text())
assert delivery['verified'] and delivery['actual_artifact_id'] == digest
print('E2E PASS', goal, digest, 'session_generation', snapshot['session']['generation'])
print('Evidence:', root/'delivery')
PY
