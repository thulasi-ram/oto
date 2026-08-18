package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// groupRow is the row model of `alert_groups`. Unexported, per the three-model
// rule: no DTO and no domain type may embed it.
type groupRow struct {
	id         uuid.UUID
	orgID      uuid.UUID
	sourceID   uuid.UUID
	clusterID  uuid.UUID
	clusterKey string

	groupKey       string
	generation     int32
	sourceGroupKey *string
	receiver       string
	groupLabels    []byte
	title          string

	state        string
	severity     *string
	stateVersion int32

	firingCount     int32
	suppressedCount int32
	resolvedCount   int32
	expiredCount    int32
	totalCount      int32
	ackedCount      int32

	lastNotificationReason *string

	firstSeenAt    time.Time
	lastActivityAt time.Time
	closedAt       *time.Time
}

var groupColumnList = []string{
	"id", "org_id", "source_id", "cluster_id", "group_key", "generation", "source_group_key",
	"receiver", "group_labels", "title", "state", "severity", "state_version", "firing_count",
	"suppressed_count", "resolved_count", "expired_count", "total_count", "acked_count",
	"last_notification_reason", "first_seen_at", "last_activity_at",
	"closed_at",
}

var groupColumns = strings.Join(groupColumnList, ", ")

// groupColumnsQualified is groupColumns aliased to `g`, for the queries that
// join `clusters`.
var groupColumnsQualified = "g." + strings.Join(groupColumnList, ", g.")

// clusterJoin resolves `cluster_key` for a generation.
//
// ⭐ `alert_groups` stores only `cluster_id`, and the contract requires
// `cluster_key`. Reading it out of `group_labels['cluster']` — which is what the
// handler used to do — is wrong twice over: `cluster` is not one of the axes
// `group_labels` now carries at all (ADR 0038 keeps it first-class rather than
// inventing a label the upstream never sent), and the value would be whatever the
// upstream labelled with rather than the identity oto keys alerts on (§C.2).
//
// NOTE (planner): the join is a primary-key lookup on `clusters.id`, one row per
// group row. The alternative is a denormalised `alert_groups.cluster_key`
// column, which is a migration this module does not own.
const clusterJoin = " JOIN clusters c ON c.id = g.cluster_id AND c.org_id = g.org_id"

// selectGroups is the read projection every group query shares.
var selectGroups = `SELECT ` + groupColumnsQualified + `, c.cluster_key
  FROM alert_groups g` + clusterJoin

func (r *groupRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.sourceID, &r.clusterID, &r.groupKey, &r.generation, &r.sourceGroupKey,
		&r.receiver, &r.groupLabels, &r.title, &r.state, &r.severity, &r.stateVersion,
		&r.firingCount, &r.suppressedCount, &r.resolvedCount, &r.expiredCount, &r.totalCount,
		&r.ackedCount, &r.lastNotificationReason, &r.firstSeenAt,
		&r.lastActivityAt, &r.closedAt, &r.clusterKey,
	}
}

func (r *groupRow) toDomain() (domain.Group, error) {
	key, err := domain.NewGroupKey(r.groupKey)
	if err != nil {
		return domain.Group{}, errs.Internal("group_key_invalid", err)
	}
	state, err := domain.NewState(r.state)
	if err != nil {
		return domain.Group{}, errs.Internal("group_state_invalid", err)
	}
	labels, err := decodeStringMap(r.groupLabels)
	if err != nil {
		return domain.Group{}, err
	}

	g, err := domain.NewGroup(domain.GroupParams{
		ID:             r.id,
		OrgID:          r.orgID,
		SourceID:       r.sourceID,
		ClusterID:      r.clusterID,
		ClusterKey:     r.clusterKey,
		Key:            key,
		Generation:     int(r.generation),
		SourceGroupKey: strOrEmpty(r.sourceGroupKey),
		Receiver:       r.receiver,
		GroupLabels:    labels,
		Title:          r.title,
		State:          state,
		Severity:       strOrEmpty(r.severity),
		StateVersion:   int(r.stateVersion),
		Counts: domain.Counts{
			Firing:     int(r.firingCount),
			Suppressed: int(r.suppressedCount),
			Resolved:   int(r.resolvedCount),
			Expired:    int(r.expiredCount),
			Total:      int(r.totalCount),
			Acked:      int(r.ackedCount),
		},
		LastNotificationReason: strOrEmpty(r.lastNotificationReason),
		FirstSeenAt:            r.firstSeenAt,
		LastActivityAt:         r.lastActivityAt,
		ClosedAt:               timeOrZero(r.closedAt),
	})
	if err != nil {
		return domain.Group{}, errs.Internal("group_row_invalid", err)
	}
	return g, nil
}

