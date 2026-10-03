#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
source "$project_root/tests/e2e/lib.sh"
source "$project_root/tests/e2e/m03_fixture.sh"
e2e_alloc m03-secrets
trap e2e_run_cleanups EXIT
m03_fixture_setup

export M03_PROJECT M03_CANDIDATE M03_RUN_DIR M03_SENTINEL M03_HOST_SECRET M03_SECRET
cd "$project_root"
GOMAXPROCS=${GOMAXPROCS:-1} go test -p 1 ./tests/e2e -run '^TestM03SandboxSecretsTrustedFlow$' -count=1 -v
