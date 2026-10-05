package repository

// THE SLACK LINK CODE STORE (git-bug a556a5c, migration 00109): one live code per Slack identity,
// stored only as a sha256; the per-user count of wrong codes; and the recorded fact of every link
// and unlink. See `identity/domain/slack_link_code.go` for why each exists.
//
// ⭐ EVERY LOOKUP IS BY HASH, INSIDE THE CALLER'S ORG, AND EVERY CONDITION THAT MAKES A CODE LIVE IS
// IN THE SAME WHERE CLAUSE AS THE WRITE. "Not consumed, not expired, not over-presented" is decided
// by the UPDATE that spends the presentation, so two concurrent confirms of one code cannot both
// succeed: the second finds no row, and a read-then-write in Go could not promise that.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// SlackLinkRepository reads and writes `slack_link_codes`, `slack_link_attempts` and
// `slack_identity_links`.
type SlackLinkRepository struct {
	q db.Querier
}

// NewSlackLinkRepository builds the repository.
func NewSlackLinkRepository(q db.Querier) *SlackLinkRepository {
	return &SlackLinkRepository{q: q}
}

func (r *SlackLinkRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// issueSlackLinkCodeSQL REPLACES the identity's code: a new code for the same Slack member is the
// moment the previous one dies, which is the design's "issuing a new code invalidates the previous
// one" in one statement rather than a DELETE and an INSERT that a crash could separate.
const issueSlackLinkCodeSQL = `
INSERT INTO slack_link_codes AS c
       (slack_identity_id, org_id, code_hash, issued_at, expires_at, presentations, consumed_at, consumed_by)
VALUES ($2, $1, $3, $4, $5, 0, NULL, NULL)
ON CONFLICT (slack_identity_id) DO UPDATE
   SET code_hash = EXCLUDED.code_hash, issued_at = EXCLUDED.issued_at, expires_at = EXCLUDED.expires_at,
       presentations = 0, consumed_at = NULL, consumed_by = NULL
 WHERE c.org_id = $1`

// IssueCode stores a fresh code for one identity, replacing any code it had.
func (r *SlackLinkRepository) IssueCode(
	ctx context.Context, s db.TenantScope, identityID uuid.UUID, hash domain.TokenHash, issuedAt, expiresAt time.Time,
) error {
	tag, err := r.db(ctx).Exec(ctx, issueSlackLinkCodeSQL,
		s.OrgID(), identityID, hash.Bytes(), issuedAt.UTC(), expiresAt.UTC())
	if err != nil {
		return mapErr(err, "slack_identity_not_found", "slack identity")
	}
	if tag.RowsAffected() != 1 {
		// The ON CONFLICT's WHERE refused: the identity's existing code belongs to another org,
		// which `slack_identities.id` being a uuid makes impossible short of a forged id.
		return errs.NotFound("slack_identity_not_found", "no such slack identity")
	}
	return nil
}

// presentSlackLinkCodeSQL spends one presentation of a LIVE code and names its identity. It does
// not consume the code: a preview is a presentation, and so is the confirm that follows it.
const presentSlackLinkCodeSQL = `
UPDATE slack_link_codes AS c
   SET presentations = c.presentations + 1
 WHERE c.org_id = $1 AND c.code_hash = $2
   AND c.consumed_at IS NULL AND c.expires_at > $3 AND c.presentations < $4
RETURNING c.slack_identity_id, c.expires_at`

// PresentCode spends one presentation of a live code. A code that is not live — wrong, used,
// expired, over-presented or another org's — is one NotFound, `slack_link_code_invalid`.
func (r *SlackLinkRepository) PresentCode(
	ctx context.Context, s db.TenantScope, hash domain.TokenHash, now time.Time,
) (uuid.UUID, time.Time, error) {
	var id uuid.UUID
	var exp time.Time
	err := r.db(ctx).QueryRow(ctx, presentSlackLinkCodeSQL,
		s.OrgID(), hash.Bytes(), now.UTC(), domain.SlackLinkCodeMaxPresentations).Scan(&id, &exp)
	if err != nil {
		return uuid.Nil, time.Time{}, codeErr(err)
	}
	return id, exp.UTC(), nil
}

// consumeSlackLinkCodeSQL spends the last presentation a code will ever have: the same liveness
// conditions as presenting it, and `consumed_at` set in the same statement, so it is single-use by
// construction.
const consumeSlackLinkCodeSQL = `
UPDATE slack_link_codes AS c
   SET presentations = c.presentations + 1, consumed_at = $3, consumed_by = $5
 WHERE c.org_id = $1 AND c.code_hash = $2
   AND c.consumed_at IS NULL AND c.expires_at > $3 AND c.presentations < $4
RETURNING c.slack_identity_id`

// ConsumeCode uses a live code up, naming the user who did, and returns its identity.
func (r *SlackLinkRepository) ConsumeCode(
	ctx context.Context, s db.TenantScope, hash domain.TokenHash, userID uuid.UUID, now time.Time,
) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db(ctx).QueryRow(ctx, consumeSlackLinkCodeSQL,
		s.OrgID(), hash.Bytes(), now.UTC(), domain.SlackLinkCodeMaxPresentations, userID).Scan(&id)
	if err != nil {
		return uuid.Nil, codeErr(err)
	}
	return id, nil
}

func codeErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SlackLinkCodeInvalid()
	}
	return mapErr(err, "slack_link_code_invalid", "link code")
}

const countSlackLinkAttemptsSQL = `
SELECT count(*)
  FROM slack_link_attempts a
 WHERE a.org_id = $1 AND a.user_id = $2 AND a.attempted_at > $3`

// CountWrongAttempts counts the wrong codes one user presented after `since`.
func (r *SlackLinkRepository) CountWrongAttempts(
	ctx context.Context, s db.TenantScope, userID uuid.UUID, since time.Time,
) (int, error) {
	var n int
	if err := r.db(ctx).QueryRow(ctx, countSlackLinkAttemptsSQL, s.OrgID(), userID, since.UTC()).Scan(&n); err != nil {
		return 0, mapErr(err, "user_not_found", "user")
	}
	return n, nil
}

const pruneSlackLinkAttemptsSQL = `
DELETE FROM slack_link_attempts
 WHERE org_id = $1 AND user_id = $2 AND attempted_at <= $3`

const insertSlackLinkAttemptSQL = `
INSERT INTO slack_link_attempts (id, org_id, user_id, attempted_at)
VALUES ($1, $2, $3, $4)`

// RecordWrongAttempt counts one wrong code against a user, first pruning that user's attempts that
// have left the window — so the table holds at most a window's worth of rows per user.
func (r *SlackLinkRepository) RecordWrongAttempt(
	ctx context.Context, s db.TenantScope, id, userID uuid.UUID, at, pruneBefore time.Time,
) error {
	if _, err := r.db(ctx).Exec(ctx, pruneSlackLinkAttemptsSQL, s.OrgID(), userID, pruneBefore.UTC()); err != nil {
		return mapErr(err, "user_not_found", "user")
	}
	if _, err := r.db(ctx).Exec(ctx, insertSlackLinkAttemptSQL, id, s.OrgID(), userID, at.UTC()); err != nil {
		return mapErr(err, "user_not_found", "user")
	}
	return nil
}

const insertSlackLinkFactSQL = `
INSERT INTO slack_identity_links
       (id, org_id, slack_identity_id, team_id, slack_user_id, change, user_id, displaced_user_id, actor_id, at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// RecordFact writes the recorded fact of one link or unlink.
func (r *SlackLinkRepository) RecordFact(ctx context.Context, s db.TenantScope, f domain.SlackLinkFact) error {
	if f.OrgID != s.OrgID() {
		return errs.Internal("slack_link_fact_scope_mismatch", nil)
	}
	var displaced *uuid.UUID
	if f.DisplacedUserID != uuid.Nil {
		v := f.DisplacedUserID
		displaced = &v
	}
	_, err := r.db(ctx).Exec(ctx, insertSlackLinkFactSQL,
		f.ID, f.OrgID, f.IdentityID, f.TeamID.String(), f.SlackUserID.String(), string(f.Change),
		f.UserID, displaced, f.ActorID, f.At.UTC())
	if err != nil {
		return mapErr(err, "slack_identity_not_found", "slack identity")
	}
	return nil
}
