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
	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	investigatorrepo "github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⭐⭐ THE ONLY WAY A REMEDY APPROVER IS GRANTED OR REVOKED (ADR 0054 §4, migration 00099,
// git-bug 47f67c8). `oto grant remedy-approver` and `oto revoke remedy-approver` call these
// two functions, and nothing else in the product writes `remedy_approver_grants`.
//
// ⛔ THEY ARE SUBCOMMANDS AND NOT ROUTES, for bootstrap's reason and one of their own.
// Bootstrap's: running one needs a shell on the host and the database credentials — the
// authority that could write the row by hand anyway — which also answers "who grants the
// first grant" before any admin role exists. Their own: double approval means two
// DIFFERENT holders, and an in-app grant would let one holder mint a second approver (an
// alt account) and approve alone. test/scope/remedy_approver_routes_test.go walks the
// mounted router to hold that no route ever writes one.
//
// ⚠️ RAW WRITES IN `internal/app`, beside bootstrap's and reset-password's, for the same
// reason as theirs: a repository method that inserts a grant would be one call away from
// every service holding that repository. `identity/repository.RemedyApproverRepository`
// only reads.
//
// ⛔ A PERMISSION, NEVER AN OBLIGATION (H-1): granting notifies no one, routes nothing and
// creates no queue.

// RemedyApproverRequest names one grant: a user, by address, on one ToolServer, by name, in
// one org, by slug.
type RemedyApproverRequest struct {
	OrgSlug    string
	ToolServer string
	Email      string
}

// RemedyApproverResult is what the operator needs to confirm the right grant moved.
type RemedyApproverResult struct {
	OrgID        uuid.UUID
	ToolServerID uuid.UUID
	UserID       uuid.UUID
	// GrantedAt is when the grant was made: now, for a grant; the original instant, for a
	// revoke.
	GrantedAt time.Time
}

// The refusals, each a sentence the CLI prints and exits non-zero on. ErrOrgNotFound is
// reset-password's.
var (
	// ErrToolServerNotFound: the org has no ToolServer by that name.
	ErrToolServerNotFound = errors.New("no ToolServer with that name in this org")
	// ErrApproverNotFound: the org has no user with that address. ⭐ A shadow member has
	// no address (00074), so it can never be found — and never hold a grant.
	ErrApproverNotFound = errors.New("no user with that email address in this org")
	// ErrReadToolServer: a grant names a `read` ToolServer. A Remedy executes through a
	// write Tool (ADR 0054 §5), so a grant on a read ToolServer would permit nothing.
	ErrReadToolServer = errors.New("a Remedy approver is granted on a write ToolServer; this one is declared read, " +
		"and a Remedy never runs through it")
	// ErrApproverDisabled: the user is disabled, and a disabled user's grant does not count.
	ErrApproverDisabled = errors.New("this user is disabled, and a disabled user's grant would not count")
	// ErrAlreadyGranted: the user already holds the grant on this ToolServer. Nothing changed.
	ErrAlreadyGranted = errors.New("this user already holds the remedy-approver grant on this ToolServer; nothing changed")
	// ErrNotGranted: revoking a grant the user does not hold. Nothing changed.
	ErrNotGranted = errors.New("this user holds no remedy-approver grant on this ToolServer; nothing was revoked")
)

// GrantRemedyApprover writes one grant, stamped with `now` and `granted_by = cli`.
//
// ⛔ IT REFUSES, RATHER THAN IGNORES, A GRANT THAT ALREADY EXISTS — bootstrap's argument: a
// silent no-op is misread as "I just granted it", and an operator who typed the wrong
// address learns nothing. It names a write ToolServer, a live org and an enabled user with
// an address, or it writes nothing.
func GrantRemedyApprover(ctx context.Context, pool *pgxpool.Pool, req RemedyApproverRequest, now time.Time) (RemedyApproverResult, error) {
	var out RemedyApproverResult
	err := remedyApproverTx(ctx, pool, req, func(ctx context.Context, q db.Querier, r RemedyApproverResult,
		ts investigatordomain.ToolServerConfig, user identitydomain.User,
	) error {
		if ts.Access != investigatordomain.AccessWrite {
			return ErrReadToolServer
		}
		if !user.Active() {
			return ErrApproverDisabled
		}
		tag, err := q.Exec(ctx, insertRemedyApproverGrantSQL, r.OrgID, ts.ID, string(investigatordomain.AccessWrite),
			user.ID, now.UTC(), identitydomain.RemedyApproverGrantedByCLI)
		if err != nil {
			return fmt.Errorf("grant: insert: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrAlreadyGranted
		}
		r.GrantedAt = now.UTC()
		out = r
		return nil
	})
	return out, err
}

// RevokeRemedyApprover deletes one grant. The user stops being able to approve a Remedy
// on this ToolServer the moment it commits; an approval they already gave is history and
// is not touched.
//
// ⛔ REVOKING A GRANT THAT DOES NOT EXIST IS REFUSED (ErrNotGranted), not ignored: an
// operator who named the wrong ToolServer or the wrong address must not walk away
// believing they took the permission from the person they meant.
func RevokeRemedyApprover(ctx context.Context, pool *pgxpool.Pool, req RemedyApproverRequest) (RemedyApproverResult, error) {
	var out RemedyApproverResult
	err := remedyApproverTx(ctx, pool, req, func(ctx context.Context, q db.Querier, r RemedyApproverResult,
		ts investigatordomain.ToolServerConfig, user identitydomain.User,
	) error {
		var at time.Time
		err := q.QueryRow(ctx, deleteRemedyApproverGrantSQL, r.OrgID, ts.ID, user.ID).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotGranted
		}
		if err != nil {
			return fmt.Errorf("revoke: delete: %w", err)
		}
		r.GrantedAt = at.UTC()
		out = r
		return nil
	})
	return out, err
}