// GroupRepository is the SQL over `alert_groups`.
//
// Every statement carries an `org_id` predicate. A missing one is a data leak.
type GroupRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewGroupRepository builds the repository over a fallback querier.
func NewGroupRepository(q db.Querier, clk clock.Clock) *GroupRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &GroupRepository{q: q, clock: clk}
}

func (r *GroupRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// GetByID reads one generation within the caller's org.
func (r *GroupRepository) GetByID(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Group, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Group{}, err
	}
	var row groupRow
	err := r.db(ctx).QueryRow(ctx,
		selectGroups+` WHERE g.org_id = $1 AND g.id = $2`,
		s.OrgID(), id).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Group{}, errs.NotFound("group_not_found", "no such alert group")
		}
		return domain.Group{}, mapErr(err, "read alert group")
	}
	return row.toDomain()
}

// GetOpenByKey reads the CURRENT generation of a group key — the ingest hot path
// (§G.4 step 4).
//
// It rides grp_open_idx, which is partial on state='open'. Only one generation of
// a given group_key is ever open, so this is effectively a unique lookup even
// though no unique constraint says so.
func (r *GroupRepository) GetOpenByKey(
	ctx context.Context, s db.TenantScope, groupKey string,
) (domain.Group, bool, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Group{}, false, err
	}
	var row groupRow
	err := r.db(ctx).QueryRow(ctx,
		selectGroups+`
		  WHERE g.org_id = $1 AND g.group_key = $2 AND g.state = 'open'
		  ORDER BY g.generation DESC
		  LIMIT 1`,
		s.OrgID(), groupKey).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Group{}, false, nil
		}
		return domain.Group{}, false, mapErr(err, "read open alert group")
	}
	g, err := row.toDomain()
	if err != nil {
		return domain.Group{}, false, err
	}
	return g, true, nil
}

// insertGroupSQL opens the next generation and resolves its cluster_key in the
// same round trip. RETURNING cannot join, so the INSERT is a CTE and the join
// happens on its output.
var insertGroupSQL = `
WITH g AS (
  INSERT INTO alert_groups (id, org_id, source_id, cluster_id, group_key, generation,
                            source_group_key, receiver, group_labels, title, state, severity,
                            state_version, first_seen_at, last_activity_at, synthetic)
  SELECT $1, $2, $3, $4, $5,
         COALESCE((SELECT max(prev.generation) FROM alert_groups prev
                    WHERE prev.org_id = $2 AND prev.group_key = $5), 0) + 1,
         $6, $7, $8, $9, 'open', $10, 1, $11, $11, $12
  RETURNING ` + groupColumns + `
)
SELECT ` + groupColumnsQualified + `, c.cluster_key
  FROM g` + clusterJoin

