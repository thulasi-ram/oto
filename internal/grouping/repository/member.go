package repository

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// memberRow is the membership projection of an `alert_cases` row.
// Unexported, per the three-model rule.
//
// ⭐ THE COLUMN NAMES ARE THE EPISODE'S, AND THE FIELD NAMES ARE MEMBERSHIP'S,
// because that is precisely what this change established: an episode's `group_id`
// IS its membership, its `started_at` IS when it joined, and its `ended_at` IS
// when it left.
type memberRow struct {
	groupID  uuid.UUID
	caseID   uuid.UUID
	orgID    uuid.UUID
	alertID  uuid.UUID
	joinedAt time.Time
	leftAt   *time.Time
}

var memberColumnList = []string{
	"group_id", "id", "org_id", "alert_id", "started_at", "ended_at",
}

var memberColumns = strings.Join(memberColumnList, ", ")

func (r *memberRow) scanDest() []any {
	return []any{&r.groupID, &r.caseID, &r.orgID, &r.alertID, &r.joinedAt, &r.leftAt}
}

func (r *memberRow) toDomain() (domain.Member, error) {
	m, err := domain.NewMember(domain.MemberParams{
		GroupID:  r.groupID,
		CaseID:   r.caseID,
		OrgID:    r.orgID,
		AlertID:  r.alertID,
		JoinedAt: r.joinedAt,
		LeftAt:   timeOrZero(r.leftAt),
	})
	if err != nil {
		return domain.Member{}, errs.Internal("member_row_invalid", err)
	}
	return m, nil
}

// MemberRepository is the SQL that reads a generation's membership.
//
// ⛔ IT OWNS NO TABLE, AND IT WRITES NOTHING. There was an `alert_group_members`
// until membership stopped being an event and became a property of the episode
// (migration 00051): once the group key is derived from the alert's own labels
// (ADR 0038), Episode → Group is many-to-one and `alert_cases.group_id` is
// the whole record. Every statement below is a READ of `alert_cases` — the
// same bounded, documented cross-module table read `Rollup` has always performed,
// not a call into another module's repository, which §F.5 rule 4 forbids.
//
// ⭐ MEMBERSHIP IS STILL HISTORY, AND IT IS NOW HISTORY SOMETHING WRITES. The old
// table's `left_at` had no production writer, so every "current members" read
// matched every row that had ever been inserted and the point-in-time replay could
// only show a generation growing. `ended_at` is written by the §B.3 state machine
// on every T5 and T6, and `case_terminal_ended` makes `ended_at IS NULL` and
// `state IN ('firing','suppressed')` the same predicate by CHECK constraint.
type MemberRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewMemberRepository builds the repository over a fallback querier.
func NewMemberRepository(q db.Querier, clk clock.Clock) *MemberRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &MemberRepository{q: q, clock: clk}
}

func (r *MemberRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

var allMembersSQL = `
SELECT ` + memberColumns + `
  FROM alert_cases
 WHERE org_id = $1 AND group_id = $2
 ORDER BY started_at ASC`

// AllMembers lists every episode that has ever been in a generation, live and
// ended, so the group card can be replayed.
//
// NOTE (planner): case_group_idx (org_id, group_id, started_at DESC), read
// backwards for the ASC order. It is NOT case_group_live_idx: this read wants the
// ended episodes, which is exactly what that partial index excludes.
func (r *MemberRepository) AllMembers(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
) ([]domain.Member, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, allMembersSQL, s.OrgID(), groupID)
	if err != nil {
		return nil, mapErr(err, "list group members")
	}
	defer rows.Close()
	return collectMembers(rows)
}

