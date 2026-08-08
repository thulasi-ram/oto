package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// apiTokenRow is one `api_tokens` row.
//
// There is no plaintext field, and there is no place to put one: the secret is
// shown once by the service that minted it and never travels through this layer.
type apiTokenRow struct {
	id         uuid.UUID
	orgID      uuid.UUID
	userID     *uuid.UUID
	kind       string
	name       string
	tokenHash  []byte
	prefix     string
	sourceID   *uuid.UUID
	lastUsedAt *time.Time
	expiresAt  *time.Time
	createdAt  time.Time
	revokedAt  *time.Time
}

func (r apiTokenRow) toDomain() (domain.APIToken, error) {
	kind, err := domain.NewTokenKind(r.kind)
	if err != nil {
		return domain.APIToken{}, errs.Internal("api_token_row_invalid", err)
	}
	hash, err := domain.NewTokenHash(r.tokenHash)
	if err != nil {
		return domain.APIToken{}, errs.Internal("api_token_row_invalid", err)
	}
	prefix, err := domain.NewTokenPrefix(r.prefix)
	if err != nil {
		return domain.APIToken{}, errs.Internal("api_token_row_invalid", err)
	}

	return domain.APIToken{
		ID:         r.id,
		OrgID:      r.orgID,
		UserID:     derefID(r.userID),
		Kind:       kind,
		Name:       r.name,
		Hash:       hash,
		Prefix:     prefix,
		SourceID:   derefID(r.sourceID),
		LastUsedAt: r.lastUsedAt,
		ExpiresAt:  r.expiresAt,
		CreatedAt:  r.createdAt.UTC(),
		RevokedAt:  r.revokedAt,
	}, nil
}

