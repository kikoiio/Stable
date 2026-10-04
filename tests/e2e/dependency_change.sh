#!/usr/bin/env bash
# V02 end-to-end check for query-time dependency discovery and directional
# evidence invalidation. Uses the local mock model and installed KiCad.
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
e2e_alloc dependency
cd "$project_root"

run_root=$E2E_ROOT
port=$E2E_PORT
goal_id=$E2E_GOAL
temporal_address="localhost:$port"
mock_pid=

cleanup() {
  if [[ -n "$mock_pid" ]]; then kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
}
e2e_on_cleanup cleanup
trap e2e_run_cleanups EXIT
source "$project_root/tests/e2e/mock_model_env.sh"
source "$project_root/tests/e2e/candidate_accept.sh"
# The supervised worker serves the chat socket and binds dev-install/share as
# its trusted project root; session traffic must address that same root.
E2E_SESSION_ROOT="$run_root/dev-install/share"
export STABLE_TEMPORAL_PORT="$port"

dev_root="$run_root/dev-install"
mkdir -p "$dev_root/bin" "$dev_root/libexec" "$dev_root/share"
go build -buildvcs=false -o "$dev_root/bin/stable" ./cmd/stable
go build -buildvcs=false -o "$dev_root/libexec/agentctl" ./cmd/agentctl
go build -buildvcs=false -o "$dev_root/libexec/agentworker" ./cmd/agentworker
temporal_bin=$(command -v temporal) || { echo 'temporal CLI required' >&2; exit 1; }
ln -sfn "$temporal_bin" "$dev_root/libexec/temporal"
ln -sfn "$project_root/fixtures" "$dev_root/share/fixtures"
ln -sfn "$project_root/schemas" "$dev_root/share/schemas"
cp -a "$project_root/workers" "$dev_root/share/workers"

start_runner() {
  "$dev_root/bin/stable" up >"$run_root/up.log" 2>&1 || { cat "$run_root/up.log" >&2; return 1; }
  for _ in $(seq 1 120); do
    if grep -q 'agent worker ready' "$run_root/worker.log" 2>/dev/null; then return 0; fi
    sleep 0.5
  done
  tail -n 20 "$run_root/up.log" "$run_root/supervisor.log" "$run_root/worker.log" >&2
  return 1
}

status_file="$run_root/status.json"
read_status() {
  "$dev_root/libexec/agentctl" status --run-root "$run_root/goals" --db "$run_root/state.db" \
    --project-root "$dev_root/share" --temporal "$temporal_address" --goal "$goal_id" >"$status_file"
}
wait_for_chat() {
  for _ in $(seq 1 120); do [[ -S "$run_root/chat.sock" ]] && return 0; sleep 0.5; done
  tail -n 20 "$run_root/worker.log" >&2
  return 1
}
accept_session=
accept_seq=0
wait_verified() {
  for _ in $(seq 1 900); do
    if [[ -n "$accept_session" ]]; then
      e2e_allow_pending_approvals "$accept_session" || true
      if read_status && python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["snapshot"]; sys.exit(0 if any(a["status"] in ("candidate_ready","awaiting_accept") for a in (s.get("actions") or [])) else 1)' "$status_file"; then
        accept_seq=$((accept_seq+1))
        e2e_accept_ready_candidate "$accept_session" "accept-$goal_id-$accept_seq" || true
      fi
    fi
    if read_status && python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["verified"] else 1)' "$status_file"; then return 0; fi
    sleep 0.5
  done
  tail -n 20 "$run_root/worker.log" >&2
  return 1
}

start_runner
wait_for_chat
accept_session=$(e2e_new_session "$run_root/session.jsonl")
# M03: acceptance is session-bound (goal SourceSessionID must match the
# accepting session), so the goal is created through the trusted chat entry
# instead of `goal create --from`; the mock transpile yields the same two
# criteria (erc-clean, sensor.connection_present).
e2e_chat create_goal --session "$accept_session" --goal "$goal_id" --text "修复传感器连接并确保 ERC 违规为零" >"$run_root/create.jsonl"
proposal_id=$(python3 - "$run_root/create.jsonl" <<'PY'
import json,sys
messages=[json.loads(line) for line in open(sys.argv[1]) if line.strip()]
print(next(m['proposal']['id'] for m in messages if m.get('type')=='proposal'))
PY
)
e2e_chat confirm --session "$accept_session" --goal "$goal_id" --proposal "$proposal_id" >"$run_root/confirm.jsonl"
python3 - "$run_root/confirm.jsonl" <<'PY'
import json,sys
ms=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]
g=next(m['goal'] for m in ms if m.get('type')=='goal_update')
assert g['status']=='active', g
PY
wait_verified
read_status
python3 - "$status_file" <<'PY'
import json, sys
s=json.load(open(sys.argv[1])); g=s['snapshot']['goal']
assert s['verified'] and len(s['snapshot']['dependencies']) == 2, s
assert all(e['current'] for e in s['evidence'] if e['criterion_id'] in ('erc-clean','sensor-connection')), s['evidence']
print(f"V02 BASELINE PASS goal={g['id']} artifact={s['actual_artifact_id']}")
for e in s['evidence']:
    if e['current']: print(f"  {e['criterion_id']} evidence={e['id']}")