// membersAtSQL is domain.Member.WasMemberAt, written as a predicate.
//
// ⭐ THE INSTANT IS AN ARGUMENT, NOT A FILTER THE CALLER APPLIES AFTERWARDS.
// `started_at <= $3` is `!t.Before(joinedAt)` and `ended_at IS NULL OR ended_at >
// $3` is `leftAt.IsZero() || t.Before(leftAt)` — the same two clauses, in the
// place that can discard the rows instead of shipping them. The service used to
// read `AllMembers` (every membership the generation has ever had) and drop most
// of them in Go, which is a replay of a forty-member group paid for at the size of
// its whole history.
//
// ⭐⭐ AND IT IS NON-MONOTONIC FOR THE FIRST TIME. While membership lived in
// `alert_group_members`, `left_at` was never written by anything, so the second
// clause was a tautology and a replay could only ever show the generation GROWING
// — a "point in time" that was really "everything up to that point". `ended_at`
// is written by the state machine, so this now answers the question it is named
// for.
var membersAtSQL = `
SELECT ` + memberColumns + `
  FROM alert_cases
 WHERE org_id = $1 AND group_id = $2
   AND started_at <= $3
   AND (ended_at IS NULL OR ended_at > $3)
 ORDER BY started_at ASC`

// MembersAt lists the episodes that were in a generation at one instant.
//
// NOTE (planner): case_group_idx (org_id, group_id, started_at DESC) carries both
// equalities and the range on `started_at`; the `ended_at` clause is a filter and
// the ORDER BY is the index read backwards. The read has NO PAGE — it answers
// "what was in the group when the thread was posted", and the caller wants the
// whole set — so a bound is not what it is short of. The partial
// case_group_live_idx is the wrong index here on purpose: a replay is precisely the
// read that wants the ended episodes back.
func (r *MemberRepository) MembersAt(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, at time.Time,
) ([]domain.Member, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, membersAtSQL, s.OrgID(), groupID, at.UTC())
	if err != nil {
		return nil, mapErr(err, "list group members at")
	}
	defer rows.Close()
	return collectMembers(rows)
}

// groupsForAlertSQL orders by `seq DESC` rather than by `started_at DESC`, and
// the two are the same order: `seq` is the episode number within the Alert,
// 1-based and gapless, so it increases exactly as `started_at` does. What `seq`
// buys is case_alert_idx (org_id, alert_id, seq DESC), which serves the whole
// statement — equalities, order and LIMIT — while `started_at` would have to be
// sorted.
var groupsForAlertSQL = `
SELECT ` + memberColumns + `
  FROM alert_cases
 WHERE org_id = $1 AND alert_id = $2 AND group_id IS NOT NULL
 ORDER BY seq DESC
 LIMIT $3`

// GroupsForAlert answers "which groups has this alert been part of", newest
// first.
//
// An episode with no group is ABSENT: `group_id` is NULL when the §C.4 key could
// not be computed and the ingest orchestrator recorded the signal groupless, and
// a membership row naming no group would be a claim the schema does not make.
func (r *MemberRepository) GroupsForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, limit int,
) ([]domain.Member, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, groupsForAlertSQL, s.OrgID(), alertID, db.ClampLimit(limit))
	if err != nil {
		return nil, mapErr(err, "list groups for alert")
	}
	defer rows.Close()
	return collectMembers(rows)
}

const distinctJoinsSQL = `
SELECT count(DISTINCT alert_id), max(started_at)
  FROM alert_cases
 WHERE org_id = $1 AND group_id = $2 AND started_at >= $3`

