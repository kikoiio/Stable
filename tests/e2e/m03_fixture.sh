# M03 fixture helper: copies the S01 project into an official and a candidate
# tree on the same filesystem, and plants an outside sentinel plus unique fake
# secret markers for the sandbox boundary scenarios. Source this file AFTER
# lib.sh (it needs E2E_ROOT and e2e_on_cleanup).
#
# Provided after m03_fixture_setup:
#   M03_SRC        repo case directory (read-only source, never modified)
#   M03_PROJECT    official project copy ($E2E_ROOT/project)
#   M03_CANDIDATE  candidate copy on the same filesystem ($E2E_ROOT/candidate)
#   M03_RUN_DIR    private run dir for desktop caches and reports
#   M03_OUTSIDE    directory outside any authorized root, holding the sentinel
#   M03_SENTINEL   sentinel file that must never leak into sandboxed output
#   M03_SECRET     unique fake model key planted in the parent environment
#   M03_HOST_SECRET  fake host secret file outside the authorized roots
#
# Executed directly, the script runs the V03 self-check: the official copy's
# SHA equals the source, cleanup leaves the repo case untouched, and the
# sentinel/secret files exist only under the fixture root.

M03_SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../cases/cases/S01_missing_wire"

m03_fixture_setup() {
  [[ -n "${E2E_ROOT:-}" ]] || { echo 'm03_fixture_setup requires e2e_alloc first' >&2; return 1; }
  M03_PROJECT="$E2E_ROOT/project"
  # Acceptance exchanges the formal root with the candidate, so both must be
  # direct siblings on the same filesystem.
  M03_CANDIDATE="$E2E_ROOT/official-candidate"
  M03_RUN_DIR="$E2E_ROOT/private-run"
  M03_OUTSIDE="$E2E_ROOT/outside"
  M03_SENTINEL="$M03_OUTSIDE/sentinel.txt"
  M03_SECRET="m03-fake-model-key-$(date +%s)-$$"
  M03_HOST_SECRET="$M03_OUTSIDE/host_secret.txt"
  mkdir -p "$M03_PROJECT" "$M03_CANDIDATE" "$M03_RUN_DIR" "$M03_OUTSIDE"
  cp "$M03_SRC/sensor.kicad_sch" "$M03_SRC/sensor.kicad_pro" "$M03_PROJECT/"
  cp "$M03_SRC/sensor.kicad_sch" "$M03_SRC/sensor.kicad_pro" "$M03_CANDIDATE/"
  # Same-filesystem requirement for renameat2(RENAME_EXCHANGE) at accept time.
  local src_dev dst_dev
  src_dev=$(stat -c %d "$M03_PROJECT") && dst_dev=$(stat -c %d "$M03_CANDIDATE")
  [[ "$src_dev" == "$dst_dev" ]] || { echo 'official and candidate on different filesystems' >&2; return 1; }
  printf 'm03-sentinel-%s\n' "$(date +%s)-$$" >"$M03_SENTINEL"
  printf 'm03-fake-host-secret-%s\n' "$(date +%s)-$$" >"$M03_HOST_SECRET"
  chmod 600 "$M03_SENTINEL" "$M03_HOST_SECRET"
  # The fake model key rides the environment exactly like a real one would.
  export STABLE_API_KEY="$M03_SECRET"
  e2e_on_cleanup m03_fixture_cleanup
}

m03_fixture_digest() {
  sha256sum "$M03_PROJECT/sensor.kicad_sch" | cut -d' ' -f1
}

m03_fixture_cleanup() {
  # Only copied data under the fixture root is removed; the repo case is the
  # read-only source and must survive every run.
  [[ -n "${E2E_ROOT:-}" && "$M03_CANDIDATE" == "$E2E_ROOT/official-candidate" ]] || {
    echo 'refusing to clean an unexpected M03 fixture path' >&2
    return 1
  }
  if [[ -n "${M03_SESSION_CANDIDATE:-}" && "$M03_SESSION_CANDIDATE" != "$E2E_ROOT/computer-candidate" ]]; then
    echo 'refusing to clean an unexpected computer candidate path' >&2
    return 1
  fi
  rm -rf -- "$M03_CANDIDATE" "${M03_SESSION_CANDIDATE:-$E2E_ROOT/.no-computer-candidate}" "$M03_RUN_DIR" "$M03_OUTSIDE"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
  e2e_alloc m03fixture
  src_sch_sha=$(sha256sum "$M03_SRC/sensor.kicad_sch" | cut -d' ' -f1)
  src_pro_sha=$(sha256sum "$M03_SRC/sensor.kicad_pro" | cut -d' ' -f1)
  m03_fixture_setup
  [[ "$(m03_fixture_digest)" == "$src_sch_sha" ]] || { echo 'official SHA differs from source' >&2; exit 1; }
  [[ -s "$M03_SENTINEL" && -s "$M03_HOST_SECRET" && -n "${STABLE_API_KEY:-}" ]] || { echo 'sentinel or secret missing' >&2; exit 1; }
  e2e_run_cleanups
  [[ "$(sha256sum "$M03_SRC/sensor.kicad_sch" | cut -d' ' -f1)" == "$src_sch_sha" ]] || { echo 'source schematic changed by cleanup' >&2; exit 1; }
  [[ "$(sha256sum "$M03_SRC/sensor.kicad_pro" | cut -d' ' -f1)" == "$src_pro_sha" ]] || { echo 'source project changed by cleanup' >&2; exit 1; }
  [[ ! -e "$M03_CANDIDATE" && ! -e "$M03_SENTINEL" ]] || { echo 'cleanup left fixture copies behind' >&2; exit 1; }
  echo "M03 FIXTURE SELF-CHECK PASS src_sha=$src_sch_sha"
fi
