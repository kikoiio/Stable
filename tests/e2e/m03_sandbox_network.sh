#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
source "$project_root/tests/e2e/lib.sh"
source "$project_root/tests/e2e/m03_fixture.sh"
e2e_alloc m03-network
trap e2e_run_cleanups EXIT
m03_fixture_setup

GOMAXPROCS=1 go build -p 1 -buildvcs=false -o "$E2E_ROOT/agentworker" ./cmd/agentworker
export M03_PROJECT M03_CANDIDATE M03_RUN_DIR M03_SENTINEL M03_HOST_SECRET M03_SECRET
export M03_SESSION_ROOT="$E2E_ROOT/session-project"
mkdir -p "$M03_SESSION_ROOT"
export M03_PROXY_HELPER="$E2E_ROOT/agentworker"
cd "$project_root"
GOMAXPROCS=1 go test -p 1 ./tests/e2e -run '^TestM03SandboxNetwork$' -count=1 -v
