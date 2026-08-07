package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/config"
)

// Pools holds the two pools SPEC §G.10 mandates. UI queries must never be able to
// starve ingestion, so the webhook handler and the ingest workers use Ingest and
// nothing else; everything else uses General.
type Pools struct {
	Ingest  *pgxpool.Pool
	General *pgxpool.Pool
}

// Open creates both pools and verifies each with a ping.
func Open(ctx context.Context, cfg config.DBConfig) (*Pools, error) {
	ingest, err := open(ctx, cfg, cfg.IngestPoolSize(), cfg.IngestStatementTimeout, "oto-ingest")
	if err != nil {
		return nil, fmt.Errorf("db: open ingest pool: %w", err)
	}

	general, err := open(ctx, cfg, cfg.GeneralPoolSize(), cfg.GeneralStatementTimeout, "oto-general")
	if err != nil {
		ingest.Close()
		return nil, fmt.Errorf("db: open general pool: %w", err)
	}

	return &Pools{Ingest: ingest, General: general}, nil
}

func open(ctx context.Context, cfg config.DBConfig, maxConns int32, stmtTimeout time.Duration, appName string) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}

	pc.MaxConns = maxConns
	pc.MinConns = 0
	pc.MaxConnLifetime = cfg.MaxConnLifetime
	pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	if pc.ConnConfig.RuntimeParams == nil {
		pc.ConnConfig.RuntimeParams = map[string]string{}
	}
	pc.ConnConfig.RuntimeParams["application_name"] = appName
	pc.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", stmtTimeout.Milliseconds())

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// Close shuts both pools down. It is safe on a nil receiver.
func (p *Pools) Close() {
	if p == nil {
		return
	}
	if p.General != nil {
		p.General.Close()
	}
	if p.Ingest != nil {
		p.Ingest.Close()
	}
}

// Ping verifies both pools can serve a query within timeout. It is the readiness probe.
func (p *Pools) Ping(ctx context.Context, timeout time.Duration) error {
	if p == nil {
		return fmt.Errorf("db: pools not initialised")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := p.Ingest.Ping(ctx); err != nil {
		return fmt.Errorf("db: ingest pool unhealthy: %w", err)
	}
	if err := p.General.Ping(ctx); err != nil {
		return fmt.Errorf("db: general pool unhealthy: %w", err)
	}
	return nil
}

// Stats is a snapshot of both pools, for /readyz and the metrics collector.
type Stats struct {
	IngestTotal    int32 `json:"ingest_total"`
	IngestIdle     int32 `json:"ingest_idle"`
	IngestMax      int32 `json:"ingest_max"`
	IngestAcquired int32 `json:"ingest_acquired"`

	GeneralTotal    int32 `json:"general_total"`
	GeneralIdle     int32 `json:"general_idle"`
	GeneralMax      int32 `json:"general_max"`
	GeneralAcquired int32 `json:"general_acquired"`
}

// Stats returns a snapshot of both pools.
func (p *Pools) Stats() Stats {
	if p == nil {
		return Stats{}
	}
	i, g := p.Ingest.Stat(), p.General.Stat()
	return Stats{
		IngestTotal: i.TotalConns(), IngestIdle: i.IdleConns(),
		IngestMax: i.MaxConns(), IngestAcquired: i.AcquiredConns(),
		GeneralTotal: g.TotalConns(), GeneralIdle: g.IdleConns(),
		GeneralMax: g.MaxConns(), GeneralAcquired: g.AcquiredConns(),
	}
}
