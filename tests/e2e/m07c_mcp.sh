#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-2}
printf '\n==> M07-C MCP service lifecycle\n'
go test -p 1 ./tests/e2e -run '^TestM07CMCPServiceLifecycle$' -count=1 -v
printf '\nM07-C e2e: all scenario groups passed.\n'
