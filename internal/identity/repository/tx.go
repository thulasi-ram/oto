package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
)

// TxRunner runs a function inside one transaction. It is the concrete half of
// the unit-of-work port `identity/api` declares.
//
// ⭐⭐ IT EXISTS BECAUSE A MINTED CREDENTIAL AND THE IDEMPOTENCY CLAIM THAT
// GUARDS IT ARE ONE FACT. `createApiToken` had NO transaction at all: it inserted
// a row into `api_tokens` and returned the plaintext, and there was nowhere for a
// second write to join. A claim that committed beside a mint that did not — or a
// mint that committed beside a claim that did not — is precisely the orphaned
// live credential the claim exists to prevent, so the two now commit together or
// neither does.
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
		// ⛔ THE REFUSAL IN `idempotency.Repository.Claim` DOES NOT MAKE THIS PATH
		// SAFE, AND AN EARLIER COMMENT HERE CLAIMED IT DID. That refusal fires
		// where the claim is taken, which is AFTER the mint — and with no
		// transaction wrapping them there is nothing left to roll the mint back:
		// the credential would already have committed, and the caller would get a
		// `500` for a token that exists and whose secret it never saw. What
		// actually keeps the degraded path from becoming the unguarded one is the
		// TRANSPORT: `identity/api` refuses a request carrying `Idempotency-Key`
		// with a `503` unless BOTH the claim store and a unit of work are wired,
		// and it does so before anything is minted.
		return fn(ctx)
	}
	return db.Tx(ctx, r.pool, fn)
}
