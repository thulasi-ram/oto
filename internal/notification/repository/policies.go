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
	id               uuid.UUID
	orgID            uuid.UUID
	name             string
	priority         int
	enabled          bool
	matchers         []byte
	reasons          []string
	channelIDs       []uuid.UUID
	throttle         []byte
	digestWindowSecs *int
	digestFloor      *int
	createdAt        time.Time
	updatedAt        time.Time
	deletedAt        *time.Time
}

// scanInto is the ONE argument list for `policyColumns`, and it exists because
// there are now four queries in this file reading the same thirteen columns.
//
// ⚠️ THE DUPLICATION IT REPLACED WAS A LIVE HAZARD, not a style problem. Each
// query spelled its own `Scan(&row.a, &row.b, …)` in the column order, so adding
// `digest_window_s` and `digest_floor` meant editing the list in four places and a
// miss would not fail to compile: pgx would scan an `INT` into a `*time.Time` and
// return a runtime error on one code path only, most likely the one with no test.
// One list next to the one column constant cannot drift.
func (r *policyRow) scanInto() []any {
	return []any{
		&r.id, &r.orgID, &r.name, &r.priority, &r.enabled,
		&r.matchers, &r.reasons, &r.channelIDs, &r.throttle,
		&r.digestWindowSecs, &r.digestFloor,
		&r.createdAt, &r.updatedAt, &r.deletedAt,
	}
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

	// NULL means "no digest", which is the shipped default and the state of every
	// row written before migration 00058. The zero Duration says the same thing in
	// Go, so there is nothing to translate and nothing to default.
	if r.digestWindowSecs != nil {
		p.Digest.Window = time.Duration(*r.digestWindowSecs) * time.Second
	}
	if r.digestFloor != nil {
		p.Digest.Floor = *r.digestFloor
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
  throttle, digest_window_s, digest_floor,
  created_at, updated_at, deleted_at`

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
		if err := rows.Scan(row.scanInto()...); err != nil {
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
	err := r.db(ctx).QueryRow(ctx, getPolicySQL, s.OrgID(), id).Scan(row.scanInto()...)
	if err != nil {
		return domain.Policy{}, mapErr(err, "policy_not_found", "notification policy")
	}
	return row.toDomain()
}

// listDigestPoliciesSQL selects the live policies that carry a digest window.
//
// ⭐ THE WINDOW IS THE WHOLE PREDICATE, AND THE REASON IS NOT REPEATED HERE.
// `policies_digest_reason_ck` (00058) already guarantees that a row with a window
// lists `digest` in `reasons`, so filtering on the array as well would be a second
// spelling of a constraint the database holds — and one that would silently return
// nothing if the constraint were ever the thing that broke. `Policy.Digests()`
// re-asks the coherent question in Go for the benefit of unstored PREVIEW
// candidates, which no query can vouch for.
//
// It rides `policies_digest_idx (org_id, priority) WHERE digest_window_s IS NOT
// NULL AND enabled AND deleted_at IS NULL`, which is partial precisely so the tick's
// per-tenant read costs the size of the digest configuration and not the size of
// the policy table — most installs will have no digest at all, and this query runs
// once a minute per tenant forever.
const listDigestPoliciesSQL = `
SELECT` + policyColumns + `
  FROM notification_policies
 WHERE org_id = $1 AND enabled AND deleted_at IS NULL
   AND digest_window_s IS NOT NULL
 ORDER BY priority ASC, created_at ASC, id ASC`

// ListWithDigest returns the live policies that ask for a periodic digest, in
// evaluation order.
//
// ⛔ ORDER IS NOT DECORATION HERE EITHER, though it means something different than
// it does for `ListLive`. Digest evaluation does NOT stop at the first match: every
// digest policy is its own subscription with its own window and its own channels,
// and two policies over the same namespace are two questions somebody asked, not a
// conflict to resolve by priority. What the order buys is a STABLE tick: the same
// set of policies is evaluated in the same sequence on every run, so a tick that
// exhausts its budget truncates the same tail rather than a different one each time.
func (r *PolicyRepository) ListWithDigest(
	ctx context.Context, s db.TenantScope,
) ([]domain.Policy, error) {
	rows, err := r.db(ctx).Query(ctx, listDigestPoliciesSQL, s.OrgID())
	if err != nil {
		return nil, mapErr(err, "policy_not_found", "list digest policies")
	}
	defer rows.Close()

	out := make([]domain.Policy, 0, 4)
	for rows.Next() {
		var row policyRow
		if err := rows.Scan(row.scanInto()...); err != nil {
			return nil, mapErr(err, "policy_not_found", "scan digest policy")
		}
		p, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "policy_not_found", "read digest policies")
	}
	return out, nil
}
