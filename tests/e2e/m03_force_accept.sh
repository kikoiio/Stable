#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
GOMAXPROCS=1 go test -p 1 ./tests/e2e -run '^TestM03ForceAcceptanceRequiresEveryFindingAndKeepsReverification$' -count=1 -v
