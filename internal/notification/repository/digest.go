package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/platform/db"
)

// DigestCase is ONE episode a digest may count: its id, when it opened, and the
// labels the policy's matchers are evaluated against.
//
// ⛔ IT WAS `DigestBucket` — ONE AXIS PAIR'S AGGREGATE `count(*)` — AND THE
// AGGREGATE HAD TO GO (git-bug `342e071`/`a8a4010`/`893cee4`, migration `00070`).
// A digest is no longer a slice of clock but a BOUNDED-LOOKBACK SET OF CASES: it
// reads `[start - DigestLookback, end)` and subtracts the Cases it has already
// reported, which is what lets an episode whose transaction committed after the tick
// had read its window still be counted by the next one. Subtracting "already
// reported" requires CASE IDENTITY, and a `count(*)` grouped by `(alertname,
// namespace)` has none — there is no way to remove three particular episodes from a
// number. So the row is the episode.
//
// ⚠️ AND THAT REVERSES AN ARGUMENT THIS FILE USED TO MAKE, ON PURPOSE. The old
// comment said a finer bucket "would change no fold — but it WOULD multiply the row
// count, and the row count is what `LIMIT $4` truncates", so a finer bucket made the
// digest quietly wrong more often for no gain. Every clause of that is still true
// except the last: the gain now exists and it is the only way to have it. What the
// argument still buys is the ORDER BY below, which is where the truncation was made
// to cost as little as possible.
//
// ⭐ THE COUNT IS NOW SUMMED IN GO AND THE MATCHERS WERE ALWAYS APPLIED IN GO, WHICH
// MEANS ONE FEWER SPLIT RATHER THAN ONE MORE. The split was forced, not chosen: a
// policy matcher may be `=~`, an Alertmanager-anchored regular expression compiled by
// `domain.Matcher`, and there is no honest way to push that into a predicate — `~` in
// Postgres has different semantics, a different flavour and a different empty-label
// rule (a missing label is the empty string here, which is Alertmanager's rule and
// the only one that makes `!=` behave sanely). Evaluating them in two places would
// mean a policy routing one set of alerts on the notification path and a different
// set on the digest path, which is worse than any query cost. Now the WHOLE decision
// — match, dedupe, sum, floor — happens in the one place, over rows SQL only had to
// fetch.
//
// ⭐ THE READ IS STILL ONCE PER SPAN PER TENANT, SO N POLICIES STILL COST FEWER THAN
// N QUERIES. Nothing in this row depends on the policy: two policies on the same
// window length ask about the same span, fold the same rows with their own matchers,
// and subtract their own marks. That is the property `service.SweepOrg`'s cache
// exists for and it is unchanged.
type DigestCase struct {
	ID uuid.UUID
	// StartedAt is when the episode OPENED — `alert_cases.started_at`.
	//
	// ⚠️ IT IS OTO'S CLOCK READ BEFORE THE TRANSACTION, NOT A COMMIT TIME, and the
	// bounded lookback exists because of that. See `domain.DigestLookback`.
	StartedAt time.Time
	// Labels is the ADR-0038 group axes — `alertname`, plus `namespace` when the
	// alert has one.
	//
	// ⛔ IT WAS SPELLED `GroupLabels` AND THE NAME OUTLIVED THE GROUP. Migration
	// `00069` dropped `alert_groups`, so these are read off `alerts` directly; they
	// are not a substitute for the dropped generation's labels but the SAME LABELS
	// FROM THEIR SOURCE, because `alert_groups.group_labels` was derived from exactly
	// those two columns. The old comment recorded that the rename was owed and that
	// it was a two-file change belonging with the API renames; both files are being
	// rewritten here anyway, so it is paid now.
	Labels map[string]string
}

