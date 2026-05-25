// Package main 是 Go Home 公网服务器的入口程序。
//
// 公网服务器职责：
//   - 提供 WebSocket JSON-RPC 2.0 端点 (/ws)，管理设备连接和信令转发
//   - 提供 Web 管理控制台静态文件服务（SPA 回退）
//   - 管理家庭、设备、配置等数据的持久化（SQLite）
//   - 协调 P2P 打洞信令，不参与数据中继
//
// 启动方式：
//
//	go run ./cmd/server
//
// 环境变量配置参见 config 包。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gohome/server/internal/config"
	"gohome/server/internal/store"
	"gohome/server/internal/ws"
)

func main() {
	cfg := config.Load()

	// 命令行参数覆盖环境变量配置
	addr := flag.String("addr", cfg.Addr, "HTTP listen address")
	udpPort := flag.Int("udp-port", cfg.UDPPort, "UDP listen port for NAT endpoint discovery (0 to disable)")
	udpPortCount := flag.Int("udp-port-count", 8, "number of consecutive UDP discovery ports to listen on")
	dbPath := flag.String("db", cfg.DBPath, "SQLite database path")
	webDist := flag.String("web-dist", cfg.WebDist, "Web console static files directory")
	flag.Parse()

	cfg.Addr = *addr
	cfg.UDPPort = *udpPort
	cfg.DBPath = *dbPath
	cfg.WebDist = *webDist

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		log.Fatalf("create data directory: %v", err)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := db.InitDefaults(cfg.DefaultAdminPassword, cfg.DefaultAuthCode); err != nil {
		log.Fatalf("init defaults: %v", err)
	}

	var udpPorts []int
	var udpConns []net.PacketConn
	// 启动 UDP 监听（用于 NAT 端点发现）
	if cfg.UDPPort > 0 {
		count := *udpPortCount
		if count < 1 {
			count = 1
		}
		if count > 32 {
			count = 32
		}
		for i := 0; i < count && cfg.UDPPort+i <= 65535; i++ {
			port := cfg.UDPPort + i
			udpConn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
			if err != nil {
				if i == 0 {
					log.Fatalf("UDP listen on port %d: %v", port, err)
				}
				log.Printf("UDP NAT discovery port %d unavailable: %v", port, err)
				continue
			}
			defer udpConn.Close()
			udpPorts = append(udpPorts, port)
			udpConns = append(udpConns, udpConn)
		}
	}

	hub := ws.NewHub(db, udpPorts...)

	for i, udpConn := range udpConns {
		port := udpPorts[i]
		go hub.ServeUDP(udpConn)
		log.Printf("UDP NAT discovery listening on :%d", port)
	}

	mux := http.NewServeMux()
	mux.Handle("/ws", hub)

	if cfg.WebDist != "" {
		fs := http.FileServer(http.Dir(cfg.WebDist))
		mux.Handle("/", spaFallback(cfg.WebDist, fs))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("Go Home server is running. WebSocket endpoint: /ws\n"))
		})
	}

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: mux,
	}

	// 启动 HTTP 服务器
	go func() {
		log.Printf("Go Home server listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// 等待中断信号，优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("received signal %v, shutting down gracefully...", sig)

	// 给正在处理的请求 10 秒时间完成
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("server shutdown error: %v", err)
	}
	log.Println("server stopped")
}

// spaFallback 实现 SPA 应用的路由回退策略：
//   - 如果请求的文件存在且不是目录，直接返回文件
//   - 否则返回 index.html（让前端路由处理）
func spaFallback(root string, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(root, filepath.Clean(r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			next.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	}
}
