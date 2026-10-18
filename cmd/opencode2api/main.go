// Command opencode2api starts the anonymous free-tier API gateway.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"opencode2api/internal/buildinfo"
	"opencode2api/internal/config"
	"opencode2api/internal/gateway"
	"opencode2api/internal/telemetry"
)

// version remains the linker injection point used by release builds.
var version = "dev"

func main() {
	buildinfo.Version = version
	configPath := flag.String("config", "config.json", "path to config.json")
	listen := flag.String("listen", "", "override the configured API listen address")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	level := new(slog.LevelVar)
	telemetry.SetLogLevel(level, cfg.Logging.Level)
	redactor := config.NewSecretRedactor()
	redactor.Replace(cfg)
	logger := telemetry.NewStructuredLogger(level, redactor)
	monitor := telemetry.NewMonitor()
	manager, err := gateway.NewRuntimeManager(ctx, *configPath, cfg, logger, monitor, redactor, level)
	if err != nil {
		logger.Error("failed to initialize runtime", "component", "runtime", "event", "runtime_initialization_failed", "error", err)
		os.Exit(1)
	}
	defer manager.Shutdown()

	apiServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           manager.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go serveHTTP(cancel, logger, apiServer)

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "component", "server", "event", "shutdown_failed", "address", apiServer.Addr, "error", err)
	}
}

func serveHTTP(cancel context.CancelFunc, logger *slog.Logger, server *http.Server) {
	logger.Info("server listening", "component", "api", "event", "server_started", "address", server.Addr, "version", buildinfo.Version)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped unexpectedly", "component", "api", "event", "server_failed", "address", server.Addr, "error", err)
		cancel()
	}
}
