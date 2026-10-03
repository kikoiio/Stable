# Source from dev e2e scripts to drive decisions with the local mock model
# instead of the Codex CLI (used when codex is unavailable). Expects
# $project_root and $run_root to be set; leaves $mock_pid for the caller's
# cleanup trap.
mock_pid=
STABLE_MOCK_LOG="$run_root/mock.log" python3 "$project_root/tests/package/mock_model.py" > "$run_root/mock.port" 2>"$run_root/mock.stderr" &
mock_pid=$!
for _ in $(seq 1 50); do [[ -s "$run_root/mock.port" ]] && break; sleep 0.1; done
[[ -s "$run_root/mock.port" ]] || { echo 'mock model did not start' >&2; exit 1; }
export STABLE_APP_CONFIG=1
export STABLE_PROVIDER=openai-compatible STABLE_MODEL=mock STABLE_API_KEY=e2e-secret-marker
export STABLE_BASE_URL="http://127.0.0.1:$(cat "$run_root/mock.port")/v1"
