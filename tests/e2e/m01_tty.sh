#!/usr/bin/env bash
# M01 production TUI acceptance through a Linux pseudo-terminal.
# This is intentionally not a Bubble Tea model test: Python starts the formal
# `stable` executable with no arguments on a controlling PTY.
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-2}

command -v temporal >/dev/null || { echo 'temporal CLI is required (install via setup-e2e-deps)' >&2; exit 1; }
mkdir -p "$project_root/.tmp"
stage=$(mktemp -d "$project_root/.tmp/m01-tty.XXXXXXXX")
cleanup() { rm -rf -- "$stage"; }
trap cleanup EXIT

mkdir -p "$stage/bin" "$stage/libexec" "$stage/share"
go build -buildvcs=false -o "$stage/bin/stable" ./cmd/stable
go build -buildvcs=false -o "$stage/libexec/agentworker" ./cmd/agentworker
cp "$(command -v temporal)" "$stage/libexec/temporal"
cp -a "$project_root/schemas" "$project_root/workers" "$project_root/fixtures" "$stage/share/"
chmod 700 "$stage" "$stage/bin" "$stage/libexec" "$stage/share"

python3 "$project_root/tests/e2e/m01_tty.py" --stage "$stage"
