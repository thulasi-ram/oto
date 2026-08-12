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
		// No pool means no unit of work to join, and this only happens in a test
		// that wired no database — where fn's own writes have nothing to write to
		// either, so running it unwrapped degrades nothing that was working.
		//
		// ⛔ IT IS NOT WHAT KEEPS AN `Idempotency-Key` HONEST. `Claim` refuses to
		// run outside a transaction, but that refusal fires AFTER the mint it
		// guards, and with no transaction there is nothing left to roll back — for
		// a rotation that would mean the old ingest token revoked, the new secret
		// gone with the failed response, and the source unable to receive alerts.
		// The transport is what prevents it: `sources/api` answers a keyed request
		// with a `503` unless both the claim store and a unit of work are wired,
		// before anything is minted.
		return fn(ctx)
	}
	return db.Tx(ctx, r.pool, fn)
}
