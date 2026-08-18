package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/platform/db"
)

// DigestBucket is one AlertGroup generation's contribution to one window: how many
// of its Cases OPENED inside it, plus the labels the policy's matchers are
// evaluated against.
//
// ⭐ THE COUNT IS AGGREGATED IN SQL AND THE MATCHERS ARE APPLIED IN GO, AND THE
// SPLIT IS FORCED RATHER THAN CHOSEN. A policy matcher may be `=~`, an
// Alertmanager-anchored regular expression compiled by `domain.Matcher`, and there
// is no honest way to push that into a predicate — `~` in Postgres has different
// semantics, a different flavour and a different empty-label rule (a missing label
// is the empty string here, which is Alertmanager's rule and the only one that makes
// `!=` behave sanely). Evaluating them in two places would mean a policy routing one
// set of alerts on the notification path and a different set on the digest path,
// which is worse than any query cost.
//
// So the window is aggregated ONCE per tenant into buckets — one row per generation
// that had activity, which is bounded by how much actually happened rather than by
// the size of the tables — and every digest policy folds the same buckets. That is
// the ticket's "one query over rows already stored", and it is what makes N policies
// cost one query instead of N.
type DigestBucket struct {
	GroupID     uuid.UUID
	GroupLabels map[string]string
	Title       string
	Severity    string
	// Cases is how many episodes OPENED in the window for this generation.
	Cases int
}

// digestBucketsSQL counts the Cases that OPENED inside a window, per generation.
//
// ⭐ IT COUNTS CASES, AND THE ALTERNATIVES ARE WRONG RATHER THAN COARSE.
// Migration 00058 carries the full argument; in short: an Alert is an IDENTITY that
// outlives its firings, so counting alerts would count something that has been
// broken all week as news; and counting NOTIFICATIONS is circular, because it counts
// oto's own chatter and would fall when a channel was throttled — while a digest
// exists precisely for the case where the individual notifications were not sent.
// A Case is one firing episode with a `started_at`, so "what happened in this
// window" is exactly the episodes that opened inside it.
//
// ⛔ SYNTHETIC GENERATIONS ARE EXCLUDED, AND THAT IS A CORRECTNESS CLAUSE. A
// delivery drill manufactures an Alert, a Case and a generation to prove the
// delivery path works (ADR 0024); including them would put a button-press in an
// operator's digest, and — because `drill.Dispose` deletes its rows while a digest
// carries no `group_id` for the cascade to reach — would leave a digest asserting a
// count of episodes that no longer exist. Filtering here means a digest never
// reports on a drill, so there is nothing for disposal to have to clean up.
//
// ⚠️ THE WINDOW IS HALF-OPEN, `[start, end)`, AND IT HAS TO BE. Adjacent windows
// share a boundary instant; `>= start AND <= end` would count an episode that opened
// exactly on the boundary in BOTH windows, which is the one arithmetic error a
// windowed count can make that no test with a random timestamp will ever catch.
//
// It rides `case_started_idx (org_id, started_at, id)` (migration 00053).
const digestBucketsSQL = `
SELECT g.id, g.group_labels, g.title, coalesce(g.severity,''), count(o.id)
  FROM alert_cases o
  JOIN alert_groups g ON g.id = o.group_id AND g.org_id = o.org_id
 WHERE o.org_id = $1
   AND o.started_at >= $2
   AND o.started_at <  $3
   AND o.group_id IS NOT NULL
   AND NOT g.synthetic
 GROUP BY g.id, g.group_labels, g.title, g.severity
 ORDER BY count(o.id) DESC, g.id ASC
 LIMIT $4`

// DigestRepository is the read model the digest tick folds.
//
// It is READ-ONLY and it owns no table. Both queries here are `SELECT`s over tables
// the notification module already reads for its card snapshot (`alert_cases`,
// `alert_groups`) plus its own `notifications`, so the digest introduces no new
// cross-module write and no new owner.
type DigestRepository struct {
	q db.Querier
}