// DistinctJoinsSince counts how many DISTINCT alerts joined a generation inside
// the storm window, and when the most recent join was.
//
// ⭐ It counts DISTINCT ALERTS, not episodes. One flapping alert re-firing forty
// times is a FLAP, damped per-alert by flap_score; forty different alerts arriving
// at once is a STORM, damped per-group by storm collapse. Counting episodes would
// collapse a group because one alert was noisy, hiding the thirty-nine quiet ones
// behind it.
//
// ⭐ AN EPISODE JOINS WHEN IT OPENS, so `started_at >= $3` is the same window the
// old `joined_at >= $3` named — read off the row that DEFINES the join rather
// than off a second row recording that it happened. §B.6 is unchanged by 00051.
//
// NOTE (planner): case_group_idx (org_id, group_id, started_at DESC) carries the
// two equalities and puts the window on the leading edge of what remains.
func (r *MemberRepository) DistinctJoinsSince(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, since time.Time,
) (int, time.Time, error) {
	if err := db.RequireScope(s); err != nil {
		return 0, time.Time{}, err
	}
	var n int64
	var last *time.Time
	err := r.db(ctx).QueryRow(ctx, distinctJoinsSQL, s.OrgID(), groupID, since.UTC()).Scan(&n, &last)
	if err != nil {
		return 0, time.Time{}, mapErr(err, "count group joins")
	}
	return int(n), timeOrZero(last), nil
}

const memberRollupSQL = `
SELECT
    count(*) FILTER (WHERE o.state = 'firing')                       AS firing,
    count(*) FILTER (WHERE o.state = 'suppressed')                   AS suppressed,
    count(*) FILTER (WHERE o.state = 'resolved')                     AS resolved,
    count(*) FILTER (WHERE o.state = 'expired')                      AS expired,
    count(*)                                                         AS total,
    count(*) FILTER (WHERE o.ack_state = 'acked')                    AS acked,
    max(a.severity)                                                  AS severity
  FROM alert_cases o
  JOIN alerts a ON a.id = o.alert_id
 WHERE o.org_id = $1 AND o.group_id = $2`

// Rollup recomputes a generation's membership counts.
//
// ⛔ IT IS OVER EVERY EPISODE THE GENERATION HAS HELD, NOT OVER THE LIVE ONES,
// and there is no `ended_at IS NULL` clause missing here. A card reading "12
// alerts, 3 firing, 9 resolved" is what the counts are FOR; restricting the
// aggregate to live members would make `resolved` and `expired` permanently zero
// and shrink `total` to the firing count. The breakdown is the answer, and
// `state` is what breaks it down.
//
// (Before 00051 this aggregate carried `m.left_at IS NULL`, which matched every
// row because nothing ever wrote `left_at`. It was computing this by accident. It
// computes it on purpose now.)
//
// It is a recomputation rather than an increment on purpose: an increment that
// misses one transition is wrong forever, while a recomputation is wrong only
// until the next one. The group's counts are a PROJECTION of its members'
// episode states, and this is the query that derives it.
//
// NOTE (layering): this reads `alert_cases` and `alerts`, which the alerts
// module owns. It is a table READ, not a call into another module's repository —
// §F.5 rule 4 forbids the latter — and it exists because the alternative is
// shipping every member's state across a service boundary on every observation.
//
// NOTE (planner): the driving index is case_group_idx (org_id, group_id,
// started_at DESC); the join to `alerts` is a primary-key lookup. The severity
// aggregate is `max()` over the RAW label, which orders lexically and is therefore
// a placeholder for a real precedence: the caller re-derives the display severity
// from the member set when it needs the §H.2 ordering.
func (r *MemberRepository) Rollup(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
) (domain.Counts, string, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Counts{}, "", err
	}
	var (
		firing, suppressed, resolved, expired, total, acked int64
		severity                                            *string
	)
	err := r.db(ctx).QueryRow(ctx, memberRollupSQL, s.OrgID(), groupID).
		Scan(&firing, &suppressed, &resolved, &expired, &total, &acked, &severity)
	if err != nil {
		return domain.Counts{}, "", mapErr(err, "roll up group members")
	}
	return domain.Counts{
		Firing:     int(firing),
		Suppressed: int(suppressed),
		Resolved:   int(resolved),
		Expired:    int(expired),
		Total:      int(total),
		Acked:      int(acked),
	}, strOrEmpty(severity), nil
}

