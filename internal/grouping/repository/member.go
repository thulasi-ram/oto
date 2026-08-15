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

// memberRow is the row model of `alert_group_members`. Unexported, per the
// three-model rule.
type memberRow struct {
	groupID      uuid.UUID
	occurrenceID uuid.UUID
	orgID        uuid.UUID
	alertID      uuid.UUID
	joinedAt     time.Time
	leftAt       *time.Time
}

var memberColumnList = []string{
	"group_id", "occurrence_id", "org_id", "alert_id", "joined_at", "left_at",
}

var memberColumns = strings.Join(memberColumnList, ", ")

func (r *memberRow) scanDest() []any {
	return []any{&r.groupID, &r.occurrenceID, &r.orgID, &r.alertID, &r.joinedAt, &r.leftAt}
}

func (r *memberRow) toDomain() (domain.Member, error) {
	m, err := domain.NewMember(domain.MemberParams{
		GroupID:      r.groupID,
		OccurrenceID: r.occurrenceID,
		OrgID:        r.orgID,
		AlertID:      r.alertID,
		JoinedAt:     r.joinedAt,
		LeftAt:       timeOrZero(r.leftAt),
	})
	if err != nil {
		return domain.Member{}, errs.Internal("member_row_invalid", err)
	}
	return m, nil
}

// MemberRepository is the SQL over `alert_group_members`.
//
// Membership is HISTORY, not a boolean: nothing in this file deletes a row. A
// member that leaves gets a `left_at`, so the group card can be replayed at any
// past instant.
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

// The group_id is re-proven to belong to the caller's org rather than trusted.
// alert_group_members.org_id is denormalised, so writing a scope's org_id beside
// another org's group would create a row every org-scoped read agrees is ours.
const joinMemberSQL = `
INSERT INTO alert_group_members (group_id, occurrence_id, org_id, alert_id, joined_at)
SELECT g.id, $2, g.org_id, $4, $5
  FROM alert_groups g
 WHERE g.id = $1 AND g.org_id = $3
ON CONFLICT (group_id, occurrence_id) DO NOTHING`

// Join adds an occurrence to a generation, idempotently.
//
// ON CONFLICT DO NOTHING because a redelivered ingest batch replays the same
// membership: joining twice is the at-least-once queue working, not an error. The
// bool reports whether this call was the one that joined, which is what decides
// whether a `group.member_joined` event is appended.
func (r *MemberRepository) Join(
	ctx context.Context, s db.TenantScope, groupID, occurrenceID, alertID uuid.UUID, at time.Time,
) (bool, error) {
	if err := db.RequireScope(s); err != nil {
		return false, err
	}
	if err := db.RequireID("group_id", groupID); err != nil {
		return false, err
	}
	if err := db.RequireID("occurrence_id", occurrenceID); err != nil {
		return false, err
	}
	if err := db.RequireID("alert_id", alertID); err != nil {
		return false, err
	}
	if at.IsZero() {
		at = r.clock.Now()
	}

	tag, err := r.db(ctx).Exec(ctx, joinMemberSQL, groupID, occurrenceID, s.OrgID(), alertID, at.UTC())
	if err != nil {
		return false, mapErr(err, "join alert group")
	}
	return tag.RowsAffected() == 1, nil
}

const leaveMemberSQL = `
UPDATE alert_group_members
   SET left_at = GREATEST(joined_at, $4)
 WHERE org_id = $1 AND group_id = $2 AND occurrence_id = $3 AND left_at IS NULL`

// Leave records that an occurrence stopped being a member. The row survives — it
// is history — and the `left_at IS NULL` predicate makes a second call a no-op.
func (r *MemberRepository) Leave(
	ctx context.Context, s db.TenantScope, groupID, occurrenceID uuid.UUID, at time.Time,
) (bool, error) {
	if err := db.RequireScope(s); err != nil {
		return false, err
	}
	if at.IsZero() {
		at = r.clock.Now()
	}
	tag, err := r.db(ctx).Exec(ctx, leaveMemberSQL, s.OrgID(), groupID, occurrenceID, at.UTC())
	if err != nil {
		return false, mapErr(err, "leave alert group")
	}
	return tag.RowsAffected() == 1, nil
}

var allMembersSQL = `
SELECT ` + memberColumns + `
  FROM alert_group_members
 WHERE org_id = $1 AND group_id = $2
 ORDER BY joined_at ASC`

// AllMembers lists every occurrence that has ever been in a generation, present
// and past, so the group card can be replayed.
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
// `joined_at <= $3` is `!t.Before(joinedAt)` and `left_at IS NULL OR left_at >
// $3` is `leftAt.IsZero() || t.Before(leftAt)` — the same two clauses, in the
// place that can discard the rows instead of shipping them. The service used to
// read `AllMembers` (every membership the generation has ever had, joined and
// departed) and drop most of them in Go, which is a replay of a forty-member
// group paid for at the size of its whole history.
var membersAtSQL = `
SELECT ` + memberColumns + `
  FROM alert_group_members
 WHERE org_id = $1 AND group_id = $2
   AND joined_at <= $3
   AND (left_at IS NULL OR left_at > $3)
 ORDER BY joined_at ASC`

