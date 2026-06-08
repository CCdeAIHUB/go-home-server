#!/usr/bin/env sh
set -eu

INSTALL_DIR="${GO_HOME_INSTALL_DIR:-/opt/go-home-server}"
DATA_DIR="${GO_HOME_DATA_DIR:-/var/lib/go-home-server}"
ENV_FILE="${GO_HOME_ENV_FILE:-/etc/go-home-server.env}"
SERVICE_USER="${GO_HOME_SERVICE_USER:-go-home}"
BIN_URL="${GO_HOME_SERVER_BINARY_URL:-}"
LOCAL_BIN="${GO_HOME_SERVER_BINARY:-./go-home-server}"

if [ "$(id -u)" -ne 0 ]; then
  echo "Please run as root, for example: sudo sh scripts/install-linux.sh" >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR" "$DATA_DIR"

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
fi

TMP_BIN="$(mktemp)"
if [ -n "$BIN_URL" ]; then
  curl -fsSL "$BIN_URL" -o "$TMP_BIN"
else
  cp "$LOCAL_BIN" "$TMP_BIN"
fi
install -m 0755 "$TMP_BIN" "$INSTALL_DIR/go-home-server"
rm -f "$TMP_BIN"
chown -R "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR"

cat > "$ENV_FILE" <<EOF
GO_HOME_ADDR=${GO_HOME_ADDR:-:8080}
GO_HOME_UDP_PORT=${GO_HOME_UDP_PORT:-8080}
GO_HOME_DB=${GO_HOME_DB:-$DATA_DIR/go-home.db}
GO_HOME_WEB_DIST=${GO_HOME_WEB_DIST:-$INSTALL_DIR/web-console}
GO_HOME_DEFAULT_ADMIN_PASSWORD=${GO_HOME_DEFAULT_ADMIN_PASSWORD:-admin}
GO_HOME_DEFAULT_AUTH_CODE=${GO_HOME_DEFAULT_AUTH_CODE:-GOHOME-CHANGE-ME}
EOF
chmod 600 "$ENV_FILE"

cat > /etc/systemd/system/go-home-server.service <<EOF
[Unit]
Description=Go Home public server
After=network-online.target
Wants=network-online.target

[Service]
User=$SERVICE_USER
Group=$SERVICE_USER
EnvironmentFile=$ENV_FILE
WorkingDirectory=$DATA_DIR
ExecStart=$INSTALL_DIR/go-home-server
Restart=always
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now go-home-server
systemctl --no-pager status go-home-server
