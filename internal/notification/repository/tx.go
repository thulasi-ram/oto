package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
)

// TxRunner runs a unit of work on the general pool.
//
// It exists so the service layer can say "these writes commit together" without
// holding a *pgxpool.Pool — a service that owns a pool is a service that can be
// tempted to query with it. The transaction travels in the context, so every
// repository underneath joins it automatically and there is no WithTx variant of
// anything.
type TxRunner struct {
	pool *pgxpool.Pool
}

// NewTxRunner builds a runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return &TxRunner{pool: pool} }

// Tx runs fn in one transaction, committing on nil and rolling back otherwise.
// It nests safely: a caller already inside a transaction joins that one.
func (r *TxRunner) Tx(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.Tx(ctx, r.pool, fn)
}