// OpenGeneration creates the NEXT generation of a group key.
//
// ⭐ The generation number is computed IN THE STATEMENT, from max(generation)+1
// under the (org_id, group_key, generation) unique constraint. Reading the max in
// Go and writing it back is the classic read-then-write race, and losing it here
// would mean two live generations of one group — two Slack threads for one
// conversation.
//
// A 23505 on groups_key_gen_uniq is therefore a genuine concurrency conflict: the
// caller should re-read the open generation and use it.
func (r *GroupRepository) OpenGeneration(
	ctx context.Context, s db.TenantScope, in NewGeneration,
) (domain.Group, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Group{}, err
	}
	if err := db.RequireID("group id", in.ID); err != nil {
		return domain.Group{}, err
	}
	if err := db.RequireID("source_id", in.SourceID); err != nil {
		return domain.Group{}, err
	}
	if err := db.RequireID("cluster_id", in.ClusterID); err != nil {
		return domain.Group{}, err
	}
	if in.GroupKey == "" || strings.TrimSpace(in.Title) == "" {
		return domain.Group{}, errs.Internal("group_row_incomplete",
			errsMissing("group_key and title are required"))
	}
	at := in.At
	if at.IsZero() {
		at = r.clock.Now()
	}
	labels, err := jsonbMap(in.GroupLabels)
	if err != nil {
		return domain.Group{}, err
	}

	var row groupRow
	err = r.db(ctx).QueryRow(ctx, insertGroupSQL,
		in.ID, s.OrgID(), in.SourceID, in.ClusterID, in.GroupKey,
		strPtr(in.SourceGroupKey), in.Receiver, labels, in.Title, strPtr(in.Severity), at.UTC(),
		in.Synthetic,
	).Scan(row.scanDest()...)
	if err != nil {
		return domain.Group{}, mapErr(err, "open alert group generation")
	}
	return row.toDomain()
}

// NewGeneration is the repository input for opening a generation.
type NewGeneration struct {
	ID        uuid.UUID
	SourceID  uuid.UUID
	ClusterID uuid.UUID
	GroupKey  string
	// SourceGroupKey is Alertmanager's raw groupKey, stored verbatim for
	// observability. OPAQUE — never parsed.
	SourceGroupKey string
	Receiver       string
	GroupLabels    map[string]string
	Title          string
	Severity       string
	At             time.Time
	// Synthetic marks a generation opened by a DELIVERY DRILL, so the dashboard
	// group counts can exclude it with an indexed predicate rather than reaching
	// `alerts` through every episode of every group in the window.
	//
	// ⛔ IT IS WRITE-ONLY HERE AND IS DELIBERATELY NOT IN `groupColumnList`. It is
	// a REPORTING fact about how a generation came to exist, not an invariant of
	// a Group, and putting it on the entity would invite code to branch on it —
	// which is exactly what must not happen: a drill's card is rendered, threaded
	// and delivered by the identical code path a real alert takes, or the drill
	// proves nothing.
	Synthetic bool
}

// ⭐ `state_version = $12` is the OPTIMISTIC LOCK, and $12 is the version the
// caller READ, not the one it is writing.
//
// `alert_groups` already carries the version column; it was simply never used as
// one. Every writer here is a read-recompute-write at READ COMMITTED — GetByID,
// then Rollup, then this UPDATE — and without the predicate two concurrent
// recomputes both read version N, both derive N+1, and both write it. The second
// one's counts win, the first one's are lost, and `notifications.idempotency_key`
// hashes N+1 for two DIFFERENT group states, so the second fact is dropped as a
// duplicate of the first (§C.7). That is a notification the channel never gets.
const updateRollupSQL = `
UPDATE alert_groups SET
    firing_count     = $3,
    suppressed_count = $4,
    resolved_count   = $5,
    expired_count    = $6,
    total_count      = $7,
    acked_count      = $8,
    severity         = $9,
    state_version    = $10,
    last_activity_at = GREATEST(first_seen_at, last_activity_at, $11),
    updated_at       = now()
WHERE org_id = $1 AND id = $2 AND state_version = $12`

