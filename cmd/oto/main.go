// Command oto is the oto binary: the HTTP API server, the job worker and the
// operational subcommands. oto is the alert history layer a Prometheus stack
// does not have.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/log"
	"github.com/thulasiram/oto/internal/platform/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "oto: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", os.Getenv("OTO_CONFIG"), "path to a YAML config file (optional)")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return nil
	}

	// 1. Configuration: defaults, then file, then OTO_* env.
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Version == "dev" && Version != "" {
		cfg.Version = Version
	}

	// 2. Logging.
	logger := log.New(cfg.Log, os.Stdout).With(
		slog.String("service", cfg.Service),
		slog.String("env", cfg.Env),
		slog.String("version", cfg.Version),
	)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 3. Telemetry: the Prometheus registry and, optionally, OTLP traces.
	tel, err := telemetry.Setup(ctx, cfg)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}

	// 4. Database. A pool that cannot connect is NOT fatal: /healthz must answer
	//    while Postgres is down so the process is not killed by a liveness probe
	//    during a database outage. /readyz is what reports the truth.
	pools, err := db.Open(ctx, cfg.DB)
	if err != nil {
		logger.Error("database unavailable at startup; serving until it returns", slog.String("error", err.Error()))
		pools = nil
	}

	container := app.New(cfg, logger, pools, tel)
	defer func() {
		if cerr := container.Close(context.WithoutCancel(ctx)); cerr != nil {
			logger.Error("shutdown", slog.String("error", cerr.Error()))
		}
	}()

	// 5. Router and server.
	router := newRouter(container)
	srv := httpx.NewServer(cfg.HTTP, router)

	logger.Info("listening",
		slog.String("addr", cfg.HTTP.Addr),
		slog.Bool("metrics", cfg.Telemetry.MetricsEnabled),
		slog.Bool("db", pools != nil),
	)

	if err := httpx.Serve(ctx, srv, cfg.HTTP); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("server: %w", err)
	}

	logger.Info("stopped")
	return nil
}
