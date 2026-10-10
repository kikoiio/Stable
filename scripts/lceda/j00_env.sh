#!/usr/bin/env bash
# Prepare the isolated environment used by the J00 compatibility probe.
#
# The default mode only records the runner and creates private directories. A
# client archive is downloaded only when J00_DOWNLOAD_CLIENT is explicitly
# enabled and both a URL and a pinned SHA-256 are available.

set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd -- "${SCRIPT_DIR}/../.." && pwd -P)
MANIFEST_PATH=${J00_MANIFEST_PATH:-"${REPO_ROOT}/fixtures/lceda/j00/manifest.json"}
MODE=${1:-prepare}

bool_enabled() {
  case "${1:-}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

die() {
  printf 'j00_env: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

validate_run_id() {
  [[ "${J00_RUN_ID}" =~ ^[A-Za-z0-9_.-]+$ ]] || die "J00_RUN_ID contains unsafe characters"
}

path_is_within() {
  local child parent
  child=$(realpath -m -- "$1")
  parent=$(realpath -m -- "$2")
  [[ "${child}" == "${parent}" || "${child}" == "${parent}"/* ]]
}

validate_project_path() {
  local path=$1 label=$2
  path_is_within "${path}" "${REPO_ROOT}" || die "${label} must stay under the repository: ${path}"
  [[ "$(realpath -m -- "${path}")" != /tmp/* ]] || die "${label} must not use /tmp"
}

J00_RUN_ID=${J00_RUN_ID:-"$(date -u +%Y%m%dT%H%M%SZ)-$$"}
validate_run_id

J00_TMP_ROOT=${J00_TMP_ROOT:-"${REPO_ROOT}/.tmp/lceda-j00"}
J00_RUN_DIR=${J00_RUN_DIR:-"${J00_TMP_ROOT}/${J00_RUN_ID}"}
J00_ARTIFACT_DIR=${J00_ARTIFACT_DIR:-"${J00_RUN_DIR}/artifacts"}
J00_WORK_DIR=${J00_WORK_DIR:-"${J00_RUN_DIR}/work"}
J00_RUNTIME_DIR=${J00_RUNTIME_DIR:-"${J00_RUN_DIR}/runtime"}
J00_HOME=${J00_HOME:-"${J00_RUN_DIR}/home"}
J00_PROFILE_DIR=${J00_PROFILE_DIR:-"${J00_RUN_DIR}/profile"}
J00_XDG_CONFIG_HOME=${J00_XDG_CONFIG_HOME:-"${J00_RUN_DIR}/xdg-config"}
J00_XDG_CACHE_HOME=${J00_XDG_CACHE_HOME:-"${J00_RUN_DIR}/xdg-cache"}
J00_XDG_DATA_HOME=${J00_XDG_DATA_HOME:-"${J00_RUN_DIR}/xdg-data"}
J00_ENV_FILE=${J00_ENV_FILE:-"${J00_RUN_DIR}/env.sh"}
J00_PID_DIR=${J00_PID_DIR:-"${J00_RUNTIME_DIR}/pids"}
J00_RESOURCE_FILE=${J00_RESOURCE_FILE:-"${J00_ARTIFACT_DIR}/environment.txt"}
J00_CLIENT_INSTALL_DIR=${J00_CLIENT_INSTALL_DIR:-"${J00_RUN_DIR}/client"}
J00_CLIENT_ARCHIVE=${J00_CLIENT_ARCHIVE:-"${J00_RUN_DIR}/client.download"}
J00_DOWNLOAD_CLIENT=${J00_DOWNLOAD_CLIENT:-0}
J00_START_XVFB=${J00_START_XVFB:-0}
J00_DISPLAY=${J00_DISPLAY:-}
export WAYLAND_DISPLAY=
export LIBGL_ALWAYS_SOFTWARE=1

for project_path in \
  "${J00_TMP_ROOT}" "${J00_RUN_DIR}" "${J00_ARTIFACT_DIR}" "${J00_WORK_DIR}" \
  "${J00_RUNTIME_DIR}" "${J00_HOME}" "${J00_PROFILE_DIR}" "${J00_XDG_CONFIG_HOME}" \
  "${J00_XDG_CACHE_HOME}" "${J00_XDG_DATA_HOME}" "${J00_ENV_FILE}" \
  "${J00_CLIENT_INSTALL_DIR}" "${J00_CLIENT_ARCHIVE}"; do
  validate_project_path "${project_path}" "J00 path"
done

cleanup_xvfb_pid() {
  local pid cmdline deadline
  [[ -s "${J00_PID_DIR}/xvfb.pid" ]] || return 0
  pid=$(<"${J00_PID_DIR}/xvfb.pid")
  [[ "${pid}" =~ ^[0-9]+$ ]] || { rm -f -- "${J00_PID_DIR}/xvfb.pid"; return 0; }
  if kill -0 "${pid}" 2>/dev/null; then
    cmdline=''
    [[ -r "/proc/${pid}/cmdline" ]] && cmdline=$(tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null || true)
    if [[ "${cmdline}" == *Xvfb* ]]; then
      kill -TERM "${pid}" 2>/dev/null || true
      deadline=$((SECONDS + 10))
      while kill -0 "${pid}" 2>/dev/null && (( SECONDS < deadline )); do sleep 1; done
      kill -KILL "${pid}" 2>/dev/null || true
    else
      printf 'j00_env: refusing to stop PID %s; command is not Xvfb\n' "${pid}" >&2
    fi
  fi
  rm -f -- "${J00_PID_DIR}/xvfb.pid"
}

cleanup_started_processes() {
  cleanup_xvfb_pid
}

cleanup_transient_state() {
  rm -f -- "${J00_CLIENT_ARCHIVE}.part"
  rm -rf -- "${J00_WORK_DIR}" "${J00_PROFILE_DIR}" "${J00_HOME}" \
    "${J00_XDG_CONFIG_HOME}" "${J00_XDG_CACHE_HOME}" "${J00_XDG_DATA_HOME}" \
    "${J00_CLIENT_INSTALL_DIR}" "${J00_RUNTIME_DIR}"
}

on_signal() {
  trap - INT TERM HUP
  cleanup_started_processes
  exit 143
}

on_exit() {
  local status=$?
  # A failed or cancelled preparation must not leave a display server behind.
  if (( status != 0 )); then
    cleanup_started_processes
    cleanup_transient_state
  fi
  return "${status}"
}

trap on_signal INT TERM HUP
trap on_exit EXIT

record_resources() {
  local arch kernel available cgroup_memory memory_psi vmstat_sample
  arch=$(uname -m 2>/dev/null || printf 'unknown')
  kernel=$(uname -sr 2>/dev/null || printf 'unknown')
  available='unknown'
  if command -v free >/dev/null 2>&1; then
    available=$(free -b 2>/dev/null | awk '/^Mem:/ {print $7; exit}' || printf 'unknown')
  fi
  cgroup_memory='unavailable'
  if [[ -r /sys/fs/cgroup/memory.max ]]; then cgroup_memory=$(< /sys/fs/cgroup/memory.max); fi
  memory_psi='unavailable'
  if [[ -r /proc/pressure/memory ]]; then memory_psi=$(tr '\n' ';' < /proc/pressure/memory); fi
  vmstat_sample='unavailable'
  if command -v vmstat >/dev/null 2>&1; then
    vmstat_sample=$(vmstat 1 1 2>/dev/null | tail -n 1 | sed 's/[[:space:]]\+/ /g' || printf 'unavailable')
  fi
  {
    printf 'run_id=%s\n' "${J00_RUN_ID}"
    printf 'repository=%s\n' "${REPO_ROOT}"
    printf 'runner_os=%s\n' "$(uname -s 2>/dev/null || printf unknown)"
    printf 'runner_arch=%s\n' "${arch}"
    printf 'kernel=%s\n' "${kernel}"
    printf 'available_memory_bytes=%s\n' "${available}"
    printf 'cgroup_memory_max=%s\n' "${cgroup_memory}"
    printf 'memory_psi=%s\n' "${memory_psi}"
    printf 'vmstat_sample=%s\n' "${vmstat_sample}"
    printf 'disk=%s\n' "$(df -Pk -- "${REPO_ROOT}" | tail -n 1 | sed 's/[[:space:]]\+/ /g')"
    printf 'tmp_root=%s\n' "${J00_TMP_ROOT}"
    printf 'run_dir=%s\n' "${J00_RUN_DIR}"
    printf 'client_download_enabled=%s\n' "${J00_DOWNLOAD_CLIENT}"
  } >"${J00_RESOURCE_FILE}"
}

manifest_value() {
  local key=$1
  [[ -f "${MANIFEST_PATH}" ]] || return 0
  require_command python3
  python3 - "${MANIFEST_PATH}" "${key}" <<'PY'
import json
import sys

path, requested = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as handle:
        value = json.load(handle)
except (OSError, ValueError):
    raise SystemExit(0)

for part in requested.split("."):
    if not isinstance(value, dict) or part not in value:
        raise SystemExit(0)
    value = value[part]
if isinstance(value, (str, int, float)) and not isinstance(value, bool):
    print(value)
PY
}

resolve_client_metadata() {
  local manifest_url manifest_sha
  if [[ -z "${J00_CLIENT_URL:-}" ]]; then
    for key in client.download_url client.source_url client.url client.source.url client.artifact.url source.url download_url source_url; do
      manifest_url=$(manifest_value "${key}")
      [[ -n "${manifest_url}" ]] && { J00_CLIENT_URL=${manifest_url}; break; }
    done
  fi
  if [[ -z "${J00_CLIENT_SHA256:-}" ]]; then
    for key in client.sha256 client.sha256sum client.installer_sha256 client.source.sha256 client.artifact.sha256 sha256 sha256sum; do
      manifest_sha=$(manifest_value "${key}")
      [[ -n "${manifest_sha}" ]] && { J00_CLIENT_SHA256=${manifest_sha}; break; }
    done
  fi
  J00_CLIENT_URL=${J00_CLIENT_URL:-}
  J00_CLIENT_SHA256=${J00_CLIENT_SHA256:-}
}

install_archive() {
  local archive=$1 target=${J00_CLIENT_INSTALL_DIR} source_name
  source_name=${J00_CLIENT_URL%%\?*}
  rm -rf -- "${target}.staging"
  mkdir -p -- "${target}.staging"
  case "${source_name}" in
    *.tar.gz|*.tgz) tar -xzf "${archive}" -C "${target}.staging" ;;
    *.tar.xz) tar -xJf "${archive}" -C "${target}.staging" ;;
    *.tar.bz2) tar -xjf "${archive}" -C "${target}.staging" ;;
    *.zip) require_command unzip; unzip -q "${archive}" -d "${target}.staging" ;;
    *.deb) require_command dpkg-deb; dpkg-deb -x "${archive}" "${target}.staging" ;;
    *.AppImage|*.appimage)
      install -m 0700 "${archive}" "${target}.staging/client.AppImage"
      ;;
    *) die "unsupported client archive format: ${archive}" ;;
  esac
  rm -rf -- "${target}"
  mv -- "${target}.staging" "${target}"
}

download_and_verify_client() {
  resolve_client_metadata
  [[ -n "${J00_CLIENT_URL}" ]] || die "client URL is not configured in the manifest"
  [[ "${J00_CLIENT_URL}" == https://* ]] || die "client URL must use HTTPS"
  [[ "${J00_CLIENT_SHA256}" =~ ^[A-Fa-f0-9]{64}$ ]] || die "client SHA-256 must be a 64-character hex digest"
  require_command sha256sum
  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --retry 2 --connect-timeout 20 --max-time 600 \
      --output "${J00_CLIENT_ARCHIVE}.part" "${J00_CLIENT_URL}"
  elif command -v wget >/dev/null 2>&1; then
    wget --https-only --timeout=30 --tries=3 -O "${J00_CLIENT_ARCHIVE}.part" "${J00_CLIENT_URL}"
  else
    die 'neither curl nor wget is available for client download'
  fi
  [[ -s "${J00_CLIENT_ARCHIVE}.part" ]] || die 'downloaded client archive is empty'
  printf '%s  %s\n' "${J00_CLIENT_SHA256}" "${J00_CLIENT_ARCHIVE}.part" | sha256sum --check --status - \
    || die 'client archive SHA-256 does not match the pinned digest'
  mv -- "${J00_CLIENT_ARCHIVE}.part" "${J00_CLIENT_ARCHIVE}"
  install_archive "${J00_CLIENT_ARCHIVE}"
  printf 'client_archive_sha256=%s\nclient_install_dir=%s\n' "${J00_CLIENT_SHA256}" "${J00_CLIENT_INSTALL_DIR}" >>"${J00_RESOURCE_FILE}"
}

start_xvfb() {
  require_command Xvfb
  mkdir -p -- "${J00_PID_DIR}"
  if [[ -z "${J00_DISPLAY}" ]]; then
    local display_number=$((90 + (RANDOM % 80)))
    while [[ -e "/tmp/.X11-unix/X${display_number}" ]]; do
      display_number=$((90 + (RANDOM % 80)))
    done
    J00_DISPLAY=":${display_number}"
  fi
  [[ "${J00_DISPLAY}" =~ ^:[0-9]+$ ]] || die "J00_DISPLAY must look like :99"
  Xvfb "${J00_DISPLAY}" -screen 0 1280x1024x24 -nolisten tcp \
    >"${J00_ARTIFACT_DIR}/xvfb.log" 2>&1 &
  local pid=$!
  printf '%s\n' "${pid}" >"${J00_PID_DIR}/xvfb.pid"
  sleep 1
  kill -0 "${pid}" 2>/dev/null || die 'Xvfb exited during startup'
}

write_env_file() {
  local quoted
  mkdir -p -- "$(dirname -- "${J00_ENV_FILE}")"
  : >"${J00_ENV_FILE}"
  chmod 0600 "${J00_ENV_FILE}"
  {
    printf '# Generated by scripts/lceda/j00_env.sh; source only in this job.\n'
    for name in J00_RUN_ID J00_RUN_DIR J00_ARTIFACT_DIR J00_WORK_DIR J00_RUNTIME_DIR \
      J00_HOME J00_PROFILE_DIR J00_XDG_CONFIG_HOME J00_XDG_CACHE_HOME J00_XDG_DATA_HOME \
      J00_ENV_FILE J00_PID_DIR J00_CLIENT_INSTALL_DIR J00_CLIENT_ARCHIVE; do
      printf -v quoted '%q' "${!name}"
      printf 'export %s=%s\n' "${name}" "${quoted}"
    done
    printf 'export DISPLAY=%q\n' "${J00_DISPLAY:-}"
    printf 'export WAYLAND_DISPLAY=\n'
    printf 'export LIBGL_ALWAYS_SOFTWARE=1\n'
    printf 'export J00_CLIENT_FLAGS=%q\n' '--disable-gpu --ozone-platform=x11'
  } >>"${J00_ENV_FILE}"
}

prepare() {
  require_command bash
  require_command uname
  require_command realpath
  require_command mkdir
  require_command rm
  require_command df
  require_command sed
  require_command awk
  require_command date
  [[ "$(uname -s)" == Linux ]] || die 'J00 requires a Linux runner'
  case "$(uname -m)" in
    x86_64|amd64) ;;
    *) die "J00 requires an x86_64 runner (found $(uname -m))" ;;
  esac
  validate_project_path "${J00_TMP_ROOT}" 'J00_TMP_ROOT'
  mkdir -p -- "${J00_ARTIFACT_DIR}" "${J00_WORK_DIR}" "${J00_RUNTIME_DIR}" \
    "${J00_HOME}" "${J00_PROFILE_DIR}" "${J00_XDG_CONFIG_HOME}" "${J00_XDG_CACHE_HOME}" \
    "${J00_XDG_DATA_HOME}" "${J00_PID_DIR}"
  record_resources
  if bool_enabled "${J00_DOWNLOAD_CLIENT}"; then download_and_verify_client; fi
  if bool_enabled "${J00_START_XVFB}"; then start_xvfb; fi
  write_env_file
  printf 'j00_env: prepared %s\n' "${J00_RUN_DIR}"
  printf 'j00_env: environment file %s\n' "${J00_ENV_FILE}"
}

cleanup() {
  cleanup_started_processes
  # Profiles and work files may contain client state; preserve only evidence.
  cleanup_transient_state
  printf 'j00_env: cleaned transient state; evidence remains in %s\n' "${J00_ARTIFACT_DIR}"
}

sanitize_artifacts() {
  require_command python3
  local redacted_dir source relative destination
  redacted_dir="${J00_RUN_DIR}/redacted-artifacts"
  mkdir -p -- "${redacted_dir}"
  [[ -d "${J00_ARTIFACT_DIR}" ]] || return 0
  while IFS= read -r -d '' source; do
    [[ -L "${source}" ]] && continue
    relative=${source#"${J00_ARTIFACT_DIR}/"}
    destination="${redacted_dir}/${relative}"
    mkdir -p -- "$(dirname -- "${destination}")"
    if LC_ALL=C grep -Iq . -- "${source}" 2>/dev/null; then
      python3 - "${source}" "${destination}" "${REPO_ROOT}" "${HOME:-}" <<'PY'
import re
import sys
from pathlib import Path

source, destination, workspace, home = sys.argv[1:]
text = Path(source).read_text(encoding="utf-8", errors="replace")
for marker in (workspace, home):
    if marker:
        text = text.replace(marker, "<redacted-path>")
text = re.sub(
    r"(?i)\b(token|password|secret|authorization|api[_-]?key)\b(\s*[:=]\s*)(\"[^\"]*\"|'[^']*'|[^,\s}\]]+)",
    r"\1\2<redacted>",
    text,
)
text = re.sub(r"(?i)([?&](?:token|key|secret|password|authorization)=)[^&\s]+", r"\1<redacted>", text)
text = re.sub(r"(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+", r"\1<redacted>", text)
Path(destination).write_text(text, encoding="utf-8")
PY
    else
      cp -- "${source}" "${destination}"
    fi
  done < <(find "${J00_ARTIFACT_DIR}" -type f -print0)
  printf 'j00_env: sanitized evidence in %s\n' "${redacted_dir}"
}

case "${MODE}" in
  prepare) prepare ;;
  cleanup) cleanup ;;
  sanitize) sanitize_artifacts ;;
  *)
    cat >&2 <<'USAGE'
usage: j00_env.sh [prepare|cleanup|sanitize]

prepare creates an isolated project-local J00 run directory. Set
J00_DOWNLOAD_CLIENT=1 to explicitly download a HTTPS client archive whose
SHA-256 is pinned in the manifest (or J00_CLIENT_SHA256). Set
J00_START_XVFB=1 to start a private Xvfb process.
USAGE
    exit 2
    ;;
esac
