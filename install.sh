#!/usr/bin/env bash
#
# 3x-ui-exporter installation and management script
# https://github.com/nike38rus/3x-ui-exporter
#
set -euo pipefail

GITHUB_REPO="nike38rus/3x-ui-exporter"
BIN_DIR="/usr/local/bin"
BIN_NAME="3x-ui-exporter"
BIN_PATH="${BIN_DIR}/${BIN_NAME}"
CONFIG_PATH="/etc/3x-ui-exporter.env"
SYSTEMD_PATH="/etc/systemd/system/3x-ui-exporter.service"

# Terminal colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

info() {
  printf "${BLUE}[INFO]${NC} %s\n" "$*"
}

success() {
  printf "${GREEN}[SUCCESS]${NC} %s\n" "$*"
}

warn() {
  printf "${YELLOW}[WARN]${NC} %s\n" "$*"
}

error() {
  printf "${RED}[ERROR]${NC} %s\n" "$*" >&2
}

abort() {
  error "$*"
  exit 1
}

check_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    abort "This script must be run as root (or with sudo)."
  fi
}

check_systemd() {
  if ! command -v systemctl >/dev/null 2>&1; then
    abort "systemd is required but systemctl was not found."
  fi
}

detect_arch() {
  local os arch
  os="$(uname -s)"
  if [[ "$os" != "Linux" ]]; then
    abort "Unsupported OS: $os. 3x-ui-exporter runs on Linux."
  fi

  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64)
      echo "amd64"
      ;;
    aarch64|arm64)
      echo "arm64"
      ;;
    *)
      abort "Unsupported CPU architecture: $arch. Supported: amd64, arm64."
      ;;
  esac
}

get_latest_version() {
  local tag=""
  # Try GitHub redirect (does not consume API rate limit)
  tag="$(curl -fsSLI -o /dev/null -w "%{url_effective}" "https://github.com/${GITHUB_REPO}/releases/latest" 2>/dev/null | awk -F'/' '{print $NF}')" || true
  if [[ -z "$tag" || "$tag" == "latest" ]]; then
    # Fallback to GitHub API
    tag="$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" 2>/dev/null | grep '"tag_name":' | head -n1 | cut -d '"' -f 4)" || true
  fi
  if [[ ! "$tag" =~ ^v[0-9]+ ]]; then
    echo "v1.0.0"
  else
    echo "$tag"
  fi
}

download_and_install_binary() {
  local target_ver="$1"
  local arch="$2"
  local archive_name="3x-ui-exporter_${target_ver}_linux_${arch}.tar.gz"
  local download_url="https://github.com/${GITHUB_REPO}/releases/download/${target_ver}/${archive_name}"

  info "Downloading ${archive_name} (${target_ver})..."
  local tmp_dir
  tmp_dir="$(mktemp -d)"

  if ! curl -fsSL "$download_url" -o "${tmp_dir}/${archive_name}"; then
    rm -rf "$tmp_dir"
    abort "Failed to download release archive from ${download_url}."
  fi

  info "Extracting binary..."
  if ! tar -xzf "${tmp_dir}/${archive_name}" -C "$tmp_dir"; then
    rm -rf "$tmp_dir"
    abort "Failed to extract release archive."
  fi

  local bin_source="${tmp_dir}/3x-ui-exporter"
  if [[ ! -f "$bin_source" ]]; then
    local found_bin
    found_bin="$(find "$tmp_dir" -type f -name "3x-ui-exporter" | head -n 1)"
    if [[ -n "$found_bin" ]]; then
      bin_source="$found_bin"
    else
      rm -rf "$tmp_dir"
      abort "Could not find 3x-ui-exporter binary inside release archive."
    fi
  fi

  info "Installing binary to ${BIN_PATH}..."
  mkdir -p "$BIN_DIR"
  install -m 755 "$bin_source" "$BIN_PATH"
  rm -rf "$tmp_dir"

  success "Binary installed: $(${BIN_PATH} -version 2>/dev/null || echo "$target_ver")"
}

create_or_update_systemd() {
  info "Configuring systemd service at ${SYSTEMD_PATH}..."
  cat <<'EOF' > "$SYSTEMD_PATH"
[Unit]
Description=3x-ui Prometheus Exporter
Documentation=https://github.com/nike38rus/3x-ui-exporter
After=network.target x-ui.service
Wants=x-ui.service

[Service]
Type=simple
User=nobody
EnvironmentFile=/etc/3x-ui-exporter.env
ExecStart=/usr/local/bin/3x-ui-exporter
Restart=on-failure
RestartSec=5s

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

  chmod 644 "$SYSTEMD_PATH"
  systemctl daemon-reload
}

read_input() {
  local prompt="$1"
  local -n out_var="$2"
  local val=""
  if [[ -t 0 ]]; then
    read -rp "$prompt" val
    out_var="$val"
    return 0
  elif [[ -r /dev/tty ]]; then
    read -rp "$prompt" val </dev/tty
    out_var="$val"
    return 0
  fi
  return 1
}

