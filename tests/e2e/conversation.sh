#!/usr/bin/env bash
# Source-level e2e for the conversational goal loop: NL criteria transpilation
# with explicit confirmation, mid-run steering, in-session human Q&A across a
# runtime restart, and conversation-bearing export. Uses the loopback mock
# model for both transpilation and decisions.
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
mkdir -p run
run_root=${E2E_CONV_ROOT:-$(mktemp -d "$project_root/run/conv-XXXXXXXX")}
port=${E2E_CONV_PORT:-17338}
address="localhost:$port"
goal_id="conv-$(date +%s)"
fixture_digest=$(sha256sum "$project_root/fixtures/sensor_board/sensor.kicad_sch" | cut -d' ' -f1)
runner_pid=
mock_pid=
chat_pid=

cleanup() {
  if [[ -n "$runner_pid" ]]; then kill "$runner_pid" 2>/dev/null || true; wait "$runner_pid" 2>/dev/null || true; fi
  if [[ -n "$chat_pid" ]]; then kill "$chat_pid" 2>/dev/null || true; wait "$chat_pid" 2>/dev/null || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$run_root" 2>/dev/null || true
}
trap cleanup EXIT

source "$project_root/tests/e2e/mock_model_env.sh"
export STABLE_STATE_DIR="$run_root"

go build -buildvcs=false -o "$run_root/bin/stable" ./cmd/stable

start_runner() {
  STABLE_TEMPORAL_PORT="$port" bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
  runner_pid=$!
  for _ in $(seq 1 120); do
    if [[ -x "$run_root/bin/agentctl" ]] && grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null; then return 0; fi
    if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/runner.log" >&2; return 1; fi
    sleep 0.5
  done
  return 1
}

start_chat() {
  "$run_root/bin/stable" chatserve --db "$run_root/state.db" --socket "$run_root/chat.sock" \
    --temporal "$address" --project-root "$project_root" --run-root "$run_root" >"$run_root/chatserve.log" 2>&1 &
  chat_pid=$!
  for _ in $(seq 1 40); do [[ -S "$run_root/chat.sock" ]] && return 0; sleep 0.25; done
  cat "$run_root/chatserve.log" >&2
  return 1
}

stable_cli() { "$run_root/bin/stable" "$@"; }
status_file="$run_root/status.json"
read_status() { "$run_root/bin/agentctl" status --run-root "$run_root" --goal "$goal_id" >"$status_file" 2>/dev/null; }

start_runner
start_chat

# --- goal creation: transpile then explicit confirm; reject path first ---
reject_out=$(stable_cli chat --create-goal "让它看起来美观" --goal "$goal_id")
[[ "$reject_out" == *"无法为该描述定义可验证的验收标准"* ]] || { echo "reject flow broken: $reject_out" >&2; exit 1; }
if read_status; then echo 'reject created a goal' >&2; exit 1; fi

create_out=$(stable_cli chat --create-goal "修复传感器连接，ERC 必须全过，J1 连接要恢复" --goal "$goal_id")
proposal_id=$(sed -n 's/^proposal \(prop-[0-9a-f]*\) status=proposed$/\1/p' <<<"$create_out")
[[ -n "$proposal_id" ]] || { echo "no proposal in: $create_out" >&2; exit 1; }
confirm_out=$(stable_cli chat --confirm "$proposal_id" --goal "$goal_id")
[[ "$confirm_out" == *"goal $goal_id status=active"* ]] || { echo "confirm did not create goal: $confirm_out" >&2; exit 1; }

# --- wait for the goal to reach waiting state with the computer open ---
for _ in $(seq 1 360); do
  read_status
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); s=x["snapshot"]; sys.exit(0 if s["goal"]["status"]=="waiting" and s["session"]["generation"]>=1 else 1)' "$status_file"; then break; fi
  if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/worker.log" >&2; exit 1; fi
  sleep 0.5
done
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); assert x["snapshot"]["goal"]["status"]=="waiting"' "$status_file"

