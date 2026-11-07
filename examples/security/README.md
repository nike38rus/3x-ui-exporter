# Secret Files Reference for 3x-ui-exporter

This directory documents the secret file format used by `--web.basic-auth-password-file` (`WEB_BASIC_AUTH_PASSWORD_FILE`) and `--web.bearer-token-file` (`WEB_BEARER_TOKEN_FILE`).

## Secret File Format

Secret files are plain text files containing the raw credential:

- **Format**: Single-line UTF-8 string containing the raw password or bearer token.
- **Whitespace Handling**: Leading and trailing whitespace, carriage returns (`\r`), and line feeds (`\n`) are automatically trimmed on startup.
- **Validation**: Empty files or files containing only whitespace are rejected at startup with an informative configuration error.
- **Permissions**: Recommended file permissions are `0600` (readable only by the exporter process owner or `DynamicUser`).

### Example Files

- [`basic-auth-password.example`](basic-auth-password.example): Sample secret file containing an HTTP Basic Auth password.
- [`bearer-token.example`](bearer-token.example): Sample secret file containing an HTTP Bearer Token.

---

## Integration Patterns

### 1. Docker Compose / Swarm Secrets

```yaml
services:
  exporter:
    image: ghcr.io/nike38rus/3x-ui-exporter:latest
    environment:
      XUI_URL: http://127.0.0.1:2053
      XUI_API_TOKEN: ${XUI_API_TOKEN}
      WEB_BEARER_TOKEN_FILE: /run/secrets/bearer_token
    secrets:
      - bearer_token

secrets:
  bearer_token:
    file: ./secrets/bearer_token.txt
```

### 2. Kubernetes Secret Volume Mount

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: 3x-ui-exporter
spec:
  template:
    spec:
      containers:
        - name: exporter
          image: ghcr.io/nike38rus/3x-ui-exporter:latest
          args:
            - "--xui-url=http://127.0.0.1:2053"
            - "--xui-api-token=$(XUI_API_TOKEN)"
            - "--web.bearer-token-file=/etc/secrets/bearer-token"
          volumeMounts:
            - name: scrape-token
              mountPath: /etc/secrets
              readOnly: true
      volumes:
        - name: scrape-token
          secret:
            secretName: prometheus-scrape-token
```

### 3. Systemd Credentials

```ini
[Service]
LoadCredential=bearer-token:/etc/3x-ui-exporter/bearer.token
Environment="WEB_BEARER_TOKEN_FILE=%d/bearer-token"
```
