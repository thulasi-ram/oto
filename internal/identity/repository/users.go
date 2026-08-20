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
//
// ⛔ `email` IS A POINTER BECAUSE THE COLUMN IS NULLABLE (00074), AND A `string`
// HERE WAS A RUNTIME ERROR RATHER THAN A TYPE ERROR. pgx scanning SQL NULL into a
// `*string` destination fails with "cannot scan NULL into *string" — a 500 on
// whichever read happened to touch the first shadow member, discovered in
// production and not by the compiler. Every scan target for a nullable column in
// this package is a pointer for exactly this reason.
type userRow struct {
	id           uuid.UUID
	orgID        uuid.UUID
	email        *string
	displayName  string
	passwordHash *string
	createdAt    time.Time
	updatedAt    time.Time
	disabledAt   *time.Time
}

func (r userRow) toDomain() (domain.User, error) {
	var (
		email domain.Email
		err   error
	)
	// ⭐ A NULL EMAIL IS A SHADOW MEMBER AND NOT A DRIFT BUG (00074). It is the row
	// oto minted for a Slack workspace member who pressed a button without ever
	// linking an account, so that the press has a principal uuid to claim under.
	// The zero domain.Email carries that absence; nothing downstream may read it as
	// "not loaded yet", which is why domain.User.IsShadow() exists to be asked.
	if r.email != nil {
		email, err = domain.NewEmail(*r.email)
		if err != nil {
			// A stored address that no longer parses is a schema-drift bug, not a
			// caller error: users_email_ck should have made it unreachable. The
			// constraint admits NULL and admits well-shaped addresses; it admits
			// nothing in between, so anything that lands here is oto's bug.
			return domain.User{}, errs.Internal("user_row_invalid", err)
		}
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
//
// ⛔ THE `orgs` JOIN IS INNER AND ITS PREDICATE IS PART OF THE CREDENTIAL CHECK,
// exactly as in resolveSessionSQL and resolveByPrefixSQL. A soft-deleted tenant
// is not a tenant any more, and the login path must ask that question in the
// same breath as every other resolver that produces a tenancy — a predicate
// present in some of them and missing from one is not a smaller version of the
// same rule, it is a hole in it. Without the join, two things went wrong at once:
//
//  1. a dead tenant's member still passed password verification, and
//     `service.Login` went on to INSERT a session row for an org that no longer
//     exists — nothing could authenticate with it, because `resolveSessionSQL`
//     DOES ask, but every attempt left an orphan row behind for the sweep;
//  2. worse, the dead row still counted towards the `LIMIT 2` below, so a LIVE
//     user in a DIFFERENT org who happened to share the address became
//     "ambiguous" and was locked out by a tenant that had been deleted.
//
// ⚠️ IT IS ALSO WHY `LIMIT 2` CAN STILL BE TRUSTED. The ambiguity this query
// refuses is "this address could log into more than one LIVE org"; counting dead
// orgs towards that ceiling turns a deletion into somebody else's lockout.
//
// ⛔⛔ THE ROLL-CALL, AND WHY THIS PARAGRAPH IS NOT THE ENFORCEMENT.
//
// An earlier version of this comment said the login, cookie and bearer resolvers
// "are the whole set of ways a request acquires an org". IT WAS WRONG, AND BEING
// WRONG CONFIDENTLY IS WHAT MADE IT EXPENSIVE: a reader who trusts a sentence
// like that stops looking, and the two resolvers it did not know about carried
// the identical defect — including the lockout half — for as long as it stood.
// One of them was live. The set is FIVE, and they are, at the time of writing:
//
//  1. resolveByEmailSQL — this one. Login: an address and a password, no org.
//  2. resolveSessionSQL (sessions.go) — a cookie names no org.
//  3. resolveByPrefixSQL (tokens.go) — a bearer PAT names no org.
//  4. resolveSlackIdentitySQL (slack_identities.go) — a Slack payload names a
//     workspace and a member. LATENT: no production caller today.
//  5. channels/repository.resolveSlackConversationSQL — a Slack payload names a
//     workspace and a conversation. LIVE, and the one a human presses: it is
//     step 1 of every `InteractionService.Apply`, and the Acknowledge button
//     drives it.
//
// All five now carry `JOIN orgs o ON … AND o.deleted_at IS NULL`. A sixth is
// `ingestion/repository.lookupTokenSQL`, the Alertmanager ingest credential; it
// is live, it does NOT carry the join, and it is recorded as an open defect in
// the guard named below rather than left to a reader's memory.
//
// ⭐ DO NOT MAINTAIN THIS LIST BY HAND ALONE — it is a summary of something
// checkable, and the checkable thing is the authority.
// `tenancy_guard_test.go` reflects over EVERY SQL constant in `internal/`,
// selects the ones that yield an org id without taking one, and requires each to
// either carry the join or be named, with a reason, as not-a-resolver or as a
// known gap. A sixth resolver written tomorrow fails that test on the day it is
// written. If this list and that test ever disagree, the test is right.
// ⛔⛔ `u.email IS NOT NULL` IS REDUNDANT AND IS WRITTEN ANYWAY, and this is the
// paragraph that says why rather than leaving it to be "simplified" out.
//
// Since 00074 `users.email` is NULLABLE: a SHADOW MEMBER is the row oto mints for a
// Slack workspace member who presses a button without ever linking an oto account,
// so that the press has a principal uuid to take an idempotency claim under. Such
// a row must never be authenticable, and in SQL it already cannot be — `u.email =
// $1` is `NULL = 'someone@example.com'`, which evaluates to NULL, which is not
// TRUE, so the row is not a candidate. `domain.NewEmail("")` also fails, so
// `Login` cannot even reach this query with an empty address.
//
// The predicate is here because "it follows from three-valued logic" is the kind of
// reasoning that is correct today and is quietly broken by the next edit — a
// `COALESCE`, an `IS NOT DISTINCT FROM`, a rewrite into an `IN` over a subquery, or
// a future resolver copied from this one against a column somebody made NOT NULL
// again. Costing one always-true test on a query that runs once per login buys a
// refusal a reader can SEE, on the one statement in this package whose failure mode
// is "somebody logged in as a Slack presser who never had an account". It also
// keeps the `LIMIT 2` ambiguity check honest: a shadow row that DID reach the
// result set would lock a real user out of their own org by counting towards it.
const resolveByEmailSQL = `
SELECT ` + userColumns + `
  FROM users u
  JOIN orgs o ON o.id = u.org_id AND o.deleted_at IS NULL
 WHERE u.email = $1 AND u.email IS NOT NULL AND u.disabled_at IS NULL
 ORDER BY u.id
 LIMIT 2`

// ResolveByEmail finds the single live user with this address, across all LIVE
// orgs. A soft-deleted tenant's member is not found here at all, so no session
// is ever minted for one.
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

// insertShadowUserSQL is the ONLY INSERT on `users` in the running product, and
// its column list is the whole of its safety argument.
//
// ⛔ IT NAMES NEITHER `email` NOR `password_hash`, so both take the column default
// — NULL — and there is no parameter through which a caller could supply either.
// That is not economy: it means this statement CANNOT create a row that
// authenticates. A shadow member is refused by `resolveByEmailSQL` (a NULL email
// never equals a presented address), by `users_pw_ck`'s NULL hash (which this
// table documents as password login disabled) and by `User.CanPasswordLogin`, and
// the statement that writes the row cannot reach past any of the three.
//
// ⚠️ `internal/app/bootstrap.go` STILL OWNS THE OTHER ONE, and the two are
// deliberately not merged. Bootstrap writes the FIRST user of a deployment, with
// an address and an argon2id hash, from a CLI command that runs once; its own
// comment argues that a repository method for that would put "create a user" one
// call away from every service holding this repository. That argument survives
// intact here precisely because this method cannot do what bootstrap's INSERT does.
//
// `created_at`/`updated_at` are NAMED and passed in: 00034 removed this table's
// DEFAULT now() so that the application owns the row's time, and a service that
// let the database fill it in would be writing a row the injected clock cannot
// reproduce (CONTEXT.md §5.2).
const insertShadowUserSQL = `
INSERT INTO users (id, org_id, display_name, created_at, updated_at)
VALUES ($1, $2, $3, $4, $4)`

// InsertShadow writes a SHADOW MEMBER: a user row with no address and no password
// (git-bug a74d6b2, migration 00074).
//
// It refuses a user that carries either, rather than silently dropping them. A
// caller holding a `domain.User` with an email is holding something this statement
// would write a DIFFERENT row for than the one they are looking at, and a
// repository that quietly narrows its argument is how a password gets lost between
// two layers that each believed the other stored it.
func (r *UserRepository) InsertShadow(ctx context.Context, s db.TenantScope, u domain.User, now time.Time) error {
	if u.OrgID != s.OrgID() {
		return errs.Internal("user_scope_mismatch", nil)
	}
	if !u.IsShadow() || !u.PasswordHash.IsZero() {
		// A wiring bug, not a caller error: the only constructor that produces the
		// argument this method accepts is domain.NewShadowUser.
		return errs.Internal("user_not_shadow", nil)
	}
	if _, err := r.db(ctx).Exec(ctx, insertShadowUserSQL,
		u.ID, u.OrgID, u.DisplayName, now.UTC()); err != nil {
		return mapErr(err, "user_not_created", "user")
	}
	return nil
}

// retireShadowSQL soft-disables a shadow member, and its WHERE clause is the
// enforcement rather than a filter.
//
// ⛔ `email IS NULL AND password_hash IS NULL` MEANS THIS STATEMENT CAN ONLY EVER
// DISABLE A SHADOW ROW. There is no other write path to `users.disabled_at` in the
// running product, and this one must not become the general one: "disable a member"
// is an RBAC-shaped operation v1 deliberately does not have (R2), and a method that
// could reach a real account would be that operation arriving through the back
// door. A caller that names a real user gets zero rows updated and finds out.
//
// `updated_at` moves with `disabled_at` because `users_time_ck` requires
// `updated_at >= created_at` and because a row whose state changed without its
// timestamp moving is a row no reader can order against anything.
const retireShadowSQL = `
UPDATE users
   SET disabled_at = $3, updated_at = $3
 WHERE org_id = $1 AND id = $2
   AND email IS NULL AND password_hash IS NULL
   AND disabled_at IS NULL`

// RetireShadow soft-disables a shadow member, reporting whether it found one.
//
// ⚠️ IT IS THE ADOPTION PATH'S SECOND HALF AND IS CALLED NOWHERE ELSE. When a
// Slack identity that oto had bound to a shadow member is LINKED to a genuine oto
// user, the shadow stops being the answer to "who is this Slack member" — and a
// live row that nothing points at any more would keep appearing on the members list
// as a duplicate of the person who just linked. Retiring it says "this stopped
// being a distinct member on this date" while keeping every `cases.acked_by` and
// `alert_snoozes.snoozed_by` row it earned, which is exactly what 00003 says a soft
// disable is for: *"A disabled user keeps their acked_by rows so the timeline stays
// honest."*
func (r *UserRepository) RetireShadow(
	ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time,
) (bool, error) {
	tag, err := r.db(ctx).Exec(ctx, retireShadowSQL, s.OrgID(), id, at.UTC())
	if err != nil {
		return false, mapErr(err, "user_not_found", "user")
	}
	return tag.RowsAffected() == 1, nil
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
//
// ⭐ SHADOW MEMBERS ARE IN IT, AND THAT IS THE DECISION RATHER THAN AN OVERSIGHT
// (00074). `users_org_idx ON users (org_id) WHERE disabled_at IS NULL` serves, in
// 00003's own words, "the members list and every 'who acked this' lookup" — and a
// shadow row IS the answer to the second one for every Slack-only presser, since
// `cases.acked_by` and `alert_snoozes.snoozed_by` now point at it. Filtering them
// out here, or hiding them behind `disabled_at`, would make the one lookup this
// index exists for return nothing for the very rows it was added to attribute, and
// it would do so silently: a missing member renders as a blank actor, not as an
// error. They are distinguishable by `User.IsShadow()` — a NULL email — and named
// by their Slack handle, and that is what a caller renders them as.
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
