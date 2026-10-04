#!/usr/bin/env bash
# M04 工具执行验证：fail-closed 拒绝、真实 bwrap 正例（Go 场景）、
# 普通任务全链流程与目标工作项全链流程（S01 案例运行器）。
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-1}

printf '\n==> M04 fail-closed tool executor\n'
go test -p 1 ./tests/e2e -run '^TestM04ToolExecutorFailClosed$' -count=1 -v

printf '\n==> M04 Linux namespace positive tool executor\n'
# The isolation boundary is bwrap, not a bare unshare: on hosts where AppArmor
# restricts unprivileged user namespaces, unshare(1) fails while the packaged
# bwrap profile still works. Probe the real dependency.
if ! command -v bwrap >/dev/null 2>&1 || ! bwrap --unshare-all --ro-bind / / /bin/true >/dev/null 2>&1; then
  printf 'SKIP: bwrap namespaces are unavailable; positive isolation evidence is not claimed.\n'
  exit 0
fi
helper=${STABLE_M04_HELPER:-}
if [[ -z "$helper" ]]; then
  if [[ -x "$project_root/dev-install/libexec/agentworker" ]]; then
    helper="$project_root/dev-install/libexec/agentworker"
  elif [[ -x "$project_root/bin/agentworker" ]]; then
    helper="$project_root/bin/agentworker"
  else
    helper_root=$(mktemp -d "${TMPDIR:-/tmp}/stable-m04-helper-XXXXXXXX")
    go build -buildvcs=false -o "$helper_root/agentworker" ./cmd/agentworker
    helper="$helper_root/agentworker"
  fi
fi
STABLE_M04_HELPER="$helper" go test -p 1 ./tests/e2e -run '^TestM04(SandboxPositive|SandboxEscapeTrio|CommandTimeoutAndExitSemantics|ToolFlowScrubsSecrets)$' -count=1 -v

printf '\n==> M04 normal task full flow (investigate → command → edit → review → accept)\n'
source "$project_root/tests/e2e/lib.sh"
e2e_alloc m04-tools
trap e2e_run_cleanups EXIT
run_root=$E2E_ROOT
source "$project_root/tests/e2e/mock_model_env.sh"
source "$project_root/tests/e2e/candidate_accept.sh"