// digestCasesSQL lists the Cases that OPENED inside a span, one row per episode.
//
// ⭐ IT LISTS CASES, AND THE ALTERNATIVES ARE WRONG RATHER THAN COARSE.
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
// nothing for disposal to have to clean up. `alerts.synthetic` carries its own
// `alerts_synthetic_idx`, and `internal/stats/repository/rollup.go` already spells
// the drill exclusion this way.
//
// ⭐ THE GROUPLESS EPISODE IS COUNTED, WHICH IS A DELIBERATE WIDENING. The
// pre-`00069` statement carried `o.group_id IS NOT NULL`, so an episode whose §C.4
// group key could not be computed was silently absent from every digest it belonged
// in — a real firing that no summary ever mentioned. There is no group to be missing
// now, so every non-synthetic Case that opened in the span is listed.
//
// ⚠️ THE SPAN IS HALF-OPEN, `[from, to)`, AND IT HAS TO BE. Adjacent windows share a
// boundary instant; `>= from AND <= to` would count an episode that opened exactly on
// the boundary in BOTH windows, which is the one arithmetic error a windowed count can
// make that no test with a random timestamp will ever catch. The half-openness is
// what makes the LOOKBACK safe too: the tail `[start - L, start)` is the previous
// window's own half-open interval, so a Case is offered to exactly two reads and the
// mark decides which one counts it.
//
// It rides `case_started_idx (org_id, started_at, id)` (migration 00053), which is
// also why the ORDER BY costs nothing.
//
// ⭐ THE LABELS ARE BUILT, NOT READ, AND THE SHAPE IS COPIED FROM ADR 0038 EXACTLY.
// `alertname` always; `namespace` ONLY when the alert has one. An absent namespace
// must stay ABSENT rather than become the empty string, because Alertmanager's rule —
// the rule `domain.Matcher` implements — treats a missing label as the empty string,
// so `namespace != "x"` has to hold for an alert with no namespace. Emitting an
// explicit empty value would change nothing for `=` and would still be a second
// spelling of the same fact, which is how two matchers start disagreeing.
//
// ⭐⭐ THE ORDER IS NEWEST FIRST, AND THAT IS WHERE `LIMIT $4` WAS MADE TO HURT AS
// LITTLE AS POSSIBLE. A truncated read UNDERCOUNTS, which can only make a digest
// quieter or absent and can never invent activity — but WHICH rows are lost is a
// choice, and this read covers two intervals with very different value. The window
// `[start, end)` is the digest's actual subject; the lookback tail `[start-L, start)`
// is almost entirely Cases a previous digest already reported, every one of which is
// marked and folds to zero. Descending on `started_at` therefore spends the whole
// limit on the window before it spends any of it on a tail that mostly cannot
// contribute. Ascending — the obvious spelling — would drop the window first and
// truncate exactly the rows the digest exists to count.
//
// The tiebreak is `o.id` because an ORDER BY that is not total makes the truncated
// tail non-deterministic: two ticks over the same span would fold different sets, and
// the second one would mark Cases the first had never seen.
const digestCasesSQL = `
SELECT o.id,
       o.started_at,
       jsonb_strip_nulls(jsonb_build_object(
           'alertname', a.alertname,
           'namespace', a.namespace))
  FROM alert_cases o
  JOIN alerts a ON a.id = o.alert_id AND a.org_id = o.org_id
 WHERE o.org_id = $1
   AND o.started_at >= $2
   AND o.started_at <  $3
   AND NOT a.synthetic
 ORDER BY o.started_at DESC, o.id DESC
 LIMIT $4`

// DigestRepository is the state the digest tick reads and the state it keeps.
//
// ⛔ IT USED TO BE READ-ONLY AND OWN NO TABLE, AND THE COMMENT THAT SAID SO WAS ALSO
// THE STATEMENT OF THREE BUGS. It read: "the digest row IS the cursor, so the only
// state that can exist is state a digest was actually sent for". That property is
// genuinely valuable — it makes "a window is covered exactly once even across a
// restart" true by construction rather than by bookkeeping — and it is also why
// `893cee4` could not be fixed: a window that was EXAMINED AND FOUND QUIET produces
// no row, so it is indistinguishable from a window that was NEVER EXAMINED, and the
// cursor cannot advance past it. Coverage and "a digest was sent" have to be two
// separately recorded facts, and migration `00070` is where they became two.
//
// So this repository now owns two small tables of its own, and neither of them is a
// second opinion about a digest:
//
//   - `notification_digest_coverage` — one row per policy, "examined up to here". It
//     advances on every window the tick LOOKED at, including a quiet one, which is
//     what makes a quiet policy cost one comparison per tick instead of six queries
//     forever.
//   - `notification_digest_cases` — one narrow row per (policy, Case) the policy
//     matched, "this episode has been accounted for, and here is the digest that
//     reported it, or NULL if the window it fell in did not clear its floor". It is
//     the dedupe state the bounded lookback needs, and its ABSENCE for a matched Case
//     older than `DigestLookback` is the unrecoverable gap `ReconcileOrg` counts.
//
// Everything else here is still a `SELECT` over tables the notification module
// already reads (`alert_cases`, `alerts`) plus its own `notifications`, so the digest
// introduces no new cross-module write and no new owner.
type DigestRepository struct {
	q db.Querier
}

