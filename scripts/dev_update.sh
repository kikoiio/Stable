#!/usr/bin/env bash
# Rebuild the release package from source and install it over the current one.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# Stop the runtime and any leftover GUI sessions before replacing the binaries.
if [[ -x "$HOME/.local/bin/stable" ]]; then "$HOME/.local/bin/stable" down || true; fi
bash "$root/scripts/package_linux.sh"
version=$(tr -d '[:space:]' < "$root/VERSION")
bash "$root/dist/stable-$version-linux-amd64/install.sh"