STABLE_RUN_LOCAL_UP=1 STABLE_TEMPORAL_PORT="$E2E_PORT" bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/runner.log" 2>&1 &
runner_pid=$!
cleanup_runner() {
  if [[ -n "${runner_pid:-}" ]]; then kill "$runner_pid" 2>/dev/null || true; wait "$runner_pid" 2>/dev/null || true; fi
  if [[ -n "${mock_pid:-}" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
}
e2e_on_cleanup cleanup_runner
e2e_wait_for_chat_socket "$run_root/chat.sock"

# The chat service root doubles as the formal project for normal tasks.
formal="$run_root/dev-install/share"
cp "$project_root/tests/cases/cases/S01_missing_wire/sensor.kicad_sch" \
   "$project_root/tests/cases/cases/S01_missing_wire/sensor.kicad_pro" "$formal/"
mkdir -p "$E2E_ROOT/outside"
sentinel_marker="m04-e2e-sentinel-$$"
printf '%s\n' "$sentinel_marker" >"$E2E_ROOT/outside/sentinel.txt"
chmod 600 "$E2E_ROOT/outside/sentinel.txt"

# Manifest of the formal project excluding the .stable service subtree, which
# legitimately grows while the service runs.
formal_manifest() {
  (cd "$formal" && find . -path ./.stable -prune -o -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum) | sha256sum | cut -d' ' -f1
}

session_id=$(e2e_new_session "$run_root/session.jsonl")
digest_before=$(formal_manifest)

# --- Happy path: read → controlled command → edit, approvals granted ---
e2e_chat run_start --session "$session_id" --text "请修复 sensor 接线并运行命令验证" >"$run_root/run1.jsonl" &
run1_pid=$!
while kill -0 "$run1_pid" 2>/dev/null; do
  e2e_allow_pending_approvals "$session_id" || true
  sleep 0.3
done
wait "$run1_pid"
e2e_allow_pending_approvals "$session_id" || true

python3 - "$run_root/run1.jsonl" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
kinds = [m['run_event']['kind'] for m in messages if m.get('type') == 'run_event']
outcome = next(m['outcome'] for m in messages if m.get('type') == 'run_outcome')
assert outcome['status'] == 'completed', outcome
for tool in ('read_file', 'command', 'edit_file'):
    assert any(k == 'tool_exec_start' for k in kinds), kinds
payloads = json.dumps([m['run_event'] for m in messages if m.get('type') == 'run_event'], ensure_ascii=False)
for tool in ('read_file', 'command', 'edit_file'):
    assert tool in payloads, tool
# C03: the controlled command's stdout is observable in the run event stream.
assert 'm04-probe-' in payloads, payloads[-400:]
# C02: tool events carry monotonically increasing run sequences.
seqs = [m['run_event']['run_seq'] for m in messages if m.get('type') == 'run_event']
assert seqs == sorted(seqs), seqs
print('run1 events ok:', len(kinds), 'events, outcome completed')
PY
# C03: the command output also reached the next model request.
grep -q 'TOOL_RESULT .*m04-probe-' "$run_root/mock.log" || { echo 'command output did not reach the next model input' >&2; exit 1; }

# C26/C27: exactly one candidate, registered ready, owned by this session.
candidate_id=$(python3 - "$run_root/state.db" "$session_id" <<'PY'
import sqlite3, sys
rows = db_rows = None
with sqlite3.connect(sys.argv[1]) as db:
    rows = db.execute("SELECT id, status FROM candidates WHERE goal_id=?", ('session-' + sys.argv[2],)).fetchall()
assert len(rows) == 1, rows
assert rows[0][1] == 'ready', rows
print(rows[0][0])
PY
)
# C14/C07: the formal project is byte-identical before acceptance.
[[ "$(formal_manifest)" == "$digest_before" ]] || { echo 'formal project changed before acceptance' >&2; exit 1; }

# Review: diff shown must mention the edited file; all findings must pass.
e2e_chat review_get --session "$session_id" --candidate "$candidate_id" >"$run_root/review1.jsonl"
read -r preview_digest candidate_digest formal_digest < <(python3 - "$run_root/review1.jsonl" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
review = next(m['review'] for m in messages if m.get('type') == 'review')
findings = review.get('findings', [])
assert findings and all(f.get('result') == 'pass' for f in findings), findings
changes = json.dumps(review, ensure_ascii=False)
assert 'sensor.kicad_sch' in changes, changes[:400]
print(review['digest'], review['candidate_digest'], review['formal_digest'])
PY
)
e2e_chat review_accept --session "$session_id" --candidate "$candidate_id" \
  --decision "accept-m04-$session_id" --preview-digest "$preview_digest" \
  --candidate-digest "$candidate_digest" --formal-digest "$formal_digest" --mode normal \
  >"$run_root/accept1.jsonl"

# AC2/C14: after acceptance the formal project carries exactly the reviewed
# change; everything else is byte-identical.
grep -q '81110618-6579-58c6-8f1f-78d9234e76d6' "$formal/sensor.kicad_sch" || { echo 'accepted wire missing from formal project' >&2; exit 1; }
python3 - "$formal" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
sch = root / 'sensor.kicad_sch'
content = sch.read_text()
assert content.count('81110618-6579-58c6-8f1f-78d9234e76d6') == 1, 'wire applied more than once'
PY
# N6: a repeated accept with the same decision returns the same receipt and
# applies nothing twice.
e2e_chat review_accept --session "$session_id" --candidate "$candidate_id" \
  --decision "accept-m04-$session_id" --preview-digest "$preview_digest" \
  --candidate-digest "$candidate_digest" --formal-digest "$formal_digest" --mode normal \
  >"$run_root/accept2.jsonl"
python3 - "$run_root/accept1.jsonl" "$run_root/accept2.jsonl" "$run_root/state.db" "accept-m04-$session_id" <<'PY'
import json, sqlite3, sys
def receipt(path):
    messages = [json.loads(line) for line in open(path) if line.strip()]
    return next(m['receipt'] for m in messages if m.get('type') == 'acceptance')
r1, r2 = receipt(sys.argv[1]), receipt(sys.argv[2])
assert r1['id'] == r2['id'], (r1, r2)
with sqlite3.connect(sys.argv[3]) as db:
    applied = db.execute("SELECT count(*) FROM acceptance_apply_journal WHERE decision_id=? AND phase='finalized'", (sys.argv[4],)).fetchone()[0]
assert applied == 1, applied
print('accept idempotent:', r1['id'])
PY

# --- C15: cancel while awaiting approval leaves no tool execution behind ---
# Fresh sessions keep the mock's tool-round counting free of run1's history.
session2_id=$(e2e_new_session "$run_root/session2.jsonl")
before_candidates=$(python3 - "$run_root/state.db" <<'PY'
import sqlite3, sys
with sqlite3.connect(sys.argv[1]) as db:
    print(db.execute('SELECT count(*) FROM candidates').fetchone()[0])
PY
)
digest_after_accept=$(formal_manifest)
e2e_chat run_start --session "$session2_id" --text "请再检查一遍 sensor 接线" >"$run_root/run2.jsonl" &
run2_pid=$!
run2_id=
for _ in $(seq 1 200); do
  run2_id=$(python3 - "$run_root/run2.jsonl" <<'PY' 2>/dev/null
import json, sys
# The streaming client block-buffers its output, so the file's tail line may
# be mid-flush; skip unparseable lines instead of failing the whole scan.
run_id = ''
try:
    for line in open(sys.argv[1]):
        line = line.strip()
        if not line:
            continue
        try:
            m = json.loads(line)
        except ValueError:
            continue
        if m.get('type') == 'run_started':
            run_id = m['run_id']
            break
except OSError:
    pass
print(run_id)
PY
)
  [[ -n "$run2_id" ]] || { sleep 0.2; continue; }
  pending=$(e2e_chat approval_list --session "$session2_id" 2>/dev/null | python3 -c 'import json,sys; print(sum(len(m.get("approvals", [])) for m in (json.loads(l) for l in sys.stdin if l.strip())))' 2>/dev/null || echo 0)
  [[ "$pending" -gt 0 ]] && break
  kill -0 "$run2_pid" 2>/dev/null || break
  sleep 0.2
done
[[ -n "$run2_id" ]] || { echo 'run2 did not start' >&2; exit 1; }
e2e_chat run_cancel --session "$session2_id" --run "$run2_id" >/dev/null
wait "$run2_pid" || true
python3 - "$run_root/run2.jsonl" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
outcome = next((m['outcome'] for m in messages if m.get('type') == 'run_outcome'), None)
assert outcome is not None and outcome['status'] == 'cancelled', outcome
# The write tool whose approval was pending must never have executed: it
# entered the executor (tool_exec_start) and waited, but no tool_exec_result
# was ever emitted for it — the sandbox was never dispatched.
results = [m['run_event'] for m in messages if m.get('type') == 'run_event' and m['run_event']['kind'] == 'tool_exec_result']
assert all(e.get('payload', {}).get('tool_name') not in ('edit_file', 'write_file') for e in results), results
print('run2 cancelled cleanly, write tool never executed')
PY

# --- C15: denying the approval also produces no candidate write ---
session3_id=$(e2e_new_session "$run_root/session3.jsonl")
e2e_chat run_start --session "$session3_id" --text "请再一次检查 sensor 接线" >"$run_root/run3.jsonl" &
run3_pid=$!
while kill -0 "$run3_pid" 2>/dev/null; do
  while read -r approval_id; do
    [[ -n "$approval_id" ]] || continue
    e2e_chat approval_resolve --session "$session3_id" --approval "$approval_id" --choice deny >/dev/null || true
  done < <(e2e_chat approval_list --session "$session3_id" 2>/dev/null | python3 -c 'import json,sys
for l in sys.stdin:
    if l.strip():
        for a in json.loads(l).get("approvals", []):
            print(a["id"])' 2>/dev/null)
  sleep 0.3
done
wait "$run3_pid" || true
python3 - "$run_root/run3.jsonl" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
outcome = next((m['outcome'] for m in messages if m.get('type') == 'run_outcome'), None)
assert outcome is not None and outcome['status'] in ('completed', 'cancelled'), outcome
print('run3 finished after denials:', outcome['status'])
PY
python3 - "$run_root/state.db" "$before_candidates" <<'PY'
import sqlite3, sys
with sqlite3.connect(sys.argv[1]) as db:
    count = db.execute('SELECT count(*) FROM candidates').fetchone()[0]
assert count == int(sys.argv[2]), (count, sys.argv[2])
print('no new candidates after cancel/deny runs')
PY
[[ "$(formal_manifest)" == "$digest_after_accept" ]] || { echo 'formal project changed during cancel/deny runs' >&2; exit 1; }

# C19: neither the planted model credential nor the outside sentinel reached
# session logs, candidate trees, or the observed run stream.
secret_marker='e2e-secret-marker'
if grep -rqs "$secret_marker" "$formal/.stable/sessions" "$run_root/dev-install/.stable-candidates" 2>/dev/null; then
  echo 'model credential leaked into persisted records or candidates' >&2; exit 1
fi
if grep -qs "$secret_marker" "$run_root/run1.jsonl"; then
  echo 'model credential leaked into the run event stream' >&2; exit 1
fi
if grep -rqs "$sentinel_marker" "$formal" "$run_root/dev-install/.stable-candidates" 2>/dev/null; then
  echo 'outside sentinel leaked into the authorized tree' >&2; exit 1
fi
printf 'normal task flow PASS (session %s, candidate %s)\n' "$session_id" "$candidate_id"

printf '\n==> M04 goal work item full flow (S01 case runner)\n'
if ! command -v kicad-cli >/dev/null 2>&1 || ! command -v eeschema >/dev/null 2>&1 || ! command -v xvfb-run >/dev/null 2>&1; then
  printf 'SKIP: kicad/xvfb unavailable; goal work item evidence is not claimed.\n'
  exit 0
fi
# Run the case runner with TMPDIR unset: this script's own e2e_alloc exported a
# nested TMPDIR (<m04-root>/tmp), and the double-nested run root pushes the
# bridge's session control socket past the 108-byte sun_path limit.
env -u TMPDIR bash "$project_root/tests/cases/run_case.sh" S01_missing_wire

printf '\nM04 tool e2e suite passed.\n'
