#!/usr/bin/env bash
# Source-level e2e for V01 criteria-change invalidation: verify a goal, confirm
# new criteria while checks are deferred (Temporal down), observe
# pending_reverification and a history-only export, let the wake replay finish
# the reverification, then change criteria mid-check with a controlled pause and
# confirm the new revision converges without stale evidence counting.
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
e2e_alloc criteria
cd "$project_root"

run_root=$E2E_ROOT
port=$E2E_PORT
address="localhost:$port"
goal_id=$E2E_GOAL
mock_pid=
chat_pid=

cleanup() {
  if [[ -n "$chat_pid" ]]; then kill "$chat_pid" 2>/dev/null || true; wait "$chat_pid" 2>/dev/null || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
}
e2e_on_cleanup cleanup
trap e2e_run_cleanups EXIT

source "$project_root/tests/e2e/mock_model_env.sh"
source "$project_root/tests/e2e/candidate_accept.sh"
# chatserve binds the repo as its trusted project root; session traffic must
# address the same root (see trustedSessionRoot).
E2E_SESSION_ROOT="$project_root"

# Build the same dev-install layout the runtime supervisor expects: it derives
# libexec/share from its own executable location, so stable must live in
# dev-install/bin for the daemon stack to find workers, fixtures and schemas.
dev_root="$run_root/dev-install"
mkdir -p "$dev_root/bin" "$dev_root/libexec" "$dev_root/share"
go build -buildvcs=false -o "$dev_root/bin/stable" ./cmd/stable
go build -buildvcs=false -o "$dev_root/libexec/agentctl" ./cmd/agentctl
go build -buildvcs=false -o "$dev_root/libexec/agentworker" ./cmd/agentworker
temporal_bin=$(command -v temporal) || { echo 'temporal CLI required' >&2; exit 1; }
ln -sfn "$temporal_bin" "$dev_root/libexec/temporal"
# The sandbox refuses mounts that cross symbolic links, so the share tree must
# be real files (mirrors scripts/run_local.sh).
rm -rf "$dev_root/share/fixtures" "$dev_root/share/schemas" "$dev_root/share/workers"
cp -a "$project_root/fixtures" "$dev_root/share/fixtures"
cp -a "$project_root/schemas" "$dev_root/share/schemas"
cp -a "$project_root/workers" "$dev_root/share/workers"

# stable up daemonizes temporal/supervisor/worker/chat and returns; readiness
# is judged by fresh 'agent worker ready' lines (chatserve starts separately).
start_runner() {
  STABLE_TEMPORAL_PORT="$port" "$run_root/dev-install/bin/stable" up >"$run_root/up.log" 2>&1 || { cat "$run_root/up.log" >&2; return 1; }
  # up returns after daemonizing; wait for the worker to connect. Logs rotate
  # across down/up cycles, so match the ready line anywhere in the live file.
  for _ in $(seq 1 120); do
    if grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null; then return 0; fi
    sleep 0.5
  done
  tail -n 5 "$run_root/up.log" "$run_root/supervisor.log" >&2
  return 1
}

# chatserve is not part of the daemon stack; start it for the session. It
# rebinds the worker-owned chat socket, so start it unconditionally to keep the
# protocol's project root deterministic.
start_chat() {
  if [[ -n "$chat_pid" ]]; then kill "$chat_pid" 2>/dev/null || true; wait "$chat_pid" 2>/dev/null || true; fi
  "$dev_root/bin/stable" chatserve --db "$run_root/state.db" --socket "$run_root/chat.sock" \
    --temporal "$address" --project-root "$project_root" --run-root "$run_root/goals" >"$run_root/chatserve.log" 2>&1 &
  chat_pid=$!
  # 40×0.25s(10s)在慢 CI runner 上不够 chatserve 完成绑定;统一用 lib.sh 的 120s 等待。
  if e2e_wait_for_chat_socket "$run_root/chat.sock"; then return 0; fi
  cat "$run_root/chatserve.log" >&2
  return 1
}

stop_runner() {
  STABLE_STATE_DIR="$run_root" "$run_root/dev-install/bin/stable" down >/dev/null 2>&1 || true
}

