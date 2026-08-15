package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TxRunner runs a function inside one transaction. It is the concrete half of
// the unit-of-work port each module's service declares, and it lives beside Tx
// because this is the layer permitted to name pgx.
//
// It exists so the service layer can say "these writes commit together" without
// holding a *pgxpool.Pool — a service that owns a pool is a service that can be
// tempted to query with it. The transaction travels in the context, so every
// repository underneath joins it automatically and there is no WithTx variant of
// anything.
type TxRunner struct{ pool *pgxpool.Pool }

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return &TxRunner{pool: pool} }

// InTx runs fn inside one transaction, committing on nil and rolling back
// otherwise. It nests safely: a ctx already carrying a transaction joins it
// rather than opening a second.
func (r *TxRunner) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r == nil || r.pool == nil {
		// No pool means no unit of work to join, and this only happens in a test
		// that wired no database — where fn's own writes have nothing to write to
		// either, so running it unwrapped degrades nothing that was working.
		//
		// ⛔ THE REFUSAL IN `idempotency.Repository.Claim` DOES NOT MAKE THIS PATH
		// SAFE, AND AN EARLIER COMMENT HERE CLAIMED IT DID. That refusal fires
		// where the claim is taken, which is AFTER the act it guards — and with no
		// transaction wrapping them there is nothing left to roll the act back:
		// for `identity`, the credential would already have committed and the
		// caller would get a `500` for a token that exists and whose secret it
		// never saw. What actually keeps the degraded path from becoming the
		// unguarded one is the TRANSPORT: a module that accepts `Idempotency-Key`
		// refuses such a request with a `503` unless BOTH the claim store and a
		// unit of work are wired, and it does so before anything is written (see
		// `identity/api`, `sources/service` and
		// `channels/service.Writer.requireClaims`).
		return fn(ctx)
	}
	return Tx(ctx, r.pool, fn)
}
