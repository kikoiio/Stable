#!/usr/bin/env bash
# Acceptance checks for the installed CLI: manifest, command surface, doctor
# diagnostics, concurrency/port protection, safe stop/restart and error
# diagnostics. Runs from an isolated temporary HOME with a restricted PATH.
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
archive="$project_root/dist/proactive-agent-0.1.0-linux-amd64.tar.gz"
test_root=$(mktemp -d /tmp/proactive-package-cli-XXXXXXXX)
export HOME="$test_root/home"
mkdir -p "$HOME"
port=17389
occupant_pid=
mock_pid=
cleanup() {
  if [[ -x "$HOME/.local/bin/proactive-agent" ]]; then proactive-agent down >/dev/null 2>&1 || true; fi
  if [[ -n "$occupant_pid" ]]; then kill "$occupant_pid" 2>/dev/null || true; wait "$occupant_pid" 2>/dev/null || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$HOME/.local/state/proactive-agent" 2>/dev/null || true
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
expect_fail() { if "$@"; then fail "expected failure: $*"; fi; }

tar -xzf "$archive" -C "$test_root"
pkg="$test_root/proactive-agent-0.1.0-linux-amd64"

# --- AC1/F1 release package complete: manifest, binary versions, pinned Temporal, sha256 ---
(cd "$project_root/dist" && sha256sum -c proactive-agent-0.1.0-linux-amd64.tar.gz.sha256 >/dev/null)
for f in bin/proactive-agent libexec/agentctl libexec/agentworker libexec/temporal \
  install.sh README.md licenses/temporal-LICENSE share/fixtures/sensor_board/sensor.kicad_sch \
  share/fixtures/sensor_board/sensor.kicad_pro share/schemas/next_action.schema.json \
  share/workers/kicad/bridge.py share/workers/computer/bridge.py; do
  [[ -f "$pkg/$f" ]] || fail "archive missing $f"
done
[[ $("$pkg/bin/proactive-agent" version) == 0.1.0 ]] || fail 'cli version'
"$pkg/bin/proactive-agent" help >/dev/null || fail 'cli help'
rc=0; "$pkg/libexec/agentctl" >/dev/null 2>&1 || rc=$?
[[ $rc -lt 126 ]] || fail 'agentctl not executable'
[[ $("$pkg/libexec/temporal" --version 2>/dev/null) == *1.9.1* ]] || fail 'temporal version not 1.9.1'
echo 'AC1 manifest PASS'

bash "$pkg/install.sh" >/dev/null
export PATH="$HOME/.local/bin:/usr/bin:/bin"
export PROACTIVE_PROVIDER=openai-compatible PROACTIVE_MODEL=mock PROACTIVE_API_KEY=cli-secret-marker
export PROACTIVE_BASE_URL="http://127.0.0.1:9/v1" PROACTIVE_TEMPORAL_PORT=$port
cd /tmp

# --- AC5/F5 command surface: help lists all commands; bad input fails ---
help_out=$(proactive-agent help)
for c in doctor 'config check' up down 'runtime status' 'goal start' 'goal status' 'goal notify' 'goal export' logs version; do
  [[ "$help_out" == *"$c"* ]] || fail "help missing $c"
done
expect_fail proactive-agent frobnicate 2>/dev/null
expect_fail proactive-agent doctor extra 2>/dev/null
expect_fail proactive-agent goal 2>/dev/null
expect_fail proactive-agent config 2>/dev/null
echo 'AC5 commands PASS'

# --- AC1/F1 dependency diagnostics: doctor names each missing item ---
tools=(python3 kicad-cli kicad eeschema Xvfb xvfb-run xprop xwininfo import)
fakebin="$test_root/fakebin"
mkdir -p "$fakebin"
for t in "${tools[@]}"; do ln -s "$(command -v "$t")" "$fakebin/$t"; done
for missing in python3 kicad-cli kicad Xvfb xvfb-run xprop import; do
  rm "$fakebin/$missing"
  out=$(PATH="$fakebin:$HOME/.local/bin" proactive-agent doctor 2>&1) && fail "doctor passed without $missing"
  [[ "$out" == *"MISSING $missing"* ]] || fail "doctor did not name $missing: $out"
  ln -s "$(command -v "$missing")" "$fakebin/$missing"
done
PATH="$fakebin:$HOME/.local/bin" proactive-agent doctor >/dev/null || fail 'doctor failed with complete PATH'
# missing package file
cp -a "$HOME/.local/opt/proactive-agent/0.1.0" "$test_root/broken"
rm "$test_root/broken/share/schemas/next_action.schema.json"
out=$("$test_root/broken/bin/proactive-agent" doctor 2>&1) && fail 'doctor passed with missing schema'
[[ "$out" == *'MISSING schema'* ]] || fail "doctor did not name schema: $out"
echo 'AC1 doctor PASS'

# --- AC5/N6 common error diagnostics (also covers no-credential output) ---
err=$(proactive-agent runtime status 2>&1) && fail 'runtime status while down passed'
[[ "$err" == *'not running'* ]] || fail "runtime status error unclear: $err"
err=$(proactive-agent goal status --goal no-such-goal 2>&1) && fail 'unknown goal passed'
err=$(PROACTIVE_BASE_URL='http://192.0.2.1/v1' proactive-agent config check 2>&1) && fail 'bad base URL passed'
combined="$err$(proactive-agent help 2>&1)"
[[ "$combined" != *'cli-secret-marker'* ]] || fail 'secret leaked in error output'
# worker startup failure: remove agentworker from a copied install, expect up to fail with log path
mv "$HOME/.local/opt/proactive-agent/0.1.0/libexec/agentworker" "$test_root/agentworker.bak"
err=$(proactive-agent up 2>&1) && fail 'up passed without agentworker'
[[ "$err" == *'supervisor.log'* ]] || fail "up error lacks log path: $err"
mv "$test_root/agentworker.bak" "$HOME/.local/opt/proactive-agent/0.1.0/libexec/agentworker"
# temporal port occupied: occupant must survive
python3 -c "import socket,time;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',$port));s.listen(1);time.sleep(120)" &
occupant_pid=$!
sleep 0.5
err=$(proactive-agent up 2>&1) && fail 'up passed with occupied port'
[[ "$err" == *'ccupied'* || "$err" == *'supervisor.log'* ]] || fail "up port error unclear: $err"
kill -0 "$occupant_pid" || fail 'port occupant was killed'
kill "$occupant_pid"; wait "$occupant_pid" 2>/dev/null || true; occupant_pid=
echo 'AC5/N6 diagnostics PASS'

# --- AC2/F2 one-command start, concurrency, safe stop/restart ---
python3 "$project_root/tests/package/mock_model.py" > "$test_root/mock.port" &
mock_pid=$!
for _ in $(seq 1 50); do [[ -s "$test_root/mock.port" ]] && break; sleep 0.1; done
export PROACTIVE_BASE_URL="http://127.0.0.1:$(cat "$test_root/mock.port")/v1"
sleep 6000 & unrelated_pid=$!
up1=$(proactive-agent up)
up2=$(proactive-agent up)
[[ $(python3 -c 'import json,sys;print(json.load(sys.stdin)["pid"])' <<<"$up1") == \
   $(python3 -c 'import json,sys;print(json.load(sys.stdin)["pid"])' <<<"$up2") ]] || fail 'second up spawned another supervisor'
[[ $(python3 -c 'import json,sys;print(json.load(sys.stdin)["running"])' <<<"$up2") == True ]] || fail 'up2 not running'
proactive-agent goal start --goal cli-test --interval 2 >/dev/null
proactive-agent down >/dev/null
for _ in $(seq 1 40); do proactive-agent runtime status >/dev/null 2>&1 || break; sleep 0.25; done
! proactive-agent runtime status >/dev/null 2>&1 || fail 'runtime still running after down'
kill -0 "$unrelated_pid" || fail 'down killed unrelated process'
proactive-agent up >/dev/null
proactive-agent goal status --goal cli-test >/dev/null || fail 'goal lost after restart'
kill "$unrelated_pid" 2>/dev/null || true
proactive-agent down >/dev/null
echo 'AC2 lifecycle PASS'
printf 'PACKAGE CLI PASS %s\n' "$test_root"
