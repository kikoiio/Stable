#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d /tmp/proactive-package-restart-XXXXXXXX)
mock_pid=
cleanup() {
  proactive-agent down >/dev/null 2>&1 || true
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$HOME/.local/state/proactive-agent" 2>/dev/null || true
}
trap cleanup EXIT
tar -xzf "$project_root/dist/proactive-agent-0.1.0-linux-amd64.tar.gz" -C "$test_root"
export HOME="$test_root/home"
mkdir -p "$HOME"
bash "$test_root/proactive-agent-0.1.0-linux-amd64/install.sh"
export PATH="$HOME/.local/bin:/usr/bin:/bin"
python3 "$project_root/tests/package/mock_model.py" > "$test_root/mock.port" &
mock_pid=$!
for _ in $(seq 1 50); do [[ -s "$test_root/mock.port" ]] && break; sleep 0.1; done
export PROACTIVE_PROVIDER=openai-compatible PROACTIVE_MODEL=mock PROACTIVE_API_KEY=restart-secret-marker
export PROACTIVE_BASE_URL="http://127.0.0.1:$(cat "$test_root/mock.port")/v1"
export PROACTIVE_TEMPORAL_PORT=17388
export PROACTIVE_CRASH_AFTER_REPAIR_MARKER="$test_root/repair.marker"
cd /tmp
proactive-agent up > "$test_root/up1.json"
proactive-agent goal start --goal restart-test --interval 2 > /dev/null
for _ in $(seq 1 120); do [[ -e "$test_root/repair.marker" ]] && break; sleep 1; done
[[ -e "$test_root/repair.marker" ]] || { echo 'repair crash did not fire' >&2; exit 1; }
for _ in $(seq 1 30); do proactive-agent runtime status >/dev/null 2>&1 || break; sleep 1; done
if proactive-agent runtime status >/dev/null 2>&1; then echo 'supervisor did not stop after Worker crash' >&2; exit 1; fi
proactive-agent goal notify --goal restart-test --event offline-1 --kind external_check_failed > "$test_root/offline.txt"
proactive-agent goal notify --goal restart-test --event offline-1 --kind external_check_failed >> "$test_root/offline.txt"
proactive-agent up > "$test_root/up2.json"
for _ in $(seq 1 120); do
  proactive-agent goal status --goal restart-test > "$test_root/status.json"
  if python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); sys.exit(0 if s["verified"] and any(e["id"]=="offline-1" and e["status"]=="processed" for e in s["snapshot"]["events"]) else 1)' "$test_root/status.json"; then break; fi
  sleep 1
done
python3 - "$test_root/status.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1])); assert s['verified'],s['unverified']
snap=s['snapshot']; assert sum(e['id']=='offline-1' for e in snap['events'])==1
assert sum(a['desired_postcondition']=={'sensor.connection_present':True} for a in snap['actions'])==1
assert any(c['status']=='succeeded' for c in snap['model_calls'])
PY
proactive-agent goal export --goal restart-test --out "$test_root/delivery" > /dev/null
proactive-agent down > /dev/null
printf 'PACKAGE RESTART PASS %s\n' "$test_root"
