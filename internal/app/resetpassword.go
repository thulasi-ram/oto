package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⭐ THE ONLY WAY AN EXISTING USER'S PASSWORD CAN BE CHANGED WITHOUT SIGNING IN.
//
// v1 has no "forgot password" flow and no admin console: `PATCH /org/settings`
// and `/api-tokens*` are session-only, and every one of them needs the very
// credential a locked-out user no longer has. Until this existed, the only
// documented recovery was `just reset` — destroying the whole database to
// regain one login.
//
// ⛔ IT IS A SUBCOMMAND AND NOT A ROUTE, for the identical reason `bootstrap`
// is one: an endpoint that rewrites somebody's password is account takeover
// with a friendly name, and running this needs a shell on the host and the
// database credentials — the same authority that could write the row by hand.
//
// ⭐ IT REVOKES EVERY LIVE SESSION FOR THE USER, IN THE SAME TRANSACTION.
// Changing the password and leaving an attacker's stolen cookie live is not a
// recovery; a session minted under the old credential must not survive it.

// ResetPasswordRequest names the one user whose password is being replaced.
type ResetPasswordRequest struct {
	// OrgSlug is the URL-safe tenant handle. Scoping by org, not just by
	// address, is deliberate: an email is unique only WITHIN an org
	// (users_email_uniq), so the slug is what makes "this user" unambiguous.
	OrgSlug string
	// Email is the user's address within that org.
	Email string
	// Password is the new password. It is hashed with argon2id before it
	// touches the database and is never logged.
	Password string
}

// ResetPasswordResult is what the operator needs to confirm the right account
// moved.
type ResetPasswordResult struct {
	OrgID  uuid.UUID
	UserID uuid.UUID
}

// MinResetPasswordBytes matches MinBootstrapPasswordBytes: this credential can
// do everything inside the org, same as the one bootstrap mints.
const MinResetPasswordBytes = MinBootstrapPasswordBytes

// ErrOrgNotFound is returned when no live org has this slug.
var ErrOrgNotFound = errors.New("no org with that slug")

// ResetPassword replaces a user's password hash and revokes every session of
// theirs, all in one transaction: a reset that changed the password but left a
// session live, or revoked sessions but left the old password unchanged, is
// worse than refusing outright.
func ResetPassword(ctx context.Context, pool *pgxpool.Pool, req ResetPasswordRequest, now time.Time) (ResetPasswordResult, error) {
	if pool == nil {
		return ResetPasswordResult{}, errors.New("reset-password: a database pool is required")
	}
	req, err := req.normalise()
	if err != nil {
		return ResetPasswordResult{}, err
	}

	email, err := identitydomain.NewEmail(req.Email)
	if err != nil {
		return ResetPasswordResult{}, fmt.Errorf("reset-password: email: %w", err)
	}

	hasher := authn.NewPasswordHasher()
	encoded, err := hasher.Hash(req.Password)
	if err != nil {
		return ResetPasswordResult{}, fmt.Errorf("reset-password: hash password: %w", err)
	}
	passwordHash, err := identitydomain.NewPasswordHash(encoded)
	if err != nil {
		return ResetPasswordResult{}, fmt.Errorf("reset-password: password hash: %w", err)
	}

	users := identityrepo.NewUserRepository(pool)
	sessions := identityrepo.NewSessionRepository(pool)

	var result ResetPasswordResult
	err = db.Tx(ctx, pool, func(ctx context.Context) error {
		q := db.FromContext(ctx, pool)

		var orgID uuid.UUID
		serr := q.QueryRow(ctx, selectLiveOrgIDBySlugSQL, req.OrgSlug).Scan(&orgID)
		if errors.Is(serr, pgx.ErrNoRows) {
			return ErrOrgNotFound
		}
		if serr != nil {
			return fmt.Errorf("reset-password: find org: %w", serr)
		}

		scope, serr := db.NewTenantScope(orgID)
		if serr != nil {
			return fmt.Errorf("reset-password: scope: %w", serr)
		}

		user, serr := users.GetByEmail(ctx, scope, email)
		if errors.Is(serr, errs.ErrNotFound) {
			return fmt.Errorf("%w: no user %s in org %s", errs.ErrNotFound, req.Email, req.OrgSlug)
		}
		if serr != nil {
			return fmt.Errorf("reset-password: find user: %w", serr)
		}

		if _, uerr := q.Exec(ctx, updatePasswordHashSQL, passwordHash.Encoded(), now.UTC(), orgID, user.ID); uerr != nil {
			return fmt.Errorf("reset-password: update password: %w", uerr)
		}
		if rerr := sessions.RevokeAllForUser(ctx, scope, user.ID, now.UTC()); rerr != nil {
			return fmt.Errorf("reset-password: revoke sessions: %w", rerr)
		}

		result = ResetPasswordResult{OrgID: orgID, UserID: user.ID}
		return nil
	})
	if err != nil {
		return ResetPasswordResult{}, err
	}
	return result, nil
}

// ⚠️ A RAW WRITE IN `internal/app`, FOR THE SAME REASON THE TWO IN bootstrap.go
// ARE: rewriting a password hash by address is not an operation the running
// product exposes anywhere — every service-layer path to `users.password_hash`
// goes through a signed-in session that already holds it — so adding a
// repository method for it would put "overwrite anyone's password" one call
// away from any service that already holds `UserRepository`.
const selectLiveOrgIDBySlugSQL = `
SELECT id FROM orgs WHERE slug = $1 AND deleted_at IS NULL`

const updatePasswordHashSQL = `
UPDATE users SET password_hash = $1, updated_at = $2 WHERE org_id = $3 AND id = $4`

// normalise trims and enforces the bounds this command owns.
func (r ResetPasswordRequest) normalise() (ResetPasswordRequest, error) {
	r.OrgSlug = strings.ToLower(strings.TrimSpace(r.OrgSlug))
	r.Email = strings.TrimSpace(r.Email)

	if r.OrgSlug == "" {
		return r, errors.New("reset-password: --org-slug is required")
	}
	if r.Email == "" {
		return r, errors.New("reset-password: --email is required")
	}
	// ⚠️ Measured in BYTES, like bootstrap's floor and the storage bound.
	if len(r.Password) < MinResetPasswordBytes {
		return r, fmt.Errorf("reset-password: --password must be at least %d characters", MinResetPasswordBytes)
	}
	return r, nil
}
