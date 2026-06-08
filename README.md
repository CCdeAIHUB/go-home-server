# Go Home Server

Go Home Server is the public coordination server for Go Home. It provides account login, family/device management, WebSocket JSON-RPC signaling, UDP endpoint discovery, the Vue web console, and SQLite storage. It never relays tunnel traffic.

## Quick Start

```bash
go work sync
go test ./...
cd web-console && npm ci && npm run build
cd ../server
GO_HOME_WEB_DIST=../web-console/dist go run ./cmd/server
```

Default endpoints:

- Web console: `http://SERVER_IP:8080/`
- WebSocket API: `ws://SERVER_IP:8080/ws`
- UDP discovery: `SERVER_IP:8080/udp`

## Configuration

The server can be configured with flags or environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `GO_HOME_ADDR` | `:8080` | HTTP/WebSocket listen address |
| `GO_HOME_UDP_PORT` | `8080` | UDP discovery base port |
| `GO_HOME_DB` | `data/go-home.db` | SQLite database path |
| `GO_HOME_WEB_DIST` | empty | Built web-console directory |
| `GO_HOME_DEFAULT_ADMIN_PASSWORD` | `admin` | Initial admin password |
| `GO_HOME_DEFAULT_AUTH_CODE` | `GOHOME-CHANGE-ME` | Initial device authorization code |

Change the default password and auth code before exposing a public server.

## Deploy With Docker Compose

```bash
git clone https://github.com/CCdeAIHUB/go-home-server.git
cd go-home-server
GO_HOME_DEFAULT_ADMIN_PASSWORD='change-me' \
GO_HOME_DEFAULT_AUTH_CODE='change-me' \
docker compose up -d --build
```

The compose file exposes TCP `8080` and UDP `8080`.

## One-Click Linux Service

After downloading a GitHub Actions artifact or Release binary, run:

```bash
sudo GO_HOME_SERVER_BINARY_URL="https://example.com/go-home-server" \
  GO_HOME_DEFAULT_ADMIN_PASSWORD='change-me' \
  GO_HOME_DEFAULT_AUTH_CODE='change-me' \
  sh scripts/install-linux.sh
```

If `GO_HOME_SERVER_BINARY_URL` is not set, the script uses `./go-home-server` in the current directory.

## CI Artifacts

Every push to `main` runs GitHub Actions and uploads:

- `go-home-server-linux-amd64`
- Docker image tags under GitHub Container Registry on successful main-branch builds

Tagged versions (`v*`) are also published as GitHub Releases.