// MembersAt lists the occurrences that were in a generation at one instant.
//
// NOTE (planner): NO INDEX SERVES THIS ONE, and that is a decision rather than
// an omission. gm_current_idx (00044) is PARTIAL on `left_at IS NULL` and a
// replay is precisely the read that wants the departed rows back, so this falls
// to the `(group_id, occurrence_id)` primary key's group prefix or to a
// sequential scan by size, with both time clauses as filters and a Sort for the
// ORDER BY. That is affordable only because the read has NO PAGE: it answers
// "what was in the group when the thread was posted", the caller wants the whole
// set, and a sort of the answer is not a sort of the membership. What SQL buys
// here is the departed rows never crossing the wire. The day this read acquires
// a bound it needs `(org_id, group_id, joined_at)` unpartialled, and not before
// — a second btree on the ingest path's own table, to serve a method with one
// caller, would be paying for a page nobody has asked for.
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

var groupsForAlertSQL = `
SELECT ` + memberColumns + `
  FROM alert_group_members
 WHERE org_id = $1 AND alert_id = $2
 ORDER BY joined_at DESC
 LIMIT $3`

// GroupsForAlert answers "which groups has this alert been part of", newest
// first (gm_alert_idx).
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
SELECT count(DISTINCT alert_id), max(joined_at)
  FROM alert_group_members
 WHERE org_id = $1 AND group_id = $2 AND joined_at >= $3`

// DistinctJoinsSince counts how many DISTINCT alerts joined a generation inside
// the storm window, and when the most recent join was.
//
// ⭐ It counts DISTINCT ALERTS, not memberships. One flapping alert re-firing
// forty times is a FLAP, damped per-alert by flap_score; forty different alerts
// arriving at once is a STORM, damped per-group by storm collapse. Counting
// memberships would collapse a group because one alert was noisy, hiding the
// thirty-nine quiet ones behind it.
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
  FROM alert_group_members m
  JOIN alert_occurrences o ON o.id = m.occurrence_id
  JOIN alerts a            ON a.id = m.alert_id
 WHERE m.org_id = $1 AND m.group_id = $2 AND m.left_at IS NULL`

// Rollup recomputes a generation's membership counts from its CURRENT members.
//
// It is a recomputation rather than an increment on purpose: an increment that
// misses one transition is wrong forever, while a recomputation is wrong only
// until the next one. The group's counts are a PROJECTION of its members'
// occurrence states, and this is the query that derives it.
//
// NOTE (layering): this joins `alert_occurrences` and `alerts`, which the alerts
// module owns. It is a table READ, not a call into another module's repository —
// §F.5 rule 4 forbids the latter — and it exists because the alternative is
// shipping every member's state across a service boundary on every observation.
//
// NOTE (planner): the driving index is gm_occ_idx / the (group_id, occurrence_id)
// primary key on alert_group_members; the two joins are primary-key lookups. The
// severity aggregate is `max()` over the RAW label, which orders lexically and is
// therefore a placeholder for a real precedence: the caller re-derives the
// display severity from the member set when it needs the §H.2 ordering.
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

// listCurrentMembersSQL is the keyset page of a generation's current members,
// and the ONLY read of them: there is no unbounded sibling to reach for.
//
// ⭐ It orders NEWEST JOIN FIRST and pages by `(joined_at, occurrence_id)`,
// which is a total order because `(group_id, occurrence_id)` is the table's
// primary key. Both callers used to read EVERY member through `Get()` and slice
// the result in Go: correct for a group of forty, a full membership fetch for a
// storm of five thousand, and a page whose `has_more` was computed from a list
// the caller had already paid to materialise. `/alert-groups/{id}/alerts` came
// here first; `/alert-groups/{id}` followed, and its twenty-row preview is now
// this query with `LIMIT 21` rather than a `sort.SliceStable` over the storm.
var listCurrentMembersSQL = `
SELECT ` + memberColumns + `
  FROM alert_group_members
 WHERE org_id = $1 AND group_id = $2 AND left_at IS NULL
   AND ($3::timestamptz IS NULL OR (joined_at, occurrence_id) < ($3, $4))
 ORDER BY joined_at DESC, occurrence_id DESC
 LIMIT $5`

