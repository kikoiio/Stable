#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-1}

printf '\n==> M04 fail-closed tool executor\n'
go test -p 1 ./tests/e2e -run '^TestM04ToolExecutorFailClosed$' -count=1 -v

printf '\n==> M04 Linux namespace positive tool executor\n'
# The isolation boundary is bwrap, not a bare unshare: on hosts where AppArmor
# restricts unprivileged user namespaces, unshare(1) fails while the packaged
# bwrap profile still works. Probe the real dependency.
if ! command -v bwrap >/dev/null 2>&1 || ! bwrap --unshare-all --ro-bind / / /bin/true >/dev/null 2>&1; then
  printf 'SKIP: bwrap namespaces are unavailable; positive isolation evidence is not claimed.\n'
  exit 0
fi
helper=${STABLE_M04_HELPER:-}
if [[ -z "$helper" ]]; then
  if [[ -x "$project_root/dev-install/libexec/agentworker" ]]; then
    helper="$project_root/dev-install/libexec/agentworker"
  elif [[ -x "$project_root/bin/agentworker" ]]; then
    helper="$project_root/bin/agentworker"
  else
    helper_root=$(mktemp -d "${TMPDIR:-/tmp}/stable-m04-helper-XXXXXXXX")
    go build -buildvcs=false -o "$helper_root/agentworker" ./cmd/agentworker
    helper="$helper_root/agentworker"
  fi
fi
STABLE_M04_HELPER="$helper" go test -p 1 ./tests/e2e -run '^TestM04(SandboxPositive|SandboxEscapeTrio|CommandTimeoutAndExitSemantics|ToolFlowScrubsSecrets)$' -count=1 -v
