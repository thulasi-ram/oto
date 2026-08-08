package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// matcherJSON is the stored shape of one element of `notification_policies.matchers`.
type matcherJSON struct {
	Name  string `json:"name"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// throttleJSON is the stored shape of `notification_policies.throttle`.
type throttleJSON struct {
	Max     int `json:"max"`
	WindowS int `json:"window_s"`
}

// policyRow is the row model of `notification_policies`. Unexported, per the
// three-model rule: no DTO and no domain type may embed it.
type policyRow struct {
	id                uuid.UUID
	orgID             uuid.UUID
	name              string
	priority          int
	enabled           bool
	matchers          []byte
	reasons           []string
	channelIDs        []uuid.UUID
	throttle          []byte
	reminderAfterSecs *int
	createdAt         time.Time
	updatedAt         time.Time
	deletedAt         *time.Time
}

func (r policyRow) toDomain() (domain.Policy, error) {
	p := domain.Policy{
		ID:         r.id,
		OrgID:      r.orgID,
		Name:       r.name,
		Priority:   r.priority,
		Enabled:    r.enabled,
		ChannelIDs: r.channelIDs,
		CreatedAt:  r.createdAt,
		UpdatedAt:  r.updatedAt,
		DeletedAt:  r.deletedAt,
	}

	var ms []matcherJSON
	if len(r.matchers) > 0 {
		if err := json.Unmarshal(r.matchers, &ms); err != nil {
			return domain.Policy{}, mapErr(err, "policy_not_found", "decode policy matchers")
		}
	}
	p.Matchers = make([]domain.Matcher, 0, len(ms))
	for _, m := range ms {
		p.Matchers = append(p.Matchers, domain.Matcher{
			Name: m.Name, Op: domain.MatchOp(m.Op), Value: m.Value,
		})
	}

	p.Reasons = make([]domain.Reason, 0, len(r.reasons))
	for _, s := range r.reasons {
		p.Reasons = append(p.Reasons, domain.Reason(s))
	}

	if len(r.throttle) > 0 {
		var t throttleJSON
		if err := json.Unmarshal(r.throttle, &t); err != nil {
			return domain.Policy{}, mapErr(err, "policy_not_found", "decode policy throttle")
		}
		p.Throttle = domain.Throttle{
			Max:    t.Max,
			Window: time.Duration(t.WindowS) * time.Second,
		}
	}

	if r.reminderAfterSecs != nil {
		p.UnackedReminderAfter = time.Duration(*r.reminderAfterSecs) * time.Second
	}

	return p, nil
}

// PolicyRepository is the SQL over `notification_policies`.
//
// It is READ-ONLY in v1's notification path. A policy is configuration, written
// by the settings API; the notification module's job is to obey it, and giving
// the evaluator a write path would be handing the thing that reads the rules a
// way to change them.
type PolicyRepository struct {
	q db.Querier
}

// NewPolicyRepository builds the repository over a fallback querier.
func NewPolicyRepository(q db.Querier) *PolicyRepository { return &PolicyRepository{q: q} }

func (r *PolicyRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const policyColumns = `
  id, org_id, name, priority, enabled, matchers, reasons, channel_ids,
  throttle, unacked_reminder_after_s, created_at, updated_at, deleted_at`

const listLivePoliciesSQL = `
SELECT` + policyColumns + `
  FROM notification_policies
 WHERE org_id = $1 AND enabled AND deleted_at IS NULL
 ORDER BY priority ASC, created_at ASC, id ASC`

// ListLive returns every enabled, undeleted policy for one org in EVALUATION
// ORDER: priority ascending, LOWER FIRST.
//
// The tiebreak on (created_at, id) is not decoration. Policy evaluation stops at
// the FIRST MATCH, so two policies sharing a priority would otherwise route the
// same alert to different channels depending on which row the planner returned
// first — a routing bug that reproduces once a week and never in a test.
func (r *PolicyRepository) ListLive(ctx context.Context, s db.TenantScope) ([]domain.Policy, error) {
	rows, err := r.db(ctx).Query(ctx, listLivePoliciesSQL, s.OrgID())
	if err != nil {
		return nil, mapErr(err, "policy_not_found", "list notification policies")
	}
	defer rows.Close()

	out := make([]domain.Policy, 0, 8)
	for rows.Next() {
		var row policyRow
		if err := rows.Scan(
			&row.id, &row.orgID, &row.name, &row.priority, &row.enabled,
			&row.matchers, &row.reasons, &row.channelIDs, &row.throttle,
			&row.reminderAfterSecs, &row.createdAt, &row.updatedAt, &row.deletedAt,
		); err != nil {
			return nil, mapErr(err, "policy_not_found", "scan notification policy")
		}
		p, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "policy_not_found", "read notification policies")
	}
	return out, nil
}

const getPolicySQL = `
SELECT` + policyColumns + `
  FROM notification_policies
 WHERE org_id = $1 AND id = $2`

// Get reads one policy, deleted or not. The settings UI must be able to show a
// deleted policy that a historical notification still points at.
func (r *PolicyRepository) Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Policy, error) {
	var row policyRow
	err := r.db(ctx).QueryRow(ctx, getPolicySQL, s.OrgID(), id).Scan(
		&row.id, &row.orgID, &row.name, &row.priority, &row.enabled,
		&row.matchers, &row.reasons, &row.channelIDs, &row.throttle,
		&row.reminderAfterSecs, &row.createdAt, &row.updatedAt, &row.deletedAt,
	)
	if err != nil {
		return domain.Policy{}, mapErr(err, "policy_not_found", "notification policy")
	}
	return row.toDomain()
}

// listReminderPoliciesSQL selects the policies a reminder could fire for.
//
// ⚠️ THE `COALESCE` IS THE ORG DEFAULT. `unacked_reminder_after_s` is NULL on a
// policy that names no delay of its own, and a NULL used to mean "no reminder,
// full stop". It now means "fall back to `orgs.settings.unacked_reminder_after_s`",
// and $2 carries that value — or NULL when the org sets none, in which case the
// COALESCE is NULL and the row is filtered out exactly as before. That is what
// makes the org-level knob a genuine DEFAULT rather than a second, competing
// setting: a policy that has an opinion still wins.
//
// ⛔ ONE STAGE, FOREVER (§G.9.1). $2 is a scalar and this is a fallback, not a
// second threshold.
const listReminderPoliciesSQL = `
SELECT` + policyColumns + `
  FROM notification_policies
 WHERE org_id = $1 AND enabled AND deleted_at IS NULL
   AND COALESCE(unacked_reminder_after_s, $2) IS NOT NULL
 ORDER BY priority ASC, created_at ASC, id ASC`

// ListWithUnackedReminder returns the live policies that ask for the ONE
// reminder stage (§G.9.1).
//
// There is no "stage" parameter and there never will be. The reminder is
// triggered by the SIGNAL's own unacked duration and delivered to the channels
// the policy already routes to; it resolves nobody and pages nobody.
// orgDefault is the org-level fallback in SECONDS, or nil when the org sets none.
func (r *PolicyRepository) ListWithUnackedReminder(
	ctx context.Context, s db.TenantScope, orgDefault *int,
) ([]domain.Policy, error) {
	rows, err := r.db(ctx).Query(ctx, listReminderPoliciesSQL, s.OrgID(), orgDefault)
	if err != nil {
		return nil, mapErr(err, "policy_not_found", "list reminder policies")
	}
	defer rows.Close()

	out := make([]domain.Policy, 0, 4)
	for rows.Next() {
		var row policyRow
		if err := rows.Scan(
			&row.id, &row.orgID, &row.name, &row.priority, &row.enabled,
			&row.matchers, &row.reasons, &row.channelIDs, &row.throttle,
			&row.reminderAfterSecs, &row.createdAt, &row.updatedAt, &row.deletedAt,
		); err != nil {
			return nil, mapErr(err, "policy_not_found", "scan reminder policy")
		}
		p, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "policy_not_found", "read reminder policies")
	}
	return out, nil
}
