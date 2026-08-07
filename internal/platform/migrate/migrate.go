package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	migrations "github.com/thulasiram/oto/db/migrations"
)

// Dialect is fixed: Postgres is the only supported system of record (R7).
const Dialect = "postgres"

// Files returns the embedded migration filesystem.
func Files() (fs.FS, error) { return migrations.FS, nil }

func provider(dsn string) (*goose.Provider, *sql.DB, error) {
	sub, err := Files()
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: fs: %w", err)
	}

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: parse dsn: %w", err)
	}
	db := stdlib.OpenDB(*cfg)

	p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("migrate: new provider: %w", err)
	}
	return p, db, nil
}

// Up applies every pending migration.
func Up(ctx context.Context, dsn string) error {
	p, db, err := provider(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate: up: %w", err)
	}
	return nil
}

// Down rolls back exactly one migration.
func Down(ctx context.Context, dsn string) error {
	p, db, err := provider(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if _, err := p.Down(ctx); err != nil {
		return fmt.Errorf("migrate: down: %w", err)
	}
	return nil
}

// Status is one migration's applied state.
type Status struct {
	Version int64
	Source  string
	Applied bool
}

// Statuses reports every known migration and whether it has been applied.
func Statuses(ctx context.Context, dsn string) ([]Status, error) {
	p, db, err := provider(dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	rows, err := p.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: status: %w", err)
	}

	out := make([]Status, 0, len(rows))
	for _, r := range rows {
		out = append(out, Status{
			Version: r.Source.Version,
			Source:  r.Source.Path,
			Applied: r.State == goose.StateApplied,
		})
	}
	return out, nil
}