// listCurrentMembersSQL is the keyset page of a generation's LIVE members, and
// the ONLY read of them: there is no unbounded sibling to reach for.
//
// ⭐ `ended_at IS NULL` IS THE WHOLE FIX. The predicate it replaces —
// `left_at IS NULL` on the old join table — matched every row ever inserted,
// because `Leave` existed at three layers and was called from nowhere. So this
// page listed resolved and expired episodes as current members, and an alert that
// resolved and re-fired inside one generation appeared TWICE, once in each state.
// `case_terminal_ended` guarantees `ended_at IS NULL` is exactly
// `state IN ('firing','suppressed')`, and the state machine writes it.
//
// ⭐ It orders NEWEST FIRST and pages by `(started_at, id)`, which is a total
// order because `id` is the primary key of `alert_cases`. Both callers used
// to read EVERY member and slice the result in Go: correct for a group of forty, a
// full membership fetch for a storm of five thousand, and a page whose `has_more`
// was computed from a list the caller had already paid to materialise.
var listCurrentMembersSQL = `
SELECT ` + memberColumns + `
  FROM alert_cases
 WHERE org_id = $1 AND group_id = $2 AND ended_at IS NULL
   AND ($3::timestamptz IS NULL OR (started_at, id) < ($3, $4))
 ORDER BY started_at DESC, id DESC
 LIMIT $5`

// ListCurrentMembers returns one keyset page of a generation's live members.
//
// NOTE (planner): the driving index is case_group_live_idx (00051), `(org_id,
// group_id, started_at DESC, id DESC) WHERE ended_at IS NULL`. It carries the two
// equalities, the partial predicate and the WHOLE sort key, so the LIMIT stops the
// scan and nothing is sorted in memory — asserted against a real plan in
// member_plan_test.go, with the index dropped as the control. The keyset arrives
// inside `$3 IS NULL OR …`, which a CUSTOM plan folds away so the row comparison
// becomes an index bound and a deep page starts at the cursor; a GENERIC plan
// cannot fold it and re-walks the rows it has passed, which a generation of a few
// thousand members can afford. case_group_idx alone cannot serve this: it carries
// no tiebreak for the `(started_at, id)` comparison and spans the generation's
// whole history rather than its live membership.
func (r *MemberRepository) ListCurrentMembers(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, p db.Keyset,
) ([]domain.Member, db.Cursor, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := db.ClampLimit(p.Limit)

	var (
		afterAt *time.Time
		afterID uuid.UUID
	)
	if !p.Cursor.IsZero() {
		afterAt = nilTime(p.Cursor.SortKey)
		afterID = p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, listCurrentMembersSQL,
		s.OrgID(), groupID, afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list group members")
	}
	defer rows.Close()

	collected, err := collectMembers(rows)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	page, hasMore := db.PageOf(collected, limit)
	if len(page) == 0 {
		return nil, db.Cursor{Hash: p.Cursor.Hash}, nil
	}
	last := page[len(page)-1]
	return page, db.NextCursor(last.JoinedAt(), last.CaseID(), p.Cursor.Hash, hasMore), nil
}

// memberAlertsSQL is the fan-out's candidate read, and it is BOUNDED.
//
// ⭐ THE `LIMIT` IS THE POINT. It carried no limit until the group verbs were
// given a ceiling: every ack, comment, snooze and unsnooze of a generation
// materialised the WHOLE membership and then opened one write transaction per
// row of it, so a storm's group button was ~5 000 sequential commits inside one
// request. The bound now arrives as SQL, where it also stops the scan, rather
// than as a slice in the service — a service that reads everything and keeps the
// first 500 has still paid for the storm.
//
// It orders OLDEST FIRST, unlike `listCurrentMembersSQL`, and that is a different
// question rather than an inconsistency: a preview shows what arrived most
// recently, a fan-out applies to the members that have been waiting longest. The
// order is also what makes a truncated fan-out reproducible — pressing the button
// twice reaches the same 500 members, not a reshuffle — which is why `o.id` is a
// TIEBREAK and not decoration: one Alertmanager batch opens every episode in it at
// the same instant, so `started_at` alone is not a total order and the claim above
// would be false for exactly the batch the ceiling exists for.
//
// NOTE (planner): case_group_live_idx is `(org_id, group_id, started_at DESC, id
// DESC) WHERE ended_at IS NULL`. It carries the two equalities and the partial
// predicate, and Postgres reads a DESC index backwards for an ASC order, so the
// LIMIT stops the scan here too.
const memberAlertsSQL = `
SELECT o.alert_id, o.id
  FROM alert_cases o
 WHERE o.org_id = $1 AND o.group_id = $2 AND o.ended_at IS NULL
 ORDER BY o.started_at ASC, o.id ASC
 LIMIT $3`

