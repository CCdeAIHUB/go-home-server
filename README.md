# Go Home Server

Go Home Server is the public coordination server for Go Home. It provides the web console, WebSocket JSON-RPC signaling, family/device management, UDP endpoint discovery, and SQLite storage.

The public server never relays tunnel traffic. Client-to-home traffic must be direct UDP P2P.

## What You Need

- A public Linux server with TCP `8080` and UDP `8080` open.
- Docker Compose, or a Linux systemd host.
- A strong admin password and a strong device authorization code.

Do not publish a server with the default password or default authorization code.

## Fastest Install: Docker Compose

```bash
git clone https://github.com/CCdeAIHUB/go-home-server.git
cd go-home-server

GO_HOME_DEFAULT_ADMIN_PASSWORD='replace-with-a-strong-password' \
GO_HOME_DEFAULT_AUTH_CODE='replace-with-a-strong-auth-code' \
docker compose up -d --build
```

Open:

```text
http://YOUR_SERVER_IP:8080/
```

The WebSocket endpoint is:

```text
ws://YOUR_SERVER_IP:8080/ws
```

## Linux systemd Install

Download the latest `go-home-server-linux-amd64` artifact from the repository Actions page, copy it to your server, then run:

```bash
chmod +x go-home-server
sudo GO_HOME_DEFAULT_ADMIN_PASSWORD='replace-with-a-strong-password' \
  GO_HOME_DEFAULT_AUTH_CODE='replace-with-a-strong-auth-code' \
  GO_HOME_SERVER_BINARY=./go-home-server \
  sh scripts/install-linux.sh
```

If you host the binary at a URL, use:

```bash
sudo GO_HOME_SERVER_BINARY_URL='https://example.com/go-home-server' \
  GO_HOME_DEFAULT_ADMIN_PASSWORD='replace-with-a-strong-password' \
  GO_HOME_DEFAULT_AUTH_CODE='replace-with-a-strong-auth-code' \
  sh scripts/install-linux.sh
```

## Upgrade

Docker:

```bash
git pull
docker compose up -d --build
```

systemd:

```bash
sudo systemctl stop go-home-server
sudo GO_HOME_SERVER_BINARY=./go-home-server sh scripts/install-linux.sh
```

The SQLite database is stored separately under `/var/lib/go-home-server` by default.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `GO_HOME_ADDR` | `:8080` | HTTP/WebSocket listen address |
| `GO_HOME_UDP_PORT` | `8080` | UDP discovery port |
| `GO_HOME_DB` | `data/go-home.db` | SQLite database path |
| `GO_HOME_WEB_DIST` | empty | Built web-console directory |
| `GO_HOME_DEFAULT_ADMIN_PASSWORD` | `admin` | Initial admin password |
| `GO_HOME_DEFAULT_AUTH_CODE` | `GOHOME-CHANGE-ME` | Initial device authorization code |

## Firewall

Open both:

```text
TCP 8080
UDP 8080
```

If you change `GO_HOME_ADDR` or `GO_HOME_UDP_PORT`, open the matching ports instead.

## Troubleshooting

Web page only shows `Go Home server is running`:

- The web console was not mounted. Rebuild the Vue console and set `GO_HOME_WEB_DIST`, or use Docker Compose.

Cannot connect from client:

- Confirm TCP `8080` is reachable.
- Confirm the client uses `http://YOUR_SERVER_IP:8080/` or `ws://YOUR_SERVER_IP:8080/ws`.
- Confirm the authorization code matches the server setting.
- Check server logs with `docker compose logs -f` or `journalctl -u go-home-server -f`.

UDP direct tunnel times out:

- Confirm UDP `8080` is open on the public server.
- Confirm the home server is online in the web console.
- The public server still does not relay traffic; it only helps peers discover endpoints.

Forgot admin password:

- Stop the service, back up the SQLite database, then update the password through a controlled maintenance workflow. Do not delete production data casually.

## Local Development

```bash
go work sync
go test ./...
cd web-console && npm ci && npm run build
cd ../server
GO_HOME_WEB_DIST=../web-console/dist go run ./cmd/server
```

## CI Artifacts

Every push to `main` runs GitHub Actions. The workflow:

- runs Go tests on Linux, macOS, and Windows where supported;
- builds the Vue web console;
- uploads the Linux server binary;
- publishes a Docker image to GitHub Container Registry on main-branch pushes;
- publishes tagged releases for `v*` tags.

## Security Notes

- Never commit real passwords, real authorization codes, SSH credentials, private keys, or personal server addresses.
- Use placeholders such as `YOUR_SERVER_IP`, `example.com`, and `replace-with-a-strong-auth-code` in documentation.
- Rotate the authorization code from the web console if it was exposed.
