#!/usr/bin/env bash
set -euo pipefail
project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
version=$(tr -d '[:space:]' < "$project_root/VERSION")
pkg_name="stable-$version-linux-amd64"
test_root=$(mktemp -d /tmp/stable-package-install-XXXXXXXX)
tar -xzf "$project_root/dist/$pkg_name.tar.gz" -C "$test_root"
export HOME="$test_root/home"
mkdir -p "$HOME/.local/state/stable"
printf 'preserve\n' > "$HOME/.local/state/stable/sentinel"
pkg="$test_root/$pkg_name"
bash "$pkg/install.sh" > /dev/null
bash "$pkg/install.sh" > /dev/null
test "$(cat "$HOME/.local/state/stable/sentinel")" = preserve
test -x "$HOME/.local/bin/stable"
test "$(PATH="$HOME/.local/bin:/usr/bin:/bin" stable version)" = "$version"
printf 'PACKAGE INSTALL PASS %s\n' "$test_root"