// SetRollup writes the recomputed membership rollup and the state version, as a
// COMPARE-AND-SET against `fromVersion` — the `state_version` the caller read
// before it recomputed anything.
//
// `state_version` is supplied by the caller, which took it from the domain: the
// version is what notification.idempotency_key hashes (§C.7), so bumping it is a
// decision about whether a fact is NEW, not a side effect of an UPDATE. That is
// exactly why it also makes a sound optimistic lock — a caller that decided the
// version must be the caller that owns the write.
//
// A lost compare-and-set is errs.KindConflict, and the caller re-reads and
// recomputes: a rollup is a pure projection of the current members, so recomputing
// it is always the right answer and never loses information.
func (r *GroupRepository) SetRollup(
	ctx context.Context, s db.TenantScope, g domain.Group, fromVersion int,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	c := g.Counts()
	tag, err := r.db(ctx).Exec(ctx, updateRollupSQL, s.OrgID(), g.ID(),
		c.Firing, c.Suppressed, c.Resolved, c.Expired, c.Total, c.Acked,
		strPtr(g.Severity()), g.StateVersion(), g.LastActivityAt().UTC(), fromVersion)
	if err != nil {
		return mapErr(err, "write group rollup")
	}
	if tag.RowsAffected() == 0 {
		return r.versionMiss(ctx, s, g.ID(), fromVersion, "write group rollup")
	}
	return nil
}

// ⛔⛔ `setStormSQL` AND `SetStorm` WERE HERE AND ARE DELETED. They wrote the
// `storm_mode`/`storm_since` pair together — all-or-nothing, because
// `groups_storm_ck` says so and half of a visible state renders as neither — under the
// same `state_version` compare-and-set `SetRollup` uses, so a storm verdict derived
// from a generation that had since moved could not announce a damping transition for
// counts nobody had.
//
// ⭐ AND THE COLUMNS THEY WROTE ARE GONE TOO (migration 00059). `storm_mode`,
// `storm_since` and `groups_storm_ck` have left `alert_groups`, so this table no
// longer has two read-only columns that no writer could ever set again — it has
// neither. `groupColumnList` no longer names them, `Close` no longer clears them,
// and `?storm=` no longer filters on them.

const closeGroupSQL = `
UPDATE alert_groups SET
    state            = 'closed',
    closed_at        = GREATEST(first_seen_at, $3),
    last_activity_at = GREATEST(first_seen_at, last_activity_at, $3),
    state_version    = $4,
    updated_at       = now()
WHERE org_id = $1 AND id = $2 AND state = 'open' AND state_version = $5`

// Close ends a generation and freezes its thread.
//
// The `state = 'open'` predicate already made this a compare-and-set on the
// state; `state_version = $5` completes it. Closing is decided from a rollup that
// proved no member is still live, and that proof is only about the generation as
// it was read — a member that joined in the meantime bumps the version, and
// freezing its thread would be freezing a live incident's conversation.
func (r *GroupRepository) Close(ctx context.Context, s db.TenantScope, g domain.Group, fromVersion int) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, closeGroupSQL, s.OrgID(), g.ID(),
		g.ClosedAt().UTC(), g.StateVersion(), fromVersion)
	if err != nil {
		return mapErr(err, "close alert group")
	}
	if tag.RowsAffected() == 0 {
		return errs.Precondition("group_already_closed",
			"this group generation does not exist, is already closed, or changed while it was being closed")
	}
	return nil
}

// versionMiss tells a vanished generation apart from a lost optimistic lock, so
// the caller can recompute in the one case and give up in the other.
func (r *GroupRepository) versionMiss(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, fromVersion int, what string,
) error {
	var live int32
	err := r.db(ctx).QueryRow(ctx,
		`SELECT state_version FROM alert_groups WHERE org_id = $1 AND id = $2`,
		s.OrgID(), groupID).Scan(&live)
	if err != nil {
		if isNoRows(err) {
			return errs.NotFound("group_not_found", "no such alert group")
		}
		return mapErr(err, what)
	}
	return errs.Conflict("group_state_version_stale",
		"this generation moved to state_version "+strconv.Itoa(int(live))+
			" while a change against "+strconv.Itoa(fromVersion)+" was being computed")
}

