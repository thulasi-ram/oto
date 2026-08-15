package repository

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
)

// TxRunner is the concrete half of the unit-of-work port `channels/service`
// declares; the runner itself is `db.TxRunner`.
//
// ⭐ THIS MODULE NEEDS ONE BECAUSE AN `Idempotency-Key` CLAIM MUST COMMIT WITH
// THE ACT IT GUARDS. `platform/idempotency.Repository.Claim` refuses to run
// outside a transaction — a claim that committed beside a create that did not is
// exactly the orphan the mechanism exists to prevent — so `createChannel` needs
// a unit of work to join, and it had none: the handler wrote straight through to
// the repository. A KEYED request is refused with a `503` before it gets here
// unless a unit of work is wired; see `channels/service.Writer.requireClaims`.
type TxRunner = db.TxRunner

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return db.NewTxRunner(pool) }
