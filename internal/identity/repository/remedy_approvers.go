package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// RemedyApproverRepository READS `remedy_approver_grants` (migration 00099, ADR 0054 §4,
// git-bug 47f67c8).
//
// ⛔⛔ IT HAS NO WRITE METHOD, AND THAT IS THE DESIGN. A grant is written and deleted by
// `oto grant` / `oto revoke` in `internal/app`, as raw statements beside bootstrap's — for
// bootstrap's reason: a repository method that inserts a grant would put "mint an
// approver" one call away from every service that already holds this repository, and
// from there one handler away from an HTTP route. Double approval needs two DIFFERENT
// holders; a holder who could mint a second from inside oto defeats it.
//
// Every read joins `users`, because what decides whether a grant COUNTS — the holder's
// soft disable and address — is on the user, not the grant.
type RemedyApproverRepository struct {
	q db.Querier
}

// NewRemedyApproverRepository builds the repository.
func NewRemedyApproverRepository(q db.Querier) *RemedyApproverRepository {
	return &RemedyApproverRepository{q: q}
}

func (r *RemedyApproverRepository) db(ctx context.Context) db.Querier {
	return db.FromContext(ctx, r.q)
}

// MaxListedRemedyApprovers bounds one ToolServer's list. A handful of people approve
// Remedies on a ToolServer; the list is one page.
const MaxListedRemedyApprovers = 200

const remedyApproverColumns = `g.user_id, u.email, u.display_name, g.granted_at, g.granted_by, u.disabled_at`

type remedyApproverRow struct {
	userID      uuid.UUID
	email       *string
	displayName string
	grantedAt   time.Time
	grantedBy   string
	disabledAt  *time.Time
}

func (row remedyApproverRow) toDomain() (domain.RemedyApprover, error) {
	var email domain.Email
	if row.email != nil {
		e, err := domain.NewEmail(*row.email)
		if err != nil {
			return domain.RemedyApprover{}, errs.Internal("remedy_approver_row_invalid", err)
		}
		email = e
	}
	return domain.RemedyApprover{UserID: row.userID, Email: email,
		DisplayName: row.displayName, GrantedAt: row.grantedAt.UTC(), GrantedBy: row.grantedBy,
		DisabledAt: row.disabledAt}, nil
}

func scanRemedyApprover(scan func(...any) error) (domain.RemedyApprover, error) {
	var row remedyApproverRow
	if err := scan(&row.userID, &row.email, &row.displayName, &row.grantedAt,
		&row.grantedBy, &row.disabledAt); err != nil {
		return domain.RemedyApprover{}, err
	}
	return row.toDomain()
}

// ListForToolServer reads every grant on one ToolServer with its holder, by address —
// disabled holders included, because the settings read shows them as not counting rather
// than hiding them. Served by `remedy_approver_grants_pk`.
func (r *RemedyApproverRepository) ListForToolServer(
	ctx context.Context, s db.TenantScope, toolServerID uuid.UUID,
) ([]domain.RemedyApprover, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, `
SELECT `+remedyApproverColumns+`
  FROM remedy_approver_grants g
  JOIN users u ON u.org_id = g.org_id AND u.id = g.user_id
 WHERE g.org_id = $1 AND g.tool_server_id = $2
 ORDER BY u.email, g.user_id
 LIMIT $3`, s.OrgID(), toolServerID, MaxListedRemedyApprovers)
	if err != nil {
		return nil, mapErr(err, "remedy_approver_not_found", "remedy approver")
	}
	defer rows.Close()
	out := []domain.RemedyApprover{}
	for rows.Next() {
		a, err := scanRemedyApprover(rows.Scan)
		if err != nil {
			return nil, mapErr(err, "remedy_approver_not_found", "remedy approver")
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "remedy_approver_not_found", "remedy approver")
	}
	return out, nil
}

// Grant reads one user's grant on one ToolServer, or KindNotFound when they hold none —
// never granted, or revoked. Whether it COUNTS is the caller's question
// (domain.RemedyApprover.Counts), so a disabled holder's grant is read like any other.
func (r *RemedyApproverRepository) Grant(
	ctx context.Context, s db.TenantScope, toolServerID, userID uuid.UUID,
) (domain.RemedyApprover, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.RemedyApprover{}, err
	}
	a, err := scanRemedyApprover(r.db(ctx).QueryRow(ctx, `
SELECT `+remedyApproverColumns+`
  FROM remedy_approver_grants g
  JOIN users u ON u.org_id = g.org_id AND u.id = g.user_id
 WHERE g.org_id = $1 AND g.tool_server_id = $2 AND g.user_id = $3`, s.OrgID(), toolServerID, userID).Scan)
	if err != nil {
		return domain.RemedyApprover{}, mapErr(err, "remedy_approver_not_found", "remedy approver")
	}
	return a, nil
}
