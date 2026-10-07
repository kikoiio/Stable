#!/usr/bin/env bash
set -euo pipefail

fail() { echo "stable uninstall: $*" >&2; exit 1; }
[[ -n ${HOME:-} && -d $HOME ]] || fail 'HOME must name an existing directory'
home=$(realpath -e -- "$HOME")
local_root="$home/.local"
install_root="$local_root/opt/stable"
bin_root="$local_root/bin"
[[ ! -L $local_root && ! -L $local_root/opt && ! -L $install_root ]] ||
  fail 'refusing symlinked Stable install path'
[[ ! -e $install_root || -d $install_root ]] || fail 'Stable install root is not a directory'
[[ ! -L $bin_root ]] || fail 'refusing symlinked user bin directory'

remove_managed_link() {
  local entry=$1 link_text resolved
  [[ -L $entry ]] || return 0
  link_text=$(readlink -- "$entry")
  if [[ $link_text == /* ]]; then
    resolved=$(realpath -m -- "$link_text")
  else
    resolved=$(realpath -m -- "$(dirname "$entry")/$link_text")
  fi
  case "$resolved" in
    "$install_root"/*) rm -f -- "$entry" ;;
  esac
}

remove_managed_link "$bin_root/stable"
remove_managed_link "$bin_root/stable-uninstall"
if [[ -d $install_root ]]; then
  shopt -s nullglob dotglob
  for entry in "$install_root"/*; do
    name=${entry##*/}
    case "$name" in
      .|..) continue ;;
      .install-*|.backup-*)
        if [[ -d $entry && ! -L $entry ]]; then
          rm -rf -- "$entry"
        else
          echo "stable uninstall: preserving unexpected entry: $entry" >&2
        fi
        ;;
      [0-9A-Za-z][0-9A-Za-z._+-]*)
        if [[ -d $entry && ! -L $entry && -f $entry/VERSION && -x $entry/bin/stable ]] &&
          [[ $(tr -d '[:space:]' < "$entry/VERSION") == "$name" ]]; then
          rm -rf -- "$entry"
        else
          echo "stable uninstall: preserving unrecognized entry: $entry" >&2
        fi
        ;;
      *) echo "stable uninstall: preserving unrecognized entry: $entry" >&2 ;;
    esac
  done
  rmdir -- "$install_root" 2>/dev/null || true
fi
printf 'Stable application files removed; user configuration and state were preserved.\n'
