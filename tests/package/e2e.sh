#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
archive="$project_root/dist/proactive-agent-0.1.0-linux-amd64.tar.gz"
test_root=$(mktemp -d /tmp/proactive-package-e2e-XXXXXXXX)
mock_pid=
cleanup() {
  if [[ -x "$HOME/.local/bin/proactive-agent" ]]; then proactive-agent down >/dev/null 2>&1 || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$HOME/.local/state/proactive-agent" 2>/dev/null || true
}
trap cleanup EXIT
tar -xzf "$archive" -C "$test_root"
export HOME="$test_root/home"
mkdir -p "$HOME"
bash "$test_root/proactive-agent-0.1.0-linux-amd64/install.sh"
export PATH="$HOME/.local/bin:/usr/bin:/bin"
python3 "$project_root/tests/package/mock_model.py" > "$test_root/mock.port" &
mock_pid=$!
for _ in $(seq 1 50); do [[ -s "$test_root/mock.port" ]] && break; sleep 0.1; done
mock_port=$(cat "$test_root/mock.port")
export PROACTIVE_PROVIDER=openai-compatible PROACTIVE_MODEL=mock PROACTIVE_API_KEY=package-secret-marker
export PROACTIVE_BASE_URL="http://127.0.0.1:$mock_port/v1"
export PROACTIVE_TEMPORAL_PORT=17387
cd /tmp
proactive-agent doctor > "$test_root/doctor.txt"
proactive-agent config check > "$test_root/config.txt"
proactive-agent up > "$test_root/up.json"
proactive-agent goal start --goal package-test --interval 2 > "$test_root/goal.txt"
proactive-agent goal notify --goal package-test --event check-1 --kind external_check_failed > "$test_root/notify.txt"
proactive-agent goal notify --goal package-test --event check-1 --kind external_check_failed >> "$test_root/notify.txt"
for _ in $(seq 1 120); do
  proactive-agent goal status --goal package-test > "$test_root/status.json"
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["verified"] else 1)' "$test_root/status.json"; then break; fi
  sleep 1
done
python3 - "$test_root" <<'PY'
import hashlib,json,pathlib,sys
root=pathlib.Path(sys.argv[1]); s=json.loads((root/'status.json').read_text())
assert s['verified'],s['unverified']
snap=s['snapshot']; assert snap['goal']['status']=='verified'
assert len([e for e in snap['events'] if e['id']=='check-1'])==1
assert any(c['provider']=='openai-compatible' and c['status']=='succeeded' for c in snap['model_calls'])
assert all(d['model_call_id'] for d in snap['decisions'])
fixture=root/'proactive-agent-0.1.0-linux-amd64/share/fixtures/sensor_board/sensor.kicad_sch'
assert hashlib.sha256(fixture.read_bytes()).hexdigest()!=s['actual_artifact_id']
PY
proactive-agent goal export --goal package-test --out "$test_root/delivery" > "$test_root/export.txt"
if grep -rn 'package-secret-marker' "$test_root/config.txt" "$test_root/status.json" "$test_root/delivery" "$HOME/.local/state/proactive-agent"/*.log; then echo 'secret leaked' >&2; exit 1; fi
proactive-agent down > "$test_root/down.txt"
printf 'PACKAGE E2E PASS %s\n' "$test_root"
