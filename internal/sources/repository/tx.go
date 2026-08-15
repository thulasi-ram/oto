package repository

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/platform/db"
)

// TxRunner is the concrete half of the unit-of-work port `sources/service`
// declares; the runner itself is `db.TxRunner`.
//
// ⭐ THIS MODULE NEEDS ONE BECAUSE A SOURCE AND ITS INGEST CREDENTIAL ARE ONE
// FACT. Creating the row and minting the token were two independent commits, so
// a failure between them left an `alert_sources` row that no Alertmanager could
// ever authenticate against — a source that looks configured, accepts a webhook
// URL into the operator's `webhook_config`, and answers 401 forever. The row and
// the credential now commit together or not at all. For a rotation the stakes
// are higher still: the old ingest token revoked, the new secret gone with the
// failed response, and the source unable to receive alerts — which is why
// `sources/service` answers a keyed request with a `503` unless both the claim
// store and a unit of work are wired, before anything is minted.
type TxRunner = db.TxRunner

// NewTxRunner builds a transaction runner over a pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return db.NewTxRunner(pool) }
