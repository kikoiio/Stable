#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
version=$(tr -d '[:space:]' < "$project_root/VERSION")
pkg_name="stable-$version-linux-amd64"
archive="$project_root/dist/$pkg_name.tar.gz"
test_root=$(mktemp -d /tmp/stable-package-e2e-XXXXXXXX)
mock_pid=
cleanup() {
  if [[ -x "$HOME/.local/bin/stable" ]]; then stable down >/dev/null 2>&1 || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$HOME/.local/state/stable" 2>/dev/null || true
}
trap cleanup EXIT
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
stable chat --create-goal "修复传感器连接，ERC 必须全过，J1 连接要恢复" --goal package-test > "$test_root/create.txt"
proposal_id=$(sed -n 's/^proposal \(prop-[0-9a-f]*\) status=proposed$/\1/p' "$test_root/create.txt")
[[ -n "$proposal_id" ]] || { echo 'no criteria proposal returned' >&2; exit 1; }
stable chat --confirm "$proposal_id" --goal package-test >> "$test_root/create.txt"
stable goal notify --goal package-test --event check-1 --kind external_check_failed > "$test_root/notify.txt"
stable goal notify --goal package-test --event check-1 --kind external_check_failed >> "$test_root/notify.txt"
for _ in $(seq 1 120); do
  stable goal status --goal package-test > "$test_root/status.json"
  if python3 -c 'import json,sys; x=json.load(open(sys.argv[1])); sys.exit(0 if x["verified"] else 1)' "$test_root/status.json"; then break; fi
  sleep 1
done
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
if grep -rn 'package-secret-marker' "$test_root/config.txt" "$test_root/status.json" "$test_root/create.txt" "$test_root/delivery" "$HOME/.local/state/stable"/*.log; then echo 'secret leaked' >&2; exit 1; fi
stable down > "$test_root/down.txt"
printf 'PACKAGE E2E PASS %s\n' "$test_root"
