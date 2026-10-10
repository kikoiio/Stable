#!/usr/bin/env bash
# M07-C full Linux runtime restart acceptance. This complements the service
# subprocess test by driving the real stable up/down supervisor lifecycle.
set -euo pipefail
umask 077
export GOMAXPROCS=${GOMAXPROCS:-2}

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
mkdir -p "$project_root/run/tmp"
export TMPDIR="$project_root/run/tmp"
source "$project_root/tests/e2e/lib.sh"
e2e_alloc m07c-runtime-restart
trap e2e_run_cleanups EXIT
cd "$project_root"

run_root=$E2E_ROOT
dev_root="$run_root/dev-install"
fixture="$run_root/mcp-fixture"
share="$dev_root/share"
stable="$dev_root/bin/stable"
socket="$run_root/chat.sock"
session_file="$run_root/session.jsonl"
reload_file="$run_root/reload.jsonl"
list_before_file="$run_root/list-before.jsonl"
transcript_before_file="$run_root/transcript-before.jsonl"
transcript_after_file="$run_root/transcript-after.jsonl"
list_after_file="$run_root/list-after.jsonl"

export STABLE_STATE_DIR="$run_root"
export XDG_CONFIG_HOME="$HOME/.config"
config_dir="$XDG_CONFIG_HOME/stable"
config_file="$config_dir/config.json"
unset STABLE_CONFIG STABLE_PROVIDER STABLE_MODEL STABLE_API_KEY STABLE_BASE_URL STABLE_TEMPORAL_PORT STABLE_PREBUILT_DIR
export STABLE_TEMPORAL_PORT="$E2E_PORT"

mkdir -p "$config_dir" "$share/.stable"
python3 - "$config_file" "$run_root" "$E2E_PORT" <<'PY'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
config = {
    'state_dir': sys.argv[2],
    'temporal_port': int(sys.argv[3]),
    'model': {
        'provider': 'openai-compatible',
        'model': 'm07c-fixture',
        'base_url': 'http://127.0.0.1:9/v1',
        'api_key': 'm07c-runtime-restart-marker',
    },
}
path.write_text(json.dumps(config) + '\n')
path.chmod(0o600)
PY

# The MCP manager reads project configuration from the install's share root.
# Keep this file across the helper's copy of fixtures/schemas/workers.
go build -p 1 -buildvcs=false -o "$fixture" ./internal/mcp/mcptest
python3 - "$share/.stable/mcp.yaml" "$fixture" <<'PY'
import json, pathlib, sys
path, command = pathlib.Path(sys.argv[1]), sys.argv[2]
path.write_text('- name: fixture\n  command: ' + json.dumps(command) + '\n')
path.chmod(0o600)
PY

# Use the same development install layout as other source-level runtime e2e.
# The command builds the local binaries and starts the real supervisor stack.
STABLE_RUN_LOCAL_UP=1 bash "$project_root/scripts/run_local.sh" "$run_root" >"$run_root/up-first.log" 2>&1 || {
  cat "$run_root/up-first.log" "$run_root/supervisor.log" "$run_root/worker.log" >&2 2>/dev/null || true
  exit 1
}

cat >"$run_root/mcp-client.py" <<'PY'
#!/usr/bin/env python3
import json
import socket
import sys
import time

socket_path, op, project_root, session_id = sys.argv[1:5]
request = {'op': op}
if op == 'session_create':
    request['project_root'] = project_root
elif op in ('mcp_reload', 'mcp_list'):
    request['session_id'] = session_id
elif op == 'session_load':
    request.update(project_root=project_root, session_id=session_id)
else:
    raise SystemExit(f'unsupported operation: {op}')

deadline = time.monotonic() + 10
while True:
    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    conn.settimeout(15)
    try:
        conn.connect(socket_path)
        break
    except (FileNotFoundError, ConnectionRefusedError):
        conn.close()
        if time.monotonic() >= deadline:
            raise
        time.sleep(0.05)

messages = []
with conn:
    conn.sendall(json.dumps(request).encode() + b'\n')
    stream = conn.makefile('rb')
    while True:
        line = stream.readline()
        if not line:
            raise SystemExit(f'service closed before done: {messages!r}')
        message = json.loads(line)
        messages.append(message)
        if message.get('type') == 'error':
            raise SystemExit(f"service error: {message.get('error', '')}")
        if message.get('type') == 'done':
            break

