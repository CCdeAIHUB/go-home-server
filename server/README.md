# Server Binary

This directory contains the Go Home public server backend.

## Run From Source

Build the web console first:

```bash
cd ../web-console
npm ci
npm run build
```

Run the server:

```bash
cd ../server
GO_HOME_WEB_DIST=../web-console/dist go run ./cmd/server
```

Open:

```text
http://YOUR_SERVER_IP:8080/
```

## Production Install

For normal deployment, use the repository-level install docs:

- Docker Compose: `docker compose up -d --build`
- Linux systemd: `scripts/install-linux.sh`

## Common Errors

Page says only `Go Home server is running`:

- `GO_HOME_WEB_DIST` is missing or points to the wrong directory.
- Rebuild `web-console` and restart the server.

Login fails:

- Confirm the admin password.
- If this is first boot, change `GO_HOME_DEFAULT_ADMIN_PASSWORD` before exposing the service.

Clients cannot connect:

- Open TCP `8080`.
- Open UDP `8080`.
- Confirm clients use the same authorization code.

## Security

Do not commit real admin passwords, authorization codes, SSH credentials, private keys, or personal server IP addresses.
