package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
)

// TxRunner runs a function inside one transaction. It is the concrete half of
// the unit-of-work port `channels/service` declares.
//
// ⭐ IT EXISTS BECAUSE AN `Idempotency-Key` CLAIM MUST COMMIT WITH THE ACT IT
// GUARDS. `platform/idempotency.Repository.Claim` refuses to run outside a
// transaction — a claim that committed beside a create that did not is exactly
// the orphan the mechanism exists to prevent — so `createChannel` needs a unit of
// work to join, and it had none: the handler wrote straight through to the
// repository.
type TxRunner struct{ pool *pgxpool.Pool }

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return &TxRunner{pool: pool} }

// InTx runs fn inside one transaction, which travels in the returned context so
// every repository call underneath joins the same unit of work. It nests safely.
func (r *TxRunner) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r == nil || r.pool == nil {
		// No pool means no unit of work to join, and this only happens in a test
		// that wired no database — where fn's own writes have nothing to write to
		// either, so running it unwrapped degrades nothing that was working. A
		// KEYED request is refused with a `503` before it gets here; see
		// `channels/service.Writer.requireClaims`.
		return fn(ctx)
	}
	return db.Tx(ctx, r.pool, fn)
}