prompt_config() {
  local in_token="${1:-${XUI_API_TOKEN:-}}"
  local in_url="${2:-${XUI_URL:-}}"
  local in_listen="${3:-${LISTEN_ADDRESS:-}}"
  local in_xray_api="${4:-${XRAY_API:-}}"

  local token="$in_token"
  local url="$in_url"
  local listen="$in_listen"
  local xray_api="$in_xray_api"

  if [[ -f "$CONFIG_PATH" ]]; then
    info "Existing configuration found at ${CONFIG_PATH}. Keeping existing settings."
    return 0
  fi

  info "No existing configuration found. Setting up ${CONFIG_PATH}..."

  if [[ -z "$token" ]]; then
    local input_token=""
    if read_input "3x-ui API Token (XUI_API_TOKEN, requires Administrator scope) [required]: " input_token; then
      token="$input_token"
      while [[ -z "$token" ]]; do
        warn "XUI_API_TOKEN is required to scrape panel configuration."
        read_input "3x-ui API Token (XUI_API_TOKEN, Administrator scope): " token || break
      done
    else
      warn "Running non-interactively without XUI_API_TOKEN set."
      warn "Please edit ${CONFIG_PATH} and set XUI_API_TOKEN (Administrator scope) before starting the service."
      token="CHANGE_ME_TOKEN"
    fi
  fi

  if [[ -z "$url" ]]; then
    local input_url=""
    if read_input "3x-ui URL [default: http://127.0.0.1:2053]: " input_url; then
      url="$input_url"
    fi
    url="${url:-http://127.0.0.1:2053}"
  fi

  if [[ -z "$listen" ]]; then
    local input_listen=""
    if read_input "Listen address for Prometheus [default: :9108]: " input_listen; then
      listen="$input_listen"
    fi
    listen="${listen:-:9108}"
  fi

  if [[ -z "$xray_api" ]]; then
    xray_api="127.0.0.1:62789"
  fi

  cat <<EOF > "$CONFIG_PATH"
# 3x-ui-exporter environment configuration
# Generated on $(date -u +"%Y-%m-%dT%H:%M:%SZ")

# 3x-ui Panel connection (token must have Administrator scope)
XUI_URL=${url}
XUI_API_TOKEN=${token}

# Xray core connection
XRAY_API=${xray_api}
# XRAY_METRICS_URL= # leave empty for auto-discovery via panel API

# Exporter HTTP server
LISTEN_ADDRESS=${listen}
SCRAPE_TIMEOUT=4s

# Logging and metrics
LOG_LEVEL=info
LOG_FORMAT=text
COLLECT_CLIENT_STATS=false
EOF

  chmod 600 "$CONFIG_PATH"
  success "Configuration saved to ${CONFIG_PATH} (permissions 0600)."
}

do_install() {
  local target_ver="$1"
  local token="$2"
  local url="$3"
  local listen="$4"
  local xray_api="$5"

  local arch
  arch="$(detect_arch)"

  if [[ -z "$target_ver" ]]; then
    info "Resolving latest release version..."
    target_ver="$(get_latest_version)"
  fi

  info "Installing 3x-ui-exporter ${target_ver} (${arch})..."

  prompt_config "$token" "$url" "$listen" "$xray_api"
  download_and_install_binary "$target_ver" "$arch"
  create_or_update_systemd

  info "Enabling and starting 3x-ui-exporter service..."
  systemctl enable 3x-ui-exporter
  systemctl restart 3x-ui-exporter

  sleep 1
  if systemctl is-active --quiet 3x-ui-exporter; then
    success "3x-ui-exporter is active and running!"
    info "Service status: systemctl status 3x-ui-exporter"
    info "Logs:           journalctl -u 3x-ui-exporter -f"
    info "Metrics:        curl -s http://localhost${listen:-:9108}/metrics"
  else
    warn "3x-ui-exporter service is not active yet. Check logs with:"
    warn "  journalctl -u 3x-ui-exporter -n 25 --no-pager"
  fi
}

do_update() {
  local target_ver="$1"
  local arch
  arch="$(detect_arch)"

  if [[ ! -f "$BIN_PATH" ]]; then
    warn "3x-ui-exporter is not currently installed. Proceeding with full installation..."
    do_install "$target_ver" "" "" "" ""
    return
  fi

  local current_ver
  current_ver="$("${BIN_PATH}" -version 2>/dev/null || echo "unknown")"
  info "Current version: ${current_ver}"

  if [[ -z "$target_ver" ]]; then
    info "Resolving latest release version..."
    target_ver="$(get_latest_version)"
  fi

  if [[ "$current_ver" == "$target_ver" ]]; then
    info "Already on latest version (${current_ver}). Re-installing to ensure binary integrity..."
  else
    info "Updating from ${current_ver} -> ${target_ver}..."
  fi

  download_and_install_binary "$target_ver" "$arch"
  create_or_update_systemd

  info "Restarting 3x-ui-exporter service..."
  systemctl restart 3x-ui-exporter
  sleep 1

  if systemctl is-active --quiet 3x-ui-exporter; then
    success "Update completed successfully! Service is active."
  else
    error "Service failed to restart cleanly after update. Check logs:"
    journalctl -u 3x-ui-exporter -n 25 --no-pager
  fi
}