// Touch records activity so an idle generation's close clock restarts.
func (r *GroupRepository) Touch(ctx context.Context, s db.TenantScope, groupID uuid.UUID, at time.Time) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if at.IsZero() {
		at = r.clock.Now()
	}
	_, err := r.db(ctx).Exec(ctx,
		`UPDATE alert_groups
		    SET last_activity_at = GREATEST(first_seen_at, last_activity_at, $3), updated_at = now()
		  WHERE org_id = $1 AND id = $2`,
		s.OrgID(), groupID, at.UTC())
	return mapErr(err, "touch alert group")
}

// SetNotificationReason records the most recent Alertmanager notification_reason
// seen for this group, which feeds the §H.6 reason-to-mode decision table.
func (r *GroupRepository) SetNotificationReason(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID, reason string,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	_, err := r.db(ctx).Exec(ctx,
		`UPDATE alert_groups SET last_notification_reason = $3, updated_at = now()
		  WHERE org_id = $1 AND id = $2`,
		s.OrgID(), groupID, strPtr(reason))
	return mapErr(err, "record notification reason")
}

// StateVersion reads the current `state_version` of one generation, for callers
// minting a §C.7 idempotency key.
func (r *GroupRepository) StateVersion(
	ctx context.Context, s db.TenantScope, groupID uuid.UUID,
) (int, error) {
	if err := db.RequireScope(s); err != nil {
		return 0, err
	}
	var v int32
	err := r.db(ctx).QueryRow(ctx,
		`SELECT state_version FROM alert_groups WHERE org_id = $1 AND id = $2`,
		s.OrgID(), groupID).Scan(&v)
	if err != nil {
		if isNoRows(err) {
			return 0, errs.NotFound("group_not_found", "no such alert group")
		}
		return 0, mapErr(err, "read group state version")
	}
	return int(v), nil
}

