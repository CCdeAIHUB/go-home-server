package config

import "os"

type Config struct {
	Addr                 string
	DBPath               string
	WebDist              string
	DefaultAdminPassword string
	DefaultAuthCode      string
}

func Load() Config {
	return Config{
		Addr:                 env("GO_HOME_ADDR", ":8080"),
		DBPath:               env("GO_HOME_DB", "data/go-home.db"),
		WebDist:              env("GO_HOME_WEB_DIST", ""),
		DefaultAdminPassword: env("GO_HOME_DEFAULT_ADMIN_PASSWORD", "admin"),
		DefaultAuthCode:      env("GO_HOME_DEFAULT_AUTH_CODE", "GOHOME-CHANGE-ME"),
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
