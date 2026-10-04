#!/usr/bin/env bash
# 用法: tests/cases/run_case.sh <case_id>
set -euo pipefail
case_id=${1:?usage: run_case.sh <case_id>}
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
src="$project_root/tests/cases/cases/$case_id"
[[ -d $src ]] || { echo "no such case: $case_id" >&2; exit 2; }
expect=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["expect"])' "$src/case.json")

if [[ $case_id != S01_missing_wire ]]; then
  echo "legacy case runner is unavailable after agentctl start removal: $case_id" >&2
  exit 2
fi

source "$project_root/tests/e2e/lib.sh"
e2e_alloc "case-$case_id"
run_root=$E2E_ROOT
port=$E2E_PORT
goal_id=$E2E_GOAL
runner_pid=
mock_pid=
cleanup() {
  if [[ -n $runner_pid ]]; then kill "$runner_pid" 2>/dev/null || true; wait "$runner_pid" 2>/dev/null || true; fi
  if [[ -n $mock_pid ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
}
e2e_on_cleanup cleanup
trap e2e_run_cleanups EXIT
mkdir -p "$run_root/bin"
if ! command -v codex >/dev/null 2>&1; then source "$project_root/tests/e2e/mock_model_env.sh"; fi
source "$project_root/tests/e2e/candidate_accept.sh"
status_file="$run_root/status.json"

STABLE_RUN_LOCAL_UP=1 STABLE_TEMPORAL_PORT="$port" bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
runner_pid=$!
for _ in $(seq 1 480); do
  if grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null && [[ -S "$run_root/chat.sock" ]]; then break; fi
  if ! kill -0 "$runner_pid" 2>/dev/null; then cat "$run_root/runner.log" >&2; exit 1; fi
  sleep 0.5
done
[[ -S "$run_root/chat.sock" ]] || { echo 'chat socket did not become ready' >&2; exit 1; }

session_id=$(e2e_new_session "$run_root/session.jsonl")
e2e_chat create_goal --session "$session_id" --goal "$goal_id" \
  --text "修复传感器连接，ERC 必须全过，J1 连接要恢复" >"$run_root/proposal.jsonl"
proposal_id=$(python3 - "$run_root/proposal.jsonl" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
print(next(m['proposal']['id'] for m in messages if m.get('type') == 'proposal'))
PY
)
e2e_chat confirm --session "$session_id" --goal "$goal_id" --proposal "$proposal_id" >"$run_root/confirm.jsonl"
python3 - "$run_root/confirm.jsonl" "$session_id" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
goal = next(m['goal'] for m in messages if m.get('type') == 'goal_update')
assert goal['status'] == 'active', goal
assert goal['source_session_id'] == sys.argv[2], goal
PY

wait_candidate() {
  for _ in $(seq 1 900); do
    e2e_allow_pending_approvals "$session_id" || true
    if "$run_root/dev-install/libexec/agentctl" status --run-root "$run_root/goals" --db "$run_root/state.db" \
      --project-root "$project_root" --goal "$goal_id" >"$status_file" 2>"$run_root/status.error" && \
      python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["snapshot"]; sys.exit(0 if any(a["status"] in ("candidate_ready", "awaiting_accept") for a in (s.get("actions") or [])) else 1)' "$status_file"; then
      return 0
    fi
    sleep 0.5
  done
  cat "$run_root/status.error" >&2
  echo 'S01 candidate did not become ready' >&2
  return 1
}

wait_candidate
pre_accept_digest=$(python3 - "$status_file" <<'PY'
import hashlib, json, pathlib, sys
status = json.loads(pathlib.Path(sys.argv[1]).read_text())
formal_path = pathlib.Path(status['snapshot']['goal']['artifact_path'])
print(hashlib.sha256(formal_path.read_bytes()).hexdigest())
PY
)
e2e_accept_ready_candidate "$session_id" "accept-$goal_id"
"$run_root/dev-install/libexec/agentctl" status --run-root "$run_root/goals" --db "$run_root/state.db" \
  --project-root "$project_root" --goal "$goal_id" >"$status_file"
python3 - "$status_file" "$run_root/acceptance-accept-$goal_id.jsonl" "$session_id" "$pre_accept_digest" "$run_root/state.db" <<'PY'
import hashlib, json, pathlib, sqlite3, sys
status = json.loads(pathlib.Path(sys.argv[1]).read_text())
snapshot = status['snapshot']
acceptance_path = pathlib.Path(sys.argv[2])
session_id = sys.argv[3]
pre_accept_digest = sys.argv[4]
decision_id = 'accept-' + snapshot['goal']['id']
messages = [json.loads(line) for line in acceptance_path.read_text().splitlines() if line]
assert snapshot['goal']['source_session_id'] == session_id
assert snapshot['goal']['status'] == 'pending_reverification', snapshot['goal']
assert not status['verified']
receipts = [m['receipt'] for m in messages if m.get('type') == 'acceptance']
assert len(receipts) == 1, messages
assert receipts[0]['decision_id'] == decision_id, receipts
assert len([a for a in snapshot['actions'] if a['desired_postcondition'] == {'sensor.connection_present': True} and a['status'] == 'applied']) == 1, snapshot['actions']
formal_path = pathlib.Path(snapshot['goal']['artifact_path'])
formal_digest = hashlib.sha256(formal_path.read_bytes()).hexdigest()
assert formal_digest != pre_accept_digest, (pre_accept_digest, formal_digest)
assert formal_digest == status['actual_artifact_id'] == snapshot['goal']['current_artifact_id']
with sqlite3.connect(sys.argv[5]) as db:
    receipt_count, finalized_count, goal_receipt_count = db.execute(
        '''SELECT
             (SELECT count(*) FROM acceptance_receipts WHERE decision_id=?),
             (SELECT count(*) FROM acceptance_apply_journal WHERE decision_id=? AND phase='finalized'),
             (SELECT count(*) FROM acceptance_receipts r JOIN candidates c ON c.id=r.candidate_id WHERE c.goal_id=?)''',
        (decision_id, decision_id, snapshot['goal']['id']),
    ).fetchone()
assert receipt_count == finalized_count == goal_receipt_count == 1, (receipt_count, finalized_count, goal_receipt_count)
print('PASS', snapshot['goal']['id'], 'status=pending_reverification', 'formal_digest=' + formal_digest)
PY
echo "evidence: $run_root"
