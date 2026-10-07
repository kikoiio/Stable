#!/usr/bin/env bash
set -euo pipefail

fail() { echo "stable install: $*" >&2; exit 1; }

[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || fail 'Linux x86_64 required'
[[ -n ${HOME:-} && -d $HOME ]] || fail 'HOME must name an existing directory'
home=$(realpath -e -- "$HOME")
source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
version_file="$source_dir/VERSION"
[[ -f $version_file && ! -L $version_file ]] || fail 'package VERSION is missing or unsafe'
version=$(tr -d '[:space:]' < "$version_file")
[[ $version =~ ^[0-9A-Za-z][0-9A-Za-z._+-]*$ ]] || fail 'package VERSION is invalid'

required=(
  bin/stable
  libexec/agentctl
  libexec/agentworker
  libexec/temporal
  share/fixtures/sensor_board/sensor.kicad_sch
  share/schemas/next_action.schema.json
  share/schemas/criteria_proposal.schema.json
  share/workers/kicad/bridge.py
  share/workers/computer/bridge.py
  install.sh
  uninstall.sh
)
for relative in "${required[@]}"; do
  path="$source_dir/$relative"
  [[ -f $path && ! -L $path ]] || fail "package is incomplete: $relative"
done
[[ -x $source_dir/bin/stable && -x $source_dir/libexec/agentctl && \
   -x $source_dir/libexec/agentworker && -x $source_dir/libexec/temporal ]] ||
  fail 'package contains a non-executable program'

local_root="$home/.local"
opt_root="$local_root/opt"
install_root="$opt_root/stable"
bin_root="$local_root/bin"
for path in "$local_root" "$opt_root" "$install_root" "$bin_root"; do
  [[ ! -L $path ]] || fail "refusing symlinked install path: $path"
done
mkdir -p -- "$install_root" "$bin_root"
[[ $(realpath -e -- "$install_root") == "$install_root" ]] || fail 'install root resolves outside its managed path'
[[ $(realpath -e -- "$bin_root") == "$bin_root" ]] || fail 'bin directory resolves outside its managed path'

target="$install_root/$version"
[[ ! -L $target ]] || fail "refusing symlinked version path: $target"
if [[ -e $target ]]; then
  [[ -d $target && -f $target/VERSION && -x $target/bin/stable ]] ||
    fail "version path exists but is not a Stable installation: $target"
  installed_version=$(tr -d '[:space:]' < "$target/VERSION")
  [[ $installed_version == "$version" ]] || fail "installed VERSION does not match path: $target"
fi

stable_link="$bin_root/stable"
uninstall_link="$bin_root/stable-uninstall"
validate_entry() {
  local entry=$1 link_text resolved
  if [[ -e $entry && ! -L $entry ]]; then
    fail "refusing to replace non-symlink command entry: $entry"
  fi
  if [[ -L $entry ]]; then
    link_text=$(readlink -- "$entry")
    if [[ $link_text == /* ]]; then
      resolved=$(realpath -m -- "$link_text")
    else
      resolved=$(realpath -m -- "$(dirname "$entry")/$link_text")
    fi
    case "$resolved" in
      "$install_root"/*) ;;
      *) fail "refusing to replace command entry outside Stable install root: $entry" ;;
    esac
  fi
}
validate_entry "$stable_link"
validate_entry "$uninstall_link"

old_stable_present=0
old_stable_target=
if [[ -L $stable_link ]]; then old_stable_present=1; old_stable_target=$(readlink -- "$stable_link"); fi
old_uninstall_present=0
old_uninstall_target=
if [[ -L $uninstall_link ]]; then old_uninstall_present=1; old_uninstall_target=$(readlink -- "$uninstall_link"); fi

stage=
backup=
old_target_present=0
restore_target() {
  if [[ -n $backup && -d $backup ]]; then
    rm -rf -- "$target"
    mv -- "$backup" "$target"
    backup=
  elif (( old_target_present == 0 )); then
    rm -rf -- "$target"
  fi
}
restore_link() {
  local entry=$1 was_present=$2 old_target=$3 temporary
  if (( was_present )); then
    temporary="$entry.restore.$$.$RANDOM"
    ln -s -- "$old_target" "$temporary"
    mv -Tf -- "$temporary" "$entry"
  else
    rm -f -- "$entry"
  fi
}
atomic_link() {
  local source=$1 entry=$2 temporary="$2.tmp.$$.$RANDOM"
  ln -s -- "$source" "$temporary"
  if ! mv -Tf -- "$temporary" "$entry"; then
    rm -f -- "$temporary"
    return 1
  fi
}
cleanup() {
  if [[ -n $stage && -d $stage ]]; then rm -rf -- "$stage"; fi
}
trap cleanup EXIT

if [[ $source_dir != "$target" ]]; then
  stage=$(mktemp -d "$install_root/.install-XXXXXXXX")
  cp -a -- "$source_dir/." "$stage/"
  for relative in "${required[@]}"; do
    [[ -f $stage/$relative && ! -L $stage/$relative ]] || fail "staged package is incomplete: $relative"
  done
  if [[ -e $target ]]; then
    [[ -d $target && ! -L $target ]] || fail "version target is not a directory: $target"
    backup=$(mktemp -d "$install_root/.backup-XXXXXXXX")
    rmdir -- "$backup"
    mv -- "$target" "$backup"
    old_target_present=1
  fi
  if ! mv -- "$stage" "$target"; then
    restore_target
    fail 'could not place the staged package; previous installation was restored'
  fi
  stage=
fi

if ! atomic_link "$target/bin/stable" "$stable_link"; then
  restore_target
  fail 'could not update stable command entry; previous installation was restored'
fi
if ! atomic_link "$target/uninstall.sh" "$uninstall_link"; then
  restore_link "$stable_link" "$old_stable_present" "$old_stable_target"
  restore_target
  fail 'could not update uninstall command entry; previous installation was restored'
fi
if [[ -n $backup && -d $backup ]]; then rm -rf -- "$backup"; backup=; fi
printf 'Installed %s\nAdd %s to PATH, then run stable doctor\n' "$target" "$bin_root"
