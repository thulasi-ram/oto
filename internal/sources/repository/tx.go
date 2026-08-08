package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
)

// TxRunner runs a function inside one transaction. It is the concrete half of
// the unit-of-work port `sources/api` declares.
//
// ⭐ IT EXISTS BECAUSE A SOURCE AND ITS INGEST CREDENTIAL ARE ONE FACT. Creating
// the row and minting the token were two independent commits, so a failure
// between them left an `alert_sources` row that no Alertmanager could ever
// authenticate against — a source that looks configured, accepts a webhook URL
// into the operator's `webhook_config`, and answers 401 forever. The row and the
// credential now commit together or not at all.
type TxRunner struct{ pool *pgxpool.Pool }

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return &TxRunner{pool: pool} }

// InTx runs fn inside one transaction, which travels in the returned context so
// every repository call underneath joins the same unit of work. It nests safely.
func (r *TxRunner) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r == nil || r.pool == nil {
		// No pool means no unit of work to join. Running fn unwrapped is strictly
		// better than failing: the caller's own error handling still applies, and
		// this only happens in a test that wired no database.
		return fn(ctx)
	}
	return db.Tx(ctx, r.pool, fn)
}