// ListCurrentMembers returns one keyset page of a generation's current members.
//
// NOTE (planner): the driving index is gm_current_idx (00044), `(org_id,
// group_id, joined_at DESC, occurrence_id DESC) WHERE left_at IS NULL`. It
// carries the two equalities, the partial predicate and the WHOLE sort key, so
// the LIMIT stops the scan and nothing is sorted in memory — asserted against a
// real plan in member_plan_test.go, with the index dropped as the control. The
// keyset arrives inside `$3 IS NULL OR …`, which a CUSTOM plan folds away so the
// row comparison becomes an index bound and a deep page starts at the cursor; a
// GENERIC plan cannot fold it and re-walks the rows it has passed, which a
// generation of a few thousand members can afford. Before 00044 there was no
// index carrying `joined_at` under `group_id` at all, so every read of this
// statement — including the twenty-row preview on the detail page — sorted the
// generation's whole current membership to return its first page.
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
	return page, db.NextCursor(last.JoinedAt(), last.OccurrenceID(), p.Cursor.Hash, hasMore), nil
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
// It orders OLDEST JOIN FIRST, unlike `listCurrentMembersSQL`, and that is a
// different question rather than an inconsistency: a preview shows what arrived
// most recently, a fan-out applies to the members that have been waiting
// longest. The order is also what makes a truncated fan-out reproducible —
// pressing the button twice reaches the same 500 members, not a reshuffle.
//
// NOTE (planner): gm_current_idx is `(org_id, group_id, joined_at DESC,
// occurrence_id DESC) WHERE left_at IS NULL`. It carries the two equalities and
// the partial predicate, and Postgres reads a DESC index backwards for an ASC
// order, so the LIMIT stops the scan here too.
const memberAlertsSQL = `
SELECT m.alert_id, m.occurrence_id
  FROM alert_group_members m
 WHERE m.org_id = $1 AND m.group_id = $2 AND m.left_at IS NULL
 ORDER BY m.joined_at ASC
 LIMIT $3`

const countCurrentMembersSQL = `
SELECT count(*)
  FROM alert_group_members
 WHERE org_id = $1 AND group_id = $2 AND left_at IS NULL`

// MemberAlert pairs a currently-joined alert with the episode that joined.
type MemberAlert struct {
	AlertID      uuid.UUID
	OccurrenceID uuid.UUID
}

// CurrentMemberAlerts lists at most `limit` CURRENTLY-JOINED members of a
// generation, oldest join first.
//
// It is what the §B.8.3 group snooze fans out over: one snooze per currently
// joined member alert. Alerts that join later are NOT snoozed — a snooze is never
// predictive.
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
		if err := rows.Scan(&m.AlertID, &m.OccurrenceID); err != nil {
			return nil, mapErr(err, "scan member alert")
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read member alerts")
	}
	return out, nil
}

// CountCurrentMembers is how many members a generation currently has.
//
// ⭐ It exists so that a fan-out stopped by its ceiling can say HOW MANY members
// it did not reach, rather than only that there were more. "500 of 5 000" and
// "500, and some others" are different sentences to the person who pressed the
// button, and only the first one lets them judge whether the group is done.
//
// It is one indexed aggregate on gm_current_idx, and the fan-out only asks when
// its candidate read came back full — a group of forty never pays for it.
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
SELECT m.group_id,
       count(DISTINCT m.alert_id) AS snoozed,
       max(a.snoozed_until)       AS until
  FROM alert_group_members m
  JOIN alerts a ON a.id = m.alert_id AND a.org_id = m.org_id
 WHERE m.org_id = $1
   AND m.group_id = ANY($2)
   AND m.left_at IS NULL
   AND a.snoozed_until > $3
 GROUP BY m.group_id`

// SnoozeRollup counts how many CURRENTLY-JOINED member alerts of each generation
// oto is quiet about, and when the last of them wakes (§B.8.6).
//
// ⭐ WHY IT IS COMPUTED AND NOT A COLUMN. Every other count on `alert_groups` is
// stored, recomputed by Rollup on each membership change. This one cannot be:
// snoozes expire ON THE CLOCK, sixty seconds at a time, with nothing touching the
// group. A stored `snoozed_count` would be stale for up to a minute after every
// expiry and would keep claiming members were muted after they had woken —
// precisely the "damper the user cannot see" that §B.6 forbids. So it is derived
// from `alerts.snoozed_until` at read time, against `now`, and it is always right.
//
// ⭐ IT IS BATCHED OVER A WHOLE PAGE OF GROUPS. One query per group would be the
// N+1 that kept the count off the group list entirely, leaving the fan-out button
// able to act and never able to show its result.
//
// It counts DISTINCT alerts: one alert can hold several episodes in one
// generation, and a snooze is scoped to the ALERT (§B.8.1), so counting
// memberships would report one muted alert twice.
//
// NOTE (layering): this joins `alerts`, which the alerts module owns — the same
// table READ that `Rollup` above already performs, and for the same reason. It is
// not a call into another module's repository, which §F.5 rule 4 forbids.
//
// NOTE (planner): driven by gm_alert_idx / the (group_id, occurrence_id) primary
// key on `alert_group_members` for the `group_id = ANY(...)` restriction, with
// `alerts` reached by primary key per member. `a.snoozed_until > $3` is a filter
// on those looked-up rows; alerts_snooze_idx is not the driving index here and
// does not need to be, because the member set is already bounded by the page.
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
