# Shared resource isolation helpers for e2e scripts. Source this file; it
# expects no variables and sets project_root itself. Each script calls
# e2e_alloc once with a label, then registers cleanup callbacks with
# e2e_on_cleanup and traps e2e_run_cleanups on EXIT.
#
# Provided variables after e2e_alloc <label>:
#   E2E_ROOT    private run root (also exported as HOME and TMPDIR base)
#   E2E_PORT    an unused loopback TCP port for Temporal
#   E2E_GOAL    a unique goal ID prefix (<label>-<timestamp>-<pid>)
#
# Two concurrently sourced copies never share a run root, port, goal ID,
# HOME or TMPDIR, and e2e_run_cleanups leaves no stray processes behind.

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

# HOME is isolated per run, but the Go build/module caches are content
# addressed and safe to share; without this every run root re-downloads the
# whole module graph.
if [[ -z "${GOCACHE:-}" ]]; then _gocache=$(go env GOCACHE 2>/dev/null || true); [[ -n "$_gocache" ]] && export GOCACHE="$_gocache"; fi
if [[ -z "${GOMODCACHE:-}" ]]; then _gomodcache=$(go env GOMODCACHE 2>/dev/null || true); [[ -n "$_gomodcache" ]] && export GOMODCACHE="$_gomodcache"; fi
unset _gocache _gomodcache

E2E_CLEANUP_FUNCS=()

e2e_free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

e2e_alloc() {
  local label=${1:?e2e_alloc requires a label}
  E2E_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/stable-e2e-${label}-XXXXXXXX")
  E2E_PORT=$(e2e_free_port)
  E2E_GOAL="${label}-$(date +%s)-$$"
  mkdir -p "$E2E_ROOT/home" "$E2E_ROOT/tmp"
  export HOME="$E2E_ROOT/home"
  export TMPDIR="$E2E_ROOT/tmp"
  export STABLE_STATE_DIR="$E2E_ROOT"
  e2e_on_cleanup e2e_default_cleanup
}

e2e_default_cleanup() {
  # Stop any supervised runtime and isolated sessions started under this root.
  if [[ -x "$E2E_ROOT/dev-install/bin/stable" ]]; then
    STABLE_STATE_DIR="$E2E_ROOT" "$E2E_ROOT/dev-install/bin/stable" down >/dev/null 2>&1 || true
  fi
  python3 "$project_root/tests/e2e/stop_sessions.py" "$E2E_ROOT" >/dev/null 2>&1 || true
}

e2e_on_cleanup() {
  E2E_CLEANUP_FUNCS+=("$1")
}

e2e_run_cleanups() {
  local idx
  for (( idx=${#E2E_CLEANUP_FUNCS[@]}-1 ; idx>=0 ; idx-- )); do
    "${E2E_CLEANUP_FUNCS[idx]}" || true
  done
  E2E_CLEANUP_FUNCS=()
}

# e2e_wait_for <attempts> <description> <command...>
# Polls the command until it exits 0; prints the description and fails otherwise.
e2e_wait_for() {
  local attempts=$1 desc=$2
  shift 2
  local i
  for (( i=0; i<attempts; i++ )); do
    if "$@"; then return 0; fi
    sleep 0.5
  done
  echo "timed out waiting for: $desc" >&2
  return 1
}

e2e_wait_for_chat_socket() {
  local socket=${1:?chat socket path required}
  e2e_wait_for 120 "chat socket $socket" test -S "$socket"
}