const countCurrentMembersSQL = `
SELECT count(*)
  FROM alert_cases
 WHERE org_id = $1 AND group_id = $2 AND ended_at IS NULL`

// MemberAlert pairs a currently-joined alert with the episode that joined.
type MemberAlert struct {
	AlertID uuid.UUID
	CaseID  uuid.UUID
}

// CurrentMemberAlerts lists at most `limit` LIVE members of a generation, oldest
// first.
//
// It is what the §B.8.3 group snooze fans out over: one snooze per live member
// alert. Alerts that join later are NOT snoozed — a snooze is never predictive.
//
// ⚠️ `limit` IS NOT A PAGE SIZE and is deliberately not run through
// `clampLimit`: the §E.1 page bounds cap at 200 because that is what an API
// response should carry, and this is a WRITE ceiling with an argument of its own
// (domain.FanOutLimit). A non-positive limit means the caller did not choose, and
// gets that ceiling rather than an unbounded read.
func (r *MemberRepository) CurrentMemberAlerts(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, limit int,
) ([]MemberAlert, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = domain.FanOutLimit
	}
	rows, err := r.db(ctx).Query(ctx, memberAlertsSQL, s.OrgID(), groupID, limit)
	if err != nil {
		return nil, mapErr(err, "list member alerts")
	}
	defer rows.Close()

	out := make([]MemberAlert, 0, 32)
	for rows.Next() {
		var m MemberAlert
		if err := rows.Scan(&m.AlertID, &m.CaseID); err != nil {
			return nil, mapErr(err, "scan member alert")
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read member alerts")
	}
	return out, nil
}

// CountCurrentMembers is how many LIVE members a generation currently has.
//
// ⭐ It exists so that a fan-out stopped by its ceiling can say HOW MANY members
// it did not reach, rather than only that there were more. "500 of 5 000" and
// "500, and some others" are different sentences to the person who pressed the
// button, and only the first one lets them judge whether the group is done.
//
// ⛔ IT IS NOT `Rollup().Total`, and the difference is the reason both exist.
// `Total` counts every episode the generation has ever held, because that is what
// the card's breakdown means; this counts the ones a verb can still act on.
//
// It is one indexed aggregate on case_group_live_idx, and the fan-out only asks
// when its candidate read came back full — a group of forty never pays for it.
func (r *MemberRepository) CountCurrentMembers(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
) (int, error) {
	if err := db.RequireScope(s); err != nil {
		return 0, err
	}
	var n int64
	if err := r.db(ctx).QueryRow(ctx, countCurrentMembersSQL, s.OrgID(), groupID).Scan(&n); err != nil {
		return 0, mapErr(err, "count current members")
	}
	return int(n), nil
}

const snoozeRollupSQL = `
SELECT o.group_id,
       count(DISTINCT o.alert_id) AS snoozed,
       max(z.snoozed_until)       AS until
  FROM alert_cases o
  JOIN alert_snoozes z ON z.alert_id = o.alert_id AND z.org_id = o.org_id
 WHERE o.org_id = $1
   AND o.group_id = ANY($2)
   AND o.ended_at IS NULL
   AND z.ended_at IS NULL
   AND z.snoozed_until > $3
 GROUP BY o.group_id`