stable_cli() { "$dev_root/bin/stable" "$@"; }
status_file="$run_root/status.json"
read_status() { "$dev_root/libexec/agentctl" status --run-root "$run_root" --goal "$goal_id" >"$status_file" 2>/dev/null; }
export_delivery() { "$dev_root/libexec/agentctl" export --run-root "$run_root" --goal "$goal_id" --out "$run_root/$1" >/dev/null; }
proposal_from() { python3 -c 'import json,sys; ms=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]; print(next(m["proposal"]["id"] for m in ms if m.get("type")=="proposal"))' "$1"; }
goal_update_field() { python3 -c 'import json,sys; ms=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]; g=next(m["goal"] for m in ms if m.get("type")=="goal_update"); print(g[sys.argv[2]])' "$1" "$2"; }
accept_seq=0
wait_status() { # python condition over status.json, driving approvals and candidate acceptance
  local probe=$1
  for _ in $(seq 1 900); do
    e2e_allow_pending_approvals "$session_id" || true
    if read_status && python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["snapshot"]; sys.exit(0 if any(a["status"] in ("candidate_ready","awaiting_accept") for a in (s.get("actions") or [])) else 1)' "$status_file"; then
      accept_seq=$((accept_seq+1))
      e2e_accept_ready_candidate "$session_id" "accept-$goal_id-$accept_seq" || true
    fi
    if read_status && python3 -c "$probe" "$status_file"; then return 0; fi
    sleep 0.5
  done
  echo "timed out waiting for status condition" >&2
  tail -n 5 "$run_root/worker.log" "$run_root/chatserve.log" >&2
  exit 1
}

start_runner
start_chat
session_id=$(e2e_new_session "$run_root/session.jsonl")

# --- phase 1: create, confirm and fully verify under revision 0 ---
e2e_chat create_goal --session "$session_id" --goal "$goal_id" --text "修复传感器连接，ERC 必须全过，J1 连接要恢复" >"$run_root/create-v0.jsonl"
proposal_v0=$(proposal_from "$run_root/create-v0.jsonl")
[[ -n "$proposal_v0" ]] || { echo 'no v0 proposal' >&2; exit 1; }
e2e_chat confirm --session "$session_id" --goal "$goal_id" --proposal "$proposal_v0" >"$run_root/confirm-v0.jsonl"
[[ "$(goal_update_field "$run_root/confirm-v0.jsonl" status)" == "active" ]] || { echo "confirm did not create goal" >&2; exit 1; }
wait_status 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["verified"] else 1)'
read_status
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); assert x["snapshot"]["goal"]["criteria_revision"]==0, x["snapshot"]["goal"]["criteria_revision"]' "$status_file"

# --- phase 2: confirm relaxed criteria while checks are deferred (Temporal down) ---
stop_runner
e2e_chat create_goal --session "$session_id" --goal "$goal_id" --text "修复传感器连接，放宽 ERC 允许 2 个违规，J1 连接要恢复" >"$run_root/create-v1.jsonl"
proposal_v1=$(proposal_from "$run_root/create-v1.jsonl")
[[ -n "$proposal_v1" ]] || { echo 'no v1 proposal' >&2; exit 1; }
e2e_chat confirm --session "$session_id" --goal "$goal_id" --proposal "$proposal_v1" >"$run_root/confirm-v1.jsonl"
[[ "$(goal_update_field "$run_root/confirm-v1.jsonl" status)" == "pending_reverification" ]] || { echo "confirm did not defer to re-verification" >&2; exit 1; }
read_status
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); g=x["snapshot"]["goal"]; assert g["status"]=="pending_reverification" and g["criteria_revision"]==1, g' "$status_file"

export_delivery delivery-p2
python3 - "$run_root" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
assert not (root/'delivery-p2'/'erc.json').exists(), 'deferred export must not carry a current ERC report'
delivery = json.loads((root/'delivery-p2'/'delivery.json').read_text())
assert not delivery['verified'], delivery['unverified']
current = [e for e in delivery['evidence'] if e['current']]
assert not current, f'stale current evidence after confirm: {current}'
history = json.loads((root/'delivery-p2'/'history'/'history.json').read_text())
assert history, 'revision-0 evidence not archived'
assert all(e['invalidated_reason'] for e in history), history
erc_hist = [e for e in history if e['criterion_id'] == 'erc-clean']
assert erc_hist and all(e['result'] == 'pass' for e in erc_hist), history
print('phase2 PASS: pending_reverification export keeps history only')
PY

