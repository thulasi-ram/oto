package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// TokenRepository resolves an ingest bearer token against `api_tokens`.
//
// ⚠️ LAYERING NOTE, the same one as SourceConfigRepository: `api_tokens` belongs
// to the `identity` module, whose service does not exist yet. The consumer-side
// port is `api.TokenAuthenticator`, so replacing this with an `identity/service`
// adapter touches one wiring line and no handler. Until then the ingest endpoint
// would be unauthenticated or unusable, and neither is acceptable.
//
// It is SELECT-only over a single unique index, and it can resolve nothing except
// an ingest token.
type TokenRepository struct {
	q db.Querier
}

// NewTokenRepository builds the repository over the ingest pool: authentication
// is on the hot path and must not queue behind a dashboard query (§G.10).
func NewTokenRepository(q db.Querier) *TokenRepository { return &TokenRepository{q: q} }

func (r *TokenRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// lookupTokenSQL rides `api_tokens_hash_idx`, the UNIQUE index on token_hash —
// one indexed lookup, which is what §G.1 budgets for authentication.
//
// The revocation and expiry predicates are IN THE QUERY rather than in Go on
// purpose: a revoked token that still authenticates because a caller forgot a
// branch is the failure mode this endpoint cannot have, and the ingest token
// lives in an `alertmanager.yml` on every cluster.
//
// `kind = 'ingest'` is equally load-bearing. A personal access token must never
// be usable here, and an ingest token must never be usable anywhere else.
const lookupTokenSQL = `
SELECT t.id, t.org_id, t.source_id
  FROM api_tokens t
 WHERE t.token_hash = $1
   AND t.kind       = 'ingest'
   AND t.revoked_at IS NULL
   AND (t.expires_at IS NULL OR t.expires_at > $2)
   AND t.source_id IS NOT NULL`

// Lookup resolves a token by the sha256 of the presented secret.
//
// It takes the DIGEST, never the secret: the plaintext token does not cross this
// boundary, so it cannot reach a query log or an error string.
func (r *TokenRepository) Lookup(ctx context.Context, digest []byte, now time.Time) (domain.IngestToken, error) {
	var (
		out      domain.IngestToken
		sourceID *uuid.UUID
	)
	if err := r.db(ctx).QueryRow(ctx, lookupTokenSQL, digest, now).
		Scan(&out.ID, &out.OrgID, &sourceID); err != nil {
		return domain.IngestToken{}, mapErr(err, "resolve the ingest token")
	}
	if sourceID != nil {
		out.SourceID = *sourceID
	}
	return out, nil
}

const touchTokenSQL = `UPDATE api_tokens SET last_used_at = $2 WHERE id = $1`

// TouchLastUsed records that a token was used.
//
// It is deliberately NOT called on the accept path. One write per webhook, on the
// ingest pool, to update a column nothing reads synchronously, would spend the
// latency budget of the one request in oto that has one — and it would make every
// ingest transaction contend on a single row per source. Callers batch it or drop
// it; "last used" is an operator convenience, not an audit record.
func (r *TokenRepository) TouchLastUsed(ctx context.Context, tokenID uuid.UUID, at time.Time) error {
	if _, err := r.db(ctx).Exec(ctx, touchTokenSQL, tokenID, at); err != nil {
		return mapErr(err, "record token use")
	}
	return nil
}
