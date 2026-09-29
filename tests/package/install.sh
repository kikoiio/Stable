#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d /tmp/proactive-package-install-XXXXXXXX)
tar -xzf "$project_root/dist/proactive-agent-0.1.0-linux-amd64.tar.gz" -C "$test_root"
export HOME="$test_root/home"
mkdir -p "$HOME/.local/state/proactive-agent"
printf 'preserve\n' > "$HOME/.local/state/proactive-agent/sentinel"
pkg="$test_root/proactive-agent-0.1.0-linux-amd64"
bash "$pkg/install.sh" > /dev/null
bash "$pkg/install.sh" > /dev/null
test "$(cat "$HOME/.local/state/proactive-agent/sentinel")" = preserve
test -x "$HOME/.local/bin/proactive-agent"
test "$(PATH="$HOME/.local/bin:/usr/bin:/bin" proactive-agent version)" = 0.1.0
printf 'PACKAGE INSTALL PASS %s\n' "$test_root"