// NewDigestRepository builds the repository over a fallback querier.
func NewDigestRepository(q db.Querier) *DigestRepository { return &DigestRepository{q: q} }

func (r *DigestRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// Buckets aggregates one window for one tenant.
//
// `limit` bounds one window's fold. A window is a bounded slice of time, so the row
// count is bounded by how busy the tenant was — but "how busy" is exactly the number
// that is unbounded during an incident, which is when this runs, so the query is
// capped like every other sweep in oto. A truncated fold UNDERCOUNTS, which can only
// make a digest quieter or absent; it can never invent activity. The caller reports
// the truncation.
func (r *DigestRepository) Buckets(
	ctx context.Context, s db.TenantScope, start, end time.Time, limit int,
) ([]DigestBucket, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, digestBucketsSQL, s.OrgID(), start.UTC(), end.UTC(), limit)
	if err != nil {
		return nil, mapErr(err, "notification_not_found", "count a digest window")
	}
	defer rows.Close()

	out := make([]DigestBucket, 0, 32)
	for rows.Next() {
		var (
			b      DigestBucket
			labels []byte
		)
		if err := rows.Scan(&b.GroupID, &labels, &b.Title, &b.Severity, &b.Cases); err != nil {
			return nil, mapErr(err, "notification_not_found", "scan a digest bucket")
		}
		// `group_labels` is JSONB, decoded the same way `snapshot.go` decodes it — a
		// malformed map degrades to empty rather than failing the fold, and an empty
		// label set simply matches no matcher that names a label.
		b.GroupLabels = decodeStringMap(labels)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "notification_not_found", "read a digest window")
	}
	return out, nil
}

// lastDigestWindowSQL is the "last window covered" cursor, and it is the ONLY
// durable state the digest tick keeps.
//
// ⭐ IT IS DERIVED FROM THE DIGESTS THEMSELVES RATHER THAN FROM A CURSOR TABLE, and
// that is what makes "a window is covered exactly once even across a restart" true
// by construction instead of by bookkeeping. A separate cursor column would be a
// second fact about the same thing, written in a second statement, which can commit
// without its digest or vice versa; here the digest row IS the cursor, so the only
// state that can exist is state a digest was actually sent for.
//
// It rides `notif_digest_uniq (org_id, policy_id, digest_window_start)
// WHERE subject_kind = 'digest'` (00058) backwards, which is one index entry read.
//
// ⚠️ IT DELIBERATELY DOES NOT FILTER `status <> 'suppressed'`. A digest that was
// minted and then suppressed — no live channel, everything snoozed — is still a
// window oto has DECIDED about, and re-deciding it on the next tick would be a second
// opinion on a settled question (the same rule `evaluate` follows when it finds a
// pre-existing suppressed intent). The recorded suppression is the trail; the window
// does not come back.
const lastDigestWindowSQL = `
SELECT max(digest_window_start)
  FROM notifications
 WHERE org_id = $1 AND policy_id = $2 AND subject_kind = 'digest'`

// LastWindow returns the newest window this policy has already digested, or the zero
// time when it has never digested at all.
//
// The zero time is NOT an error and must not become one: it is a policy whose digest
// was just enabled, and the caller answers it by covering exactly ONE window — the
// most recent closed one — rather than replaying the policy's whole history into a
// channel (see domain.Digest.DigestWindows).
func (r *DigestRepository) LastWindow(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID,
) (time.Time, error) {
	if err := db.RequireScope(s); err != nil {
		return time.Time{}, err
	}
	var at *time.Time
	err := r.db(ctx).QueryRow(ctx, lastDigestWindowSQL, s.OrgID(), policyID).Scan(&at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// `max()` over no rows is one row holding NULL, so this is not reachable
		// today. It is handled rather than ignored because a future `GROUP BY` here
		// would make it reachable, and "never digested" is the correct reading.
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, mapErr(err, "notification_not_found", "read the last digest window")
	case at == nil:
		return time.Time{}, nil
	}
	return at.UTC(), nil
}
