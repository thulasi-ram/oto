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
	if err := requireScope(s); err != nil {
		return false, err
	}
	if err := requireID("group_id", groupID); err != nil {
		return false, err
	}
	if err := requireID("occurrence_id", occurrenceID); err != nil {
		return false, err
	}
	if err := requireID("alert_id", alertID); err != nil {
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
	if err := requireScope(s); err != nil {
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

var currentMembersSQL = `
SELECT ` + memberColumns + `
  FROM alert_group_members
 WHERE org_id = $1 AND group_id = $2 AND left_at IS NULL
 ORDER BY joined_at ASC`

// CurrentMembers lists the occurrences still in a generation.
func (r *MemberRepository) CurrentMembers(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
) ([]domain.Member, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, currentMembersSQL, s.OrgID(), groupID)
	if err != nil {
		return nil, mapErr(err, "list group members")
	}
	defer rows.Close()
	return collectMembers(rows)
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
	if err := requireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, allMembersSQL, s.OrgID(), groupID)
	if err != nil {
		return nil, mapErr(err, "list group members")
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
	if err := requireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, groupsForAlertSQL, s.OrgID(), alertID, clampLimit(limit))
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
	if err := requireScope(s); err != nil {
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
	if err := requireScope(s); err != nil {
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

// listCurrentMembersSQL is the keyset page of a generation's current members.
//
// ⭐ It orders NEWEST JOIN FIRST and pages by `(joined_at, occurrence_id)`,
// which is a total order because `(group_id, occurrence_id)` is the table's
// primary key. The handler used to read EVERY member through `Get()` and slice
// the result in Go: correct for a group of forty, a full membership fetch for a
// storm of five thousand, and a page whose `has_more` was computed from a list
// the caller had already paid to materialise.
var listCurrentMembersSQL = `
SELECT ` + memberColumns + `
  FROM alert_group_members
 WHERE org_id = $1 AND group_id = $2 AND left_at IS NULL
   AND ($3::timestamptz IS NULL OR (joined_at, occurrence_id) < ($3, $4))
 ORDER BY joined_at DESC, occurrence_id DESC
 LIMIT $5`

// ListCurrentMembers returns one keyset page of a generation's current members.
//
// NOTE (planner): the driving path is the `(group_id, occurrence_id)` primary
// key with `left_at IS NULL` and the keyset applied as a filter. A generation
// holds at most a few thousand members, so this is a bounded index range and not
// the org-wide scan the alert list is careful about.
func (r *MemberRepository) ListCurrentMembers(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, p db.Keyset,
) ([]domain.Member, db.Cursor, error) {
	if err := requireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := clampLimit(p.Limit)

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
	page, hasMore := pageOf(collected, limit)
	if len(page) == 0 {
		return nil, db.Cursor{Hash: p.Cursor.Hash}, nil
	}
	last := page[len(page)-1]
	return page, nextCursor(last.JoinedAt(), last.OccurrenceID(), p.Cursor.Hash, hasMore), nil
}

const memberAlertsSQL = `
SELECT m.alert_id, m.occurrence_id
  FROM alert_group_members m
 WHERE m.org_id = $1 AND m.group_id = $2 AND m.left_at IS NULL
 ORDER BY m.joined_at ASC`

// MemberAlert pairs a currently-joined alert with the episode that joined.
type MemberAlert struct {
	AlertID      uuid.UUID
	OccurrenceID uuid.UUID
}

// CurrentMemberAlerts lists the CURRENTLY-JOINED members of a generation.
//
// It is what the §B.8.3 group snooze fans out over: one snooze per currently
// joined member alert. Alerts that join later are NOT snoozed — a snooze is never
// predictive.
func (r *MemberRepository) CurrentMemberAlerts(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
) ([]MemberAlert, error) {
	if err := requireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, memberAlertsSQL, s.OrgID(), groupID)
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
