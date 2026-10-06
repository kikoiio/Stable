#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
version=$(tr -d '[:space:]' < "$project_root/VERSION")
pkg_name="stable-$version-linux-amd64"
archive="$project_root/dist/$pkg_name.tar.gz"
test_root=$(mktemp -d /tmp/stable-package-e2e-XXXXXXXX)
mock_pid=
dump_diagnostics() {
  rc=$?
  [[ $rc -eq 0 ]] && return 0
  echo '--- status.json ---' >&2
  cat "$test_root/status.json" 2>/dev/null >&2 || true
  echo '--- state logs (tail) ---' >&2
  for f in "$HOME/.local/state/stable"/*.log; do
    [[ -f $f ]] || continue
    echo "== $f" >&2
    tail -50 "$f" >&2
  done
  return 0
}
cleanup() {
  if [[ -x "$HOME/.local/bin/stable" ]]; then stable down >/dev/null 2>&1 || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$HOME/.local/state/stable" 2>/dev/null || true
}
trap 'dump_diagnostics; cleanup' EXIT
tar -xzf "$archive" -C "$test_root"
export HOME="$test_root/home"
mkdir -p "$HOME"
bash "$test_root/$pkg_name/install.sh"
export PATH="$HOME/.local/bin:/usr/bin:/bin"
python3 "$project_root/tests/package/mock_model.py" > "$test_root/mock.port" &
mock_pid=$!
for _ in $(seq 1 50); do [[ -s "$test_root/mock.port" ]] && break; sleep 0.1; done
mock_port=$(cat "$test_root/mock.port")
export STABLE_PROVIDER=openai-compatible STABLE_MODEL=mock STABLE_API_KEY=package-secret-marker
export STABLE_BASE_URL="http://127.0.0.1:$mock_port/v1"
export STABLE_TEMPORAL_PORT=17387
cd /tmp
stable doctor > "$test_root/doctor.txt"
stable config check > "$test_root/config.txt"
stable up > "$test_root/up.json"
# M03: chat moves to the trusted chatserve protocol on the worker-owned socket;
# repair actions land in a candidate that must be accepted before verification.
source "$project_root/tests/e2e/candidate_accept.sh"
run_root="$HOME/.local/state/stable"
# The chat protocol validates project_root against the supervisor-bound share
# directory, so session traffic must address the installed share, not a scratch.
E2E_SESSION_ROOT="$HOME/.local/opt/stable/$version/share"
status_file="$test_root/status.json"
mkdir -p "$E2E_SESSION_ROOT"
for _ in $(seq 1 120); do [[ -S "$run_root/chat.sock" ]] && break; sleep 0.5; done
[[ -S "$run_root/chat.sock" ]] || { echo 'chat.sock missing after up' >&2; exit 1; }
session_id=$(e2e_new_session "$test_root/session.jsonl")
e2e_chat create_goal --session "$session_id" --goal package-test --text "修复传感器连接，ERC 必须全过，J1 连接要恢复" > "$test_root/create.txt"
proposal_id=$(python3 - "$test_root/create.txt" <<'PY'
import json,sys
messages=[json.loads(line) for line in open(sys.argv[1]) if line.strip()]
print(next(m['proposal']['id'] for m in messages if m.get('type')=='proposal'))
PY
)
[[ -n "$proposal_id" ]] || { echo 'no criteria proposal returned' >&2; exit 1; }
e2e_chat confirm --session "$session_id" --goal package-test --proposal "$proposal_id" >> "$test_root/create.txt"
stable goal notify --goal package-test --event check-1 --kind external_check_failed > "$test_root/notify.txt"
stable goal notify --goal package-test --event check-1 --kind external_check_failed >> "$test_root/notify.txt"
accepted=0
for _ in $(seq 1 120); do
  e2e_allow_pending_approvals "$session_id" || true
  if stable goal status --goal package-test > "$status_file" 2>/dev/null; then
    if python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["snapshot"]; sys.exit(0 if any(a["status"] in ("candidate_ready","awaiting_accept") for a in (s.get("actions") or [])) else 1)' "$status_file"; then
      e2e_accept_ready_candidate "$session_id" "accept-package-test" || true
      accepted=$((accepted+1))
    fi
    if python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["verified"] else 1)' "$status_file"; then break; fi
  fi
  sleep 1
done
[[ "$accepted" -ge 1 ]] || { echo 'candidate was never accepted' >&2; exit 1; }
python3 - "$test_root" "$pkg_name" "$proposal_id" <<'PY'
import hashlib,json,pathlib,sys
root=pathlib.Path(sys.argv[1]); proposal_id=sys.argv[3]; s=json.loads((root/'status.json').read_text())
assert s['verified'],s['unverified']
snap=s['snapshot']; assert snap['goal']['status']=='verified'
assert len([e for e in snap['events'] if e['id']=='check-1'])==1
assert any(c['provider']=='openai-compatible' and c['status']=='succeeded' for c in snap['model_calls'])
assert all(d['model_call_id'] for d in snap['decisions'])
assert snap['goal']['criteria_revision']==0
p=[x for x in snap['criteria_proposals'] if x['id']==proposal_id][0]
assert p['status']=='confirmed' and {c['kind'] for c in p['criteria']}=={'kicad.erc_clean','sensor.connection_present'}
assert snap['conversation'] and any(m['kind']=='criteria_confirm' for m in snap['conversation'])
fixture=root/sys.argv[2]/'share/fixtures/sensor_board/sensor.kicad_sch'
assert hashlib.sha256(fixture.read_bytes()).hexdigest()!=s['actual_artifact_id']
PY
stable goal export --goal package-test --out "$test_root/delivery" > "$test_root/export.txt"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["snapshot"]["conversation"], "delivery missing conversation"' "$test_root/delivery/delivery.json"
if grep -rn 'package-secret-marker' "$test_root/config.txt" "$test_root/status.json" "$test_root/create.txt" "$test_root/session.jsonl" "$test_root/delivery" "$HOME/.local/state/stable"/*.log; then echo 'secret leaked' >&2; exit 1; fi
stable down > "$test_root/down.txt"
printf 'PACKAGE E2E PASS %s\n' "$test_root"
