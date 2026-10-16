#!/usr/bin/env bash
set -euo pipefail

fail() { echo "stable install: $*" >&2; exit 1; }
[[ $(uname -s) == Darwin ]] || fail 'macOS required'
[[ -n ${HOME:-} && -d $HOME ]] || fail 'HOME must name an existing directory'
source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
version=$(tr -d '[:space:]' < "$source_dir/VERSION")
[[ $version =~ ^[0-9A-Za-z][0-9A-Za-z._+-]*$ ]] || fail 'package VERSION is invalid'
for relative in bin/stable libexec/agentctl libexec/agentworker libexec/temporal share/schemas/next_action.schema.json share/schemas/criteria_proposal.schema.json install.sh VERSION; do
  [[ -f $source_dir/$relative && ! -L $source_dir/$relative ]] || fail "package is incomplete: $relative"
done
[[ -x $source_dir/bin/stable && -x $source_dir/libexec/agentctl && -x $source_dir/libexec/agentworker && -x $source_dir/libexec/temporal ]] ||
  fail 'package contains a non-executable program'

install_root="$HOME/.local/opt/stable"
bin_root="$HOME/.local/bin"
for path in "$HOME/.local" "$HOME/.local/opt" "$install_root" "$bin_root"; do
  [[ ! -L $path ]] || fail "refusing symlinked install path: $path"
done
mkdir -p "$install_root" "$bin_root"
target="$install_root/$version"
if [[ -e $target ]]; then
  [[ -d $target && ! -L $target && -f $target/VERSION && -x $target/bin/stable ]] || fail "version path exists but is not a Stable installation: $target"
  [[ $(tr -d '[:space:]' < "$target/VERSION") == "$version" ]] || fail "installed VERSION does not match path: $target"
else
  stage=$(mktemp -d "$install_root/.install-XXXXXXXX")
  trap '[[ -z ${stage:-} ]] || rm -rf "$stage"' EXIT
  cp -a "$source_dir/." "$stage/"
  mv "$stage" "$target"
  stage=
fi

link="$bin_root/stable"
if [[ -e $link && ! -L $link ]]; then fail "refusing to replace non-symlink command entry: $link"; fi
if [[ -L $link ]]; then
  current=$(readlink "$link")
  case "$current" in
    "$install_root"/*/bin/stable) ;;
    *) fail "refusing to replace command entry outside Stable install root: $link" ;;
  esac
fi
temporary="$link.tmp.$$"
ln -s "$target/bin/stable" "$temporary"
mv -f "$temporary" "$link"
printf 'Installed %s\nRun %s doctor; add %s to PATH to use stable by name.\n' "$target" "$link" "$bin_root"
