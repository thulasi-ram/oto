package app

import (
	"context"
	"log/slog"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/telemetry"
)

// Container holds every long-lived dependency, wired by explicit constructors.
// There is no codegen and no runtime DI container: the dependency graph of oto is
// meant to be readable top to bottom in this one file.
type Container struct {
	Config    config.Config
	Logger    *slog.Logger
	Clock     clock.Clock
	Pools     *db.Pools
	Telemetry *telemetry.Telemetry

	// Domain services are added here as they are built. Each is constructed from
	// its repository plus the ports it declares; nothing reaches for a global.
}

// New assembles the container. Ownership of Pools and Telemetry passes to it:
// Close releases both.
func New(cfg config.Config, logger *slog.Logger, pools *db.Pools, tel *telemetry.Telemetry) *Container {
	return &Container{
		Config:    cfg,
		Logger:    logger,
		Clock:     clock.New(),
		Pools:     pools,
		Telemetry: tel,
	}
}

// Close releases everything the container owns, in reverse construction order.
func (c *Container) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	err := c.Telemetry.Shutdown(ctx)
	c.Pools.Close()
	return err
}