do_uninstall() {
  info "Uninstalling 3x-ui-exporter..."

  if systemctl is-active --quiet 3x-ui-exporter 2>/dev/null; then
    info "Stopping service..."
    systemctl stop 3x-ui-exporter || true
  fi

  if systemctl is-enabled --quiet 3x-ui-exporter 2>/dev/null; then
    info "Disabling service..."
    systemctl disable 3x-ui-exporter || true
  fi

  if [[ -f "$SYSTEMD_PATH" ]]; then
    info "Removing ${SYSTEMD_PATH}..."
    rm -f "$SYSTEMD_PATH"
    systemctl daemon-reload
  fi

  if [[ -f "$BIN_PATH" ]]; then
    info "Removing ${BIN_PATH}..."
    rm -f "$BIN_PATH"
  fi

  if [[ -f "$CONFIG_PATH" ]]; then
    if [[ -t 0 ]]; then
      read -rp "Remove configuration file ${CONFIG_PATH}? [y/N]: " del_cfg
      case "$del_cfg" in
        [yY]|[yY][eE][sS])
          rm -f "$CONFIG_PATH"
          info "Configuration removed."
          ;;
        *)
          info "Configuration preserved at ${CONFIG_PATH}."
          ;;
      esac
    else
      info "Configuration preserved at ${CONFIG_PATH}."
    fi
  fi

  success "3x-ui-exporter has been uninstalled."
}

do_status() {
  if [[ -f "$BIN_PATH" ]]; then
    info "Binary path: ${BIN_PATH} ($(${BIN_PATH} -version 2>/dev/null || echo "version unknown"))"
  else
    warn "Binary not found at ${BIN_PATH}"
  fi

  if [[ -f "$CONFIG_PATH" ]]; then
    info "Config path: ${CONFIG_PATH}"
  else
    warn "Config not found at ${CONFIG_PATH}"
  fi

  systemctl status 3x-ui-exporter --no-pager || true
}

show_help() {
  cat <<EOF
Usage: $0 [command] [options]

Commands:
  install      Install 3x-ui-exporter as a systemd service (default)
  update       Update the exporter binary to the latest version (preserves config)
  uninstall    Remove the exporter binary and systemd service
  status       Show current service status and version
  restart      Restart the systemd service

Options:
  -v, --version <tag>      Target release version (e.g. v1.0.0, default: latest)
  -t, --token <token>      3x-ui API Bearer Token (requires Administrator scope)
  -u, --url <url>          3x-ui Panel URL (default: http://127.0.0.1:2053)
  -l, --listen <addr>      Metrics listen address (default: :9108)
  -h, --help               Show this help message

Environment Variables:
  VERSION, XUI_API_TOKEN, XUI_URL, LISTEN_ADDRESS, XRAY_API

Examples:
  # Interactive installation:
  sudo bash install.sh

  # One-line automated install:
  curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | \
    sudo XUI_API_TOKEN="your_token" bash

  # Update to latest version:
  sudo bash install.sh update
EOF
}

main() {
  local cmd="install"
  local target_ver="${VERSION:-}"
  local token="${XUI_API_TOKEN:-}"
  local url="${XUI_URL:-}"
  local listen="${LISTEN_ADDRESS:-}"
  local xray_api="${XRAY_API:-}"

  while [[ $# -gt 0 ]]; do
    case "$1" in
      install|update|uninstall|status|restart)
        cmd="$1"
        shift
        ;;
      -v|--version)
        target_ver="$2"
        shift 2
        ;;
      -t|--token)
        token="$2"
        shift 2
        ;;
      -u|--url)
        url="$2"
        shift 2
        ;;
      -l|--listen)
        listen="$2"
        shift 2
        ;;
      -h|--help)
        show_help
        exit 0
        ;;
      *)
        error "Unknown argument: $1"
        show_help
        exit 1
        ;;
    esac
  done

  check_root
  check_systemd

  case "$cmd" in
    install)
      if [[ -f "$BIN_PATH" && -f "$CONFIG_PATH" ]]; then
        info "3x-ui-exporter is already installed. Running update..."
        do_update "$target_ver"
      else
        do_install "$target_ver" "$token" "$url" "$listen" "$xray_api"
      fi
      ;;
    update)
      do_update "$target_ver"
      ;;
    uninstall)
      do_uninstall
      ;;
    status)
      do_status
      ;;
    restart)
      systemctl restart 3x-ui-exporter
      do_status
      ;;
  esac
}

main "$@"
