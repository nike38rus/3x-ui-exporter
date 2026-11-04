# Complete 3x-ui + Monitoring Stack

This example provides an out-of-the-box, complete deployment containing:
- **3x-ui Panel**: The main 3x-ui service with Xray core (`ghcr.io/mhsanaei/3x-ui:latest`).
- **3x-ui Exporter**: Sidecar container attached directly to the panel network stack (`network_mode: service:3x-ui`).
- **Prometheus**: Collects and stores metrics every 5 seconds.
- **Grafana**: Pre-configured with automatic datasource provisioning and the pre-loaded 3x-ui dashboard.

---

## Quick Start

1. **Prepare configuration**:
   ```bash
   cp .env.example .env
   ```

2. **Start the stack**:
   ```bash
   docker compose up -d
   ```

3. **Obtain API token**:
   - Open 3x-ui in your browser: `http://<your-host-ip>:2053` (default login: `admin` / `admin`).
   - Go to **Panel Settings** $\to$ generate/copy your **API Token**.
   - Paste the token into `.env`:
     ```dotenv
     XUI_API_TOKEN=your-token-here
     ```
   - Restart the exporter to apply:
     ```bash
     docker compose up -d exporter
     ```

4. **View Dashboards in Grafana**:
   - Open Grafana: `http://<your-host-ip>:3000` (default login: `admin` / `admin`).
   - The **3x-ui** dashboard is automatically provisioned and ready under the **3x-ui** folder!
