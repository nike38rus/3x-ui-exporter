# 3x-ui exporter

[![CI](https://github.com/nike38rus/3x-ui-exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/nike38rus/3x-ui-exporter/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/nike38rus/3x-ui-exporter?logo=github)](https://github.com/nike38rus/3x-ui-exporter/releases)
[![Container Image](https://img.shields.io/badge/GHCR-docker_image-blue?logo=docker)](https://github.com/nike38rus/3x-ui-exporter/pkgs/container/3x-ui-exporter)
[![Go Version](https://img.shields.io/github/go-mod/go-version/nike38rus/3x-ui-exporter)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

High-performance, stateless Prometheus exporter for the **[3x-ui](https://github.com/MHSanaei/3x-ui)** control panel and its running **Xray core**.

Provides real-time visibility into traffic throughput, multi-node cluster health, client account lifecycle and active sessions, memory and GC behavior, balancer selection and fallback states, and outbound latency health via Xray's Observatory probe statistics.

---

## Table of Contents

- [Overview](#overview)
- [Key Features](#key-features)
- [Architecture](#architecture)
- [Quick Start](#quick-start)
  - [Docker Compose (Recommended)](#docker-compose-recommended)
  - [Prometheus Configuration](#prometheus-configuration)
  - [Systemd Service Installation](#systemd-service-installation-standalone-host-vm)
  - [Manual Binary Execution](#manual-binary-execution)
- [Configuration Reference](#configuration-reference)
- [Security & Remote Access: TLS and Authentication](#security-remote-access-tls-and-authentication)
- [Metrics Reference](#metrics-reference)
  - [Exporter Diagnostics](#exporter-diagnostics)
  - [Panel, Node Cluster, and Inbound Status](#panel-node-cluster-and-inbound-status)
  - [Traffic Statistics](#traffic-statistics)
  - [Balancers and Routing](#balancers-and-routing)
  - [Observatory and RTT](#observatory-and-rtt)
  - [Xray Go Runtime and Memory](#xray-go-runtime-and-memory)
- [Grafana Dashboard](#grafana-dashboard)
  - [Overview Dashboard](#overview-dashboard)
  - [Dashboard Quality Score](#dashboard-quality-score)
- [Technical Principles](#technical-principles)
  - [Endpoint Discovery](#endpoint-discovery)
  - [Balancer Strategy Semantics](#balancer-strategy-semantics)
  - [Fault Isolation and Request Budget](#fault-isolation-and-request-budget)
- [Development and Build](#development-and-build)
- [Disclaimer and Maintenance](#disclaimer-and-maintenance)
- [License](#license)

---

## Overview

Traditional exporters for 3x-ui often rely on periodic background polling, store stale metrics in memory caches, or invoke heavy Xray gRPC RPCs (`StatsService`, `ObservatoryService`, `GetSysStats`) that add unnecessary CPU and synchronization overhead.

**3x-ui-exporter** operates strictly on a **synchronous, pull-based model**:
- Every Prometheus scrape triggers fresh queries across upstream interfaces with an enforced deadline (`SCRAPE_TIMEOUT`).
- All traffic, memory/GC, and observatory measurements are retrieved in **a single lightweight HTTP request** to Xray's internal `/debug/vars` (expvars) endpoint.
- Panel configuration and versions are queried in parallel via 3x-ui REST API.
- Runtime routing and balancers are queried concurrently via public Xray gRPC services.
- If an upstream source fails, independent metrics continue to be served with clear source-level failure indicators (`xui_exporter_scrape_success{source="..."} 0`).

---

## Key Features

- **Zero-Cache & No Stale Data:** Measurements are never cached across scrapes. If a service stops, metrics reflect the failure immediately instead of masking it with historical values.
- **Ultra-Fast Scrape Latency:** Parallel upstream collection and optimized HTTP connection pooling deliver complete scrapes in low single-digit milliseconds ($< 5\text{ ms}$).
- **Automatic Metrics Discovery:** Automatically extracts Xray's `metrics.listen` address directly from current panel settings, with support for explicit URL overrides.
- **Role-Aware Balancer Tracking:** Distinguishes between `primary` targets, configured `fallback` paths, and active `override` states across leastLoad, leastPing, random, and roundRobin strategies.
- **Comprehensive Observatory Insights:** Exports burst observatory probe counts, failure rates, and RTT distributions (mean, min, max, deviation).
- **Multi-Node Cluster & Client Lifecycle Telemetry:** Monitors remote cluster nodes (online status, latency, CPU/RAM, uptime, per-node online clients, cross-node inbound traffic) and tracks deduplicated client accounts by lifecycle state (`active`, `disabled`, `expired`, `depleted`).
- **Built-In TLS, mTLS & HTTP Authentication:** Securely expose `/metrics` for remote Prometheus scraping using TLS 1.2+, Mutual TLS (mTLS), Basic Auth, or Bearer tokens (with secret file support and anti-fingerprinting responses).
- **Automated `systemd` Installer:** One-command installation, in-place updates, TLS & token setup (`config-tls`), and automated 3-source health self-tests (`api`, `grpc`, `expvars`).
- **Hardened Distroless/Scratch Image:** Final Docker image is built `FROM scratch` with a statically linked Go binary: ~15 MB total size, runs as non-root user `65532:65532`, with zero OS packages and zero CVEs.
- **Out-of-the-Box Grafana Dashboards:** Includes production-verified Grafana dashboards (in both Classic and Schema v2 formats) featuring a comprehensive Overview, multi-job support, and an automated outbound **Quality Score**.

---

## Architecture

The exporter acts as a bridge between Prometheus and the 3x-ui / Xray ecosystem:

```mermaid
flowchart LR
    Prom["Prometheus\n(/metrics)"] --> Exp["3x-ui-exporter\n(Stateless & Concurrent)"]

    subgraph "Upstream Sources"
        direction TB
        Exp -->|"Parallel API (HTTP)\n/panel/api/xray/\n/panel/api/server/status\n/panel/api/inbounds/list\n/panel/api/nodes/list\n/panel/api/clients/onlines"| Panel["3x-ui Panel\n(Config, Status & Nodes)"]
        Exp -->|"Single HTTP Request\n/debug/vars (expvars)"| Expvars["Xray Core Expvars\n(Traffic, Mem/GC, Observatory)"]
        Exp -->|"Concurrent gRPC\nHandlerService\nRoutingService"| Core["Xray Core gRPC\n(Outbounds & Balancers)"]
    end
```

### Upstream Sources at a Glance

| Source | Protocol & Endpoint | Purpose & Retrieved Data |
| --- | --- | --- |
| **`api`** | HTTP REST `POST /panel/api/xray/`<br>`GET /panel/api/server/status`<br>`GET /panel/api/inbounds/list`<br>`GET /panel/api/nodes/list`<br>`POST /panel/api/clients/onlines` | Discovers balancer rules, selector prefixes, subscription outbounds, panel version, Xray version, process state, open TCP/UDP sockets, inbound administrative states, node cluster health, client account lifecycle states, and online clients count. |
| **`expvars`** | HTTP `GET /debug/vars` | Fetches inbound/outbound traffic counters, memory/GC stats, and observatory probe/RTT metrics in **one** roundtrip. |
| **`grpc`** | gRPC `ListOutbounds`<br>`GetBalancerInfo` | Inspects live runtime outbound inventory and queries active balancer selection/override targets concurrently. |
| **`exporter`** | Internal runtime diagnostics | Emits scrape success, collection timing per source, and exporter build version. |

---

## Quick Start

### Docker Compose (Recommended)

The recommended deployment places the exporter alongside 3x-ui by joining the panel's container network namespace. This allows secure, loopback-only communication without exposing Xray's gRPC control port or expvars listener to external networks.

#### Option A: Adding to an existing Compose file

Add the `exporter` service directly to your existing 3x-ui `compose.yaml` (assuming your panel service key is `x-ui`):

```yaml
services:
  x-ui:
    # Your existing 3x-ui service definition
    image: ghcr.io/mhsanaei/3x-ui:latest
    container_name: 3x-ui
    # ...

  exporter:
    image: ghcr.io/nike38rus/3x-ui-exporter:1.4.1
    network_mode: service:x-ui
    depends_on:
      x-ui:
        condition: service_started
        restart: true
    environment:
      XUI_URL: http://127.0.0.1:2053
      XUI_API_TOKEN: ${XUI_API_TOKEN:?Set XUI_API_TOKEN}
      XRAY_API: 127.0.0.1:62789
      # XRAY_METRICS_URL is empty by default to auto-discover metrics.listen
      XRAY_METRICS_URL: ""
      LISTEN_ADDRESS: :9108
      SCRAPE_TIMEOUT: 4s
      LOG_LEVEL: info
      LOG_FORMAT: text
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    restart: unless-stopped
```

#### Option B: Standalone Compose Deployment

If deploying as a standalone stack against a running 3x-ui container on the same Docker host:

```sh
# Clone repository and prepare environment
cp .env.example .env
# Edit .env to set your XUI_API_TOKEN and XRAY_CONTAINER name
docker compose up -d
```

#### Option C: Full All-in-One Monitoring Stack

If you want to spin up the **entire monitoring infrastructure** (3x-ui-exporter + Prometheus + Grafana with pre-provisioned dashboards) in a single command, see the ready-to-run [Full Monitoring Stack Example](examples/full-monitoring-stack):

```sh
cd examples/full-monitoring-stack
cp .env.example .env
# Edit .env and supply your XUI_API_TOKEN
docker compose up -d
```
Grafana will be immediately available at `http://localhost:3000` with the 3x-ui dashboard preloaded.

### Prometheus Configuration

Add the scrape job to your `prometheus.yml`:

```yaml
# Local / Internal Scrape (HTTP without auth)
scrape_configs:
  - job_name: xui-exporter
    scrape_interval: 5s
    scrape_timeout: 5s
    static_configs:
      - targets: ["127.0.0.1:9108"]

  # Remote VPS Scrape across the Internet (HTTPS + Bearer Token)
  - job_name: xui-vps-remote
    scrape_interval: 5s
    scrape_timeout: 5s
    scheme: https
    bearer_token: "your-secret-bearer-token"
    tls_config:
      # If using self-signed certificate or certificate issued to IP address:
      insecure_skip_verify: true
      # If using a trusted domain certificate (Let's Encrypt), set to false:
      # insecure_skip_verify: false
    static_configs:
      - targets: ["vps-ip-or-domain:9108"]
```

> [!NOTE]
> Ensure that Prometheus's `scrape_timeout` is slightly greater than or equal to the exporter's `SCRAPE_TIMEOUT` (e.g. 5s scrape timeout vs 4s exporter deadline).

### Systemd Service Installation (Standalone Host / VM)

For standalone Linux virtual machines where 3x-ui runs directly under systemd without Docker, an automated installer script sets up the binary, hardened systemd service, and environment configuration:

#### Fresh Installation

```sh
# One-liner automated installation (interactive prompt for token):
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | sudo bash

# Or pass parameters non-interactively (with optional TLS & Bearer token):
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | \
  sudo bash -s -- \
    -t "your-panel-token" \
    -u "http://127.0.0.1:2053" \
    -C "/root/cert/ip/fullchain.pem" \
    -K "/root/cert/ip/privkey.pem"
```

The installer:
- Detects the CPU architecture (`amd64` / `arm64`) and downloads the latest release binary to `/usr/local/bin/3x-ui-exporter`.
- Creates a secure environment file at `/etc/3x-ui-exporter.env` (permissions `0600`).
- Configures a sandboxed systemd service at `/etc/systemd/system/3x-ui-exporter.service` with `DynamicUser` isolation and `LoadCredential` for root-owned TLS certificates.
- Enables and starts the service immediately, followed by self-test validation.

#### Updating and Version Pinning

To update an existing installation to the latest release while strictly preserving your `/etc/3x-ui-exporter.env` configuration:

```sh
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | sudo bash -s update
```

To switch to a specific release version, downgrade, or install the rolling bleeding-edge build from `main`:

```sh
# Pin specific version:
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | sudo bash -s update -v v1.5.0

# Install or update to edge build (latest commit on main):
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | sudo bash -s update -v edge
```

#### Inbound TLS & Prometheus Authentication

To secure the metrics endpoint for remote Prometheus scrapes across the public internet using TLS and a Bearer token:

```sh
# Configure TLS and auto-generate a secure Bearer token on an existing installation:
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | \
  sudo bash -s config-tls \
    --tls-cert /root/cert/ip/fullchain.pem \
    --tls-key /root/cert/ip/privkey.pem

# Or run interactively (prompts for paths and auto-generates a token):
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | \
  sudo bash -s config-tls
```

The installer:
- Configures systemd `LoadCredential` to securely grant the sandboxed `DynamicUser` read access to root-owned TLS private keys without relaxing file permissions.
- Configures `WEB_BEARER_TOKEN` in `/etc/3x-ui-exporter.env` and prompts whether to require authentication on `/healthz` (`WEB_AUTH_HEALTHZ`, or via `--auth-healthz` / `--no-auth-healthz`).
- Performs security and scrape validation: ensures unauthenticated requests are rejected (HTTP 401), authenticated scrapes succeed over HTTPS, and all three upstream sources (`api`, `grpc`, `expvars`) report healthy.

#### Service Management

```sh
# Run service status check and 3-source scrape self-test (api, grpc, expvars):
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | sudo bash -s status

# Check binary version and systemd status directly:
sudo /usr/local/bin/3x-ui-exporter -version
sudo systemctl status 3x-ui-exporter

# View live logs
sudo journalctl -u 3x-ui-exporter -f

# Uninstall service and binary (preserves configuration):
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | sudo bash -s uninstall

# Full purge (removes service, binary, and configuration):
curl -sSL https://raw.githubusercontent.com/nike38rus/3x-ui-exporter/main/install.sh | sudo bash -s uninstall --purge
```

### Manual Binary Execution

You can run the static binary directly on the host or inside a virtual machine:

```sh
export XUI_URL=http://127.0.0.1:2053
export XUI_API_TOKEN="your-panel-api-token"
export XRAY_API=127.0.0.1:62789
export LISTEN_ADDRESS=:9108
export SCRAPE_TIMEOUT=4s

./bin/3x-ui-exporter
```

---

## Configuration Reference

All settings can be configured via command-line flags or environment variables. CLI flags take precedence over environment variables.

| CLI Flag | Environment Variable | Default | Description |
| --- | --- | --- | --- |
| `--xui-url` | `XUI_URL` | *None (Required)* | 3x-ui panel base URL, including optional base path (e.g. `http://127.0.0.1:2053`). |
| `--xui-api-token` | `XUI_API_TOKEN` | *None (Required)* | 3x-ui Bearer API token with **Administrator** scope. Prefer environment variable to prevent process list exposure. |
| `--xray-api` | `XRAY_API` | `127.0.0.1:62789` | Xray gRPC host and port (`host:port`). |
| `--xray-metrics-url` | `XRAY_METRICS_URL` | `""` *(empty)* | Xray expvars HTTP base URL. If empty, automatically discovered from panel `metrics.listen`. |
| `--listen-address` | `LISTEN_ADDRESS` | `:9108` | Address and port for the exporter HTTP server to bind. |
| `--scrape-timeout` | `SCRAPE_TIMEOUT` | `4s` | Total deadline per scrape request across all upstream sources. |
| `--log-level` | `LOG_LEVEL` | `info` | Log verbosity level: `debug`, `info`, `warn`, `error`. |
| `--log-format` | `LOG_FORMAT` | `text` | Log format output: `text` or `json`. |
| `--collect-client-stats` | `COLLECT_CLIENT_STATS` | `false` | When `true`, parses individual user/client counters into `xui_client_traffic_bytes_total`. |
| `--xui-insecure-skip-verify` | `XUI_INSECURE_SKIP_VERIFY` | `false` | When `true`, skips TLS certificate verification for 3x-ui panel requests (e.g. for local `https://127.0.0.1:...` with domain certificate). |
| `--web.basic-auth-username` | `WEB_BASIC_AUTH_USERNAME` | `""` *(empty)* | Username for HTTP Basic Authentication on exporter endpoints. |
| `--web.basic-auth-password` | `WEB_BASIC_AUTH_PASSWORD` | `""` *(empty)* | Password for HTTP Basic Authentication (prefer environment variable to avoid process list exposure). |
| `--web.basic-auth-password-file` | `WEB_BASIC_AUTH_PASSWORD_FILE` | `""` *(empty)* | Path to file containing password for HTTP Basic Authentication. |
| `--web.bearer-token` | `WEB_BEARER_TOKEN` | `""` *(empty)* | Bearer token for HTTP Authorization (`Authorization: Bearer <token>`). Prefer environment variable. |
| `--web.bearer-token-file` | `WEB_BEARER_TOKEN_FILE` | `""` *(empty)* | Path to file containing Bearer token for HTTP Authorization. |
| `--web.auth-healthz` | `WEB_AUTH_HEALTHZ` | `false` | When `true`, requires authentication for `/healthz` endpoint (by default `/healthz` is unauthenticated for probes). |
| `--web.auth-realm` | `WEB_AUTH_REALM` | `metrics` | Authentication realm reported in `WWW-Authenticate` header (defaults to generic `metrics` to avoid fingerprinting). |
| `--web.tls-cert-file` | `WEB_TLS_CERT_FILE` | `""` *(empty)* | Path to server TLS certificate / certificate chain file. Enables HTTPS. |
| `--web.tls-key-file` | `WEB_TLS_KEY_FILE` | `""` *(empty)* | Path to server TLS private key file. Enables HTTPS. |
| `--web.tls-client-ca-file` | `WEB_TLS_CLIENT_CA_FILE` | `""` *(empty)* | Path to client CA certificate file for Mutual TLS (mTLS) client certificate verification. |
| `--version` | — | `false` | Prints exporter version and exits. |

> [!IMPORTANT]
> **API Token Permissions:** The API token generated in 3x-ui must have the **Administrator** scope. Restricted scopes (such as `monitoring`) do not grant access to the Xray routing configuration (`/panel/api/xray/`), inbound inventories (`/panel/api/inbounds/list`), or cluster nodes, causing scrapes to fail with HTTP `403 Forbidden`.

---

## Security & Remote Access: TLS and Authentication

For securely publishing metrics to untrusted networks or the public internet, `3x-ui-exporter` includes built-in HTTPS and authentication support without requiring an external reverse proxy.

### Security Highlights

- **Timing Attack Protection**: Password and token comparisons are evaluated in constant time using `crypto/subtle.ConstantTimeCompare` combined with fixed-length SHA-256 hashes.
- **Modern TLS**: Enforces TLS 1.2+ minimum version (`tls.VersionTLS12`) and secure AEAD cipher suites (ECDHE with AES-GCM and ChaCha20-Poly1305). Insecure ciphers (CBC, RC4, 3DES, static RSA) are completely disabled.
- **Mutual TLS (mTLS)**: Optionally verify client certificates against a trusted CA (`--web.tls-client-ca-file`), enforcing `tls.RequireAndVerifyClientCert`.
- **Flexible Authentication**: Supports HTTP Basic Auth, Bearer Token, or both simultaneously.
- **Stealth & Anti-Fingerprinting**: Unauthenticated probe responses never disclose that 3x-ui or Xray is running on the host. The `WWW-Authenticate` realm defaults to a generic `realm="metrics"`, and the HTML landing page uses a generic "Metrics Exporter" title without vendor fingerprints.
- **Probe-Friendly `/healthz`**: The `/healthz` liveness probe endpoint remains accessible without credentials by default so Kubernetes liveness probes or systemd watchdogs function without secrets. Set `--web.auth-healthz=true` to require authentication for all endpoints.

### Usage Examples

#### 1. Bearer Token over HTTPS

```sh
./bin/3x-ui-exporter \
  --web.tls-cert-file=/etc/ssl/certs/exporter.crt \
  --web.tls-key-file=/etc/ssl/private/exporter.key \
  --web.bearer-token="super-secret-token-12345"
```

Or via environment variables:

```sh
export WEB_TLS_CERT_FILE=/etc/ssl/certs/exporter.crt
export WEB_TLS_KEY_FILE=/etc/ssl/private/exporter.key
export WEB_BEARER_TOKEN="super-secret-token-12345"
./bin/3x-ui-exporter
```

#### 2. Basic Authentication

```sh
./bin/3x-ui-exporter \
  --web.basic-auth-username="prom-user" \
  --web.basic-auth-password="complex-password-here"
```

Or using a secret file:

```sh
./bin/3x-ui-exporter \
  --web.basic-auth-username="prom-user" \
  --web.basic-auth-password-file=/run/secrets/basic_auth_password
```

#### 3. Mutual TLS (mTLS) + Bearer Token

```sh
./bin/3x-ui-exporter \
  --web.tls-cert-file=/etc/ssl/certs/server.crt \
  --web.tls-key-file=/etc/ssl/private/server.key \
  --web.tls-client-ca-file=/etc/ssl/certs/client-ca.crt \
  --web.bearer-token="cluster-scrape-token"
```

### Secret File Format (`password-file` & `token-file`)

When using `--web.basic-auth-password-file` or `--web.bearer-token-file` (or their environment variables `WEB_BASIC_AUTH_PASSWORD_FILE` / `WEB_BEARER_TOKEN_FILE`), credentials are loaded from the filesystem to avoid leaking secrets in process tables or process monitors:

- **Format**: Plain text file containing the raw password or bearer token as a UTF-8 string.
- **Whitespace & Newlines**: Leading and trailing whitespace, carriage returns (`\r`), and line feeds (`\n`) are automatically trimmed during initialization. Files created with standard text editors, `echo "secret" > file`, Docker Secrets, or Kubernetes Secret volume mounts work seamlessly out-of-the-box.
- **Validation**: If the specified file does not exist or contains only whitespace, the exporter halts on startup with a descriptive configuration error.
- **Permissions**: Protect secret files with restrictive file permissions (recommended `0600` or read-only mount).

Example files and deployment integration guides in the repository:
- [`examples/security/basic-auth-password.example`](examples/security/basic-auth-password.example): Sample secret file containing an HTTP Basic Auth password.
- [`examples/security/bearer-token.example`](examples/security/bearer-token.example): Sample secret file containing an HTTP Bearer Token.
- [`examples/security/README.md`](examples/security/README.md): Guide covering Docker Secrets, Kubernetes Secret volume mounts, and systemd `LoadCredential`.

### Scraping Configurations

#### Prometheus Configuration (`prometheus.yml`)

**Bearer Token over HTTPS:**

```yaml
scrape_configs:
  - job_name: "3x-ui"
    scheme: https
    bearer_token: "super-secret-token-12345"
    static_configs:
      - targets: ["your-server.example.com:9108"]
    tls_config:
      ca_file: /etc/prometheus/certs/ca.crt
```

**Basic Authentication over HTTPS:**

```yaml
scrape_configs:
  - job_name: "3x-ui"
    scheme: https
    basic_auth:
      username: "prom-user"
      password: "complex-password-here"
    static_configs:
      - targets: ["your-server.example.com:9108"]
    tls_config:
      ca_file: /etc/prometheus/certs/ca.crt
```

**Mutual TLS (mTLS):**

```yaml
scrape_configs:
  - job_name: "3x-ui-mtls"
    scheme: https
    static_configs:
      - targets: ["your-server.example.com:9108"]
    tls_config:
      ca_file: /etc/prometheus/certs/server-ca.crt
      cert_file: /etc/prometheus/certs/client.crt
      key_file: /etc/prometheus/certs/client.key
```

#### Grafana Agent / Grafana Alloy

```river
prometheus.scrape "xui" {
  targets = [{
    __address__ = "your-server.example.com:9108",
  }]
  forward_to = [prometheus.remote_write.default.receiver]
  scheme     = "https"

  authorization {
    type        = "Bearer"
    credentials = "super-secret-token-12345"
  }

  tls_config {
    ca_file = "/etc/agent/certs/ca.crt"
  }
}
```

---

## Metrics Reference

All metrics are prefixed with `xui_`. Missing or unknown measurements remain absent (never synthesized as zero unless confirmed by Xray).

### Exporter Diagnostics

Diagnostics describe the collection process itself. All three source series are emitted on every scrape.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `xui_exporter_scrape_success` | Gauge | `source` (`api`, `expvars`, `grpc`) | `1` if source requests succeeded and satisfied contract; `0` on error. |
| `xui_exporter_scrape_duration_seconds` | Gauge | `source` (`api`, `expvars`, `grpc`) | Elapsed wall-clock time spent querying this source in the current scrape. |
| `xui_exporter_build_info` | Gauge | `version` | Always `1`. Identifies the running exporter version. |

### Panel, Node Cluster, and Inbound Status

Derived from 3x-ui REST API (`GET /panel/api/server/status`, `POST /panel/api/clients/onlines`, `GET /panel/api/inbounds/list`, `GET /panel/api/nodes/list`).

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `xui_panel_info` | Gauge | `version` | Always `1`. Panel version reported by 3x-ui. |
| `xui_xray_info` | Gauge | `version` | Always `1`. Xray binary version reported by the panel. |
| `xui_xray_state` | Gauge | `state` (`running`, `stop`, `error`) | `1` for the active state, `0` for inactive states. |
| `xui_open_sockets` | Gauge | `protocol` (`tcp`, `udp`) | Current open network sockets reported by the panel environment. |
| `xui_client_online` | Gauge | — | Current active client sessions across the cluster reported by panel. |
| `xui_client_count` | Gauge | `state` (`active`, `disabled`, `expired`, `depleted`) | Total configured client accounts in the panel database by lifecycle state. |
| `xui_inbound_enabled` | Gauge | `inbound`, `remark`, `node` | `1` if inbound is administratively enabled in the panel; `0` if disabled. Enriched with inbound remark and node name (`local` for master panel). |
| `xui_inbound_bytes_total` | Counter | `inbound`, `remark`, `node`, `direction` (`uplink`, `downlink`) | Cumulative inbound traffic bytes across all nodes reported by the panel database. |
| `xui_node_online` | Gauge | `node` | `1` if remote node is reported online by master panel heartbeat; `0` if offline. |
| `xui_node_online_clients` | Gauge | `node` | Current active clients online connected to this node reported by panel (`local` for master panel). |
| `xui_node_latency_seconds` | Gauge | `node` | Round-trip latency between master panel and remote node in seconds. |
| `xui_node_xray_state` | Gauge | `node`, `state` (`running`, `stop`, `error`) | Remote node Xray core state captured during heartbeat. |
| `xui_node_cpu_percent` | Gauge | `node` | Remote node CPU utilization percent. |
| `xui_node_memory_percent` | Gauge | `node` | Remote node memory utilization percent. |
| `xui_node_uptime_seconds` | Gauge | `node` | Remote node uptime in seconds. |
| `xui_node_info` | Gauge | `node`, `xray_version`, `panel_version` | Always `1`. Remote node software version metadata. |

> [!NOTE]
> Client account states (`xui_client_count`) are mutually exclusive and deduplicated across inbounds:
> - `active`: Account is administratively enabled, not expired, and traffic quota is not exhausted.
> - `disabled`: Administratively disabled (`enable: false`).
> - `expired`: Expiration timestamp elapsed (`expiryTime <= now`).
> - `depleted`: Cumulative traffic meets or exceeds configured quota (`up + down >= total`).

### Traffic Statistics

Derived from Xray expvars (`stats.inbound`, `stats.outbound`, and optional `stats.user`).

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `xui_inbound_traffic_bytes_total` | Counter | `inbound`, `direction` (`uplink`, `downlink`) | Cumulative bytes traversing the inbound handler. |
| `xui_outbound_traffic_bytes_total` | Counter | `outbound`, `direction` (`uplink`, `downlink`) | Cumulative bytes traversing the outbound handler. |
| `xui_client_traffic_bytes_total` | Counter | `client`, `direction` (`uplink`, `downlink`) | Cumulative bytes per user email/tag (enabled via `--collect-client-stats`). |

> [!TIP]
> Use PromQL `rate()` or `increase()` over traffic counters to calculate network throughput (bytes per second).

### Balancers and Routing

Derived from panel routing configuration and runtime Xray gRPC `RoutingService`.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `xui_balancer_state` | Gauge | `balancer` | Current balancer operational routing state: `0` (unavailable), `1` (fallback), `2` (primary), `3` (override). |
| `xui_balancer_outbound_selected` | Gauge | `balancer`, `outbound`, `role` (`primary`, `fallback`, `override`) | `1` if selected/active; `0` if inactive. |
| `xui_balancer_outbound_available` | Gauge | `balancer` | `1` if balancer resolves to an available runtime outbound (structural check). |

#### Selection Roles

- **`primary`:** Represents known selector candidates. `1` for the currently preferred target, `0` for others.
- **`fallback`:** Emitted when `fallbackTag` is configured. `1` when primary selection is empty and no override is active; otherwise `0`.
- **`override`:** `1` when runtime override is active, `0` otherwise.

### Observatory and RTT

Derived from Xray expvars (`observatory`). Supports ordinary observatory and HealthPing burstObservatory.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `xui_observatory_outbound_alive` | Gauge | `outbound` | `1` = alive, `0` = unreachable according to Xray probes. |
| `xui_observatory_outbound_delay_seconds` | Gauge | `outbound` | Delay in seconds. Latest probe (ordinary) or window average (burst). Omitted if not alive. |
| `xui_observatory_outbound_rtt_mean_seconds` | Gauge | `outbound` | Mean RTT across the observation window in seconds. |
| `xui_observatory_outbound_rtt_min_seconds` | Gauge | `outbound` | Minimum RTT in the burst window in seconds. |
| `xui_observatory_outbound_rtt_max_seconds` | Gauge | `outbound` | Maximum RTT in the burst window in seconds. |
| `xui_observatory_outbound_rtt_deviation_seconds` | Gauge | `outbound` | RTT deviation in the burst window in seconds. |
| `xui_observatory_outbound_probe_samples` | Gauge | `outbound` | Total probes executed in the current burst window. |
| `xui_observatory_outbound_probe_failed_samples` | Gauge | `outbound` | Failed probes in the current burst window. |

### Xray Go Runtime and Memory

Derived from Xray expvars (`memstats`). Measures the memory footprint of the proxied Xray core process (not the exporter).

| Metric | Type | Description |
| --- | --- | --- |
| `xui_xray_memory_alloc_bytes` | Gauge | Bytes allocated on heap currently in use (`Alloc`). |
| `xui_xray_memory_sys_bytes` | Gauge | Total bytes obtained from the OS by the runtime (`Sys`). |
| `xui_xray_memory_alloc_bytes_total` | Counter | Cumulative bytes allocated for heap objects (`TotalAlloc`). |
| `xui_xray_memory_mallocs_total` | Counter | Cumulative count of allocated heap objects. |
| `xui_xray_memory_frees_total` | Counter | Cumulative count of freed heap objects. |
| `xui_xray_memory_live_objects` | Gauge | Current count of live allocated heap objects. |
| `xui_xray_gc_cycles_total` | Counter | Number of completed garbage collection cycles. |
| `xui_xray_gc_pause_seconds_total` | Counter | Cumulative GC stop-the-world pause duration in seconds. |

---

## Grafana Dashboard

The repository includes pre-built, production-tested Grafana dashboards:
- **`grafana-3x-ui.v2.json`**: Modern dashboard using Grafana resource schema v2 (recommended, requires Grafana 11+).
- **`grafana-3x-ui.classic.json`**: Classic dashboard format (deprecated; will not be supported in future major releases).

### Overview Dashboard

The default view surfaces Xray state, versions, target health, balancer modes, observed outbound health, and aggregate throughput:

![Dashboard Overview](docs/images/overview.png)

<details>
<summary>Click to view detailed section screenshots</summary>

#### Clients
![Clients](docs/images/clients.png)

#### Inbounds
![Inbounds](docs/images/inbounds.png)

#### Outbounds
![Outbounds](docs/images/outbounds.png)

#### Balancers & Observatory
![Balancers & Observatory](docs/images/balancers.png)

#### Connected Nodes
![Connected Nodes](docs/images/nodes.png)

#### Xray Runtime
![Xray Runtime](docs/images/xray.png)

#### Exporter Diagnostics
![Exporter Diagnostics](docs/images/exporter.png)

</details>

### Dashboard Quality Score

The dashboard calculates an outbound **Quality Score** (0–100, higher is better) using burst observatory window statistics over the selected dashboard time range (`$__range`).

Rather than looking at raw ping numbers in isolation, the score combines probe success, latency consistency, and speed into a single intuitive metric.

$$\text{Quality} = 100 \cdot \left(\frac{R}{100}\right)^{0.50} \cdot \left(\frac{S}{100}\right)^{0.30} \cdot \left(\frac{L}{100}\right)^{0.20}$$

#### How It Works

The score is computed as a **weighted geometric mean** of three components:

1. **Reliability ($R$, weight 50%): Can packets get through?**
   - Formula: $R = 100 \cdot e^{-10 \cdot f}$, where $f = \frac{\sum \text{failed probes}}{\sum \text{total probes}}$ is the probe failure ratio.
   - **In plain terms:** Measures packet loss during health probes. Packet drops are penalized heavily on an exponential scale:
     - **0% packet loss** $\rightarrow$ **100** points (perfect)
     - **3% packet loss** $\rightarrow$ **~74** points
     - **5% packet loss** $\rightarrow$ **~60** points
     - **10% packet loss** $\rightarrow$ **~37** points
     - **25%+ packet loss** $\rightarrow$ **< 10** points
   - *Rationale:* A proxy route that drops connections is frustrating to use, no matter how fast its successful pings are.

2. **RTT Stability ($S$, weight 30%): Does latency jump around?**
   - Formula: $S = 100 \cdot e^{-2 \cdot (P_{95} - P_{50})}$
   - **In plain terms:** Measures jitter and latency spikes by calculating the spread between worst-case tail latency (95th percentile, $P_{95}$) and typical median latency (50th percentile, $P_{50}$):
     - **Consistent latency** (e.g. median 100 ms, tail 130 ms, spread = 30 ms) $\rightarrow$ **~94** points.
     - **Erratic spikes** (e.g. median 100 ms, tail 800 ms, spread = 700 ms) $\rightarrow$ drops to **~25** points.
   - *Rationale:* High jitter causes stuttering in voice/video calls, game rubber-banding, and stalled web page loads.

3. **Latency ($L$, weight 20%): How fast is it?**
   - Formula: $L = \text{clamp}(130 - 100 \cdot P_{50}, 0, 100)$
   - **In plain terms:** Rewards low median response time ($P_{50}$):
     - Latency **$\le$ 300 ms** receives the full **100** points.
     - Above **300 ms**, the score decreases linearly (10 points lost for each additional 100 ms).
     - Latency **$\ge$ 1.3 seconds** receives **0** points.
   - *Rationale:* Web browsing feels fast under 300 ms, so an outbound with 60 ms is not penalized relative to 180 ms, but multi-second roundtrips are penalized.

#### Why Geometric Mean?

In a standard arithmetic average, a completely dead outbound ($R = 0$) could still score $50$ points if latency and stability happened to look acceptable. 

With a **geometric mean**, if *any* single component collapses to zero, the entire overall score drops to zero. An outbound must be reliable, stable, *and* reasonably fast to earn a high score.

#### Score Interpretation

| Score | Rating | Meaning |
| --- | --- | --- |
| **90 – 100** | 🟢 Excellent | Zero packet loss, rock-solid latency, fast response. Ideal for gaming and VoIP. |
| **70 – 89** | 🟡 Good | Highly reliable with minor jitter or moderate latency. Great for streaming and browsing. |
| **40 – 69** | 🟠 Degraded | Noticeable packet loss or severe latency spikes. May cause buffering or slow page loads. |
| **0 – 39** | 🔴 Critical | High failure rate or extreme lag. Outbound needs investigation or fallback activation. |

---

## Technical Principles

### Endpoint Discovery

When `XRAY_METRICS_URL` is omitted, the exporter inspects `metrics.listen` in the panel settings retrieved during the current scrape:
- `0.0.0.0:PORT` or `:PORT` is normalized to `http://127.0.0.1:PORT`.
- `[::]:PORT` is normalized to `http://[::1]:PORT`.
- Loopback addresses require the exporter to share Xray's container network namespace or host network.
- Discovery does not guess ports or reuse stale addresses across scrapes. If discovery fails or settings are unavailable, `source="expvars"` emits success `0`. Setting an explicit `XRAY_METRICS_URL` removes panel dependency.

### Balancer Strategy Semantics

| Strategy | Primary Selection | Configured Fallback | Override Activity |
| --- | --- | --- | --- |
| **leastLoad** | Fully Supported | Supported | Supported |
| **leastPing** | Supported by API / Tests | Supported | Supported |
| **random** | Omitted *(unknown)* | Omitted *(unknown)* | Supported |
| **roundRobin** | Omitted *(unknown)* | Omitted *(unknown)* | Supported |

Xray's `GetBalancerInfo` exposes candidate pools, not actual connection logs, for `random` and `roundRobin`. We intentionally omit primary and fallback selection for these strategies unless an override is actively set.

### Fault Isolation and Request Budget

- Scrapes are bound by a strict context deadline (`SCRAPE_TIMEOUT`).
- Upstream requests run concurrently:
  - Total collection time is bounded by $\max(T_{api}, T_{expvars}, T_{grpc})$.
  - Independent sources survive sibling failures.
- Normal request budget:
  - 5 panel API requests (`POST /panel/api/xray/`, `GET /panel/api/server/status`, `GET /panel/api/inbounds/list`, `GET /panel/api/nodes/list`, `POST /panel/api/clients/onlines`)
  - 1 expvars HTTP request (`GET /debug/vars`)
  - 1 `ListOutbounds` gRPC call + $N$ parallel `GetBalancerInfo` calls (where $N$ is the count of configured balancers).

---

## Development and Build

### Prerequisites

- Go 1.27+ (uses `testing/synctest`)
- Docker (optional, for container builds)

### Common Make Targets

```sh
# Prepare build cache and download dependencies
make download

# Run tests with race detector and go vet
make test

# Build native binary into bin/3x-ui-exporter
make build

# Cross-compile static Linux binaries for amd64 and arm64
make linux

# Refresh third-party license notices
make licenses

# Package release archives with checksums
make release VERSION=v1.5.0
```

---

## Disclaimer and Maintenance

This project was developed for personal infrastructure and is provided as-is, without any maintenance contract, SLA, or warranty of ongoing support.

Issues and pull requests are welcome, but please note that maintenance and code reviews are handled on a best-effort basis without strict timelines or service-level guarantees.

---

## License

- Exporter code is licensed under the [MIT License](LICENSE).
- Unmodified, linked Xray components are licensed under [MPL-2.0](https://github.com/XTLS/Xray-core).
- Third-party dependencies retain their respective licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) and [third_party_licenses/](third_party_licenses/).
