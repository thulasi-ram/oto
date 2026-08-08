package main

import (
	"context"
	"log/slog"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// serve runs the HTTP surface until ctx ends, then drains in-flight requests.
//
// The route table, the middleware order and every security grouping belong to
// `internal/app` — the composition root is the one place allowed to know which
// routes must sit outside the timeout, outside the authenticator and outside
// anything that touches the request body. This function is deliberately four
// lines long: a router assembled here would be a second place those rules could
// be written differently.
func serve(ctx context.Context, c *app.Container) error {
	cfg := c.Config.HTTP
	srv := httpx.NewServer(cfg, c.Router())

	c.Logger.Info("listening",
		slog.String("addr", cfg.Addr),
		slog.Bool("metrics", c.Config.Telemetry.MetricsEnabled),
		slog.Bool("db", c.Pools != nil),
		slog.Bool("workers", c.WorkersEnabled))

	// Serve returns only after the server has stopped accepting and drained; the
	// worker pool is drained afterwards, by the container's own Close.
	return httpx.Serve(ctx, srv, cfg)
}
