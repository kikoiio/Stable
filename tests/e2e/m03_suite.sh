#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-1}

run() {
  printf '\n==> %s\n' "$1"
  shift
  "$@"
}

# Namespace and GUI checks share host resources, so keep this suite serial.
run 'sandbox filesystem boundary' bash tests/e2e/m03_sandbox_files.sh
run 'sandbox network boundary' bash tests/e2e/m03_sandbox_network.sh
run 'trusted service secret scan' bash tests/e2e/m03_sandbox_secrets.sh
run 'KiCad bridge contract' python3 -m unittest discover -s workers/kicad
run 'computer bridge contract' python3 -m unittest discover -s workers/computer
run 'isolated KiCad candidate repair and ERC' bash tests/e2e/m03_kicad_candidate.sh
run 'isolated computer session lifecycle and recovery' bash tests/e2e/m03_computer_session.sh
run 'trusted candidate review and acceptance' bash tests/e2e/m03_acceptance.sh
run 'force acceptance finding confirmation' bash tests/e2e/m03_force_accept.sh
run 'acceptance interruption and SQLite restart recovery' bash tests/e2e/m03_accept_restart.sh
run 'M03 service and package regression' go test -p 1 ./internal/permission ./internal/platform/sandbox ./internal/candidate ./internal/conversation ./internal/execution ./internal/store ./internal/runtime ./cmd/stable ./cmd/agentworker -count=1
run 'full Go test suite' go test -p 1 $(go list ./... | grep -v '^stable/tests/e2e$') -count=1
printf '\nM03 serialized E2E suite passed.\n'
