#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
version=$(tr -d '[:space:]' < "$project_root/VERSION")
pkg_name="stable-$version-linux-amd64"
test_root=$(mktemp -d /tmp/stable-package-restart-XXXXXXXX)
mock_pid=
cleanup() {
  stable down >/dev/null 2>&1 || true
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$HOME/.local/state/stable" 2>/dev/null || true
}
trap cleanup EXIT
tar -xzf "$project_root/dist/$pkg_name.tar.gz" -C "$test_root"
export HOME="$test_root/home"
mkdir -p "$HOME"
bash "$test_root/$pkg_name/install.sh"
export PATH="$HOME/.local/bin:/usr/bin:/bin"
python3 "$project_root/tests/package/mock_model.py" > "$test_root/mock.port" &
mock_pid=$!
for _ in $(seq 1 50); do [[ -s "$test_root/mock.port" ]] && break; sleep 0.1; done
export STABLE_PROVIDER=openai-compatible STABLE_MODEL=mock STABLE_API_KEY=restart-secret-marker
export STABLE_BASE_URL="http://127.0.0.1:$(cat "$test_root/mock.port")/v1"
export STABLE_TEMPORAL_PORT=17388
export STABLE_CRASH_AFTER_REPAIR_MARKER="$test_root/repair.marker"
cd /tmp
stable up > "$test_root/up1.json"
# M03: repair lands in a candidate; drive review/acceptance over the chat
# protocol the way the TUI would. The worker serves the socket and binds the
# installed share as its trusted project root.
source "$project_root/tests/e2e/candidate_accept.sh"
run_root="$HOME/.local/state/stable"
E2E_SESSION_ROOT="$HOME/.local/opt/stable/$version/share"
status_file="$test_root/status.json"
for _ in $(seq 1 120); do [[ -S "$run_root/chat.sock" ]] && break; sleep 0.5; done
[[ -S "$run_root/chat.sock" ]] || { echo 'chat.sock missing after up' >&2; exit 1; }
session_id=$(e2e_new_session "$test_root/session.jsonl")
# M03: acceptance is session-bound (goal SourceSessionID must match the
# accepting session), so the goal is created through the trusted chat entry.
e2e_chat create_goal --session "$session_id" --goal restart-test --text "修复传感器连接，ERC 必须全过，J1 连接要恢复" > "$test_root/create.jsonl"
proposal_id=$(python3 - "$test_root/create.jsonl" <<'PY'
import json,sys
messages=[json.loads(line) for line in open(sys.argv[1]) if line.strip()]
print(next(m['proposal']['id'] for m in messages if m.get('type')=='proposal'))
PY
)
e2e_chat confirm --session "$session_id" --goal restart-test --proposal "$proposal_id" > "$test_root/confirm.jsonl"
for _ in $(seq 1 120); do
  # Approvals gate the repair action; answer them so the crash can fire.
  e2e_allow_pending_approvals "$session_id" || true
  [[ -e "$test_root/repair.marker" ]] && break
  sleep 1
done
[[ -e "$test_root/repair.marker" ]] || { echo 'repair crash did not fire' >&2; exit 1; }
for _ in $(seq 1 30); do stable runtime status >/dev/null 2>&1 || break; sleep 1; done
if stable runtime status >/dev/null 2>&1; then echo 'supervisor did not stop after Worker crash' >&2; exit 1; fi
stable goal notify --goal restart-test --event offline-1 --kind external_check_failed > "$test_root/offline.txt"
stable goal notify --goal restart-test --event offline-1 --kind external_check_failed >> "$test_root/offline.txt"
stable up > "$test_root/up2.json"
accept_seq=0
for _ in $(seq 1 120); do
  e2e_allow_pending_approvals "$session_id" || true
  if stable goal status --goal restart-test > "$status_file" 2>/dev/null; then
    if python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["snapshot"]; sys.exit(0 if any(a["status"] in ("candidate_ready","awaiting_accept") for a in (s.get("actions") or [])) else 1)' "$status_file"; then
      e2e_accept_ready_candidate "$session_id" "accept-restart-$accept_seq" || true
      accept_seq=$((accept_seq+1))
    fi
    if python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); sys.exit(0 if s["verified"] and any(e["id"]=="offline-1" and e["status"]=="processed" for e in s["snapshot"]["events"]) else 1)' "$status_file"; then break; fi
  fi
  sleep 1
done
python3 - "$status_file" <<'PY'
import json,sys
s=json.load(open(sys.argv[1])); assert s['verified'],s['unverified']
snap=s['snapshot']; assert sum(e['id']=='offline-1' for e in snap['events'])==1
assert sum(a['desired_postcondition']=={'sensor.connection_present':True} for a in snap['actions'])==1
assert any(c['status']=='succeeded' for c in snap['model_calls'])
PY
stable goal export --goal restart-test --out "$test_root/delivery" > /dev/null
stable down > /dev/null
printf 'PACKAGE RESTART PASS %s\n' "$test_root"
