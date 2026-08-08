package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// slackIdentityRow is one `slack_identities` row.
type slackIdentityRow struct {
	id          uuid.UUID
	orgID       uuid.UUID
	teamID      string
	slackUserID string
	handle      *string
	userID      *uuid.UUID
	linkedAt    *time.Time
	createdAt   time.Time
}

func (r slackIdentityRow) toDomain() (domain.SlackIdentity, error) {
	team, err := domain.NewSlackTeamID(r.teamID)
	if err != nil {
		return domain.SlackIdentity{}, errs.Internal("slack_identity_row_invalid", err)
	}
	member, err := domain.NewSlackUserID(r.slackUserID)
	if err != nil {
		return domain.SlackIdentity{}, errs.Internal("slack_identity_row_invalid", err)
	}
	handle := ""
	if r.handle != nil {
		handle = *r.handle
	}
	return domain.SlackIdentity{
		ID:          r.id,
		OrgID:       r.orgID,
		TeamID:      team,
		SlackUserID: member,
		Handle:      handle,
		UserID:      derefID(r.userID),
		LinkedAt:    r.linkedAt,
		CreatedAt:   r.createdAt.UTC(),
	}, nil
}

// SlackIdentityRepository reads and writes `slack_identities`.
type SlackIdentityRepository struct {
	q db.Querier
}

// NewSlackIdentityRepository builds the repository.
func NewSlackIdentityRepository(q db.Querier) *SlackIdentityRepository {
	return &SlackIdentityRepository{q: q}
}