PY
read_status
baseline_connection_id=$(python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); print(next(e["id"] for e in s["evidence"] if e["criterion_id"]=="sensor-connection" and e["current"]))' "$status_file")

if [[ "${1:-}" == "--baseline-only" ]]; then exit 0; fi

# Change only an ERC-specific project setting. The schematic and criteria stay
# byte-for-byte unchanged; the status entry point must persist the change.
project_file="$run_root/goals/$goal_id/sensor.kicad_pro"
python3 - "$project_file" <<'PY'
import json, pathlib, sys
p=pathlib.Path(sys.argv[1]); data=json.loads(p.read_text())
data['erc']={'severity_profile':'v02-change'}
p.write_text(json.dumps(data, sort_keys=True))
PY
read_status
python3 - "$status_file" <<'PY'
import json, sys
s=json.load(open(sys.argv[1])); g=s['snapshot']['goal']; evidence=s['evidence']
assert g['dependency_revision'] >= 1, g
assert not s['verified'], s
erc=[e for e in evidence if e['criterion_id']=='erc-clean']
conn=[e for e in evidence if e['criterion_id']=='sensor-connection']
assert erc and all(not e['current'] and e['invalidated_reason'] for e in erc), erc
assert conn and any(e['current'] for e in conn), conn
print('V02 PROJECT ERC CHANGE PASS: ERC invalidated, connection evidence retained')
PY
  "$dev_root/libexec/agentctl" export --run-root "$run_root/goals" --db "$run_root/state.db" \
  --project-root "$dev_root/share" --temporal "$temporal_address" --goal "$goal_id" --out "$run_root/delivery-pending" >/dev/null
python3 - "$run_root/delivery-pending" <<'PY'
import json, pathlib, sys
p=pathlib.Path(sys.argv[1]); d=json.loads((p/'delivery.json').read_text())
assert not d['verified'] and not (p/'erc.json').exists(), d
assert (p/'history'/'history.json').exists(), 'invalidated ERC report was not archived'
print('V02 EXPORT HISTORY PASS: old ERC report is not current delivery')
PY

# Worker wake is automatic; wait until ERC has fresh matching evidence and the
# retained connection evidence is still the original ID.
wait_verified
read_status
python3 - "$status_file" <<'PY'
import json, sys
s=json.load(open(sys.argv[1])); ev=s['evidence']
erc=[e for e in ev if e['criterion_id']=='erc-clean' and e['current']]
conn=[e for e in ev if e['criterion_id']=='sensor-connection' and e['current']]
assert s['verified'] and erc and conn, s
assert any(e['dependency_fingerprint'] == s['snapshot']['dependencies'][0]['fingerprint'] for e in erc), erc
print('V02 AUTOMATIC DIRECTIONAL RECHECK PASS')
PY

check_erc_change() {
  local label=$1
  read_status
  python3 - "$status_file" "$label" <<'PY'
import json, sys
s=json.load(open(sys.argv[1])); ev=s['evidence']
erc=[e for e in ev if e['criterion_id']=='erc-clean']
conn=[e for e in ev if e['criterion_id']=='sensor-connection']
assert erc and all(not e['current'] for e in erc), (sys.argv[2],erc)
assert conn and any(e['current'] for e in conn), (sys.argv[2],conn)
print(f"V02 {sys.argv[2]} directional invalidation PASS")
PY
  wait_verified
  read_status
  python3 - "$status_file" "$label" "$baseline_connection_id" <<'PY'
import json, sys
s=json.load(open(sys.argv[1])); ev=s['evidence']
assert s['verified'], (sys.argv[2],s['unverified'])
assert any(e['criterion_id']=='erc-clean' and e['current'] for e in ev), (sys.argv[2],ev)
assert any(e['criterion_id']=='sensor-connection' and e['current'] and e['id']==sys.argv[3] for e in ev), (sys.argv[2],ev)
PY
}

