#!/usr/bin/env bash
# Acceptance checks for the installed CLI: manifest, command surface, doctor
# diagnostics, concurrency/port protection, safe stop/restart and error
# diagnostics. Runs from an isolated temporary HOME with a restricted PATH.
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
version=$(tr -d '[:space:]' < "$project_root/VERSION")
pkg_name="stable-$version-linux-amd64"
archive="$project_root/dist/$pkg_name.tar.gz"
test_root=$(mktemp -d /tmp/stable-package-cli-XXXXXXXX)
export HOME="$test_root/home"
mkdir -p "$HOME"
port=17389
occupant_pid=
mock_pid=
cleanup() {
  if [[ -x "$HOME/.local/bin/stable" ]]; then stable down >/dev/null 2>&1 || true; fi
  if [[ -n "$occupant_pid" ]]; then kill "$occupant_pid" 2>/dev/null || true; wait "$occupant_pid" 2>/dev/null || true; fi
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$HOME/.local/state/stable" 2>/dev/null || true
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
expect_fail() { if "$@"; then fail "expected failure: $*"; fi; }

tar -xzf "$archive" -C "$test_root"
pkg="$test_root/$pkg_name"

# --- AC1/F1 release package complete: manifest, binary versions, pinned Temporal, sha256 ---
(cd "$project_root/dist" && sha256sum -c "$pkg_name.tar.gz.sha256" >/dev/null)
for f in bin/stable libexec/agentctl libexec/agentworker libexec/temporal \
  install.sh uninstall.sh README.md licenses/temporal-LICENSE share/fixtures/sensor_board/sensor.kicad_sch \
  share/fixtures/sensor_board/sensor.kicad_pro share/schemas/next_action.schema.json \
  share/schemas/criteria_proposal.schema.json \
  share/workers/kicad/bridge.py share/workers/computer/bridge.py; do
  [[ -f "$pkg/$f" ]] || fail "archive missing $f"
done
[[ $("$pkg/bin/stable" version) == "$version" ]] || fail 'cli version'
"$pkg/bin/stable" help >/dev/null || fail 'cli help'
rc=0; "$pkg/libexec/agentctl" >/dev/null 2>&1 || rc=$?
[[ $rc -lt 126 ]] || fail 'agentctl not executable'
[[ $("$pkg/libexec/temporal" --version 2>/dev/null) == *1.9.1* ]] || fail 'temporal version not 1.9.1'
echo 'AC1 manifest PASS'

bash "$pkg/install.sh" >/dev/null
export PATH="$HOME/.local/bin:/usr/bin:/bin"
export STABLE_PROVIDER=openai-compatible STABLE_MODEL=mock STABLE_API_KEY=cli-secret-marker
export STABLE_BASE_URL="http://127.0.0.1:9/v1" STABLE_TEMPORAL_PORT=$port
cd /tmp

# --- AC5/F5 command surface: help lists all commands; bad input fails ---
help_out=$(stable help)
for c in doctor 'config check' up down 'runtime status' 'goal create' 'goal status' 'goal notify' 'goal export' demo logs version; do
  [[ "$help_out" == *"$c"* ]] || fail "help missing $c"
done
expect_fail stable frobnicate 2>/dev/null
expect_fail stable goal start --goal removed 2>/dev/null
expect_fail stable doctor extra 2>/dev/null
expect_fail stable goal 2>/dev/null
expect_fail stable config 2>/dev/null
echo 'AC5 commands PASS'

# --- AC1/F1 dependency diagnostics: doctor names each missing item ---
# doctor 只查 PATH 存在性(exec.LookPath),不执行工具;主机缺某个工具时
# (如无 kicad 的 CI runner)用可执行 stub 代替符号链接,语义等价。
tools=(python3 kicad-cli kicad eeschema Xvfb xvfb-run xprop xwininfo import)
fakebin="$test_root/fakebin"
mkdir -p "$fakebin"
fakebin_add() {
  local target
  if target=$(command -v "$1"); then
    ln -s "$target" "$fakebin/$1"
  else
    printf '#!/bin/sh\nexit 0\n' > "$fakebin/$1"
    chmod +x "$fakebin/$1"
  fi
}
for t in "${tools[@]}"; do fakebin_add "$t"; done
for missing in python3 kicad-cli kicad Xvfb xvfb-run xprop import; do
  rm "$fakebin/$missing"
  out=$(PATH="$fakebin:$HOME/.local/bin" stable doctor 2>&1) && fail "doctor passed without $missing"
  [[ "$out" == *"MISSING $missing"* ]] || fail "doctor did not name $missing: $out"
  fakebin_add "$missing"
done
# doctor 的 isolated 检查在 bwrap 内探测(bwrap 与工具必须解析到 /usr 等真实系统目录),
# fakebin 遮蔽 + 裁剪 PATH 会让 probe 全灭;"全量通过"断言必须用真实 PATH。
PATH="$HOME/.local/bin:/usr/bin:/bin" stable doctor >/dev/null || fail 'doctor failed with complete PATH'
# missing package file
cp -a "$HOME/.local/opt/stable/$version" "$test_root/broken"
rm "$test_root/broken/share/schemas/next_action.schema.json"
out=$("$test_root/broken/bin/stable" doctor 2>&1) && fail 'doctor passed with missing schema'
[[ "$out" == *'MISSING schema'* ]] || fail "doctor did not name schema: $out"
echo 'AC1 doctor PASS'

# --- AC5/N6 common error diagnostics (also covers no-credential output) ---
err=$(stable runtime status 2>&1) && fail 'runtime status while down passed'
[[ "$err" == *'not running'* ]] || fail "runtime status error unclear: $err"
err=$(stable goal status --goal no-such-goal 2>&1) && fail 'unknown goal passed'
err=$(STABLE_BASE_URL='http://192.0.2.1/v1' stable config check 2>&1) && fail 'bad base URL passed'
combined="$err$(stable help 2>&1)"
[[ "$combined" != *'cli-secret-marker'* ]] || fail 'secret leaked in error output'
# worker startup failure: remove agentworker from a copied install, expect up to fail with log path
mv "$HOME/.local/opt/stable/$version/libexec/agentworker" "$test_root/agentworker.bak"
err=$(stable up 2>&1) && fail 'up passed without agentworker'
[[ "$err" == *'supervisor.log'* ]] || fail "up error lacks log path: $err"
mv "$test_root/agentworker.bak" "$HOME/.local/opt/stable/$version/libexec/agentworker"
# temporal port occupied: occupant must survive
python3 -c "import socket,time;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',$port));s.listen(1);time.sleep(120)" &
occupant_pid=$!
sleep 0.5
err=$(stable up 2>&1) && fail 'up passed with occupied port'
[[ "$err" == *'ccupied'* || "$err" == *'supervisor.log'* ]] || fail "up port error unclear: $err"
kill -0 "$occupant_pid" || fail 'port occupant was killed'
kill "$occupant_pid"; wait "$occupant_pid" 2>/dev/null || true; occupant_pid=
echo 'AC5/N6 diagnostics PASS'

# --- AC2/F2 one-command start, concurrency, safe stop/restart ---
python3 "$project_root/tests/package/mock_model.py" > "$test_root/mock.port" &
mock_pid=$!
for _ in $(seq 1 50); do [[ -s "$test_root/mock.port" ]] && break; sleep 0.1; done
export STABLE_BASE_URL="http://127.0.0.1:$(cat "$test_root/mock.port")/v1"
sleep 6000 & unrelated_pid=$!
up1=$(stable up)
up2=$(stable up)
stable doctor >/dev/null || fail 'doctor failed while Stable runtime was running'
[[ $(python3 -c 'import json,sys;print(json.load(sys.stdin)["pid"])' <<<"$up1") == \
   $(python3 -c 'import json,sys;print(json.load(sys.stdin)["pid"])' <<<"$up2") ]] || fail 'second up spawned another supervisor'
[[ $(python3 -c 'import json,sys;print(json.load(sys.stdin)["running"])' <<<"$up2") == True ]] || fail 'up2 not running'
cat > "$test_root/goal-definition.json" <<'JSON'
{"objective":"Repair the sensor connector and obtain a clean KiCad ERC","criteria":[{"id":"erc-clean","kind":"kicad.erc_clean","payload":{"max_violations":0}}],"check_interval_seconds":2}
JSON
[[ -S "$HOME/.local/state/stable/chat.sock" ]] || fail 'chat.sock missing after up'
stable goal create --goal cli-test --from "$test_root/goal-definition.json" >/dev/null
stable down >/dev/null
for _ in $(seq 1 40); do stable runtime status >/dev/null 2>&1 || break; sleep 0.25; done
! stable runtime status >/dev/null 2>&1 || fail 'runtime still running after down'
kill -0 "$unrelated_pid" || fail 'down killed unrelated process'
# M03: bare stable without a controlling terminal must refuse to start the TUI
# instead of falling back to a non-interactive chat mode.
out=$(printf '/quit\n' | stable 2>&1) && fail 'bare stable should refuse non-interactive stdin'
[[ "$out" == *'interactive terminal'* ]] || fail "unexpected bare stable error: $out"
stable goal status --goal cli-test >/dev/null || fail 'goal lost after restart'
kill "$unrelated_pid" 2>/dev/null || true
stable down >/dev/null
echo 'AC2 lifecycle PASS'
printf 'PACKAGE CLI PASS %s\n' "$test_root"
