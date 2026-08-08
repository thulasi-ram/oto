package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// sessionRow is one `sessions` row. The cookie's plaintext appears nowhere: the
// column, and this struct, hold its sha256.
type sessionRow struct {
	id        uuid.UUID
	orgID     uuid.UUID
	userID    uuid.UUID
	tokenHash []byte
	userAgent *string
	createdAt time.Time
	expiresAt time.Time
	revokedAt *time.Time
}

func (r sessionRow) toDomain() (domain.Session, error) {
	hash, err := domain.NewTokenHash(r.tokenHash)
	if err != nil {
		return domain.Session{}, errs.Internal("session_row_invalid", err)
	}
	ua := ""
	if r.userAgent != nil {
		ua = *r.userAgent
	}
	return domain.Session{
		ID:        r.id,
		OrgID:     r.orgID,
		UserID:    r.userID,
		Hash:      hash,
		UserAgent: ua,
		CreatedAt: r.createdAt.UTC(),
		ExpiresAt: r.expiresAt.UTC(),
		RevokedAt: r.revokedAt,
	}, nil
}

// SessionRepository reads and writes `sessions`.
type SessionRepository struct {
	q db.Querier
}

// NewSessionRepository builds the repository.
func NewSessionRepository(q db.Querier) *SessionRepository { return &SessionRepository{q: q} }