// List is the groups list — the default UI landing view.
//
// ⭐ EVERY FILTER IS APPLIED IN SQL, BEFORE THE LIMIT. That is the whole point
// of the signature: this method used to accept `states` alone, and the handler
// post-filtered the returned page in memory, which is not filtering but
// truncation — 50 rows fetched, 43 discarded, 7 returned, and a `has_more`
// cursor pointing past everything the filter never got to look at. A predicate
// evaluated after pagination is silently wrong on every page but the last.
//
// Keyset: `(last_activity_at DESC, id DESC)` on grp_list_idx, or
// `(first_seen_at DESC, id DESC)`.
//
// NOTE (planner): `-last_activity_at` rides grp_list_idx
// (org_id, state, last_activity_at DESC, id DESC). `-first_seen_at` has NO
// covering index and is a filter-then-sort over the org's generations; it is the
// rarer of the two and `alert_groups` is a small table beside `alerts`. Adding
// (org_id, first_seen_at DESC, id DESC) is a migration this module does not own.
func (r *GroupRepository) List(
	ctx context.Context, s db.TenantScope, f domain.GroupFilter, sort string, p db.Keyset,
) ([]domain.Group, db.Cursor, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}

	sortCol := "g.last_activity_at"
	switch sort {
	case "", domain.SortLastActivityDesc:
	case domain.SortFirstSeenDesc:
		sortCol = "g.first_seen_at"
	default:
		return nil, db.Cursor{}, errs.Validation("sort_invalid",
			"sort must be one of: -last_activity_at, -first_seen_at")
	}
	limit := db.ClampLimit(p.Limit)

	args := []any{s.OrgID()}
	where := " WHERE g.org_id = $1"
	add := func(clause string, values ...any) {
		args = append(args, values...)
		where += clause
	}

	if len(f.States) > 0 {
		add(fmt.Sprintf(" AND g.state = ANY($%d)", len(args)+1), f.States)
	}
	if len(f.Severities) > 0 {
		add(fmt.Sprintf(" AND g.severity = ANY($%d)", len(args)+1), f.Severities)
	}
	if len(f.ClusterKeys) > 0 {
		add(fmt.Sprintf(" AND c.cluster_key = ANY($%d)", len(args)+1), f.ClusterKeys)
	}
	if f.SourceID != nil {
		add(fmt.Sprintf(" AND g.source_id = $%d", len(args)+1), *f.SourceID)
	}
	if f.Receiver != "" {
		add(fmt.Sprintf(" AND g.receiver = $%d", len(args)+1), f.Receiver)
	}
	if f.FullyAcked != nil {
		// "Fully acked" needs a member to be acked ABOUT: a generation with no
		// members is not acknowledged, it is empty, and reporting it as acked
		// would be a receipt nobody signed.
		if *f.FullyAcked {
			where += " AND g.total_count > 0 AND g.acked_count >= g.total_count"
		} else {
			where += " AND (g.total_count = 0 OR g.acked_count < g.total_count)"
		}
	}
	if f.Since != nil {
		add(fmt.Sprintf(" AND g.last_activity_at >= $%d", len(args)+1), f.Since.UTC())
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		// NOTE (planner): no index serves this, and there is deliberately no
		// pretence that one does. `alert_groups` holds one row per generation
		// per receiver — thousands, not millions — so a bounded scan of one
		// org's generations is the honest cost of a free-text box here, unlike
		// on `alerts` where alerts_text_idx exists precisely because it is not.
		add(fmt.Sprintf(
			" AND (g.title ILIKE '%%' || $%d || '%%' OR g.group_labels::text ILIKE '%%' || $%d || '%%')",
			len(args)+1, len(args)+1), q)
	}
	if !p.Cursor.IsZero() {
		args = append(args, p.Cursor.SortKey.UTC(), p.Cursor.ID)
		where += fmt.Sprintf(" AND (%s, g.id) < ($%d, $%d)", sortCol, len(args)-1, len(args))
	}
	args = append(args, limit+1)

	sql := selectGroups + where +
		fmt.Sprintf(" ORDER BY %s DESC, g.id DESC LIMIT $%d", sortCol, len(args))

	rows, err := r.db(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list alert groups")
	}
	defer rows.Close()

	collected, err := collectGroups(rows, limit+1)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	page, hasMore := db.PageOf(collected, limit)
	if len(page) == 0 {
		return nil, db.Cursor{Hash: p.Cursor.Hash}, nil
	}
	last := page[len(page)-1]
	sortKey := last.LastActivityAt()
	if sortCol == "g.first_seen_at" {
		sortKey = last.FirstSeenAt()
	}
	return page, db.NextCursor(sortKey, last.ID(), p.Cursor.Hash, hasMore), nil
}

var closeCandidatesSQL = selectGroups + `
 WHERE g.org_id = $1 AND g.state = 'open' AND g.last_activity_at < $2
 ORDER BY g.last_activity_at ASC
 LIMIT $3`

// CloseCandidates feeds the `group.close` sweep: open generations idle past
// group_close_delay_s (grp_close_idx).
//
// The caller still checks each candidate for LIVE members before closing it: a
// generation with a firing member is idle in the sense that nothing has been
// written recently, but closing it would freeze the thread of a live incident.
func (r *GroupRepository) CloseCandidates(
	ctx context.Context, s db.TenantScope, idleBefore time.Time, limit int,
) ([]domain.Group, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if idleBefore.IsZero() {
		return nil, errs.Internal("close_bound_missing", errsMissing("idleBefore is required"))
	}
	n := db.ClampLimit(limit)

	rows, err := r.db(ctx).Query(ctx, closeCandidatesSQL, s.OrgID(), idleBefore.UTC(), n)
	if err != nil {
		return nil, mapErr(err, "list close candidates")
	}
	defer rows.Close()
	return collectGroups(rows, n)
}

func collectGroups(rows pgx.Rows, capacity int) ([]domain.Group, error) {
	out := make([]domain.Group, 0, capacity)
	for rows.Next() {
		var row groupRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "scan alert group")
		}
		g, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "read alert groups")
	}
	return out, nil
}

func errsMissing(what string) error {
	return errs.New(errs.KindInternal, "row_model_invalid", "repository: "+what)
}
