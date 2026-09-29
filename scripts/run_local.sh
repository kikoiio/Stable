#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
run_root=${1:-"$project_root/run"}
temporal_port=${PROACTIVE_TEMPORAL_PORT:-7233}
temporal_address="localhost:${temporal_port}"
mkdir -p "$run_root/bin"

cd "$project_root"
go build -buildvcs=false -o "$run_root/bin/agentctl" ./cmd/agentctl
go build -buildvcs=false -o "$run_root/bin/agentworker" ./cmd/agentworker

temporal server start-dev --headless --port "$temporal_port" --db-filename "$run_root/temporal.db" >"$run_root/temporal.log" 2>&1 &
temporal_pid=$!
worker_pid=
cleanup() {
  if [[ -n "$worker_pid" ]]; then kill "$worker_pid" 2>/dev/null || true; wait "$worker_pid" 2>/dev/null || true; fi
  kill "$temporal_pid" 2>/dev/null || true
  wait "$temporal_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

ready=0
for _ in $(seq 1 60); do
  if temporal operator namespace describe --address "$temporal_address" --namespace default >/dev/null 2>&1; then ready=1; break; fi
  if ! kill -0 "$temporal_pid" 2>/dev/null; then break; fi
  sleep 0.25
done
if [[ "$ready" != 1 ]]; then
  cat "$run_root/temporal.log" >&2
  exit 1
fi

worker_args=(--db "$run_root/state.db" --run-root "$run_root" --temporal "$temporal_address" --project-root "$project_root")
if [[ "${PROACTIVE_APP_CONFIG:-}" == 1 ]]; then worker_args+=(--app-config); fi
"$run_root/bin/agentworker" "${worker_args[@]}" >"$run_root/worker.log" 2>&1 &
worker_pid=$!
sleep 1
if ! kill -0 "$worker_pid" 2>/dev/null; then cat "$run_root/worker.log" >&2; exit 1; fi

printf 'Temporal: %s\nData: %s\nWorker PID: %s\n' "$temporal_address" "$run_root" "$worker_pid"
printf 'Create goal: %s/bin/agentctl start --run-root %s --temporal %s --project-root %s\n' "$run_root" "$run_root" "$temporal_address" "$project_root"
printf 'Use agentctl status, notify, and export with --run-root %s. Press Ctrl-C to stop.\n' "$run_root"
wait "$worker_pid"
