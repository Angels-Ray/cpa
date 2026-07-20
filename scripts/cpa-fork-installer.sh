#!/usr/bin/env bash
# Install / upgrade CLIProxyAPI from a GitHub fork release (default: Angels-Ray/cpa).
# Layout and asset naming follow the official cliproxyapi-installer.
#
# One-liner (after this file is on the default branch of your fork):
#   curl -fsSL https://raw.githubusercontent.com/Angels-Ray/cpa/main/scripts/cpa-fork-installer.sh | bash
#
# Env overrides:
#   REPO_OWNER=Angels-Ray
#   REPO_NAME=cpa
#   INSTALL_DIR=$HOME/cliproxyapi
#   ASSET_VARIANT=auto|default|no-plugin
#   RELEASE_TAG=          # empty = latest; or e.g. v2026.07.20
#   GH_TOKEN=             # optional, private repo / higher rate limit
#   SKIP_SYSTEMD=0
#   RESTART_SERVICE=1     # restart user systemd unit if it was running
#   WAIT_ASSETS=1
#   WAIT_TIMEOUT=900
#   PGSTORE_DSN=          # optional; only this is needed to enable Postgres (written to systemd unit, %%-escaped)

set -euo pipefail

REPO_OWNER="${REPO_OWNER:-Angels-Ray}"
REPO_NAME="${REPO_NAME:-cpa}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/cliproxyapi}"
ASSET_VARIANT="${ASSET_VARIANT:-auto}"
RELEASE_TAG="${RELEASE_TAG:-}"
SKIP_SYSTEMD="${SKIP_SYSTEMD:-0}"
RESTART_SERVICE="${RESTART_SERVICE:-1}"
WAIT_ASSETS="${WAIT_ASSETS:-1}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-900}"
# Optional Postgres store: only DSN is required. Spool defaults to WorkingDirectory/pgstore.
PGSTORE_DSN="${PGSTORE_DSN:-}"
SCRIPT_NAME="cpa-fork-installer.sh"

API_BASE="https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_ok() { echo -e "${GREEN}[OK]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_err() { echo -e "${RED}[ERR]${NC} $*" >&2; }
log_step() { echo -e "${CYAN}[STEP]${NC} $*"; }

usage() {
  cat <<EOF
${SCRIPT_NAME} — install/upgrade CLIProxyAPI from fork GitHub Releases

Usage:
  ${SCRIPT_NAME} [install|upgrade|status|uninstall|help]

Env:
  REPO_OWNER      default: Angels-Ray
  REPO_NAME       default: cpa
  INSTALL_DIR     default: \$HOME/cliproxyapi
  RELEASE_TAG     empty=latest, or v2026.07.20
  ASSET_VARIANT   auto|default|no-plugin (default: auto)
  GH_TOKEN        optional GitHub token
  SKIP_SYSTEMD    1 to skip user systemd unit
  WAIT_ASSETS     1 to wait while CI uploads assets (default: 1)
  WAIT_TIMEOUT    seconds (default: 900)
  PGSTORE_DSN     set this alone to enable Postgres (unit gets %%-escaped DSN)

Notes:
  - Only PGSTORE_DSN is needed; spool defaults to WorkingDirectory/pgstore (no LOCAL_PATH).
  - Pass a normal DSN (password may contain %26); installer escapes % for systemd.
  - Upgrade keeps existing unit PGSTORE_DSN unless you pass a new one.
  - Does not write .env.

Examples:
  ${SCRIPT_NAME}
  ${SCRIPT_NAME} status
  RELEASE_TAG=v2026.07.20 ${SCRIPT_NAME}
  PGSTORE_DSN='postgresql://u:p%26@host:5432/db?sslmode=require' ${SCRIPT_NAME}
  REPO_OWNER=Angels-Ray REPO_NAME=cpa ${SCRIPT_NAME} upgrade

Official-style one-liner (file must exist on that branch):
  curl -fsSL https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/main/scripts/cpa-fork-installer.sh | bash
EOF
}