func derefID(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

func nullableID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// APITokenRepository reads and writes `api_tokens`.
type APITokenRepository struct {
	q db.Querier
}

// NewAPITokenRepository builds the repository.
func NewAPITokenRepository(q db.Querier) *APITokenRepository { return &APITokenRepository{q: q} }

func (r *APITokenRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const apiTokenColumns = `t.id, t.org_id, t.user_id, t.kind, t.name, t.token_hash, t.prefix,
       t.source_id, t.last_used_at, t.expires_at, t.created_at, t.revoked_at`

func scanToken(dst *apiTokenRow, scan func(...any) error) error {
	return scan(&dst.id, &dst.orgID, &dst.userID, &dst.kind, &dst.name, &dst.tokenHash, &dst.prefix,
		&dst.sourceID, &dst.lastUsedAt, &dst.expiresAt, &dst.createdAt, &dst.revokedAt)
}

const insertTokenSQL = `
INSERT INTO api_tokens (id, org_id, user_id, kind, name, token_hash, prefix, source_id,
                        expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// Insert persists a newly minted token.
func (r *APITokenRepository) Insert(ctx context.Context, s db.TenantScope, t domain.APIToken) error {
	if t.OrgID != s.OrgID() {
		// The scope is the authority; a row claiming a different org is a service
		// bug and must never reach the driver.
		return errs.Internal("api_token_scope_mismatch", nil)
	}
	_, err := r.db(ctx).Exec(ctx, insertTokenSQL,
		t.ID, t.OrgID, nullableID(t.UserID), string(t.Kind), t.Name,
		t.Hash.Bytes(), t.Prefix.String(), nullableID(t.SourceID), t.ExpiresAt, t.CreatedAt)
	if err != nil {
		return mapErr(err, "token_not_found", "token")
	}
	return nil
}

const selectTokenSQL = `
SELECT ` + apiTokenColumns + `
  FROM api_tokens t
 WHERE t.org_id = $1 AND t.id = $2`

// Get returns one token within the caller's org.
func (r *APITokenRepository) Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.APIToken, error) {
	var row apiTokenRow
	if err := scanToken(&row, r.db(ctx).QueryRow(ctx, selectTokenSQL, s.OrgID(), id).Scan); err != nil {
		return domain.APIToken{}, mapErr(err, "token_not_found", "token")
	}
	return row.toDomain()
}

// resolveByPrefixSQL is the FIRST HALF of PAT verification, and the only half
// that touches the database.
//
// ⚠️ ONE OF THE FOUR UNSCOPED QUERIES IN THIS MODULE. A presented bearer token
// names no org — resolving one is what produces the org — so this query cannot
// take a TenantScope. Every query downstream of it does.
//
// ⭐ IT SELECTS BY PREFIX, NOT BY HASH. Selecting by hash would be one index
// probe, and would be wrong here for a subtle reason: it makes the database's
// b-tree descent the comparison that decides authentication, and a b-tree
// descent is not constant-time. Selecting by the PUBLIC display prefix returns a
// small candidate set — the prefix is shown in the UI precisely because it
// reveals nothing — and the service then decides with crypto/subtle over the
// full digest.
//
// `kind = 'pat'` is load-bearing: an ingest token must never authenticate the
// API. The middleware also refuses an `oto_ingest_…` before any lookup, so this
// is the second of two independent refusals.
//
// The joins are INNER and their predicates are part of the credential check: a
// disabled user's token and a soft-deleted org's token resolve to no row at all,
// so there is no code path in which either is examined and then let through.
//
// Revocation and expiry are predicates here as well as in domain.APIToken.Usable.
const resolveByPrefixSQL = `
SELECT ` + apiTokenColumns + `, ` + subjectColumns + `
  FROM api_tokens t
  JOIN orgs  o ON o.id = t.org_id  AND o.deleted_at  IS NULL
  JOIN users u ON u.id = t.user_id AND u.disabled_at IS NULL
 WHERE t.prefix     = $1
   AND t.kind       = 'pat'
   AND t.revoked_at IS NULL
   AND (t.expires_at IS NULL OR t.expires_at > $2)
 LIMIT 32`

// ResolveByPrefix returns every live PAT sharing a display prefix, with the
// subject each would authenticate as.
//
// ⚠️ THIS METHOD HAS AUTHENTICATED NOTHING. The caller MUST finish the job with
// a constant-time comparison of the presented secret's digest against
// Token.Hash. The 32-row ceiling bounds the work a prefix collision — or an
// attacker minting tokens to force one — can cause.
func (r *APITokenRepository) ResolveByPrefix(
	ctx context.Context, prefix domain.TokenPrefix, now time.Time,
) ([]domain.AuthenticatedToken, error) {
	rows, err := r.db(ctx).Query(ctx, resolveByPrefixSQL, prefix.String(), now.UTC())
	if err != nil {
		return nil, mapErr(err, "token_not_found", "token")
	}
	defer rows.Close()

	var out []domain.AuthenticatedToken
	for rows.Next() {
		var (
			row  apiTokenRow
			subj subjectCols
		)
		if err := scanToken(&row, func(dst ...any) error {
			return rows.Scan(append(dst, subj.targets()...)...)
		}); err != nil {
			return nil, mapErr(err, "token_not_found", "token")
		}

		token, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		subject, err := subj.toDomain(row.orgID)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.AuthenticatedToken{Token: token, Subject: subject})
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "token_not_found", "token")
	}
	return out, nil
}

const listTokensSQL = `
SELECT ` + apiTokenColumns + `
  FROM api_tokens t
 WHERE t.org_id     = $1
   AND t.kind       = $2
   AND t.revoked_at IS NULL
   AND ($3::uuid IS NULL OR t.user_id = $3)
   AND ($4::timestamptz IS NULL OR (t.created_at, t.id) < ($4, $5))
 ORDER BY t.created_at DESC, t.id DESC
 LIMIT $6`

// List pages live tokens of one kind, newest first, optionally narrowed to one
// owner. It rides `api_tokens_org_idx` (org_id, kind) WHERE revoked_at IS NULL.
func (r *APITokenRepository) List(
	ctx context.Context, s db.TenantScope, kind domain.TokenKind, userID uuid.UUID, k db.Keyset,
) ([]domain.APIToken, db.Cursor, error) {
	limit := pageLimit(k.Limit)

	var (
		after   *time.Time
		afterID uuid.UUID
	)
	if !k.Cursor.IsZero() {
		t := k.Cursor.SortKey.UTC()
		after, afterID = &t, k.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, listTokensSQL,
		s.OrgID(), string(kind), nullableID(userID), after, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "token_not_found", "token")
	}
	defer rows.Close()

	out := make([]domain.APIToken, 0, limit)
	var last apiTokenRow
	for rows.Next() {
		var row apiTokenRow
		if err := scanToken(&row, rows.Scan); err != nil {
			return nil, db.Cursor{}, mapErr(err, "token_not_found", "token")
		}
		if len(out) == limit {
			return out, db.Cursor{SortKey: last.createdAt.UTC(), ID: last.id, Hash: k.Cursor.Hash, HasMore: true}, nil
		}
		t, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		out = append(out, t)
		last = row
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "token_not_found", "token")
	}
	return out, db.Cursor{Hash: k.Cursor.Hash}, nil
}

// revokeTokenSQL is idempotent by construction: revoking an already-revoked
// token keeps the FIRST revocation time. A second DELETE must not move the
// timestamp, because the timestamp is when the credential stopped working.
const revokeTokenSQL = `
UPDATE api_tokens
   SET revoked_at = $3
 WHERE org_id = $1 AND id = $2 AND revoked_at IS NULL`

// Revoke marks a token revoked and reports whether it existed at all.
//
// The `found` result distinguishes "already revoked" (200/204, nothing to do)
// from "no such token in your org" (404) WITHOUT a second round trip, and
// without ever telling a caller about a token in somebody else's org: the
// org_id predicate makes a cross-tenant id indistinguishable from a nonexistent
// one, which is the only honest answer.
func (r *APITokenRepository) Revoke(ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := r.db(ctx).Exec(ctx, revokeTokenSQL, s.OrgID(), id, at.UTC())
	if err != nil {
		return false, mapErr(err, "token_not_found", "token")
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}

	// Nothing was updated: either the token does not exist here, or it was
	// already revoked. Only the second is a success.
	var exists bool
	err = r.db(ctx).QueryRow(ctx,
		`SELECT true FROM api_tokens WHERE org_id = $1 AND id = $2`, s.OrgID(), id).Scan(&exists)
	if err != nil {
		return false, mapErr(err, "token_not_found", "token")
	}
	return exists, nil
}

const touchTokenSQL = `
UPDATE api_tokens SET last_used_at = $3 WHERE org_id = $1 AND id = $2`

// TouchLastUsed records that a token authenticated a request.
//
// It is an operator convenience, not an audit record, and the service treats a
// failure here as non-fatal: a request must not fail because a bookkeeping
// UPDATE could not get a row lock.
func (r *APITokenRepository) TouchLastUsed(ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time) error {
	if _, err := r.db(ctx).Exec(ctx, touchTokenSQL, s.OrgID(), id, at.UTC()); err != nil {
		return mapErr(err, "token_not_found", "token")
	}
	return nil
}
