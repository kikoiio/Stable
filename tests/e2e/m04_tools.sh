#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-1}

printf '\n==> M04 fail-closed tool executor\n'
go test -p 1 ./tests/e2e -run '^TestM04ToolExecutorFailClosed$' -count=1 -v

printf '\n==> M04 Linux namespace positive tool executor\n'
if ! command -v bwrap >/dev/null 2>&1 || ! unshare -Ur true >/dev/null 2>&1; then
  printf 'SKIP: user namespaces are unavailable; positive isolation evidence is not claimed.\n'
  exit 0
fi
helper=${STABLE_M04_HELPER:-}
if [[ -z "$helper" ]]; then
  if [[ -x "$project_root/dev-install/libexec/agentworker" ]]; then
    helper="$project_root/dev-install/libexec/agentworker"
  elif [[ -x "$project_root/bin/agentworker" ]]; then
    helper="$project_root/bin/agentworker"
  else
    printf 'SKIP: agentworker helper is not built; positive isolation evidence is not claimed.\n'
    exit 0
  fi
fi
STABLE_M04_HELPER="$helper" go test -p 1 ./tests/e2e -run '^TestM04SandboxPositive$' -count=1 -v