for message in messages:
    print(json.dumps(message, ensure_ascii=False))
PY
chmod 0700 "$run_root/mcp-client.py"

python3 "$run_root/mcp-client.py" "$socket" session_create "$share" '' >"$session_file"
session_id=$(python3 - "$session_file" <<'PY'
import json, sys
messages = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
print(next(message['session']['id'] for message in messages if message.get('type') == 'session'))
PY
)
[[ -n "$session_id" ]] || { echo 'session_create returned no session ID' >&2; exit 1; }

python3 "$run_root/mcp-client.py" "$socket" mcp_reload "$share" "$session_id" >"$reload_file"
python3 "$run_root/mcp-client.py" "$socket" mcp_list "$share" "$session_id" >"$list_before_file"
python3 "$run_root/mcp-client.py" "$socket" session_load "$share" "$session_id" >"$transcript_before_file"
python3 - "$reload_file" "$list_before_file" "$transcript_before_file" <<'PY'
import json, sys
def messages(path):
    return [json.loads(line) for line in open(path) if line.strip()]
reloads = messages(sys.argv[1])
report = next(m['mcp_report'] for m in reloads if m.get('type') == 'mcp_report')
assert (report['before'], report['after']) == (1, 1), report
listed = messages(sys.argv[2])
servers = next(m['mcp_list']['servers'] for m in listed if m.get('type') == 'mcp_list')
assert len(servers) == 1 and servers[0]['name'] == 'fixture' and servers[0]['state'] == 'connected', servers
transcript = next(m['transcript'] for m in messages(sys.argv[3]) if m.get('type') == 'transcript')
events = [(e['type'], e['data']) for e in transcript['events'] if e['type'] in ('mcp_reload', 'mcp_server')]
assert [kind for kind, _ in events] == ['mcp_reload', 'mcp_server'], events
assert events[0][1]['trigger'] == 'manual' and (events[0][1]['before'], events[0][1]['after']) == (1, 1), events
assert events[1][1]['name'] == 'fixture' and events[1][1]['source'] == 'project' and events[1][1]['state'] == 'connected', events
print('M07-C pre-restart MCP state and transcript PASS')
PY

first_status="$run_root/runtime-first.json"
"$stable" runtime status >"$first_status"
first_pid=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["pid"])' "$first_status")
"$stable" down >"$run_root/down.log"
stopped=0
for _ in $(seq 1 80); do
  if ! "$stable" runtime status >"$run_root/runtime-stopped.json" 2>/dev/null; then stopped=1; break; fi
  sleep 0.25
done
[[ "$stopped" == 1 ]] || { echo 'stable down did not stop the runtime supervisor' >&2; exit 1; }

"$stable" up >"$run_root/up-second.log" 2>&1 || {
  cat "$run_root/up-second.log" "$run_root/supervisor.log" "$run_root/worker.log" >&2 2>/dev/null || true
  exit 1
}
second_status="$run_root/runtime-second.json"
"$stable" runtime status >"$second_status"
second_pid=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["pid"])' "$second_status")
[[ "$second_pid" != "$first_pid" ]] || { echo "runtime supervisor PID did not change across stable down/up: $first_pid" >&2; exit 1; }

python3 "$run_root/mcp-client.py" "$socket" session_load "$share" "$session_id" >"$transcript_after_file"
python3 "$run_root/mcp-client.py" "$socket" mcp_list "$share" "$session_id" >"$list_after_file"
python3 - "$transcript_before_file" "$transcript_after_file" "$list_after_file" <<'PY'
import json, sys
def messages(path):
    return [json.loads(line) for line in open(path) if line.strip()]
def lifecycle(path):
    transcript = next(m['transcript'] for m in messages(path) if m.get('type') == 'transcript')
    return [(e['type'], e['data']) for e in transcript['events'] if e['type'] in ('mcp_reload', 'mcp_server')]
before, after = lifecycle(sys.argv[1]), lifecycle(sys.argv[2])
assert after == before, (before, after)
servers = next(m['mcp_list']['servers'] for m in messages(sys.argv[3]) if m.get('type') == 'mcp_list')
assert len(servers) == 1 and servers[0]['name'] == 'fixture' and servers[0]['state'] == 'connected', servers
print('M07-C full stable restart replay and MCP state PASS')
PY

printf 'M07-C RUNTIME RESTART PASS %s\n' "$run_root"