// NewDigestRepository builds the repository over a fallback querier.
func NewDigestRepository(q db.Querier) *DigestRepository { return &DigestRepository{q: q} }

func (r *DigestRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// Cases lists one span's episodes for one tenant.
//
// `limit` bounds one read. A span is a bounded slice of time, so the row count is
// bounded by how busy the tenant was — but "how busy" is exactly the number that is
// unbounded during an incident, which is when this runs, so the query is capped like
// every other sweep in oto. A truncated read UNDERCOUNTS, which can only make a
// digest quieter or absent; it can never invent activity. See `digestCasesSQL` for
// why the ORDER BY makes the truncation land on the rows that could not have
// contributed anyway.
func (r *DigestRepository) Cases(
	ctx context.Context, s db.TenantScope, from, to time.Time, limit int,
) ([]DigestCase, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, digestCasesSQL, s.OrgID(), from.UTC(), to.UTC(), limit)
	if err != nil {
		return nil, mapErr(err, "notification_not_found", "list a digest span")
	}
	defer rows.Close()

	out := make([]DigestCase, 0, 64)
	for rows.Next() {
		var (
			c      DigestCase
			labels []byte
		)
		if err := rows.Scan(&c.ID, &c.StartedAt, &labels); err != nil {
			return nil, mapErr(err, "notification_not_found", "scan a digest case")
		}
		c.StartedAt = c.StartedAt.UTC()
		// The label map is JSONB, decoded the same way `snapshot.go` decodes it — a
		// malformed map degrades to empty rather than failing the fold, and an empty
		// label set simply matches no matcher that names a label.
		c.Labels = decodeStringMap(labels)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "notification_not_found", "read a digest span")
	}
	return out, nil
}

// digestCoveredToSQL is the cursor: the INSTANT this policy's digests have been
// examined up to.
//
// ⭐⭐ IT IS AN INSTANT AND NOT A WINDOW START, WHICH IS THE FIX FOR `342e071`. This
// query used to be `SELECT max(digest_window_start) FROM notifications ... WHERE
// subject_kind = 'digest'` — the START of the last window a digest was sent for. A
// start is meaningless without the LENGTH in force when it was sent, and the length
// was stored nowhere, so `DigestWindows` re-floored the start under the CURRENT
// length and stepped forward by the CURRENT length. Narrowing `digest_window_s` then
// re-tiled a span an earlier digest had already summarised into new, shorter windows
// that all sat AFTER the recorded start and were therefore all treated as uncovered.
// An instant does not change meaning when the tiling changes.
//
// ⭐ IT IS ALSO A ROW OF ITS OWN AND NOT `max(digest_covered_to)` OVER THE DIGESTS,
// WHICH IS THE FIX FOR `893cee4`. Deriving the cursor from the digests themselves is
// what made it impossible for a window to be examined without being sent: a window
// below its policy's floor writes no row — deliberately, because a `suppressed` row
// per empty window would put one row per policy per ten minutes into the audit log
// forever — so the cursor froze, the next tick re-derived the same span one window
// longer, and after `MaxDigestBackfill` quiet windows the tick ran six aggregate
// queries and logged a data-loss warning every sixty seconds, forever, on a policy
// that had never had anything to send. Coverage is not a fact about a message; it is
// a fact about how far the READER got, and it belongs on its own row.
//
// ⚠️ IT IS NOT THE SAME FACT AS `notifications.digest_covered_to`, AND THE TWO MUST
// NOT BE MERGED. The column on the digest row says what THAT MESSAGE covered — an
// immutable render fact, written once, which is what lets a card state its own span
// instead of inferring one from the policy's current configuration. The row here says
// how far the POLICY has been examined, which advances on quiet windows too and is
// therefore always at or ahead of the newest message. One is evidence; the other is
// a position.
const digestCoveredToSQL = `
SELECT covered_to
  FROM notification_digest_coverage
 WHERE org_id = $1 AND policy_id = $2`

