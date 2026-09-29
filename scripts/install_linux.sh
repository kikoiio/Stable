#!/usr/bin/env bash
set -euo pipefail
source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
version=0.1.0
if [[ $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then echo 'Linux x86_64 required' >&2; exit 1; fi
if [[ ! -x "$source_dir/bin/proactive-agent" || ! -x "$source_dir/libexec/temporal" ]]; then echo 'Incomplete release package' >&2; exit 1; fi
target="$HOME/.local/opt/proactive-agent/$version"
mkdir -p "$HOME/.local/opt/proactive-agent" "$HOME/.local/bin"
if [[ "$source_dir" != "$target" ]]; then
  staging=$(mktemp -d "$HOME/.local/opt/proactive-agent/.install-XXXXXXXX")
  trap 'rm -rf "$staging"' EXIT
  cp -a "$source_dir/." "$staging/"
  if [[ -e "$target" ]]; then
    if [[ ! -x "$target/bin/proactive-agent" ]]; then echo "Existing install is incomplete: $target" >&2; exit 1; fi
    rm -rf "$staging"
  else
    mv "$staging" "$target"
  fi
fi
ln -sfn "$target/bin/proactive-agent" "$HOME/.local/bin/proactive-agent"
printf 'Installed %s\nAdd %s to PATH, then run proactive-agent doctor\n' "$target" "$HOME/.local/bin"
