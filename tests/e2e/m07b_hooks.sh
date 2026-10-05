#!/usr/bin/env bash
# M07-B hooks 端到端：两级合并、拒绝拦截、通知回流、once、reload 与重启投影。
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-2}
printf '\n==> M07-B hooks merge, reject, notify, reload and restart\n'
go test -p 1 ./tests/e2e -run '^TestM07BHooksListRejectNotifyReloadAndRestart$' -count=1 -v
printf '\nM07-B e2e: all scenario groups passed.\n'