# Project symbol table and a selected project library file each affect ERC
# only. Keep both mapped libraries valid so KiCad can execute the recheck.
goal_dir="$run_root/goals/$goal_id"
mkdir -p "$goal_dir/symbols"
cp /usr/share/kicad/symbols/Connector.kicad_sym /usr/share/kicad/symbols/Device.kicad_sym "$goal_dir/symbols/"
cat >"$goal_dir/sym-lib-table" <<'TABLE'
(sym_lib_table
 (lib (name "Connector")(type "KiCad")(uri "${KIPRJMOD}/symbols/Connector.kicad_sym")(options "")(descr "V02 project library"))
 (lib (name "Device")(type "KiCad")(uri "${KIPRJMOD}/symbols/Device.kicad_sym")(options "")(descr "V02 project library"))
)
TABLE
check_erc_change "PROJECT SYMBOL TABLE"
printf '\n; V02 content change\n' >>"$goal_dir/symbols/Connector.kicad_sym"
check_erc_change "SELECTED PROJECT LIBRARY"

# Return to global mappings: only the project content affects the fingerprint.
rm "$goal_dir/sym-lib-table"
check_erc_change "PROJECT TABLE REMOVAL"

# M03: KiCad user state moved into per-check private directories seeded from
# the pristine system template, so a host-side "global" table no longer feeds
# ERC. The environment-variable phase below covers out-of-band dependency
# invalidation instead.

# Restart the runtime so its worker receives the changed environment. M03
# resolves and normalizes dependency paths before hashing, so aliasing the same
# directory must NOT invalidate evidence; the raw value must also never leak.
ln -s /usr/share/kicad/symbols "$run_root/symbols-alias"
export KICAD9_SYMBOL_DIR="$run_root/symbols-alias"
"$dev_root/bin/stable" down >/dev/null 2>&1 || true
start_runner
wait_verified
read_status
python3 - "$status_file" "$run_root/symbols-alias" <<'PY'
import json, pathlib, sys
s=json.load(open(sys.argv[1])); text=pathlib.Path(sys.argv[1]).read_text()
assert s['verified'], s
assert any(e['criterion_id']=='erc-clean' and e['current'] for e in s['evidence']), s
assert any(e['criterion_id']=='sensor-connection' and e['current'] for e in s['evidence']), s
assert sys.argv[2] not in text, 'raw symbol path leaked into status'
PY
read_status
python3 - "$status_file" "$baseline_connection_id" <<'PY'
import json,sys
s=json.load(open(sys.argv[1])); ev=s['evidence']
assert s['verified'] and any(e['criterion_id']=='erc-clean' and e['current'] for e in ev), s
assert any(e['criterion_id']=='sensor-connection' and e['id']==sys.argv[2] and e['current'] for e in ev), ev
PY
printf 'V02 PATH VARIABLE PASS: ERC refreshed without exposing the value\n'

# M03: the isolated bridge clears the environment and pins tool identity to the
# trusted installed binaries, so a host-side PATH shim can no longer change the
# recorded kicad-cli version. Tool-identity invalidation is covered by the
# CONNECTION CHECKER VERSION phases below, which change the checker shipped in
# the installed worker share that the sandbox actually executes.

# Change the checker identity used for connection evidence in the isolated
# installed workers. Its ERC evidence and report remain current.
erc_id_before=$(python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); print(next(e["id"] for e in s["evidence"] if e["criterion_id"]=="erc-clean" and e["current"]))' "$status_file")
python3 - "$dev_root/share/workers/kicad/schematic.py" <<'PY'
import pathlib,sys
p=pathlib.Path(sys.argv[1]); source=p.read_text()
old="CONNECTION_CHECKER_VERSION = '1'"
assert old in source, source
p.write_text(source.replace(old,"CONNECTION_CHECKER_VERSION = 'v02-test'",1))
PY
"$dev_root/bin/stable" down >/dev/null 2>&1 || true
start_runner
wait_verified
read_status
python3 - "$status_file" "$baseline_connection_id" "$erc_id_before" <<'PY'
import json,sys
s=json.load(open(sys.argv[1])); ev=s['evidence']
assert s['verified'], s
erc=[e for e in ev if e['criterion_id']=='erc-clean' and e['current']]
assert erc and erc[0]['id']==sys.argv[3], 'ERC evidence must be retained, not re-run'
conn=[e for e in ev if e['criterion_id']=='sensor-connection' and e['current']]
assert conn and conn[0]['id']!=sys.argv[2], ev
PY
printf 'V02 CONNECTION CHECKER RECHECK PASS: ERC evidence retained\n'

