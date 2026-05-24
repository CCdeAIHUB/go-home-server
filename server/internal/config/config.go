// Package config 提供公网服务器的配置加载功能。
//
// 所有配置通过环境变量读取，支持默认值。主要配置项包括：
//   - GO_HOME_ADDR: 监听地址（默认 ":8080"）
//   - GO_HOME_DB: SQLite 数据库路径（默认 "data/go-home.db"）
//   - GO_HOME_WEB_DIST: Web 控制台静态文件目录（默认为空）
//   - GO_HOME_DEFAULT_ADMIN_PASSWORD: 初始管理员密码（默认 "admin"）
//   - GO_HOME_DEFAULT_AUTH_CODE: 初始授权码（默认 "GOHOME-CHANGE-ME"，生产环境必须修改）
package config

import (
	"os"
	"strconv"
)

// Config 包含公网服务器的所有配置项。
type Config struct {
	// Addr HTTP 监听地址，格式为 "host:port"。
	Addr string
	// UDPPort UDP 监听端口，用于设备 NAT 端点发现。0 表示不启用。
	UDPPort int
	// DBPath SQLite 数据库文件路径。
	DBPath string
	// WebDist Web 控制台静态文件目录路径，为空则不提供 Web UI。
	WebDist string
	// DefaultAdminPassword 首次启动时使用的默认管理员密码。
	// 仅在数据库中没有管理员密码哈希时生效，之后修改通过 Web 控制台完成。
	DefaultAdminPassword string
	// DefaultAuthCode 首次启动时使用的默认授权码。
	// 仅在数据库中没有授权码时生效，之后修改通过 Web 控制台完成。
	// 生产环境必须修改此值。
	DefaultAuthCode string
}

// Load 从环境变量加载配置，未设置的环境变量使用默认值。
func Load() Config {
	return Config{
		Addr:                 env("GO_HOME_ADDR", ":8080"),
		UDPPort:              envInt("GO_HOME_UDP_PORT", 8080),
		DBPath:               env("GO_HOME_DB", "data/go-home.db"),
		WebDist:              env("GO_HOME_WEB_DIST", ""),
		DefaultAdminPassword: env("GO_HOME_DEFAULT_ADMIN_PASSWORD", "admin"),
		DefaultAuthCode:      env("GO_HOME_DEFAULT_AUTH_CODE", "GOHOME-CHANGE-ME"),
	}
}

// env 读取环境变量，如果未设置或为空则返回 fallback。
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// envInt 读取环境变量为整数，如果未设置或解析失败则返回 fallback。
func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return fallback
}
