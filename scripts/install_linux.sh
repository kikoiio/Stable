#!/usr/bin/env bash
set -euo pipefail
source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
version=$(tr -d '[:space:]' < "$source_dir/VERSION")
if [[ $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then echo 'Linux x86_64 required' >&2; exit 1; fi
if [[ ! -x "$source_dir/bin/stable" || ! -x "$source_dir/libexec/temporal" ]]; then echo 'Incomplete release package' >&2; exit 1; fi
target="$HOME/.local/opt/stable/$version"
mkdir -p "$HOME/.local/opt/stable" "$HOME/.local/bin"
if [[ "$source_dir" != "$target" ]]; then
  staging=$(mktemp -d "$HOME/.local/opt/stable/.install-XXXXXXXX")
  trap 'rm -rf "$staging"' EXIT
  cp -a "$source_dir/." "$staging/"
  # Replace any existing install of this version; config and state live outside $target.
  rm -rf "$target.old"
  if [[ -e "$target" ]]; then mv "$target" "$target.old"; fi
  mv "$staging" "$target"
  rm -rf "$target.old"
fi
ln -sfn "$target/bin/stable" "$HOME/.local/bin/stable"
printf 'Installed %s\nAdd %s to PATH, then run stable doctor\n' "$target" "$HOME/.local/bin"
