package repository

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
)

// TxRunner is the concrete half of the unit-of-work port `identity/api`
// declares; the runner itself is `db.TxRunner`.
//
// ⭐⭐ THIS MODULE NEEDS ONE BECAUSE A MINTED CREDENTIAL AND THE IDEMPOTENCY
// CLAIM THAT GUARDS IT ARE ONE FACT. `createApiToken` had NO transaction at all:
// it inserted a row into `api_tokens` and returned the plaintext, and there was
// nowhere for a second write to join. A claim that committed beside a mint that
// did not — or a mint that committed beside a claim that did not — is precisely
// the orphaned live credential the claim exists to prevent, so the two now
// commit together or neither does. `identity/api` refuses a request carrying
// `Idempotency-Key` with a `503` unless BOTH the claim store and a unit of work
// are wired, and it does so before anything is minted.
type TxRunner = db.TxRunner

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return db.NewTxRunner(pool) }
