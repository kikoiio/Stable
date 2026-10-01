#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
run_root=${1:-"$project_root/run"}
dev_root="$run_root/dev-install"
temporal_bin=$(command -v temporal) || { echo 'Temporal CLI is required for make run' >&2; exit 1; }
export STABLE_STATE_DIR=${STABLE_STATE_DIR:-"$run_root/dev-state"}

if [[ -x "$dev_root/bin/stable" ]]; then
  "$dev_root/bin/stable" down >/dev/null 2>&1 || true
fi

mkdir -p "$dev_root/bin" "$dev_root/libexec" "$dev_root/share"
cd "$project_root"
go build -p 2 -buildvcs=false -o "$dev_root/bin/stable" ./cmd/stable
go build -p 2 -buildvcs=false -o "$dev_root/libexec/agentctl" ./cmd/agentctl
go build -p 2 -buildvcs=false -o "$dev_root/libexec/agentworker" ./cmd/agentworker
ln -sfn "$temporal_bin" "$dev_root/libexec/temporal"
ln -sfn "$project_root/fixtures" "$dev_root/share/fixtures"
ln -sfn "$project_root/schemas" "$dev_root/share/schemas"
ln -sfn "$project_root/workers" "$dev_root/share/workers"

echo "Development state: $STABLE_STATE_DIR"
echo "To stop the background runtime: STABLE_STATE_DIR='$STABLE_STATE_DIR' '$dev_root/bin/stable' down"
exec "$dev_root/bin/stable"
