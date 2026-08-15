package repository

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
)

// TxRunner is the concrete half of the unit-of-work port `notification/service`
// declares; the runner itself is `db.TxRunner`.
type TxRunner = db.TxRunner

// NewTxRunner builds a runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return db.NewTxRunner(pool) }
