package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"gohome/server/internal/config"
	"gohome/server/internal/store"
	"gohome/server/internal/ws"
)

func main() {
	cfg := config.Load()
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

	hub := ws.NewHub(db)
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

	log.Printf("Go Home server listening on %s", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, mux); err != nil {
		log.Fatal(err)
	}
}

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
