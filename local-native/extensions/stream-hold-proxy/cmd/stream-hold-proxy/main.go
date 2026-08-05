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

	"sub2api.local/stream-hold-proxy/internal/proxy"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("sub2api-stream-hold-proxy %s\n", version)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})).With("service", "sub2api-stream-hold-proxy", "version", version)

	cfg, err := proxy.LoadConfig()
	if err != nil {
		logger.Error("config_invalid", "error", err)
		os.Exit(1)
	}

	handler, err := proxy.New(cfg, logger)
	if err != nil {
		logger.Error("proxy_init_failed", "error", err)
		os.Exit(1)
	}
	defer handler.Close()

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("proxy_started",
			"listen_addr", cfg.ListenAddr,
			"upstream_url", cfg.UpstreamURL.String(),
			"protected_paths", cfg.ProtectedPaths,
			"enabled", handler.Enabled(),
		)
		errCh <- server.ListenAndServe()
	}()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-signalCh:
		logger.Info("shutdown_requested", "signal", sig.String())
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server_failed", "error", err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("shutdown_failed", "error", err)
		os.Exit(1)
	}
	logger.Info("proxy_stopped")
}