# An unconfirmed connection checker version has the same fail-closed behavior.
python3 - "$dev_root/share/workers/kicad/schematic.py" <<'PY'
import pathlib,sys
p=pathlib.Path(sys.argv[1]); source=p.read_text()
old="CONNECTION_CHECKER_VERSION = 'v02-test'"
assert old in source, source
p.write_text(source.replace(old,"CONNECTION_CHECKER_VERSION = ''",1))
PY
"$dev_root/bin/stable" down >/dev/null 2>&1 || true
start_runner
read_status
python3 - "$status_file" <<'PY'
import json,sys
s=json.load(open(sys.argv[1])); conn=next(d for d in s['snapshot']['dependencies'] if d['family']=='sensor.connection')
assert not s['verified'] and not conn['available'] and 'version unavailable' in conn['reason'], s
assert any(e['criterion_id']=='erc-clean' and e['current'] for e in s['evidence']), s['evidence']
print('V02 UNKNOWN CONNECTION CHECKER VERSION PASS')
PY
# Retired-phase restoration (2026-10-04 wake-replay residual): the recovery
# path is healthy; earlier failures were a verified->unverified transient flip
# racing single-read assertions. Convergence therefore requires verified AND
# every dependency available on three consecutive reads before asserting.
"$dev_root/bin/stable" down >/dev/null 2>&1 || true
python3 - "$dev_root/share/workers/kicad/schematic.py" <<'PY'
import pathlib,sys
p=pathlib.Path(sys.argv[1]); source=p.read_text()
old="CONNECTION_CHECKER_VERSION = ''"
assert old in source, source
p.write_text(source.replace(old,"CONNECTION_CHECKER_VERSION = 'v02-test'",1))
PY
start_runner
converged() {
  read_status && python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); sys.exit(0 if s["verified"] and all(d["available"] for d in s["snapshot"]["dependencies"]) else 1)' "$status_file"
}
stable_hits=0
for _ in $(seq 1 900); do
  if converged; then
    stable_hits=$((stable_hits+1))
    [[ $stable_hits -ge 3 ]] && break
    sleep 3
  else
    stable_hits=0
    sleep 0.5
  fi
done
read_status
python3 - "$status_file" <<'PY'
import json,sys
s=json.load(open(sys.argv[1])); assert s['verified'] and all(d['available'] for d in s['snapshot']['dependencies']), s
print('V02 CHECKER VERSION RECOVERY PASS')
PY

# A required input disappearing keeps the target pending and explains the
# missing source. Stop the worker before restoring it so the durable wake is
# replayed after restart rather than relying on another user command.
project_file="$goal_dir/sensor.kicad_pro"
cp "$project_file" "$run_root/sensor.kicad_pro.saved"
rm "$project_file"
read_status
python3 - "$status_file" <<'PY'
import json,sys
s=json.load(open(sys.argv[1])); g=s['snapshot']['goal']
erc=next(d for d in s['snapshot']['dependencies'] if d['family']=='kicad.erc')
assert not s['verified'] and not erc['available'] and 'missing' in erc['reason'].lower(), s
assert g['status']=='pending_reverification', g
assert not any(e['criterion_id']=='erc-clean' and e['current'] for e in s['evidence']), s['evidence']
print('V02 MISSING REQUIRED INPUT PASS')
PY
"$dev_root/libexec/agentctl" export --run-root "$run_root/goals" --db "$run_root/state.db" \
  --project-root "$dev_root/share" --temporal "$temporal_address" --goal "$goal_id" --out "$run_root/delivery-unavailable" >/dev/null
python3 - "$run_root/delivery-unavailable/delivery.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1])); assert not d['verified'] and d['snapshot']['goal']['status']=='pending_reverification', d
assert not any(e['criterion_id']=='erc-clean' and e['current'] for e in d['evidence']), d['evidence']
PY
"$dev_root/bin/stable" down >/dev/null 2>&1 || true
cp "$run_root/sensor.kicad_pro.saved" "$project_file"
read_status
python3 - "$run_root/state.db" <<'PY'
import sqlite3,sys
c=sqlite3.connect(sys.argv[1]); rows=c.execute("select id from events where kind='dependency_changed'").fetchall()
assert len(rows) >= 1, rows
PY
"$dev_root/bin/stable" down >/dev/null 2>&1 || true
start_runner
stable_hits=0
for _ in $(seq 1 900); do
  if converged; then
    stable_hits=$((stable_hits+1))
    [[ $stable_hits -ge 3 ]] && break
    sleep 3
  else
    stable_hits=0
    sleep 0.5
  fi
done
read_status
python3 - "$status_file" <<'PY'
import json,sys
s=json.load(open(sys.argv[1])); assert s['verified'] and all(d['available'] for d in s['snapshot']['dependencies']), s
print('V02 WAKE REPLAY AND RECOVERY PASS')
PY
printf 'E2E DEPENDENCY PASS %s\nEvidence: %s\n' "$goal_id" "$run_root"
