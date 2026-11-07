#!/usr/bin/env bash
#
# 3x-ui-exporter installation and management script
# https://github.com/nike38rus/3x-ui-exporter
#
set -euo pipefail

GITHUB_REPO="nike38rus/3x-ui-exporter"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
BIN_NAME="3x-ui-exporter"
BIN_PATH="${BIN_DIR}/${BIN_NAME}"
CONFIG_PATH="${CONFIG_PATH:-/etc/3x-ui-exporter.env}"
SYSTEMD_PATH="${SYSTEMD_PATH:-/etc/systemd/system/3x-ui-exporter.service}"

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
  if [[ "${SKIP_ROOT_CHECK:-0}" -ne 1 && "$(id -u)" -ne 0 ]]; then
    abort "This script must be run as root (or with sudo)."
  fi
}

check_systemd() {
  if [[ "${SKIP_SYSTEMD_CHECK:-0}" -ne 1 ]] && ! command -v systemctl >/dev/null 2>&1; then
    abort "systemd is required but systemctl was not found."
  fi
}

check_dependencies() {
  local missing=()
  local tool
  for tool in curl tar; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      missing+=("$tool")
    fi
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    abort "Required utilities missing: ${missing[*]}. Please install them (e.g. apt-get install -y ${missing[*]})."
  fi
}

detect_arch() {
  local os arch
  os="${TARGET_OS:-$(uname -s)}"
  if [[ "$os" != "Linux" ]]; then
    abort "Unsupported OS: $os. 3x-ui-exporter runs on Linux."
  fi

  arch="${TARGET_ARCH:-$(uname -m)}"
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
    echo "v1.3.0"
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

  local cred_lines=""
  if [[ -f "$CONFIG_PATH" ]]; then
    local cert_src key_src
    cert_src="$(grep -E '^TLS_CERT_SOURCE=' "$CONFIG_PATH" | head -n1 | cut -d= -f2- | tr -d '\"'\'' ' || true)"
    key_src="$(grep -E '^TLS_KEY_SOURCE=' "$CONFIG_PATH" | head -n1 | cut -d= -f2- | tr -d '\"'\'' ' || true)"
    if [[ -n "$cert_src" && -n "$key_src" ]]; then
      cred_lines="$(printf '\n# Secure credential loading for DynamicUser\nLoadCredential=tls-cert:%s\nLoadCredential=tls-key:%s' "$cert_src" "$key_src")"
    fi
  fi

  cat <<EOF > "$SYSTEMD_PATH"
[Unit]
Description=3x-ui Prometheus Exporter
Documentation=https://github.com/nike38rus/3x-ui-exporter
After=network.target x-ui.service
Wants=x-ui.service

[Service]
Type=simple
DynamicUser=yes
EnvironmentFile=/etc/3x-ui-exporter.env
ExecStart=/usr/local/bin/3x-ui-exporter
Restart=on-failure
RestartSec=5s

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true${cred_lines}

[Install]
WantedBy=multi-user.target
EOF

  chmod 644 "$SYSTEMD_PATH"
  systemctl daemon-reload
}

read_input() {
  local prompt="$1"
  local var_name="$2"
  local val=""
  if [[ -t 0 ]]; then
    read -rp "$prompt" val
    printf -v "$var_name" "%s" "$val"
    return 0
  elif [[ -r /dev/tty ]]; then
    read -rp "$prompt" val </dev/tty
    printf -v "$var_name" "%s" "$val"
    return 0
  fi
  return 1
}