# --- mid-run steering: user message is consumed at the next decision round ---
say_out=$(stable_cli chat --goal "$goal_id" --say "方向确认：继续修复传感器连接")
[[ "$say_out" == *"[user/text]"* ]] || { echo "say echo missing: $say_out" >&2; exit 1; }
for _ in $(seq 1 120); do
  read_status
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if any(e["kind"]=="user_message" and e["status"]=="processed" for e in x["snapshot"]["events"]) else 1)' "$status_file"; then break; fi
  sleep 0.5
done
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); assert sum(e["kind"]=="user_message" for e in x["snapshot"]["events"])==1' "$status_file"

# --- in-session Q&A: marker triggers ask_human and an agent question ---
stable_cli chat --goal "$goal_id" --say "这个烧痕我修不了，请停下来问我" >/dev/null
for _ in $(seq 1 120); do
  read_status
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["snapshot"]["goal"]["status"]=="needs_human" else 1)' "$status_file"; then break; fi
  sleep 0.5
done
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); assert x["snapshot"]["goal"]["status"]=="needs_human"' "$status_file"
decisions_before=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["snapshot"]["decisions"]))' "$status_file")

# The question survives a full runtime restart; the timer stays quiet across
# several would-be check intervals.
kill "$runner_pid" 2>/dev/null || true; wait "$runner_pid" 2>/dev/null || true; runner_pid=
start_runner
start_chat
sleep 35
read_status
decisions_after=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["snapshot"]["decisions"]))' "$status_file")
[[ "$decisions_before" == "$decisions_after" ]] || { echo "timer re-decided while waiting for human ($decisions_before -> $decisions_after)" >&2; exit 1; }

stable_cli chat --goal "$goal_id" --reply "按 J1.2 处理，修复后继续 ERC" >/dev/null
for _ in $(seq 1 900); do
  read_status
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["verified"] else 1)' "$status_file"; then break; fi
  if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/worker.log" >&2; exit 1; fi
  sleep 0.5
done
read_status
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); assert x["verified"], x["unverified"]' "$status_file"

# --- export carries the conversation and criteria confirmation chain ---
"$run_root/bin/agentctl" export --run-root "$run_root" --goal "$goal_id" --out "$run_root/delivery" >/dev/null
python3 - "$run_root" "$goal_id" "$fixture_digest" "$proposal_id" <<'PY'
import hashlib, json, pathlib, sys
root, goal, fixture_digest, proposal_id = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
status = json.loads((root/'status.json').read_text())
snap = status['snapshot']
assert snap['goal']['status'] == 'verified' and status['verified']
assert snap['goal']['criteria_revision'] == 0
assert snap['criteria_proposals'], 'no proposals in snapshot'
p = [x for x in snap['criteria_proposals'] if x['id'] == proposal_id][0]
assert p['status'] == 'confirmed' and p['goal_id'] == goal and '修复传感器连接' in p['raw_text']
assert {c['kind'] for c in p['criteria']} == {'kicad.erc_clean', 'sensor.connection_present'}
roles = {m['role'] for m in snap['conversation']}
assert 'user' in roles and 'system' in roles
kinds_msg = [m['kind'] for m in snap['conversation']]
assert 'criteria_proposal' in kinds_msg and 'criteria_confirm' in kinds_msg and 'reply' in kinds_msg
delivery = json.loads((root/'delivery'/'delivery.json').read_text())
assert delivery['snapshot']['conversation'], 'delivery missing conversation'
assert hashlib.sha256((root/'delivery'/'sensor.kicad_sch').read_bytes()).hexdigest() == status['actual_artifact_id']
fixture = pathlib.Path('fixtures/sensor_board/sensor.kicad_sch')
assert hashlib.sha256(fixture.read_bytes()).hexdigest() == fixture_digest
print('CONVERSATION E2E PASS', goal, status['actual_artifact_id'])
PY
printf 'Evidence: %s\n' "$run_root/delivery"
