#!/usr/bin/env bash
set -u

missing=0
check_config=$(mktemp -d)
trap 'rm -rf "$check_config"' EXIT
export XDG_CACHE_HOME="$check_config/cache"
export XDG_CONFIG_HOME="$check_config/config"
export XDG_DATA_HOME="$check_config/data"

check() {
  local name="$1" command_name="$2"
  shift 2
  if command -v "$command_name" >/dev/null 2>&1; then
    local version
    if (($#)); then
      if ! version=$("$command_name" "$@" 2>&1); then
        printf '%-18s unusable   %s\n' "$name" "${version##*$'\n'}"
        missing=1
        return
      fi
      version=${version##*$'\n'}
    else
      version=$(command -v "$command_name")
    fi
    printf '%-18s available  %s\n' "$name" "${version:-$(command -v "$command_name")}"
  else
    printf '%-18s missing    %s\n' "$name" "$command_name"
    missing=1
  fi
}

check 'Go' go version
check 'Temporal CLI' temporal --version
check 'Python' python3 --version
check 'KiCad CLI' kicad-cli version
check 'KiCad GUI' eeschema
check 'Virtual display' Xvfb
check 'Virtual wrapper' xvfb-run
check 'Window inspector' xprop
check 'Screenshot' import
check 'Codex CLI' codex --version

exit "$missing"