// SnoozeRollup counts how many LIVE member alerts of each generation oto is quiet
// about, and when the last of them wakes (§B.8.6).
//
// ⭐ WHY IT IS COMPUTED AND NOT A COLUMN. Every other count on `alert_groups` is
// stored, recomputed by Rollup on each membership change. This one cannot be:
// snoozes expire ON THE CLOCK, sixty seconds at a time, with nothing touching the
// group. A stored `snoozed_count` would be stale for up to a minute after every
// expiry and would keep claiming members were muted after they had woken —
// precisely the "damper the user cannot see" that §B.6 forbids. So it is derived
// at read time, against `now`, and it is always right.
//
// ⭐ IT DERIVES FROM `alert_snoozes`, NOT FROM A MIRROR ON `alerts`. There used
// to be a denormalised `alerts.snoozed_until` and this read it; the mirror is
// gone (00048) because the same read on the notification path was deciding
// whether oto speaks at all, from a bare timestamp that could not name who asked.
// `z.ended_at IS NULL` and `z.snoozed_until > $3` are the two halves of "in
// force": the first says the row has not been closed, the second that its clock
// has not run out, and both are needed because the expiry job is a sweep and not
// a trigger.
//
// ⚠️ `o.ended_at` AND `z.ended_at` ARE DIFFERENT FACTS AND BOTH ARE REQUIRED.
// The first is "this episode is still live", which is the membership predicate
// since 00051; the second is "this snooze has not been lifted". A query that
// dropped either would count quiet that is not there.
//
// ⭐ IT IS BATCHED OVER A WHOLE PAGE OF GROUPS. One query per group would be the
// N+1 that kept the count off the group list entirely, leaving the fan-out button
// able to act and never able to show its result.
//
// It counts DISTINCT alerts: one alert can hold several episodes in one
// generation, and a snooze is scoped to the ALERT (§B.8.1), so counting episodes
// would report one muted alert twice.
//
// NOTE (layering): this joins `alert_snoozes`, which the alerts module owns — a
// table READ, the same kind `Rollup` above already performs. It is not a call
// into another module's repository, which §F.5 rule 4 forbids.
//
// NOTE (planner): driven by case_group_live_idx for the `group_id = ANY(...)`
// restriction under the live predicate, with `alert_snoozes` probed per member
// through alert_snoozes_active_idx — UNIQUE (alert_id) WHERE ended_at IS NULL, so
// at most one row comes back and the DISTINCT below cannot be doing work the index
// has not already done. The join is INNER rather than LEFT because a group with no
// snoozed member wants no row at all: an absent entry is what the caller reads as
// "nothing here is quiet".
func (r *MemberRepository) SnoozeRollup(
	ctx context.Context, s db.TenantScope, groupIDs []uuid.UUID, now time.Time,
) (map[uuid.UUID]domain.SnoozeRollup, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if len(groupIDs) == 0 {
		return map[uuid.UUID]domain.SnoozeRollup{}, nil
	}
	if now.IsZero() {
		now = r.clock.Now()
	}

	rows, err := r.db(ctx).Query(ctx, snoozeRollupSQL, s.OrgID(), groupIDs, now.UTC())
	if err != nil {
		return nil, mapErr(err, "roll up group snoozes")
	}
	defer rows.Close()

	out := make(map[uuid.UUID]domain.SnoozeRollup, len(groupIDs))
	for rows.Next() {
		var (
			id    uuid.UUID
			n     int64
			until *time.Time
		)
		if err := rows.Scan(&id, &n, &until); err != nil {
			return nil, mapErr(err, "scan group snooze rollup")
		}
		out[id] = domain.SnoozeRollup{Count: int(n), Until: timeOrZero(until)}
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read group snooze rollups")
	}
	return out, nil
}

func collectMembers(rows pgx.Rows) ([]domain.Member, error) {
	out := make([]domain.Member, 0, 32)
	for rows.Next() {
		var row memberRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan group member")
		}
		m, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read group members")
	}
	return out, nil
}
