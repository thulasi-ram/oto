package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/platform/db"
)

// DigestBucket is one SIGNAL AXIS's contribution to one window: how many Cases
// OPENED inside it, plus the labels the policy's matchers are evaluated against.
//
// ⛔ IT WAS ONE `alert_groups` GENERATION'S CONTRIBUTION (git-bug `7570090`,
// migration `00069`). The generation is dropped, so the bucket needs a new unit, and
// the unit is the ADR-0038 GROUP AXES — `alertname`, plus `namespace` when the alert
// has one — read off `alerts` directly. That is not a substitute for the generation's
// labels, it is the SAME LABELS FROM THEIR SOURCE: `alert_groups.group_labels` was
// derived from exactly those two columns, which is why `foldDigest` reads the same
// map today as it did yesterday and no matcher had to change.
//
// ⛔ `GroupID uuid.UUID`, `Title string` AND `Severity string` WERE HERE AND ARE ALL
// THREE DELETED. Each was a column of the dropped table, and none has a successor
// that is not a fabrication:
//
//   - `GroupID` named a generation row. There is no row and no id. A bucket is a
//     derived axis pair, not an entity.
//   - `Severity` was the generation's severity, computed by the grouping module over
//     its members. A bucket spans many alerts with many severities and this package
//     owns no ordering over them ('warning' sorts after 'critical', so `max()` is
//     worse than nothing).
//   - `Title` was the RENDERED group title. Nothing renders a bucket.
//
// ⚠️ AND THE DECIDING FACT IS THAT NO PRODUCTION CODE READ ANY OF THE THREE.
// `foldDigest` reads `GroupLabels` and `Cases`; `emit` reads neither. Filling them
// with `uuid.Nil` and `""` to keep a test compiling would be a projection reporting
// values it did not read, which is the defect `internal/app/adapters.go` refuses by
// name. The one caller is the `bucket()` helper in
// `internal/notification/service/digest_tick_test.go`, which must drop three fields.
//
// ⚠️ `GroupLabels` KEEPS ITS NAME ON PURPOSE AND THE NAME IS NOW SLIGHTLY WRONG.
// `notification/service.foldDigest` reads `b.GroupLabels` and that package is not
// this one's to edit; renaming it to `Labels` is a two-file change and belongs with
// the API-layer renames the ticket still owes. The map's CONTENTS are unchanged.
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
	GroupLabels map[string]string
	// Cases is how many episodes OPENED in the window for this axis.
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
// ⛔ SYNTHETIC SIGNAL IS EXCLUDED, AND THAT IS A CORRECTNESS CLAUSE. A delivery
// drill manufactures an Alert and a Case to prove the delivery path works (ADR 0024);
// including them would put a button-press in an operator's digest, and — because
// `drill.Dispose` deletes its rows while a digest names no conversation for the
// cascade to reach — would leave a digest asserting a count of episodes that no
// longer exist. Filtering here means a digest never reports on a drill, so there is
// nothing for disposal to have to clean up.
//
// ⚠️ THE MARK MOVED FROM `alert_groups.synthetic` TO `alerts.synthetic` AND THE TEST
// IS THE SAME TEST. The drill manufactured BOTH marks together and disposal removed
// both together; `alerts.synthetic` is the one that survives 00069, it carries its
// own `alerts_synthetic_idx`, and `internal/stats/repository/rollup.go` already
// spells the drill exclusion this way. This is not a weaker filter, it is the
// surviving spelling of the same one.
//
// ⭐ AND THE GROUPLESS EPISODE COMES BACK INTO THE COUNT, WHICH IS A DELIBERATE
// WIDENING. The old statement carried `o.group_id IS NOT NULL`, so an episode whose
// §C.4 group key could not be computed was silently absent from every digest it
// belonged in — a real firing that no summary ever mentioned. There is no group to
// be missing now, so every non-synthetic Case that opened in the window is counted.
// A digest may therefore report a LARGER number than the pre-migration code did for
// the same window, and the larger number is the correct one.
//
// ⚠️ THE WINDOW IS HALF-OPEN, `[start, end)`, AND IT HAS TO BE. Adjacent windows
// share a boundary instant; `>= start AND <= end` would count an episode that opened
// exactly on the boundary in BOTH windows, which is the one arithmetic error a
// windowed count can make that no test with a random timestamp will ever catch.
//
// It rides `case_started_idx (org_id, started_at, id)` (migration 00053).
//
// ⭐ THE LABELS ARE BUILT, NOT READ, AND THE SHAPE IS COPIED FROM ADR 0038 EXACTLY.
// `alertname` always; `namespace` ONLY when the alert has one. An absent namespace
// must stay ABSENT rather than become the empty string, because Alertmanager's rule —
// the rule `domain.Matcher` implements — treats a missing label as the empty string,
// so `namespace != "x"` has to hold for an alert with no namespace. Emitting an
// explicit empty value would change nothing for `=` and would still be a second
// spelling of the same fact, which is how two matchers start disagreeing.
//
// ⚠️ THE BUCKET IS THE AXIS PAIR AND NOT THE ALERT, AND THE DIFFERENCE IS `$4`.
// Every alert sharing an axis pair matches every matcher identically, so splitting
// them into separate buckets would change no fold — but it WOULD multiply the row
// count, and the row count is what `LIMIT $4` truncates. A truncated fold undercounts
// (see `Buckets`), so a finer bucket makes the digest quietly wrong more often for no
// gain. The axis pair is also the closest surviving thing to the old `group_key`,
// which is what the generation was keyed by, so the bucket count stays in the range
// the limit was chosen for.
//
// The tiebreak is `(alertname, namespace)` rather than an id because there is no id
// any more, and an ORDER BY that is not total makes the truncated tail non-
// deterministic — two ticks over the same window would fold different sets.
const digestBucketsSQL = `
SELECT jsonb_strip_nulls(jsonb_build_object(
           'alertname', a.alertname,
           'namespace', a.namespace)),
       count(o.id)
  FROM alert_cases o
  JOIN alerts a ON a.id = o.alert_id AND a.org_id = o.org_id
 WHERE o.org_id = $1
   AND o.started_at >= $2
   AND o.started_at <  $3
   AND NOT a.synthetic
 GROUP BY a.alertname, a.namespace
 ORDER BY count(o.id) DESC, a.alertname ASC, a.namespace ASC NULLS LAST
 LIMIT $4`

// DigestRepository is the read model the digest tick folds.
//
// It is READ-ONLY and it owns no table. Both queries here are `SELECT`s over tables
// the notification module already reads for its card snapshot (`alert_cases`,
// `alerts` — which is where `alert_groups` went, git-bug `7570090`) plus its own
// `notifications`, so the digest introduces no new cross-module write and no new
// owner.
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
		if err := rows.Scan(&labels, &b.Cases); err != nil {
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