// remedyApproverTx resolves the org, the ToolServer and the user, then runs fn — all in
// one transaction, so the ToolServer cannot be deleted between being found and granted on.
func remedyApproverTx(
	ctx context.Context, pool *pgxpool.Pool, req RemedyApproverRequest,
	fn func(ctx context.Context, q db.Querier, r RemedyApproverResult, ts investigatordomain.ToolServerConfig, user identitydomain.User) error,
) error {
	if pool == nil {
		return errors.New("a database pool is required")
	}
	req, err := req.normalise()
	if err != nil {
		return err
	}
	name, err := investigatordomain.NewToolServerName(req.ToolServer)
	if err != nil {
		return fmt.Errorf("--toolserver: %s", messageOf(err))
	}
	email, err := identitydomain.NewEmail(req.Email)
	if err != nil {
		return fmt.Errorf("--email: %s", messageOf(err))
	}

	toolServers := investigatorrepo.NewToolServerRepository(pool)
	users := identityrepo.NewUserRepository(pool)

	return db.Tx(ctx, pool, func(ctx context.Context) error {
		q := db.FromContext(ctx, pool)

		var orgID uuid.UUID
		serr := q.QueryRow(ctx, selectLiveOrgIDBySlugSQL, req.OrgSlug).Scan(&orgID)
		if errors.Is(serr, pgx.ErrNoRows) {
			return ErrOrgNotFound
		}
		if serr != nil {
			return fmt.Errorf("find org: %w", serr)
		}
		scope, serr := db.NewTenantScope(orgID)
		if serr != nil {
			return fmt.Errorf("scope: %w", serr)
		}

		found, serr := toolServers.ByNames(ctx, scope, []string{name})
		if serr != nil {
			return fmt.Errorf("find ToolServer: %w", serr)
		}
		if len(found) != 1 {
			return ErrToolServerNotFound
		}

		user, serr := users.GetByEmail(ctx, scope, email)
		if errs.IsKind(serr, errs.KindNotFound) {
			return ErrApproverNotFound
		}
		if serr != nil {
			return fmt.Errorf("find user: %w", serr)
		}
		// ⛔ Belt and braces: `users.email = $2` never matches a NULL, so a shadow member
		// cannot arrive here. If one ever did, it is refused rather than granted.
		if user.IsShadow() {
			return ErrApproverNotFound
		}

		return fn(ctx, q, RemedyApproverResult{OrgID: orgID, ToolServerID: found[0].ID, UserID: user.ID}, found[0], user)
	})
}

// ⭐ ON CONFLICT DO NOTHING, and zero rows affected is ErrAlreadyGranted: the primary key
// (tool_server_id, user_id) decides "already holds it" under concurrency, not a read.
// `tool_server_access` is always 'write' — the third column of the foreign key that keeps
// a grant off a read ToolServer in the schema as well as here.
const insertRemedyApproverGrantSQL = `
INSERT INTO remedy_approver_grants (org_id, tool_server_id, tool_server_access, user_id, granted_at, granted_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tool_server_id, user_id) DO NOTHING`

const deleteRemedyApproverGrantSQL = `
DELETE FROM remedy_approver_grants
 WHERE org_id = $1 AND tool_server_id = $2 AND user_id = $3
RETURNING granted_at`

// normalise trims and enforces the three required flags.
func (r RemedyApproverRequest) normalise() (RemedyApproverRequest, error) {
	r.OrgSlug = strings.ToLower(strings.TrimSpace(r.OrgSlug))
	r.ToolServer = strings.TrimSpace(r.ToolServer)
	r.Email = strings.TrimSpace(r.Email)
	switch {
	case r.OrgSlug == "":
		return r, errors.New("--org is required")
	case r.ToolServer == "":
		return r, errors.New("--toolserver is required")
	case r.Email == "":
		return r, errors.New("--email is required")
	}
	return r, nil
}

// messageOf is a domain refusal's sentence without its kind and code: the CLI prints it
// after the flag it is about.
func messageOf(err error) string {
	if e, ok := errs.As(err); ok && e.Message != "" {
		return e.Message
	}
	return err.Error()
}
