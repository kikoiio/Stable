#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
source "$project_root/tests/e2e/lib.sh"
source "$project_root/tests/e2e/m03_fixture.sh"
e2e_alloc m03-kicad
e2e_on_cleanup m03_fixture_cleanup
trap e2e_run_cleanups EXIT
m03_fixture_setup
export M03_CANDIDATE M03_RUN_DIR M03_PROJECT
export M03_BRIDGE_CANDIDATE="$E2E_ROOT/official-candidate"
export M03_BRIDGE_BINARY="$E2E_ROOT/agentworker"
export M03_BRIDGE_SCRIPT="$project_root/workers/kicad/bridge.py"
mkdir -p "$M03_RUN_DIR"
GOMAXPROCS=1 go build -p 1 -buildvcs=false -o "$M03_BRIDGE_BINARY" ./cmd/agentworker
cd "$project_root"
GOMAXPROCS=1 go test -p 1 ./tests/e2e -run '^TestM03KicadRepairRunsOnlyAgainstCandidateInLinuxSandbox$' -count=1 -v