prompt_config() {
  local in_token="${1:-${XUI_API_TOKEN:-}}"
  local in_url="${2:-${XUI_URL:-}}"
  local in_listen="${3:-${LISTEN_ADDRESS:-}}"
  local in_xray_api="${4:-${XRAY_API:-}}"
  local in_tls_cert="${5:-${TLS_CERT:-${WEB_TLS_CERT_FILE:-}}}"
  local in_tls_key="${6:-${TLS_KEY:-${WEB_TLS_KEY_FILE:-}}}"
  local in_bearer_token="${7:-${BEARER_TOKEN:-${WEB_BEARER_TOKEN:-}}}"
  local in_auth_healthz="${8:-${WEB_AUTH_HEALTHZ:-}}"

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
    if read_input "3x-ui URL (use https:// if panel has SSL) [default: http://127.0.0.1:2053]: " input_url; then
      url="$input_url"
    fi
    url="${url:-http://127.0.0.1:2053}"
  fi

  if [[ -n "$url" && ! "$url" =~ ^https?:// ]]; then
    warn "URL '${url}' does not start with http:// or https://. Assuming http://${url}"
    url="http://${url}"
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

  local enable_sec="n"
  if [[ -n "$in_tls_cert" || -n "$in_tls_key" || -n "$in_bearer_token" ]]; then
    enable_sec="y"
  elif [[ -t 0 || -r /dev/tty ]]; then
    read_input "Enable TLS & Bearer Token authentication for Prometheus? [y/N]: " enable_sec || true
  fi

  local tls_cert="$in_tls_cert"
  local tls_key="$in_tls_key"
  local bearer_token="$in_bearer_token"

  if [[ "$enable_sec" =~ ^[yY] ]]; then
    if [[ -z "$tls_cert" ]]; then
      while true; do
        read_input "Path to TLS certificate file (e.g. fullchain.pem): " tls_cert || true
        if [[ -z "$tls_cert" ]]; then
          warn "TLS certificate path is required when TLS is enabled."
        elif [[ ! -f "$tls_cert" ]]; then
          warn "File '${tls_cert}' does not exist. Please check the path."
        else
          break
        fi
      done
    fi

    if [[ -z "$tls_key" ]]; then
      while true; do
        read_input "Path to TLS private key file (e.g. privkey.pem): " tls_key || true
        if [[ -z "$tls_key" ]]; then
          warn "TLS private key path is required when TLS is enabled."
        elif [[ ! -f "$tls_key" ]]; then
          warn "File '${tls_key}' does not exist. Please check the path."
        else
          break
        fi
      done
    fi

    if [[ -n "$tls_cert" && -f "$tls_cert" ]]; then
      tls_cert="$(cd "$(dirname "$tls_cert")" 2>/dev/null && pwd)/$(basename "$tls_cert")"
    fi
    if [[ -n "$tls_key" && -f "$tls_key" ]]; then
      tls_key="$(cd "$(dirname "$tls_key")" 2>/dev/null && pwd)/$(basename "$tls_key")"
    fi

    if [[ -z "$bearer_token" ]]; then
      local gen_token
      gen_token="$(openssl rand -hex 24 2>/dev/null || head -c 24 /dev/urandom | tr -dc 'a-zA-Z0-9' 2>/dev/null || echo "xui-$(date +%s%N)")"
      local input_token=""
      read_input "Bearer Token for Prometheus [press Enter to auto-generate]: " input_token || true
      bearer_token="${input_token:-$gen_token}"
    fi

    local auth_healthz="$in_auth_healthz"
    if [[ -z "$auth_healthz" ]]; then
      local input_hz="y"
      read_input "Require authentication for /healthz endpoint too? (recommended if public) [Y/n]: " input_hz || true
      if [[ "$input_hz" =~ ^[nN] ]]; then
        auth_healthz="false"
      else
        auth_healthz="true"
      fi
    fi
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

  if [[ -n "$tls_cert" && -n "$tls_key" ]]; then
    cat <<EOF >> "$CONFIG_PATH"

# Inbound TLS & HTTP Authentication
TLS_CERT_SOURCE=${tls_cert}
TLS_KEY_SOURCE=${tls_key}
WEB_TLS_CERT_FILE=/run/credentials/3x-ui-exporter.service/tls-cert
WEB_TLS_KEY_FILE=/run/credentials/3x-ui-exporter.service/tls-key
EOF
  fi

  if [[ -n "$bearer_token" ]]; then
    cat <<EOF >> "$CONFIG_PATH"
WEB_BEARER_TOKEN=${bearer_token}
WEB_AUTH_HEALTHZ=${auth_healthz:-true}
EOF
  fi

  chmod 600 "$CONFIG_PATH"
  success "Configuration saved to ${CONFIG_PATH} (permissions 0600)."
}

verify_service_and_metrics() {
  local show_service_hints="${1:-1}"

  # Resolve local scrape address for display and self-test
  local active_listen=""
  local tls_enabled=0
  local bearer_token=""
  local auth_healthz=""
  if [[ -f "$CONFIG_PATH" ]]; then
    active_listen="$(grep -E '^LISTEN_ADDRESS=' "$CONFIG_PATH" | head -n1 | cut -d= -f2- | tr -d '\"'\'' ' || true)"
    if grep -E -q '^(WEB_TLS_CERT_FILE|TLS_CERT_SOURCE)=' "$CONFIG_PATH"; then
      tls_enabled=1
    fi
    bearer_token="$(grep -E '^WEB_BEARER_TOKEN=' "$CONFIG_PATH" | head -n1 | cut -d= -f2- | tr -d '\"'\'' ' || true)"
    auth_healthz="$(grep -E '^WEB_AUTH_HEALTHZ=' "$CONFIG_PATH" | head -n1 | cut -d= -f2- | tr -d '\"'\'' ' || true)"
  fi
  active_listen="${active_listen:-:9108}"
  auth_healthz="${auth_healthz:-false}"

  local metrics_addr="127.0.0.1:9108"
  if [[ "$active_listen" =~ ^:([0-9]+)$ ]]; then
    metrics_addr="127.0.0.1${active_listen}"
  elif [[ "$active_listen" =~ ^(0\.0\.0\.0|\[::\]|::):([0-9]+)$ ]]; then
    metrics_addr="127.0.0.1:${BASH_REMATCH[2]}"
  else
    metrics_addr="$active_listen"
  fi

  local scheme="http"
  local curl_args=("-s" "--max-time" "3")
  if [[ "$tls_enabled" -eq 1 ]]; then
    scheme="https"
    curl_args+=("-k")
  fi

  local auth_args=()
  if [[ -n "$bearer_token" ]]; then
    auth_args=("-H" "Authorization: Bearer ${bearer_token}")
  fi

  if [[ "$show_service_hints" -eq 1 ]]; then
    info "Service status: systemctl status 3x-ui-exporter"
    info "Logs:           journalctl -u 3x-ui-exporter -f"
  fi

  if [[ -n "$bearer_token" ]]; then
    info "Metrics:        curl -sk -H \"Authorization: Bearer ${bearer_token}\" ${scheme}://${metrics_addr}/metrics"
    info "Bearer Token:   ${bearer_token}"
  else
    info "Metrics:        curl -sk ${scheme}://${metrics_addr}/metrics"
  fi

  if [[ "${SKIP_PROBE:-0}" -ne 1 ]]; then
    # If bearer token is enabled, verify authentication behavior
    if [[ -n "$bearer_token" ]]; then
      local unauth_code
      unauth_code="$(curl -s -k --max-time 3 -o /dev/null -w "%{http_code}" "${scheme}://${metrics_addr}/metrics" 2>/dev/null || true)"
      if [[ "$unauth_code" == "401" ]]; then
        success "Security check passed: unauthenticated /metrics requests are rejected (HTTP 401)."
      else
        warn "Security check warning: unauthenticated /metrics returned HTTP ${unauth_code} (expected 401)."
      fi

      local hz_code
      hz_code="$(curl -s -k --max-time 3 -o /dev/null -w "%{http_code}" "${scheme}://${metrics_addr}/healthz" 2>/dev/null || true)"
      if [[ "$auth_healthz" == "true" ]]; then
        if [[ "$hz_code" == "401" ]]; then
          success "Security check passed: unauthenticated /healthz requests are rejected (HTTP 401)."
        fi
      else
        if [[ "$hz_code" == "200" ]]; then
          success "Liveness check passed: /healthz is open for status monitors (HTTP 200)."
        fi
      fi
    fi

    local probe_out
    probe_out="$(curl "${curl_args[@]}" "${auth_args[@]}" "${scheme}://${metrics_addr}/metrics" 2>/dev/null || true)"

    if [[ -z "$probe_out" ]]; then
      warn "Self-test: could not connect to ${scheme}://${metrics_addr}/metrics"
      warn "View logs: journalctl -u 3x-ui-exporter -n 25 --no-pager"
      return 0
    fi

    local api_val expvars_val grpc_val
    api_val="$(echo "$probe_out" | grep -E '^xui_exporter_scrape_success\{source="api"\}' | awk '{print $2}' || true)"
    expvars_val="$(echo "$probe_out" | grep -E '^xui_exporter_scrape_success\{source="expvars"\}' | awk '{print $2}' || true)"
    grpc_val="$(echo "$probe_out" | grep -E '^xui_exporter_scrape_success\{source="grpc"\}' | awk '{print $2}' || true)"

    if [[ -z "$api_val" || -z "$expvars_val" || -z "$grpc_val" ]]; then
      warn "Self-test: endpoint responded, but scrape_success metrics are missing or incomplete."
      warn "View metrics output: curl -sk ${scheme}://${metrics_addr}/metrics"
      return 0
    fi

    if [[ "$api_val" == "1" && "$expvars_val" == "1" && "$grpc_val" == "1" ]]; then
      success "Self-test passed! All scrape sources are healthy (api: 1, grpc: 1, expvars: 1)."
    else
      warn "Self-test: one or more metric sources reported failure (api: ${api_val}, grpc: ${grpc_val}, expvars: ${expvars_val}):"
      if [[ "$api_val" != "1" ]]; then
        warn "  - api (0): failed to scrape 3x-ui API. Check XUI_URL and XUI_API_TOKEN (must have Administrator scope)."
      fi
      if [[ "$grpc_val" != "1" ]]; then
        warn "  - grpc (0): failed to communicate with Xray gRPC API. Check XRAY_API."
      fi
      if [[ "$expvars_val" != "1" ]]; then
        warn "  - expvars (0): failed to scrape Xray metrics (/debug/vars). Check panel metrics.listen or XRAY_METRICS_URL."
      fi
      warn "View detailed logs: journalctl -u 3x-ui-exporter -n 25 --no-pager"
    fi
  fi
}

do_install() {
  local target_ver="$1"
  local token="$2"
  local url="$3"
  local listen="$4"
  local xray_api="$5"
  local tls_cert="$6"
  local tls_key="$7"
  local bearer_token="$8"
  local auth_healthz="$9"

  local arch
  arch="$(detect_arch)"

  if [[ -z "$target_ver" ]]; then
    info "Resolving latest release version..."
    target_ver="$(get_latest_version)"
  fi

  info "Installing 3x-ui-exporter ${target_ver} (${arch})..."

  prompt_config "$token" "$url" "$listen" "$xray_api" "$tls_cert" "$tls_key" "$bearer_token" "$auth_healthz"
  download_and_install_binary "$target_ver" "$arch"
  create_or_update_systemd

  info "Enabling and starting 3x-ui-exporter service..."
  systemctl enable 3x-ui-exporter
  systemctl restart 3x-ui-exporter

  sleep 1
  if systemctl is-active --quiet 3x-ui-exporter; then
    success "3x-ui-exporter is active and running!"
    verify_service_and_metrics 1
  else
    warn "3x-ui-exporter service is not active yet. Check logs with:"
    warn "  journalctl -u 3x-ui-exporter -n 25 --no-pager"
  fi
}

do_config_tls() {
  local tls_cert="${1:-${TLS_CERT:-${WEB_TLS_CERT_FILE:-}}}"
  local tls_key="${2:-${TLS_KEY:-${WEB_TLS_KEY_FILE:-}}}"
  local bearer_token="${3:-${BEARER_TOKEN:-${WEB_BEARER_TOKEN:-}}}"
  local auth_healthz="${4:-${WEB_AUTH_HEALTHZ:-}}"

  if [[ ! -f "$CONFIG_PATH" ]]; then
    abort "Configuration file ${CONFIG_PATH} does not exist. Please run install first."
  fi

  info "Configuring TLS & authentication for 3x-ui-exporter..."

  if [[ -z "$tls_cert" ]]; then
    while true; do
      read_input "Path to TLS certificate file (e.g. fullchain.pem): " tls_cert || true
      if [[ -z "$tls_cert" ]]; then
        warn "TLS certificate path is required."
      elif [[ ! -f "$tls_cert" ]]; then
        warn "File '${tls_cert}' does not exist. Please check the path."
      else
        break
      fi
    done
  fi

  if [[ -z "$tls_key" ]]; then
    while true; do
      read_input "Path to TLS private key file (e.g. privkey.pem): " tls_key || true
      if [[ -z "$tls_key" ]]; then
        warn "TLS private key path is required."
      elif [[ ! -f "$tls_key" ]]; then
        warn "File '${tls_key}' does not exist. Please check the path."
      else
        break
      fi
    done
  fi

  if [[ ! -f "$tls_cert" ]]; then
    abort "Certificate file '${tls_cert}' not found."
  fi
  if [[ ! -f "$tls_key" ]]; then
    abort "Private key file '${tls_key}' not found."
  fi

  tls_cert="$(cd "$(dirname "$tls_cert")" 2>/dev/null && pwd)/$(basename "$tls_cert")"
  tls_key="$(cd "$(dirname "$tls_key")" 2>/dev/null && pwd)/$(basename "$tls_key")"

  if [[ -z "$bearer_token" ]]; then
    local gen_token
    gen_token="$(openssl rand -hex 24 2>/dev/null || head -c 24 /dev/urandom | tr -dc 'a-zA-Z0-9' 2>/dev/null || echo "xui-$(date +%s%N)")"
    local input_token=""
    read_input "Bearer Token for Prometheus [press Enter to auto-generate]: " input_token || true
    bearer_token="${input_token:-$gen_token}"
  fi

  if [[ -z "$auth_healthz" ]]; then
    local input_hz="y"
    read_input "Require authentication for /healthz endpoint too? (recommended if public) [Y/n]: " input_hz || true
    if [[ "$input_hz" =~ ^[nN] ]]; then
      auth_healthz="false"
    else
      auth_healthz="true"
    fi
  fi

  # If LISTEN_ADDRESS is currently local-only, offer to open to all interfaces
  local current_listen
  current_listen="$(grep -E '^LISTEN_ADDRESS=' "$CONFIG_PATH" | head -n1 | cut -d= -f2- | tr -d '\"'\'' ' || true)"
  if [[ "$current_listen" =~ ^127\.0\.0\.1:([0-9]+)$ ]]; then
    local port="${BASH_REMATCH[1]}"
    local change_bind="y"
    read_input "Current LISTEN_ADDRESS is local-only (${current_listen}). Change to :${port} to allow external Prometheus scrapes? [Y/n]: " change_bind || true
    if [[ ! "$change_bind" =~ ^[nN] ]]; then
      sed -i.bak -E "s|^LISTEN_ADDRESS=.*|LISTEN_ADDRESS=:${port}|" "$CONFIG_PATH"
      rm -f "${CONFIG_PATH}.bak"
      info "Updated LISTEN_ADDRESS=:${port}"
    fi
  fi

  info "Saving TLS & authentication settings to ${CONFIG_PATH}..."
  sed -i.bak -E '/^(TLS_CERT_SOURCE|TLS_KEY_SOURCE|WEB_TLS_CERT_FILE|WEB_TLS_KEY_FILE|WEB_BEARER_TOKEN|WEB_AUTH_HEALTHZ)=/d' "$CONFIG_PATH"
  rm -f "${CONFIG_PATH}.bak"

  cat <<EOF >> "$CONFIG_PATH"

# Inbound TLS & HTTP Authentication
TLS_CERT_SOURCE=${tls_cert}
TLS_KEY_SOURCE=${tls_key}
WEB_TLS_CERT_FILE=/run/credentials/3x-ui-exporter.service/tls-cert
WEB_TLS_KEY_FILE=/run/credentials/3x-ui-exporter.service/tls-key
WEB_BEARER_TOKEN=${bearer_token}
WEB_AUTH_HEALTHZ=${auth_healthz}
EOF

  chmod 600 "$CONFIG_PATH"
  success "Configuration saved to ${CONFIG_PATH} (permissions 0600)."

  create_or_update_systemd

  info "Restarting 3x-ui-exporter service..."
  systemctl restart 3x-ui-exporter
  sleep 1

  if systemctl is-active --quiet 3x-ui-exporter; then
    success "3x-ui-exporter is active with TLS and authentication!"
    verify_service_and_metrics 1
  else
    error "Service failed to restart cleanly with new TLS configuration. Check logs:"
    journalctl -u 3x-ui-exporter -n 25 --no-pager
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
    verify_service_and_metrics 0
  else
    error "Service failed to restart cleanly after update. Check logs:"
    journalctl -u 3x-ui-exporter -n 25 --no-pager
  fi
}

do_uninstall() {
  local purge="${1:-0}"
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
    if [[ "$purge" -eq 1 ]]; then
      rm -f "$CONFIG_PATH"
      info "Configuration removed (--purge)."
    else
      local del_cfg=""
      if read_input "Remove configuration file ${CONFIG_PATH}? [y/N]: " del_cfg; then
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

  if systemctl is-active --quiet 3x-ui-exporter 2>/dev/null; then
    verify_service_and_metrics 0
  fi
}

show_help() {
  cat <<EOF
Usage: $0 [command] [options]

Commands:
  install      Install 3x-ui-exporter as a systemd service (default)
  update       Update the exporter binary to the latest version (preserves config)
  config-tls   Configure inbound TLS certificate and Bearer token for Prometheus
  uninstall    Remove the exporter binary and systemd service (--purge to also remove config)
  status       Show current service status, metrics, and health check
  restart      Restart the systemd service

Options:
  -v, --version <tag>      Target release version (e.g. v1.3.0, edge, default: latest)
  -t, --token <token>      3x-ui API Bearer Token (requires Administrator scope)
  -u, --url <url>          3x-ui Panel URL (default: http://127.0.0.1:2053)
  -l, --listen <addr>      Metrics listen address (default: :9108)
  -C, --tls-cert <path>    Path to TLS certificate file (e.g. fullchain.pem)
  -K, --tls-key <path>     Path to TLS private key file (e.g. privkey.pem)
  -B, --bearer-token <tok> Bearer token for Prometheus authentication
  --auth-healthz           Require authentication for /healthz endpoint too
  --no-auth-healthz        Keep /healthz publicly accessible without authentication
  -p, --purge              Also remove configuration file during uninstall
  -h, --help               Show this help message

Environment Variables:
  VERSION, XUI_API_TOKEN, XUI_URL, LISTEN_ADDRESS, XRAY_API,
  TLS_CERT (or WEB_TLS_CERT_FILE), TLS_KEY (or WEB_TLS_KEY_FILE),
  BEARER_TOKEN (or WEB_BEARER_TOKEN), WEB_AUTH_HEALTHZ

Examples:
  # Interactive installation:
  sudo bash install.sh

  # Configure TLS and authentication on existing installation:
  sudo bash install.sh config-tls

  # Automated install with TLS & Bearer token:
  sudo bash install.sh -t "xui_token" -C "/path/to/fullchain.pem" -K "/path/to/privkey.pem"

  # Update to latest version:
  sudo bash install.sh update

  # Complete uninstall including config:
  sudo bash install.sh uninstall --purge
EOF
}

main() {
  local cmd="install"
  local target_ver="${VERSION:-}"
  local token="${XUI_API_TOKEN:-}"
  local url="${XUI_URL:-}"
  local listen="${LISTEN_ADDRESS:-}"
  local xray_api="${XRAY_API:-}"
  local tls_cert="${TLS_CERT:-${WEB_TLS_CERT_FILE:-}}"
  local tls_key="${TLS_KEY:-${WEB_TLS_KEY_FILE:-}}"
  local bearer_token="${BEARER_TOKEN:-${WEB_BEARER_TOKEN:-}}"
  local auth_healthz="${WEB_AUTH_HEALTHZ:-}"
  local purge_config=0

  while [[ $# -gt 0 ]]; do
    case "$1" in
      install|update|uninstall|status|restart|config-tls|tls)
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
      -C|--tls-cert)
        tls_cert="$2"
        shift 2
        ;;
      -K|--tls-key)
        tls_key="$2"
        shift 2
        ;;
      -B|--bearer-token)
        bearer_token="$2"
        shift 2
        ;;
      --auth-healthz)
        auth_healthz="true"
        shift
        ;;
      --no-auth-healthz)
        auth_healthz="false"
        shift
        ;;
      -p|--purge)
        purge_config=1
        shift
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
  check_dependencies

  if [[ "$target_ver" == "vedge" ]]; then
    target_ver="edge"
  elif [[ -n "$target_ver" && "$target_ver" =~ ^[0-9] ]]; then
    target_ver="v${target_ver}"
  fi

  case "$cmd" in
    install)
      if [[ -f "$BIN_PATH" && -f "$CONFIG_PATH" ]]; then
        info "3x-ui-exporter is already installed. Running update..."
        do_update "$target_ver"
      else
        do_install "$target_ver" "$token" "$url" "$listen" "$xray_api" "$tls_cert" "$tls_key" "$bearer_token" "$auth_healthz"
      fi
      ;;
    update)
      do_update "$target_ver"
      ;;
    config-tls|tls)
      do_config_tls "$tls_cert" "$tls_key" "$bearer_token" "$auth_healthz"
      ;;
    uninstall)
      do_uninstall "$purge_config"
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
