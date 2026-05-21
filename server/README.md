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

