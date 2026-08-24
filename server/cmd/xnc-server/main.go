package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"xnc/server/internal/api"
	"xnc/server/internal/bootstrap"
	"xnc/server/internal/config"
	"xnc/server/internal/db"
	"xnc/server/internal/registry"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /api/health and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	if *healthcheck {
		if err := probe(cfg.ListenAddr); err != nil {
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := db.OpenStore(ctx, cfg)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := bootstrap.EnsureAdmin(ctx, st, cfg); err != nil {
		slog.Error("bootstrap", "err", err)
		os.Exit(1)
	}

	reg := registry.New()
	h := api.NewRouter(st, cfg, reg)
	// 优雅停机：先停 TURN 池健康探测等后台 worker，再等 HTTP 连接排空。
	if closer, ok := h.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: h, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("listening", "addr", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

func probe(addr string) error {
	c := &http.Client{Timeout: 3 * time.Second}
	if addr == ":8080" {
		addr = "127.0.0.1:8080"
	}
	resp, err := c.Get("http://" + addr + "/api/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
