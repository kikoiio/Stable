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
if [[ -n "${STABLE_PREBUILT_DIR:-}" ]]; then
  # Reuse a directory built by an earlier run_local invocation (or make
  # prebuilt); several isolated run roots can share one build. Binaries are
  # copied (not linked) so the runtime resolves libexec/share inside this run
  # root's dev-install.
  prebuilt="$STABLE_PREBUILT_DIR"
  for binary in bin/stable libexec/agentctl libexec/agentworker; do
    [[ -x "$prebuilt/$binary" ]] || { echo "prebuilt directory lacks $binary" >&2; exit 1; }
    cp -a "$prebuilt/$binary" "$dev_root/$binary"
  done
else
  go build -p 2 -buildvcs=false -o "$dev_root/bin/stable" ./cmd/stable
  go build -p 2 -buildvcs=false -o "$dev_root/libexec/agentctl" ./cmd/agentctl
  go build -p 2 -buildvcs=false -o "$dev_root/libexec/agentworker" ./cmd/agentworker
fi
ln -sfn "$temporal_bin" "$dev_root/libexec/temporal"
# The sandbox refuses mounts that cross symbolic links, so the share tree must
# be real files. Copies are small and keep the running install decoupled from
# later repo edits.
rm -rf "$dev_root/share/fixtures" "$dev_root/share/schemas" "$dev_root/share/workers"
cp -a "$project_root/fixtures" "$dev_root/share/fixtures"
cp -a "$project_root/schemas" "$dev_root/share/schemas"
cp -a "$project_root/workers" "$dev_root/share/workers"

echo "Development state: $STABLE_STATE_DIR"
echo "To stop the background runtime: STABLE_STATE_DIR='$STABLE_STATE_DIR' '$dev_root/bin/stable' down"
if [[ "${STABLE_RUN_LOCAL_UP:-}" == "1" || ! -t 0 ]]; then
  # Non-interactive callers (including e2e scripts) need the supervised
  # runtime, not the production TUI, which requires a controlling terminal.
  exec "$dev_root/bin/stable" up
fi
exec "$dev_root/bin/stable"
