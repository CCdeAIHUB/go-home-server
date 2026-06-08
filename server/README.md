# 公网服务器

公网服务器提供：

- `/ws` WebSocket JSON-RPC API。
- Vue3 Web 控制台静态文件托管。
- SQLite 数据库存储。
- 设备状态、家庭管理、P2P 信令协调。

启动：

```powershell
go run ./cmd/server
```

常用环境变量：

- `GO_HOME_ADDR`
- `GO_HOME_DB`
- `GO_HOME_WEB_DIST`
- `GO_HOME_DEFAULT_ADMIN_PASSWORD`
- `GO_HOME_DEFAULT_AUTH_CODE`

## 部署方式

### Docker Compose

```bash
GO_HOME_DEFAULT_ADMIN_PASSWORD='change-me' \
GO_HOME_DEFAULT_AUTH_CODE='change-me' \
docker compose up -d --build
```

### Linux systemd

先从 GitHub Actions 或 Release 下载 `go-home-server` 二进制文件，然后执行：

```bash
sudo GO_HOME_SERVER_BINARY_URL="https://example.com/go-home-server" \
  GO_HOME_DEFAULT_ADMIN_PASSWORD='change-me' \
  GO_HOME_DEFAULT_AUTH_CODE='change-me' \
  sh scripts/install-linux.sh
```

未设置 `GO_HOME_SERVER_BINARY_URL` 时，脚本会使用当前目录下的 `./go-home-server`。