// CoveredTo returns the instant this policy has been examined up to, or the zero
// time when it has never been examined at all.
//
// The zero time is NOT an error and must not become one: it is a policy whose digest
// was just enabled — or one whose only digests predate migration 00070 and therefore
// carry no coverage row — and the caller answers it by covering exactly ONE window,
// the most recent closed one, rather than replaying the policy's whole history into a
// channel (see domain.Digest.DigestWindows).
func (r *DigestRepository) CoveredTo(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID,
) (time.Time, error) {
	if err := db.RequireScope(s); err != nil {
		return time.Time{}, err
	}
	var at time.Time
	err := r.db(ctx).QueryRow(ctx, digestCoveredToSQL, s.OrgID(), policyID).Scan(&at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, mapErr(err, "notification_not_found", "read the digest coverage")
	}
	return at.UTC(), nil
}

// advanceCoverageSQL moves one policy's coverage forward.
//
// ⛔ `GREATEST` IS WHAT MAKES IT MONOTONE, AND MONOTONE IS THE WHOLE CONTRACT. Two
// pods can tick in the same second, and the loser may finish second while holding an
// OLDER `reached` — because it read a stale cursor, or because its clock lags (the
// standing skew tax, git-bug `b21ba93`). A plain assignment would then move coverage
// BACKWARDS, and the next tick would re-derive windows that had already been examined:
// harmless for the messages, because `notif_digest_uniq` refuses the second row and
// every Case in the span is already marked, but it would also re-run the queries the
// cursor exists to avoid, indefinitely, whenever two pods disagreed about the time.
// A cursor that can go backwards is not a cursor.
const advanceCoverageSQL = `
INSERT INTO notification_digest_coverage (org_id, policy_id, covered_to, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (org_id, policy_id) DO UPDATE
   SET covered_to = GREATEST(notification_digest_coverage.covered_to, EXCLUDED.covered_to),
       updated_at = EXCLUDED.updated_at`

// AdvanceCoverage records that this policy has now been examined up to `reached`.
//
// It is called for a window that was EXAMINED, whatever the examination decided —
// sent, below the floor, or already covered by another pod. That is the point: the
// old design only ever wrote a cursor as a side effect of sending, so "quiet" and
// "never looked at" were the same absence.
func (r *DigestRepository) AdvanceCoverage(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID, reached, now time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	_, err := r.db(ctx).Exec(ctx, advanceCoverageSQL,
		s.OrgID(), policyID, reached.UTC(), now.UTC())
	if err != nil {
		return mapErr(err, "notification_not_found", "advance the digest coverage")
	}
	return nil
}

// markedCasesSQL lists the Cases in a span this policy has already accounted for.
//
// It rides `digest_case_span_idx (org_id, started_at, policy_id)` as an index-only
// scan: the range is on `started_at` and `policy_id` is filtered inside the index, so
// no heap page is touched. Leading with `policy_id` instead would serve this one query
// marginally better and would need a SECOND index for the retention sweep, which
// deletes across every policy at once — and a tenant has a handful of digest policies,
// not thousands, so the filter is cheap and one index is worth more than the seek.
const markedCasesSQL = `
SELECT case_id
  FROM notification_digest_cases
 WHERE org_id = $1
   AND policy_id = $2
   AND started_at >= $3
   AND started_at <  $4`

// Marked is the set of Cases in `[from, to)` this policy has already accounted for.
//
// ⭐ "ACCOUNTED FOR" IS WIDER THAN "REPORTED", AND THE DIFFERENCE IS THE POINT. A
// mark whose `reported_in` is NULL means the window this Case fell in was EXAMINED
// AND FOUND QUIET — it did not clear the policy's floor — and re-offering it to a
// later digest would be a second opinion on a settled question, exactly as
// re-examining a closed window was under the old design. The set returned here is
// therefore what the lookback SUBTRACTS, and it is why a two-minute tail cannot
// double-report.
func (r *DigestRepository) Marked(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID, from, to time.Time,
) (map[uuid.UUID]struct{}, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, markedCasesSQL, s.OrgID(), policyID, from.UTC(), to.UTC())
	if err != nil {
		return nil, mapErr(err, "notification_not_found", "read the digest marks")
	}
	defer rows.Close()

	out := make(map[uuid.UUID]struct{}, 64)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err, "notification_not_found", "scan a digest mark")
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "notification_not_found", "read the digest marks")
	}
	return out, nil
}

