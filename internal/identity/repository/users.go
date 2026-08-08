package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// userRow is one `users` row.
//
// `passwordHash` is on this struct because the column is, and for no other
// reason: it is read only by the login path and is never carried past
// toDomain(), which wraps it in a domain.PasswordHash whose String() is a
// redaction.
type userRow struct {
	id           uuid.UUID
	orgID        uuid.UUID
	email        string
	displayName  string
	passwordHash *string
	createdAt    time.Time
	updatedAt    time.Time
	disabledAt   *time.Time
}

func (r userRow) toDomain() (domain.User, error) {
	email, err := domain.NewEmail(r.email)
	if err != nil {
		// A stored address that no longer parses is a schema-drift bug, not a
		// caller error: users_email_ck should have made it unreachable.
		return domain.User{}, errs.Internal("user_row_invalid", err)
	}

	hash := domain.NoPassword()
	if r.passwordHash != nil {
		hash, err = domain.NewPasswordHash(*r.passwordHash)
		if err != nil {
			return domain.User{}, errs.Internal("user_row_invalid", err)
		}
	}

	return domain.User{
		ID:           r.id,
		OrgID:        r.orgID,
		Email:        email,
		DisplayName:  r.displayName,
		PasswordHash: hash,
		CreatedAt:    r.createdAt.UTC(),
		UpdatedAt:    r.updatedAt.UTC(),
		DisabledAt:   r.disabledAt,
	}, nil
}

// UserRepository reads and writes `users`.
type UserRepository struct {
	q db.Querier
}

// NewUserRepository builds the repository.
func NewUserRepository(q db.Querier) *UserRepository { return &UserRepository{q: q} }

func (r *UserRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const userColumns = `u.id, u.org_id, u.email, u.display_name, u.password_hash,
       u.created_at, u.updated_at, u.disabled_at`

func scanUser(dst *userRow, scan func(...any) error) error {
	return scan(&dst.id, &dst.orgID, &dst.email, &dst.displayName, &dst.passwordHash,
		&dst.createdAt, &dst.updatedAt, &dst.disabledAt)
}

const selectUserSQL = `
SELECT ` + userColumns + `
  FROM users u
 WHERE u.org_id = $1 AND u.id = $2`

// Get returns one user within the caller's org.
func (r *UserRepository) Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.User, error) {
	var row userRow
	err := scanUser(&row, r.db(ctx).QueryRow(ctx, selectUserSQL, s.OrgID(), id).Scan)
	if err != nil {
		return domain.User{}, mapErr(err, "user_not_found", "user")
	}
	return row.toDomain()
}

const selectUserByEmailSQL = `
SELECT ` + userColumns + `
  FROM users u
 WHERE u.org_id = $1 AND u.email = $2`

// GetByEmail returns one user within the caller's org. `users.email` is CITEXT,
// so the comparison is case-insensitive in Postgres and the domain lower-cases
// on the way in — both, deliberately, so neither side has to trust the other.
func (r *UserRepository) GetByEmail(ctx context.Context, s db.TenantScope, email domain.Email) (domain.User, error) {
	var row userRow
	err := scanUser(&row, r.db(ctx).QueryRow(ctx, selectUserByEmailSQL, s.OrgID(), email.String()).Scan)
	if err != nil {
		return domain.User{}, mapErr(err, "user_not_found", "user")
	}
	return row.toDomain()
}

// resolveByEmailSQL is deliberately org-BLIND and deliberately LIMIT 2.
//
// `users_email_uniq` is (org_id, email): an address is unique within an org and
// not across the deployment. The login request carries no org — the contract's
// LoginRequest is email and password and nothing else — so this query is the one
// that PRODUCES the tenancy, and cannot itself be scoped by it.
//
// LIMIT 2 rather than LIMIT 1 is the point. One row is an unambiguous login; two
// rows means the address exists in more than one org and there is no honest way
// to pick, so the caller sees the same 401 as a wrong password. LIMIT 1 would
// silently authenticate whichever row the planner happened to return first,
// which is a cross-tenant login decided by a physical ordering.
//
// Disabled users are excluded in SQL as well as in the domain: a predicate a
// caller can forget is a predicate that will be forgotten.
const resolveByEmailSQL = `
SELECT ` + userColumns + `
  FROM users u
 WHERE u.email = $1 AND u.disabled_at IS NULL
 ORDER BY u.id
 LIMIT 2`

// ResolveByEmail finds the single live user with this address, across all orgs.
//
// ⚠️ ONE OF THE FOUR UNSCOPED QUERIES IN THIS MODULE. It takes no TenantScope
// because it is what a TenantScope is derived FROM. Everything downstream of it
// is scoped.
//
// It returns errs.NotFound both when nothing matched and when the address is
// ambiguous. The service turns either into the same unspecific 401.
func (r *UserRepository) ResolveByEmail(ctx context.Context, email domain.Email) (domain.User, error) {
	rows, err := r.db(ctx).Query(ctx, resolveByEmailSQL, email.String())
	if err != nil {
		return domain.User{}, mapErr(err, "user_not_found", "user")
	}
	defer rows.Close()

	var found []userRow
	for rows.Next() {
		var row userRow
		if err := scanUser(&row, rows.Scan); err != nil {
			return domain.User{}, mapErr(err, "user_not_found", "user")
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		return domain.User{}, mapErr(err, "user_not_found", "user")
	}
	if len(found) != 1 {
		return domain.User{}, errs.NotFound("user_not_found", "no such user")
	}
	return found[0].toDomain()
}

const listMembersSQL = `
SELECT ` + userColumns + `
  FROM users u
 WHERE u.org_id = $1
   AND u.disabled_at IS NULL
   AND ($2::timestamptz IS NULL OR (u.created_at, u.id) < ($2, $3))
 ORDER BY u.created_at DESC, u.id DESC
 LIMIT $4`

// ListMembers pages the org's live users, newest first.
//
// Keyset, never OFFSET (CONTEXT.md §5.8): the ordering tuple is
// (created_at DESC, id DESC) and the uuidv7 breaks ties deterministically.
func (r *UserRepository) ListMembers(ctx context.Context, s db.TenantScope, k db.Keyset) ([]domain.User, db.Cursor, error) {
	limit := pageLimit(k.Limit)

	var (
		after   *time.Time
		afterID uuid.UUID
	)
	if !k.Cursor.IsZero() {
		t := k.Cursor.SortKey.UTC()
		after, afterID = &t, k.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, listMembersSQL, s.OrgID(), after, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "user_not_found", "user")
	}
	defer rows.Close()

	out := make([]domain.User, 0, limit)
	var last userRow
	for rows.Next() {
		var row userRow
		if err := scanUser(&row, rows.Scan); err != nil {
			return nil, db.Cursor{}, mapErr(err, "user_not_found", "user")
		}
		if len(out) == limit {
			// The (limit+1)th row proves there is more without a COUNT.
			return out, db.Cursor{SortKey: last.createdAt.UTC(), ID: last.id, Hash: k.Cursor.Hash, HasMore: true}, nil
		}
		u, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		out = append(out, u)
		last = row
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "user_not_found", "user")
	}
	return out, db.Cursor{Hash: k.Cursor.Hash}, nil
}

// pageLimit bounds a page the way SPEC §E.1 does, so a repository called from a
// worker rather than a handler cannot ask for an unbounded scan.
func pageLimit(n int) int {
	const (
		defaultLimit = 50
		maxLimit     = 200
	)
	switch {
	case n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	default:
		return n
	}
}
