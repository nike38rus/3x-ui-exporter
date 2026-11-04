# Full Monitoring Stack for 3x-ui

This example provides an out-of-the-box, complete monitoring stack containing:
- **3x-ui Exporter**: Scrapes 3x-ui panel and Xray core.
- **Prometheus**: Collects and stores metrics every 5 seconds.
- **Grafana**: Pre-configured with automatic datasource provisioning and pre-loaded 3x-ui dashboards.

## Architecture & Modes

Depending on how your 3x-ui is installed, choose one of two deployment patterns:

### Pattern A: Sidecar Mode (Recommended when 3x-ui runs in Docker)

When 3x-ui is already running in a Docker container on the same host:
- The exporter attaches directly to the 3x-ui network namespace (`network_mode: container:3x-ui`).
- It accesses the panel on `127.0.0.1:2053` and Xray gRPC on `127.0.0.1:62789` without exposing internal gRPC ports to the external network.

### Pattern B: Bridge Mode (When 3x-ui runs on bare metal / host / remote)

When 3x-ui is installed natively (via bash script) or on another host:
- Set `XUI_URL` to the panel address (e.g., `http://192.168.1.100:2053`).
- Set `XRAY_API` to the exposed Xray gRPC port (e.g., `192.168.1.100:62789`).

---

## Quick Start

1. Copy `.env.example` to `.env` in this directory:
   ```bash
   cp .env.example .env
   ```

2. Edit `.env` and fill in your panel credentials:
   ```dotenv
   XUI_API_TOKEN=your_3x_ui_api_token_here
   XUI_URL=http://127.0.0.1:2053
   XRAY_API=127.0.0.1:62789
   ```

3. Start the entire monitoring stack:
   ```bash
   docker compose up -d
   ```

4. Open Grafana:
   - URL: `http://localhost:3000`
   - Default credentials: `admin` / `admin`
   - The **3x-ui** dashboard is automatically provisioned and ready in the *General* folder.