http_get() {
  local url="$1"
  local auth_args=()
  if [[ -n "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]]; then
    auth_args=(-H "Authorization: Bearer ${GH_TOKEN:-$GITHUB_TOKEN}")
  fi
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${auth_args[@]}" -H "Accept: application/vnd.github+json" \
      -H "User-Agent: cpa-fork-installer" "$url"
  elif command -v wget >/dev/null 2>&1; then
    if [[ -n "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]]; then
      wget -qO- --header="Authorization: Bearer ${GH_TOKEN:-$GITHUB_TOKEN}" \
        --header="Accept: application/vnd.github+json" \
        --header="User-Agent: cpa-fork-installer" "$url"
    else
      wget -qO- --header="Accept: application/vnd.github+json" \
        --header="User-Agent: cpa-fork-installer" "$url"
    fi
  else
    log_err "need curl or wget"
    exit 1
  fi
}

download_file() {
  local url="$1"
  local out="$2"
  local auth_args=()
  if [[ -n "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]]; then
    auth_args=(-H "Authorization: Bearer ${GH_TOKEN:-$GITHUB_TOKEN}")
  fi
  log_info "downloading $(basename "$url")"
  if command -v curl >/dev/null 2>&1; then
    curl -fL "${auth_args[@]}" -o "$out" "$url"
  else
    if [[ -n "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]]; then
      wget -O "$out" --header="Authorization: Bearer ${GH_TOKEN:-$GITHUB_TOKEN}" "$url"
    else
      wget -O "$out" "$url"
    fi
  fi
  [[ -f "$out" ]] || { log_err "download failed"; exit 1; }
  log_ok "download completed"
}

check_deps() {
  local missing=()
  if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
    missing+=("curl|wget")
  fi
  command -v tar >/dev/null 2>&1 || missing+=("tar")
  if [[ ${#missing[@]} -gt 0 ]]; then
    log_err "missing tools: ${missing[*]}"
    exit 1
  fi
}

detect_linux_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "linux_amd64" ;;
    arm64|aarch64) echo "linux_aarch64" ;;
    *)
      log_err "unsupported arch: $(uname -m) (need x86_64 or aarch64)"
      exit 1
      ;;
  esac
}

is_openwrt_system() {
  [[ -f /etc/openwrt_release ]] && return 0
  [[ -f /etc/os-release ]] && grep -qi openwrt /etc/os-release && return 0
  return 1
}

is_musl_system() {
  if command -v ldd >/dev/null 2>&1 && ldd --version 2>&1 | grep -qi musl; then
    return 0
  fi
  [[ -f /etc/alpine-release ]] && return 0
  local f
  for f in /lib/ld-musl-*.so* /usr/lib/ld-musl-*.so*; do
    [[ -e "$f" ]] && return 0
  done
  return 1
}

detect_asset_variant() {
  case "$ASSET_VARIANT" in
    default|no-plugin) echo "$ASSET_VARIANT" ;;
    auto)
      if is_openwrt_system || is_musl_system; then
        echo "no-plugin"
      else
        echo "default"
      fi
      ;;
    *)
      log_err "ASSET_VARIANT must be auto|default|no-plugin"
      exit 1
      ;;
  esac
}

release_api_url() {
  if [[ -n "$RELEASE_TAG" ]]; then
    echo "${API_BASE}/releases/tags/${RELEASE_TAG}"
  else
    echo "${API_BASE}/releases/latest"
  fi
}

fetch_release_json() {
  local url
  url="$(release_api_url)"
  local body=""
  local deadline=$((SECONDS + WAIT_TIMEOUT))
  while true; do
    body="$(http_get "$url" 2>/dev/null || true)"
    if [[ -n "$body" ]] && echo "$body" | grep -q '"tag_name"'; then
      if [[ "$WAIT_ASSETS" != "1" ]] || echo "$body" | grep -q 'browser_download_url'; then
        echo "$body"
        return 0
      fi
      log_info "release exists but assets not uploaded yet (CI still running?)"
    else
      if [[ -n "$RELEASE_TAG" ]]; then
        log_info "release ${RELEASE_TAG} not found yet"
      else
        log_info "no latest release yet"
      fi
    fi
    if [[ "$WAIT_ASSETS" != "1" ]] || (( SECONDS >= deadline )); then
      break
    fi
    log_info "retry in 15s... ($((deadline - SECONDS))s left)"
    sleep 15
  done
  log_err "failed to fetch release JSON from ${url}"
  log_info "ensure Actions finished and a Release exists: https://github.com/${REPO_OWNER}/${REPO_NAME}/releases"
  exit 1
}

# stdout: version|download_url|filename  (version without leading v)
extract_asset() {
  local release_info="$1"
  local os_arch="$2"
  local variant="$3"

  local tag version
  tag="$(echo "$release_info" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4)"
  version="${tag#v}"
  if [[ -z "$version" ]]; then
    log_err "cannot parse tag_name"
    exit 1
  fi

  local expected="CLIProxyAPI_${version}_${os_arch}"
  if [[ "$variant" == "no-plugin" ]]; then
    expected="${expected}_no-plugin"
  fi
  expected="${expected}.tar.gz"

  local download_url=""
  download_url="$(
    echo "$release_info" |
      grep -o '"browser_download_url": *"[^"]*"' |
      cut -d'"' -f4 |
      while IFS= read -r u; do
        if [[ "$(basename "$u")" == "$expected" ]]; then
          echo "$u"
          break
        fi
      done
  )"

  if [[ -z "$download_url" && "$variant" == "default" ]]; then
    # fallback to no-plugin if default missing
    local fb="CLIProxyAPI_${version}_${os_arch}_no-plugin.tar.gz"
    download_url="$(
      echo "$release_info" |
        grep -o '"browser_download_url": *"[^"]*"' |
        cut -d'"' -f4 |
        while IFS= read -r u; do
          if [[ "$(basename "$u")" == "$fb" ]]; then
            echo "$u"
            break
          fi
        done
    )"
    if [[ -n "$download_url" ]]; then
      log_warn "default asset missing, fallback to ${fb}"
      expected="$fb"
      variant="no-plugin"
    fi
  fi

  if [[ -z "$download_url" ]]; then
    log_err "asset not found: ${expected}"
    log_info "available assets:"
    echo "$release_info" | grep -o '"name": *"[^"]*"' | cut -d'"' -f4 | sed 's/^/  - /' || true
    exit 1
  fi

  echo "${version}|${download_url}|${expected}|${variant}"
}

is_installed() { [[ -f "${INSTALL_DIR}/version.txt" ]]; }

get_current_version() {
  if is_installed; then
    cat "${INSTALL_DIR}/version.txt" 2>/dev/null || echo "unknown"
  else
    echo "none"
  fi
}

get_current_variant() {
  if [[ -f "${INSTALL_DIR}/asset-variant.txt" ]]; then
    cat "${INSTALL_DIR}/asset-variant.txt" 2>/dev/null || echo "unknown"
  else
    echo "unknown"
  fi
}

is_service_running() {
  systemctl --user is-active --quiet cliproxyapi.service 2>/dev/null
}

stop_service() {
  if is_service_running; then
    log_info "stopping systemd user service"
    systemctl --user stop cliproxyapi.service || true
  fi
}

restart_service() {
  log_info "restarting systemd user service"
  systemctl --user restart cliproxyapi.service || true
  sleep 2
  if is_service_running; then
    log_ok "service running"
  else
    log_warn "service may not be running; check: systemctl --user status cliproxyapi.service"
  fi
}

stop_loose_processes() {
  local pids
  pids="$(pgrep -f 'cli-proxy-api' 2>/dev/null || true)"
  if [[ -z "$pids" ]]; then
    return 0
  fi
  log_info "stopping loose cli-proxy-api processes"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  sleep 2
  pids="$(pgrep -f 'cli-proxy-api' 2>/dev/null || true)"
  if [[ -n "$pids" ]]; then
    # shellcheck disable=SC2086
    kill -9 $pids 2>/dev/null || true
  fi
}

# systemd Environment= treats % as a specifier; literal % must be written as %%.
# Output a full Environment="KEY=value" line (quoted).
systemd_environment_line() {
  local key="$1"
  local value="$2"
  local escaped
  escaped="${value//%/%%}"
  # Escape embedded double-quotes for unit file quoting.
  escaped="${escaped//\"/\\\"}"
  printf 'Environment="%s=%s"\n' "$key" "$escaped"
}

# Read KEY from an existing unit Environment= line (supports quoted/unquoted).
# Reverses systemd %% → % in the value.
read_unit_environment() {
  local unit_file="$1"
  local key="$2"
  [[ -f "$unit_file" ]] || return 0

  local line raw val
  while IFS= read -r line || [[ -n "$line" ]]; do
    case "$line" in
      Environment=*)
        raw="${line#Environment=}"
        # Strip surrounding quotes on the whole assignment if present.
        if [[ "$raw" == \"*\" ]]; then
          raw="${raw:1:${#raw}-2}"
        fi
        if [[ "$raw" == "${key}="* ]]; then
          val="${raw#"${key}="}"
          # Unescape systemd percent: %% → %
          val="${val//%%/%}"
          printf '%s' "$val"
          return 0
        fi
        ;;
    esac
  done <"$unit_file"
  return 0
}

create_systemd_service() {
  local install_dir="$1"
  local systemd_dir="$HOME/.config/systemd/user"
  local unit="${systemd_dir}/cliproxyapi.service"
  mkdir -p "$systemd_dir"

  # Only PGSTORE_DSN is managed. Installer env wins; else keep existing unit value.
  # App defaults spool to WorkingDirectory/pgstore — no PGSTORE_LOCAL_PATH needed.
  local dsn="${PGSTORE_DSN}"
  if [[ -z "$dsn" ]]; then
    dsn="$(read_unit_environment "$unit" "PGSTORE_DSN" || true)"
  fi

  {
    cat <<EOF
[Unit]
Description=CLIProxyAPI Service (fork install)
After=network.target

[Service]
Type=simple
WorkingDirectory=${install_dir}
ExecStart=${install_dir}/cli-proxy-api
Restart=always
RestartSec=10
EOF
    systemd_environment_line "HOME" "${HOME}"
    if [[ -n "$dsn" ]]; then
      systemd_environment_line "PGSTORE_DSN" "$dsn"
      log_info "systemd: PGSTORE_DSN set (percent chars escaped as %%)"
      log_info "spool defaults to ${install_dir}/pgstore (WorkingDirectory/pgstore)"
    fi
    cat <<'EOF'

[Install]
WantedBy=default.target
EOF
  } >"$unit"

  systemctl --user daemon-reload 2>/dev/null || log_warn "daemon-reload failed (ok if no systemd user session)"
  cp "$unit" "${install_dir}/cliproxyapi.service" 2>/dev/null || true
  log_ok "systemd unit: ${unit}"
  if [[ -n "$dsn" ]]; then
    log_info "Postgres store enabled in unit; after start look for: postgres-backed token store enabled"
  fi
}

backup_config() {
  local config="${INSTALL_DIR}/config.yaml"
  if [[ ! -f "$config" ]]; then
    echo ""
    return
  fi
  local backup_dir="${INSTALL_DIR}/config_backup"
  mkdir -p "$backup_dir"
  local backup_file="${backup_dir}/config_$(date +%Y%m%d_%H%M%S).yaml"
  cp "$config" "$backup_file"
  log_info "config backed up: ${backup_file}"
  echo "$backup_file"
}

setup_config_and_binary() {
  local version_dir="$1"
  local backup_file="${2:-}"

  local binary
  binary="$(find "$version_dir" \( -name 'cli-proxy-api' -o -name 'CLIProxyAPI' \) -type f | head -1)"
  if [[ -z "$binary" ]]; then
    log_err "binary not found in ${version_dir}"
    exit 1
  fi
  chmod +x "$binary"
  cp "$binary" "${INSTALL_DIR}/cli-proxy-api"
  chmod +x "${INSTALL_DIR}/cli-proxy-api"
  log_ok "binary → ${INSTALL_DIR}/cli-proxy-api"

  local config="${INSTALL_DIR}/config.yaml"
  if [[ -n "$backup_file" && -f "$backup_file" ]]; then
    cp "$backup_file" "$config"
    log_ok "restored config from backup"
    return
  fi
  if [[ -f "$config" ]]; then
    log_ok "kept existing config.yaml"
    return
  fi
  local example="${version_dir}/config.example.yaml"
  if [[ -f "$example" ]]; then
    cp "$example" "$config"
    log_ok "created config.yaml from example (edit api-keys before first run)"
  else
    log_warn "no config.example.yaml; create config.yaml yourself"
  fi
}

cleanup_old_versions() {
  local current="$1"
  [[ -d "$INSTALL_DIR" ]] || return 0
  # keep newest 2 version dirs (name contains dots)
  local old
  old="$(find "$INSTALL_DIR" -maxdepth 1 -mindepth 1 -type d -printf '%f\n' 2>/dev/null | grep -E '^[0-9]' | sort -V | head -n -2 || true)"
  [[ -z "$old" ]] && return 0
  while IFS= read -r v; do
    [[ -z "$v" || "$v" == "$current" ]] && continue
    rm -rf "${INSTALL_DIR}/${v}"
    log_info "removed old version dir: ${v}"
  done <<<"$old"
}

install_or_upgrade() {
  check_deps

  local os_arch variant
  os_arch="$(detect_linux_arch)"
  variant="$(detect_asset_variant)"
  log_step "repo=${REPO_OWNER}/${REPO_NAME}"
  log_step "platform=${os_arch} variant=${variant}"
  log_step "install_dir=${INSTALL_DIR}"
  if [[ -n "$RELEASE_TAG" ]]; then
    log_step "release_tag=${RELEASE_TAG}"
  else
    log_step "release_tag=latest"
  fi

  local current
  current="$(get_current_version)"
  local current_var
  current_var="$(get_current_variant)"
  local upgrading=0
  local service_was_running=0
  if [[ "$current" != "none" ]]; then
    upgrading=1
    log_info "installed: ${current} (${current_var})"
    if is_service_running; then
      service_was_running=1
    fi
  fi

  local release_info
  release_info="$(fetch_release_json)"

  local parsed version url filename final_variant
  parsed="$(extract_asset "$release_info" "$os_arch" "$variant")"
  version="$(echo "$parsed" | cut -d'|' -f1)"
  url="$(echo "$parsed" | cut -d'|' -f2)"
  filename="$(echo "$parsed" | cut -d'|' -f3)"
  final_variant="$(echo "$parsed" | cut -d'|' -f4)"

  log_step "target version=${version}"
  log_step "asset=${filename}"

  if [[ "$upgrading" -eq 1 && "$current" == "$version" && "$current_var" == "$final_variant" ]]; then
    log_ok "already up to date (${version}, ${final_variant})"
    return 0
  fi

  if [[ "$upgrading" -eq 1 ]]; then
    stop_service
    stop_loose_processes
  fi

  local backup_file=""
  if [[ "$upgrading" -eq 1 ]]; then
    backup_file="$(backup_config)"
  fi

  mkdir -p "$INSTALL_DIR"
  local version_dir="${INSTALL_DIR}/${version}"
  rm -rf "$version_dir"
  mkdir -p "$version_dir"

  local tmp
  tmp="$(mktemp)"
  # shellcheck disable=SC2064
  trap 'rm -f "'"$tmp"'"' RETURN
  download_file "$url" "$tmp"
  tar -xzf "$tmp" -C "$version_dir"
  rm -f "$tmp"
  trap - RETURN

  setup_config_and_binary "$version_dir" "$backup_file"
  echo "$version" >"${INSTALL_DIR}/version.txt"
  echo "$final_variant" >"${INSTALL_DIR}/asset-variant.txt"

  if [[ "$SKIP_SYSTEMD" != "1" ]]; then
    create_systemd_service "$INSTALL_DIR"
  fi

  cleanup_old_versions "$version"

  if [[ "$upgrading" -eq 1 && "$service_was_running" -eq 1 && "$RESTART_SERVICE" == "1" && "$SKIP_SYSTEMD" != "1" ]]; then
    restart_service
  fi

  if [[ "$upgrading" -eq 1 ]]; then
    log_ok "upgraded ${current} → ${version} (${final_variant})"
  else
    log_ok "installed ${version} (${final_variant})"
  fi
  log_info "bin: ${INSTALL_DIR}/cli-proxy-api"
  log_info "config: ${INSTALL_DIR}/config.yaml"
  log_info "start: cd ${INSTALL_DIR} && ./cli-proxy-api"
  if [[ "$SKIP_SYSTEMD" != "1" ]]; then
    log_info "or: systemctl --user enable --now cliproxyapi.service"
  fi
}

show_status() {
  echo "CLIProxyAPI fork install status"
  echo "repo: ${REPO_OWNER}/${REPO_NAME}"
  echo "install_dir: ${INSTALL_DIR}"
  echo "version: $(get_current_version)"
  echo "asset_variant: $(get_current_variant)"
  if [[ -f "${INSTALL_DIR}/cli-proxy-api" ]]; then
    echo "binary: present"
  else
    echo "binary: missing"
  fi
  if [[ -f "${INSTALL_DIR}/config.yaml" ]]; then
    echo "config: present"
  else
    echo "config: missing"
  fi
  if is_service_running; then
    echo "service: running"
  else
    echo "service: not running"
  fi

  local unit="$HOME/.config/systemd/user/cliproxyapi.service"
  local dsn_in_unit
  dsn_in_unit="$(read_unit_environment "$unit" "PGSTORE_DSN" || true)"
  if [[ -n "$dsn_in_unit" ]]; then
    echo "pgstore: enabled (PGSTORE_DSN in systemd unit)"
    echo "pgstore_spool: ${INSTALL_DIR}/pgstore (default WorkingDirectory/pgstore)"
  else
    echo "pgstore: not configured (file store mode)"
  fi
  if [[ -d "${INSTALL_DIR}/pgstore" ]]; then
    echo "pgstore_dir: present"
  fi
}

uninstall_all() {
  if [[ ! -d "$INSTALL_DIR" ]]; then
    log_warn "not installed: ${INSTALL_DIR}"
    exit 0
  fi
  if is_service_running; then
    stop_service
  fi
  stop_loose_processes
  if [[ -f "$HOME/.config/systemd/user/cliproxyapi.service" ]]; then
    systemctl --user disable cliproxyapi.service 2>/dev/null || true
    rm -f "$HOME/.config/systemd/user/cliproxyapi.service"
    systemctl --user daemon-reload 2>/dev/null || true
  fi
  log_warn "removing ${INSTALL_DIR}"
  rm -rf "$INSTALL_DIR"
  log_ok "uninstalled"
}

main() {
  case "${1:-install}" in
    install|upgrade|"")
      install_or_upgrade
      ;;
    status)
      show_status
      ;;
    uninstall)
      uninstall_all
      ;;
    -h|--help|help)
      usage
      ;;
    *)
      log_err "unknown command: $1"
      usage
      exit 1
      ;;
  esac
}

main "$@"
