package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/thulasiram/oto/internal/platform/migrate"
)

// migrateUp applies BOTH migration systems, in this order (db/migrations/00016_river.sql):
//
//  1. goose, over `db/migrations` — oto's own schema.
//  2. River's own migrator — `river_job`, `river_leader`, `river_queue` and
//     friends.
//
// River owns its tables and versions them on its own ledger (`river_migration`),
// entirely separate from goose's. Hand-writing a copy would drift from the
// driver on the very next release, and the queue's claim query is the one piece
// of SQL in the system where a subtle divergence means a lost alert rather than
// a compile error.
//
// ⛔ ORDER MATTERS ON THE WAY DOWN TOO: River's tables come back off with
// `river migrate-down`, NEVER `goose down`.
func migrateUp(ctx context.Context, dsn string, logger *slog.Logger) error {
	if err := migrate.Up(ctx, dsn); err != nil {
		return fmt.Errorf("goose: %w", err)
	}
	logger.InfoContext(ctx, "migrate: oto schema is up to date")

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("river: connect: %w", err)
	}
	defer pool.Close()

	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("river: migrator: %w", err)
	}
	res, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return fmt.Errorf("river: migrate up: %w", err)
	}
	logger.InfoContext(ctx, "migrate: queue schema is up to date",
		slog.Int("river_versions_applied", len(res.Versions)))
	return nil
}