func (r *SlackIdentityRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const slackIdentityColumns = `si.id, si.org_id, si.team_id, si.slack_user_id, si.slack_handle,
       si.user_id, si.linked_at, si.created_at`

func scanSlackIdentity(dst *slackIdentityRow, scan func(...any) error) error {
	return scan(&dst.id, &dst.orgID, &dst.teamID, &dst.slackUserID, &dst.handle,
		&dst.userID, &dst.linkedAt, &dst.createdAt)
}

// upsertSlackIdentitySQL leans on `slack_identities_uniq (org_id, team_id,
// slack_user_id)`. A repeat sighting of the same Slack member is not a conflict
// — it is the same person pressing a button again — so it refreshes the
// denormalised handle and returns the existing row.
//
// It deliberately does NOT touch user_id or linked_at. Linking is a separate,
// explicit transition (see Link) because slack_identities_link_ck makes the pair
// all-or-nothing, and an upsert that could half-write it would be a 23514.
//
// ⛔ `AS si` IS LOAD-BEARING AND IT WAS MISSING. `slackIdentityColumns` is
// `si.`-qualified because every SELECT in this file joins under that alias — and
// a RETURNING clause can only name columns of the table the statement touched,
// which an unaliased INSERT calls `slack_identities`. Without the alias this
// statement raised `42P01: missing FROM-clause entry for table "si"` on EVERY
// call, so `Upsert` had never once succeeded. Nothing noticed, because until the
// Slack Acknowledge button was wired to a consumer this method had no callers at
// all: the table existed, the repository existed, and the only code path that
// would have exercised it was the one that did nothing.
const upsertSlackIdentitySQL = `
INSERT INTO slack_identities AS si (id, org_id, team_id, slack_user_id, slack_handle, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT ON CONSTRAINT slack_identities_uniq
DO UPDATE SET slack_handle = EXCLUDED.slack_handle
RETURNING ` + slackIdentityColumns

// Upsert records a sighting of a Slack member, returning the stored identity.
func (r *SlackIdentityRepository) Upsert(
	ctx context.Context, s db.TenantScope, si domain.SlackIdentity, now time.Time,
) (domain.SlackIdentity, error) {
	if si.OrgID != s.OrgID() {
		return domain.SlackIdentity{}, errs.Internal("slack_identity_scope_mismatch", nil)
	}
	var handle *string
	if si.Handle != "" {
		v := si.Handle
		handle = &v
	}

	var row slackIdentityRow
	err := scanSlackIdentity(&row, r.db(ctx).QueryRow(ctx, upsertSlackIdentitySQL,
		si.ID, si.OrgID, si.TeamID.String(), si.SlackUserID.String(), handle, now.UTC()).Scan)
	if err != nil {
		return domain.SlackIdentity{}, mapErr(err, "slack_identity_not_found", "slack identity")
	}
	return row.toDomain()
}

const selectSlackIdentityByUserSQL = `
SELECT ` + slackIdentityColumns + `
  FROM slack_identities si
 WHERE si.org_id = $1 AND si.user_id = $2
 ORDER BY si.linked_at DESC
 LIMIT 1`

// GetByUser returns the Slack identity linked to an oto user, if any. It is what
// `UserDTO.slack_user_id` is rendered from.
func (r *SlackIdentityRepository) GetByUser(
	ctx context.Context, s db.TenantScope, userID uuid.UUID,
) (domain.SlackIdentity, error) {
	var row slackIdentityRow
	err := scanSlackIdentity(&row, r.db(ctx).QueryRow(ctx, selectSlackIdentityByUserSQL, s.OrgID(), userID).Scan)
	if err != nil {
		return domain.SlackIdentity{}, mapErr(err, "slack_identity_not_found", "slack identity")
	}
	return row.toDomain()
}

const selectSlackIdentitySQL = `
SELECT ` + slackIdentityColumns + `
  FROM slack_identities si
 WHERE si.org_id = $1 AND si.team_id = $2 AND si.slack_user_id = $3`

// GetBySlackUser returns one identity within the caller's org.
func (r *SlackIdentityRepository) GetBySlackUser(
	ctx context.Context, s db.TenantScope, team domain.SlackTeamID, member domain.SlackUserID,
) (domain.SlackIdentity, error) {
	var row slackIdentityRow
	err := scanSlackIdentity(&row, r.db(ctx).QueryRow(ctx, selectSlackIdentitySQL,
		s.OrgID(), team.String(), member.String()).Scan)
	if err != nil {
		return domain.SlackIdentity{}, mapErr(err, "slack_identity_not_found", "slack identity")
	}
	return row.toDomain()
}

// resolveSlackIdentitySQL is org-blind because a Slack interaction payload names
// a workspace and a member, never an org.
//
// ⚠️ ONE OF THE FOUR UNSCOPED QUERIES IN THIS MODULE, and the only one whose
// caller has already authenticated by another means: the Slack HMAC signature
// (§H.8) proves the request came from Slack BEFORE this runs. Resolving the org
// from the team is what makes an ack pressed in Slack land in the right tenant.
//
// LIMIT 2 for the same reason ResolveByEmail uses it: `slack_identities_uniq` is
// per-org, so one workspace connected to two orgs is representable, and picking
// the planner's first row would decide a tenancy by physical ordering.
const resolveSlackIdentitySQL = `
SELECT ` + slackIdentityColumns + `
  FROM slack_identities si
 WHERE si.team_id = $1 AND si.slack_user_id = $2
 ORDER BY si.id
 LIMIT 2`

// ResolveBySlackUser finds the single identity for a workspace member, across
// all orgs. An ambiguous result is reported as not found.
func (r *SlackIdentityRepository) ResolveBySlackUser(
	ctx context.Context, team domain.SlackTeamID, member domain.SlackUserID,
) (domain.SlackIdentity, error) {
	rows, err := r.db(ctx).Query(ctx, resolveSlackIdentitySQL, team.String(), member.String())
	if err != nil {
		return domain.SlackIdentity{}, mapErr(err, "slack_identity_not_found", "slack identity")
	}
	defer rows.Close()

	var found []slackIdentityRow
	for rows.Next() {
		var row slackIdentityRow
		if err := scanSlackIdentity(&row, rows.Scan); err != nil {
			return domain.SlackIdentity{}, mapErr(err, "slack_identity_not_found", "slack identity")
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		return domain.SlackIdentity{}, mapErr(err, "slack_identity_not_found", "slack identity")
	}
	if len(found) != 1 {
		return domain.SlackIdentity{}, errs.NotFound("slack_identity_not_found", "no such slack identity")
	}
	return found[0].toDomain()
}

// linkSlackIdentitySQL writes both halves of slack_identities_link_ck in one
// statement. There is no path that writes one without the other.
//
// `AS si` for the same reason `upsertSlackIdentitySQL` needs it: the shared
// column list is alias-qualified, and an unaliased UPDATE has no `si` to name.
// This statement carried the identical defect and had never succeeded either.
const linkSlackIdentitySQL = `
UPDATE slack_identities AS si
   SET user_id = $3, linked_at = $4
 WHERE si.org_id = $1 AND si.id = $2
RETURNING ` + slackIdentityColumns

// Link binds an identity to an oto user within the caller's org.
func (r *SlackIdentityRepository) Link(
	ctx context.Context, s db.TenantScope, id, userID uuid.UUID, at time.Time,
) (domain.SlackIdentity, error) {
	var row slackIdentityRow
	err := scanSlackIdentity(&row, r.db(ctx).QueryRow(ctx, linkSlackIdentitySQL,
		s.OrgID(), id, userID, at.UTC()).Scan)
	if err != nil {
		return domain.SlackIdentity{}, mapErr(err, "slack_identity_not_found", "slack identity")
	}
	return row.toDomain()
}