func (r *SessionRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const sessionColumns = `s.id, s.org_id, s.user_id, s.token_hash, s.user_agent,
       s.created_at, s.expires_at, s.revoked_at`

func scanSession(dst *sessionRow, scan func(...any) error) error {
	return scan(&dst.id, &dst.orgID, &dst.userID, &dst.tokenHash, &dst.userAgent,
		&dst.createdAt, &dst.expiresAt, &dst.revokedAt)
}

const insertSessionSQL = `
INSERT INTO sessions (id, org_id, user_id, token_hash, user_agent, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

// Insert persists a new session.
func (r *SessionRepository) Insert(ctx context.Context, s db.TenantScope, sess domain.Session) error {
	if sess.OrgID != s.OrgID() {
		return errs.Internal("session_scope_mismatch", nil)
	}
	var ua *string
	if sess.UserAgent != "" {
		v := sess.UserAgent
		ua = &v
	}
	_, err := r.db(ctx).Exec(ctx, insertSessionSQL,
		sess.ID, sess.OrgID, sess.UserID, sess.Hash.Bytes(), ua, sess.CreatedAt, sess.ExpiresAt)
	if err != nil {
		return mapErr(err, "session_not_found", "session")
	}
	return nil
}

// resolveSessionSQL is the cookie -> session lookup on every authenticated
// browser request.
//
// ⚠️ ONE OF THE FOUR UNSCOPED QUERIES IN THIS MODULE. A cookie names no org; the
// row it resolves to is what supplies one.
//
// It rides `sessions_hash_idx`, the UNIQUE index on token_hash. Selecting by
// hash is correct HERE and wrong for `api_tokens` (see resolveByPrefixSQL): a
// session secret has no public display half, so there is nothing to select by
// except the digest, and the digest is the full 32 bytes rather than a
// four-character prefix.
//
// ⭐ EXPIRY IS ENFORCED IN THE PREDICATE, SERVER-SIDE, and again by
// domain.Session.Live. A stale session is not merely "not returned" — it is
// never scanned, so no code path exists in which one is examined and then let
// through. Both enforcements read the injected clock's `now`, never Postgres's
// now(), so a test can advance time without waiting for it.
//
// The joins are INNER and carry credential predicates of their own: a disabled
// user's session and a soft-deleted org's session resolve to no row. Disabling a
// user therefore ends their browser session at the next request, without a sweep
// having to run.
const resolveSessionSQL = `
SELECT ` + sessionColumns + `, ` + subjectColumns + `
  FROM sessions s
  JOIN orgs  o ON o.id = s.org_id  AND o.deleted_at  IS NULL
  JOIN users u ON u.id = s.user_id AND u.disabled_at IS NULL
 WHERE s.token_hash = $1
   AND s.revoked_at IS NULL
   AND s.expires_at > $2`

// ResolveByHash returns the live session with this digest, and the subject it
// authenticates as.
//
// ⚠️ ONE OF THE FOUR UNSCOPED QUERIES IN THIS MODULE. Unlike the PAT resolver,
// this one IS decided by the lookup: a session secret has no public display
// half, so there is nothing to select by except the full 32-byte digest, and the
// digest is unguessable rather than merely unique. The service still re-checks
// Live() on the result, so expiry is enforced twice.
func (r *SessionRepository) ResolveByHash(
	ctx context.Context, hash domain.TokenHash, now time.Time,
) (domain.AuthenticatedSession, error) {
	var (
		row  sessionRow
		subj subjectCols
	)
	err := scanSession(&row, func(dst ...any) error {
		return r.db(ctx).QueryRow(ctx, resolveSessionSQL, hash.Bytes(), now.UTC()).
			Scan(append(dst, subj.targets()...)...)
	})
	if err != nil {
		return domain.AuthenticatedSession{}, mapErr(err, "session_not_found", "session")
	}

	sess, err := row.toDomain()
	if err != nil {
		return domain.AuthenticatedSession{}, err
	}
	subject, err := subj.toDomain(row.orgID)
	if err != nil {
		return domain.AuthenticatedSession{}, err
	}
	return domain.AuthenticatedSession{Session: sess, Subject: subject}, nil
}

const revokeSessionSQL = `
UPDATE sessions SET revoked_at = $3 WHERE org_id = $1 AND id = $2 AND revoked_at IS NULL`

// Revoke ends one session. It is idempotent: revoking twice keeps the first
// revocation time, and logging out of an already-dead session is a success.
func (r *SessionRepository) Revoke(ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time) error {
	if _, err := r.db(ctx).Exec(ctx, revokeSessionSQL, s.OrgID(), id, at.UTC()); err != nil {
		return mapErr(err, "session_not_found", "session")
	}
	return nil
}

const revokeUserSessionsSQL = `
UPDATE sessions SET revoked_at = $3 WHERE org_id = $1 AND user_id = $2 AND revoked_at IS NULL`

// RevokeAllForUser ends every live session a user holds. It is the seam a
// password change and an account disable both need; leaving it out would make
// either of those a change to this file rather than a call into it.
func (r *SessionRepository) RevokeAllForUser(ctx context.Context, s db.TenantScope, userID uuid.UUID, at time.Time) error {
	if _, err := r.db(ctx).Exec(ctx, revokeUserSessionsSQL, s.OrgID(), userID, at.UTC()); err != nil {
		return mapErr(err, "session_not_found", "session")
	}
	return nil
}

// deleteExpiredSQL rides `sessions_expiry_idx (expires_at) WHERE revoked_at IS
// NULL`, the index that exists for exactly this sweep.
const deleteExpiredSQL = `
DELETE FROM sessions
 WHERE id IN (SELECT id FROM sessions WHERE expires_at < $1 LIMIT $2)`

// DeleteExpired removes sessions whose window has closed, in bounded batches.
//
// ⚠️ UNSCOPED BY DESIGN — it is a maintenance sweep across every tenant, run by
// a job and never by a request. It is HYGIENE, NOT ENFORCEMENT: expiry is
// already enforced by resolveSessionSQL and by domain.Session.Live, so a sweep
// that never ran would leak table space and nothing else. Making a security
// property depend on a cron is how the property stops holding.
func (r *SessionRepository) DeleteExpired(ctx context.Context, before time.Time, batch int) (int64, error) {
	if batch <= 0 {
		batch = 1000
	}
	tag, err := r.db(ctx).Exec(ctx, deleteExpiredSQL, before.UTC(), batch)
	if err != nil {
		return 0, mapErr(err, "session_not_found", "session")
	}
	return tag.RowsAffected(), nil
}
