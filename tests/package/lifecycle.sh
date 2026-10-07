#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
version=$(tr -d '[:space:]' < "$project_root/VERSION")
pkg_name="stable-$version-linux-amd64"
current_pkg="$project_root/dist/$pkg_name"
test_root=$(mktemp -d /tmp/stable-package-lifecycle-XXXXXXXX)
export HOME="$test_root/home"
mkdir -p "$HOME"
cleanup() {
  if [[ ${KEEP_TEST_ROOT:-0} == 1 ]]; then
    echo "Lifecycle test files kept at $test_root" >&2
  else
    rm -rf -- "$test_root"
  fi
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
expect_install_failure() {
  local package_dir=$1 output
  if output=$(cd "$package_dir" && bash ./install.sh 2>&1); then
    fail "invalid package unexpectedly installed: $package_dir"
  fi
  [[ $output == *'stable install:'* ]] || fail "install failed without a useful diagnostic: $output"
}
resolved_link() {
  readlink -f -- "$1"
}
assert_user_data() {
  [[ $(cat "$config") == config-preserve ]] || fail 'install changed user config'
  [[ $(cat "$state") == state-preserve ]] || fail 'install changed user state'
}

[[ -d $current_pkg ]] || fail "package directory not found: $current_pkg"
[[ -x $current_pkg/uninstall.sh ]] || fail 'package is missing executable uninstall.sh'

# Use the current complete package as a synthetic older build. The stub CLI
# gives the fixture a distinct observable version without another Go build.
old_version=0.0.9
old_pkg="$test_root/$pkg_name-old"
cp -a -- "$current_pkg" "$old_pkg"
printf '%s\n' "$old_version" > "$old_pkg/VERSION"
cat > "$old_pkg/bin/stable" <<'OLDCLI'
#!/usr/bin/env sh
printf '0.0.9\n'
OLDCLI
chmod +x "$old_pkg/bin/stable"

config="$HOME/.config/stable/config.json"
state="$HOME/.local/state/stable/sentinel"
mkdir -p "$(dirname "$config")" "$(dirname "$state")"
printf 'config-preserve\n' > "$config"
printf 'state-preserve\n' > "$state"

# First install and same-version reinstall are idempotent.
bash "$old_pkg/install.sh" >/dev/null
[[ $(PATH="$HOME/.local/bin:/usr/bin:/bin" stable version) == "$old_version" ]] || fail 'first install selected wrong version'
assert_user_data
[[ $(resolved_link "$HOME/.local/bin/stable") == "$HOME/.local/opt/stable/$old_version/bin/stable" ]] || fail 'stable link does not target the installed version'
[[ $(resolved_link "$HOME/.local/bin/stable-uninstall") == "$HOME/.local/opt/stable/$old_version/uninstall.sh" ]] || fail 'uninstall link does not target the installed version'
bash "$old_pkg/install.sh" >/dev/null
[[ $(PATH="$HOME/.local/bin:/usr/bin:/bin" stable version) == "$old_version" ]] || fail 'same-version reinstall changed the active version'
assert_user_data
echo 'LIFECYCLE first install and reinstall PASS'

# Upgrade switches the command link but retains the older version for rollback.
bash "$current_pkg/install.sh" >/dev/null
[[ $(PATH="$HOME/.local/bin:/usr/bin:/bin" stable version) == "$version" ]] || fail 'upgrade did not select current version'
[[ -d $HOME/.local/opt/stable/$old_version ]] || fail 'upgrade removed the previous version'
[[ $(resolved_link "$HOME/.local/bin/stable") == "$HOME/.local/opt/stable/$version/bin/stable" ]] || fail 'upgrade left stable pointing to the wrong version'
assert_user_data
# Reinstalling the older package rolls back through the same supported path.
bash "$old_pkg/install.sh" >/dev/null
[[ $(PATH="$HOME/.local/bin:/usr/bin:/bin" stable version) == "$old_version" ]] || fail 'rollback did not restore the older version'
[[ -d $HOME/.local/opt/stable/$version ]] || fail 'rollback removed the newer version'
assert_user_data
echo 'LIFECYCLE upgrade and rollback PASS'

# Reject an incomplete package before modifying the active installation.
bad_pkg="$test_root/incomplete"
cp -a -- "$current_pkg" "$bad_pkg"
rm -- "$bad_pkg/libexec/agentworker"
expect_install_failure "$bad_pkg"
[[ $(PATH="$HOME/.local/bin:/usr/bin:/bin" stable version) == "$old_version" ]] || fail 'invalid package changed the active command'
[[ $(resolved_link "$HOME/.local/bin/stable") == "$HOME/.local/opt/stable/$old_version/bin/stable" ]] || fail 'invalid package changed the stable link'
bad_version_pkg="$test_root/invalid-version"
cp -a -- "$current_pkg" "$bad_version_pkg"
printf '../escape\n' > "$bad_version_pkg/VERSION"
expect_install_failure "$bad_version_pkg"
[[ $(PATH="$HOME/.local/bin:/usr/bin:/bin" stable version) == "$old_version" ]] || fail 'invalid VERSION changed the active command'
assert_user_data
echo 'LIFECYCLE invalid package protection PASS'

# Preserve a same-named command owned by the user and all user data on uninstall.
external_stable="$test_root/external-stable"
cat > "$external_stable" <<'EXTERNAL'
#!/usr/bin/env sh
printf 'external stable\n'
EXTERNAL
chmod +x "$external_stable"
ln -sfn -- "$external_stable" "$HOME/.local/bin/stable"
printf 'unrelated\n' > "$HOME/.local/bin/unrelated-command"
mkdir -p "$HOME/.local/opt/stable/0.9.9"
printf 'user-owned directory\n' > "$HOME/.local/opt/stable/0.9.9/notes.txt"
"$HOME/.local/bin/stable-uninstall" >/dev/null
[[ ! -e $HOME/.local/opt/stable/$old_version ]] || fail 'uninstall left the old version installed'
[[ ! -e $HOME/.local/opt/stable/$version ]] || fail 'uninstall left the current version installed'
[[ ! -e $HOME/.local/bin/stable-uninstall && ! -L $HOME/.local/bin/stable-uninstall ]] || fail 'uninstall left its managed command entry'
[[ $(resolved_link "$HOME/.local/bin/stable") == "$external_stable" ]] || fail 'uninstall removed or changed the user-owned stable command'
[[ $(cat "$HOME/.local/bin/unrelated-command") == unrelated ]] || fail 'uninstall changed an unrelated command'
[[ $(cat "$HOME/.local/opt/stable/0.9.9/notes.txt") == 'user-owned directory' ]] || fail 'uninstall removed an unrecognized directory'
[[ $(cat "$config") == config-preserve ]] || fail 'uninstall removed user config'
[[ $(cat "$state") == state-preserve ]] || fail 'uninstall removed user state'
echo 'LIFECYCLE uninstall and data preservation PASS'
