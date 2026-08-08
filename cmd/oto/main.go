// Command oto is the oto binary: the HTTP API server, the job worker and the
// operational subcommands. oto is the alert history layer a Prometheus stack
// does not have.
//
// Subcommands:
//
//	oto            API and worker in ONE process (the default, and what a
//	               single-container deployment runs)
//	oto api        the HTTP API only; jobs are enqueued, never worked
//	oto worker     the job worker only; no listener
//	oto migrate    apply every pending migration and exit
//	oto version    print the version and exit
//
// `api` and `worker` exist so a deployment can scale the two independently — a
// storm needs more ingest workers, not more HTTP handlers — and so a bad
// deployment of one cannot take the other down with it.
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
	"github.com/thulasiram/oto/internal/platform/log"
	"github.com/thulasiram/oto/internal/platform/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "oto: %v\n", err)
		os.Exit(1)
	}
}

// mode is what this process does.
type mode struct {
	serveHTTP bool
	runJobs   bool
	name      string
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

	m, err := modeOf(flag.Arg(0))
	if err != nil {
		return err
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
		slog.String("mode", m.name),
	)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if flag.Arg(0) == "migrate" {
		return migrateUp(ctx, cfg.DB.URL, logger)
	}

	// 3. Telemetry: the Prometheus registry and, optionally, OTLP traces.
	tel, err := telemetry.Setup(ctx, cfg)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}

	// 4. Database. A pool that cannot connect is NOT fatal for the API: /healthz
	//    must answer while Postgres is down so the process is not killed by a
	//    liveness probe during an outage. /readyz reports the truth.
	//
	//    A WORKER is different: it has nothing to do without a database, and a
	//    worker that starts anyway would sit reporting healthy while working
	//    nothing.
	pools, err := db.Open(ctx, cfg.DB)
	if err != nil {
		if m.runJobs && !m.serveHTTP {
			return fmt.Errorf("database: %w", err)
		}
		logger.Error("database unavailable at startup; serving until it returns",
			slog.String("error", err.Error()))
		pools = nil
	}

	if cfg.DB.AutoMigrate && pools != nil {
		if err := migrateUp(ctx, cfg.DB.URL, logger); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	container, err := app.New(ctx, app.Options{
		Config:     cfg,
		Logger:     logger,
		Pools:      pools,
		Telemetry:  tel,
		RunWorkers: m.runJobs,
	})
	if err != nil {
		return err
	}

	// ⭐ SHUTDOWN ORDER. The container drains the worker pool BEFORE it closes the
	// pools, and this defer runs AFTER the server has stopped accepting. Closing a
	// pool under a running job turns a graceful shutdown into a burst of
	// connection errors and leaves jobs in `running` until the rescuer notices —
	// minutes of an alert sitting undelivered for no reason at all.
	defer func() {
		if cerr := container.Close(context.WithoutCancel(ctx)); cerr != nil {
			logger.Error("shutdown", slog.String("error", cerr.Error()))
		}
	}()

	if err := container.Start(ctx); err != nil {
		return err
	}

	if !m.serveHTTP {
		// `oto worker`: nothing listens. Block until a signal, then fall into the
		// deferred drain above.
		logger.Info("worker running", slog.Bool("jobs", container.WorkersEnabled))
		<-ctx.Done()
		logger.Info("stopped")
		return nil
	}

	if err := serve(ctx, container); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("server: %w", err)
	}
	logger.Info("stopped")
	return nil
}

// modeOf maps the subcommand onto what this process does.
func modeOf(arg string) (mode, error) {
	switch arg {
	case "", "serve":
		// The default is BOTH, because the overwhelmingly common deployment of a
		// self-hosted tool is one container.
		return mode{serveHTTP: true, runJobs: true, name: "api+worker"}, nil
	case "api":
		return mode{serveHTTP: true, runJobs: false, name: "api"}, nil
	case "worker":
		return mode{serveHTTP: false, runJobs: true, name: "worker"}, nil
	case "migrate":
		return mode{name: "migrate"}, nil
	case "version":
		fmt.Println(versionString())
		os.Exit(0)
		return mode{}, nil
	default:
		return mode{}, fmt.Errorf("unknown subcommand %q; try: serve | api | worker | migrate | version", arg)
	}
}