// markCasesSQL records that a policy has accounted for a set of Cases.
//
// ⛔ `DO NOTHING` AND NOT `DO UPDATE`, AND A MARK IS THEREFORE WRITE-ONCE. The
// tempting refinement is to let a later, wider digest upgrade a `reported_in IS NULL`
// mark into a real report — and it would be wrong. A mark with no report means the
// window was examined and did not clear its floor, which is a decision oto made about
// that episode; the pre-`00070` design reached the same conclusion by re-querying the
// closed window and getting the same answer, under a comment observing that
// "re-examining a closed window is a query whose answer cannot have changed". Letting
// the answer change would make whether an episode is reported depend on when somebody
// edited `digest_window_s`.
//
// ⚠️ THERE IS NO FK ON `case_id`, ON THE SAME TERMS `notifications.alert_id` AND
// `case_id` HAVE NONE. `alert_cases` is reapable (ADR 0024, `case.reap`), and a mark
// whose Case has aged out is still the truthful statement that oto accounted for it —
// a cascade would delete the evidence and make the reconciler report the episode as
// never reported. The retention sweep is what bounds this table, not the parent's
// lifetime.
const markCasesSQL = `
INSERT INTO notification_digest_cases (
  org_id, policy_id, case_id, started_at, reported_in, marked_at)
SELECT $1, $2, c.case_id, c.started_at, $5, $6
  FROM unnest($3::uuid[], $4::timestamptz[]) AS c(case_id, started_at)
ON CONFLICT (org_id, policy_id, case_id) DO NOTHING`

// Mark records that this policy has accounted for these Cases.
//
// `reportedIn` is the digest that reported them, or nil when the window they fell in
// did not clear the policy's floor. Both are marks; only one is a report. See
// `markCasesSQL`.
//
// It is a single statement over arrays rather than a loop, so it is atomic on its own
// and composes with the caller's transaction when there is one: the digest row, its
// deliveries, their jobs AND these marks commit together, which is what stops a crash
// between the message and the mark from re-reporting the same episodes.
func (r *DigestRepository) Mark(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID,
	reportedIn *uuid.UUID, cases []DigestCase, now time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if len(cases) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(cases))
	at := make([]time.Time, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, c.ID)
		at = append(at, c.StartedAt.UTC())
	}
	_, err := r.db(ctx).Exec(ctx, markCasesSQL,
		s.OrgID(), policyID, ids, at, reportedIn, now.UTC())
	if err != nil {
		return mapErr(err, "notification_not_found", "mark the digest cases")
	}
	return nil
}

// pruneMarksSQL is the retention sweep over the mark table.
const pruneMarksSQL = `
DELETE FROM notification_digest_cases
 WHERE org_id = $1 AND started_at < $2`

// PruneMarks deletes the marks for Cases that opened before `before`, and reports how
// many rows went.
//
// ⭐ IT IS WHAT KEEPS THE DEDUPE STATE PROPORTIONAL TO RECENT ACTIVITY RATHER THAN TO
// ALL OF HISTORY, and that proportionality is the reason the bounded lookback was
// chosen over a permanent membership record. `before` is derived from
// `domain.DigestMarkRetention`, which is sized by the two readers that need a mark
// LONGER than the lookback does — a re-examined wide window, and the reconciler — not
// by the dedupe itself.
//
// ⚠️ PRUNING TOO EAGERLY IS NOT A LOST OPTIMISATION, IT IS A FALSE ALARM. The
// reconciler reads the ABSENCE of a mark as "this episode was never reported", so a
// horizon shorter than `DigestReconcileHorizon` would make it report every episode
// older than the horizon as a gap. The retention constant is derived to sit outside
// it and the caller passes nothing else.
func (r *DigestRepository) PruneMarks(
	ctx context.Context, s db.TenantScope, before time.Time,
) (int64, error) {
	if err := db.RequireScope(s); err != nil {
		return 0, err
	}
	tag, err := r.db(ctx).Exec(ctx, pruneMarksSQL, s.OrgID(), before.UTC())
	if err != nil {
		return 0, mapErr(err, "notification_not_found", "prune the digest marks")
	}
	return tag.RowsAffected(), nil
}