# --- phase 3: restart; the pending wake replays and auto reverification passes ---
start_runner
start_chat
# Freeze the goal before exporting: after verification the goal re-evaluates
# on its 30s interval, and one evaluation can occupy the goal for minutes
# under load (CI observed a single EvaluateGoal holding it waiting/"computer
# session opened" for 7 minutes), so polling export until it catches a
# verified snapshot is unreliable — delivery.verified also requires the goal
# to be quiescent. Stop the worker the moment the goal reads verified, export
# the frozen state, and if an evaluation slipped in first, restart and retry.
freeze_verified() { # revision; leaves the worker stopped on a verified goal
  local rev=$1 attempt
  for attempt in $(seq 1 5); do
    wait_status 'import json,sys; x=json.load(open(sys.argv[1])); g=x["snapshot"]["goal"]; sys.exit(0 if x["verified"] and g["status"]=="verified" and g["criteria_revision"]=='"$rev"' else 1)'
    stop_runner
    read_status
    if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); g=x["snapshot"]["goal"]; sys.exit(0 if x["verified"] and g["status"]=="verified" else 1)' "$status_file"; then
      return 0
    fi
    start_runner
    start_chat
  done
  echo 'goal did not stay verified across worker stop' >&2
  exit 1
}
freeze_verified 1
export_delivery delivery-p3
python3 - "$run_root" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
delivery = json.loads((root/'delivery-p3'/'delivery.json').read_text())
assert delivery['verified'], delivery['unverified']
current = [e for e in delivery['evidence'] if e['current']]
assert current and all(e['criteria_revision'] == '1' for e in current), current
erc = json.loads((root/'delivery-p3'/'erc.json').read_text())
total = sum(len(s['violations']) for s in erc['sheets'])
assert total <= 2, total
print('phase3 PASS: replayed wake re-verified under revision 1')
PY

# --- phase 4: confirm tightened criteria mid-run, controlled pause, converge ---
# freeze_verified left the worker stopped; bring it back so the v2 confirm
# lands mid-run instead of taking the deferred path covered by phase 2.
# stable up rebinds the chat socket with its own root, so chatserve must be
# restarted too before any e2e_chat call.
start_runner
start_chat
e2e_chat create_goal --session "$session_id" --goal "$goal_id" --text "修复传感器连接，ERC 必须全过，J1 连接要恢复" >"$run_root/create-v2.jsonl"
proposal_v2=$(proposal_from "$run_root/create-v2.jsonl")
[[ -n "$proposal_v2" ]] || { echo 'no v2 proposal' >&2; exit 1; }
e2e_chat confirm --session "$session_id" --goal "$goal_id" --proposal "$proposal_v2" >"$run_root/confirm-v2.jsonl"
[[ "$(goal_update_field "$run_root/confirm-v2.jsonl" status)" == "pending_reverification" ]] || { echo "v2 confirm did not defer" >&2; exit 1; }
stop_runner
sleep 2
read_status
python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); g=x["snapshot"]["goal"]; assert g["status"]=="pending_reverification" and g["criteria_revision"]==2, g' "$status_file"
start_runner
start_chat
freeze_verified 2
export_delivery delivery-p4
python3 - "$run_root" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
delivery = json.loads((root/'delivery-p4'/'delivery.json').read_text())
assert delivery['verified'], delivery['unverified']
current = [e for e in delivery['evidence'] if e['current']]
assert current and all(e['criteria_revision'] == '2' for e in current), current
assert any(e['criteria_revision'] == '1' and not e['current'] for e in delivery['evidence']), 'revision-1 evidence missing from history view'
history = json.loads((root/'delivery-p4'/'history'/'history.json').read_text())
reports = ' '.join(e.get('archived_report', '') for e in history)
assert '-v0-' in reports and '-v1-' in reports, history
erc = json.loads((root/'delivery-p4'/'erc.json').read_text())
total = sum(len(s['violations']) for s in erc['sheets'])
assert total == 0, total
print('phase4 PASS: mid-check change converged on revision 2 without stale evidence')
PY

printf 'E2E CRITERIA PASS %s\nEvidence: %s\n' "$goal_id" "$run_root/delivery"
