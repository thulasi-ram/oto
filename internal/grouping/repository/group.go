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

// groupRow is the row model of `alert_groups`. Unexported, per the three-model
// rule: no DTO and no domain type may embed it.
type groupRow struct {
	id        uuid.UUID
	orgID     uuid.UUID
	sourceID  uuid.UUID
	clusterID uuid.UUID

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

	stormMode              bool
	stormSince             *time.Time
	lastNotificationReason *string

	firstSeenAt    time.Time
	lastActivityAt time.Time
	closedAt       *time.Time
}

var groupColumnList = []string{
	"id", "org_id", "source_id", "cluster_id", "group_key", "generation", "source_group_key",
	"receiver", "group_labels", "title", "state", "severity", "state_version", "firing_count",
	"suppressed_count", "resolved_count", "expired_count", "total_count", "acked_count",
	"storm_mode", "storm_since", "last_notification_reason", "first_seen_at", "last_activity_at",
	"closed_at",
}

var groupColumns = strings.Join(groupColumnList, ", ")

func (r *groupRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.sourceID, &r.clusterID, &r.groupKey, &r.generation, &r.sourceGroupKey,
		&r.receiver, &r.groupLabels, &r.title, &r.state, &r.severity, &r.stateVersion,
		&r.firingCount, &r.suppressedCount, &r.resolvedCount, &r.expiredCount, &r.totalCount,
		&r.ackedCount, &r.stormMode, &r.stormSince, &r.lastNotificationReason, &r.firstSeenAt,
		&r.lastActivityAt, &r.closedAt,
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
		StormMode:              r.stormMode,
		StormSince:             timeOrZero(r.stormSince),
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
	if err := requireScope(s); err != nil {
		return domain.Group{}, err
	}
	var row groupRow
	err := r.db(ctx).QueryRow(ctx,
		`SELECT `+groupColumns+` FROM alert_groups WHERE org_id = $1 AND id = $2`,
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
	if err := requireScope(s); err != nil {
		return domain.Group{}, false, err
	}
	var row groupRow
	err := r.db(ctx).QueryRow(ctx,
		`SELECT `+groupColumns+`
		   FROM alert_groups
		  WHERE org_id = $1 AND group_key = $2 AND state = 'open'
		  ORDER BY generation DESC
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

var insertGroupSQL = `
INSERT INTO alert_groups (id, org_id, source_id, cluster_id, group_key, generation,
                          source_group_key, receiver, group_labels, title, state, severity,
                          state_version, first_seen_at, last_activity_at)
SELECT $1, $2, $3, $4, $5,
       COALESCE((SELECT max(g.generation) FROM alert_groups g
                  WHERE g.org_id = $2 AND g.group_key = $5), 0) + 1,
       $6, $7, $8, $9, 'open', $10, 1, $11, $11
RETURNING ` + groupColumns

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
	if err := requireScope(s); err != nil {
		return domain.Group{}, err
	}
	if err := requireID("group id", in.ID); err != nil {
		return domain.Group{}, err
	}
	if err := requireID("source_id", in.SourceID); err != nil {
		return domain.Group{}, err
	}
	if err := requireID("cluster_id", in.ClusterID); err != nil {
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
}

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
WHERE org_id = $1 AND id = $2`

// SetRollup writes the recomputed membership rollup and the state version.
//
// `state_version` is supplied by the caller, which took it from the domain: the
// version is what notification.idempotency_key hashes (§C.7), so bumping it is a
// decision about whether a fact is NEW, not a side effect of an UPDATE.
func (r *GroupRepository) SetRollup(
	ctx context.Context, s db.TenantScope, g domain.Group,
) error {
	if err := requireScope(s); err != nil {
		return err
	}
	c := g.Counts()
	tag, err := r.db(ctx).Exec(ctx, updateRollupSQL, s.OrgID(), g.ID(),
		c.Firing, c.Suppressed, c.Resolved, c.Expired, c.Total, c.Acked,
		strPtr(g.Severity()), g.StateVersion(), g.LastActivityAt().UTC())
	if err != nil {
		return mapErr(err, "write group rollup")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("group_not_found", "no such alert group")
	}
	return nil
}

const setStormSQL = `
UPDATE alert_groups SET
    storm_mode    = $3,
    storm_since   = $4,
    state_version = $5,
    updated_at    = now()
WHERE org_id = $1 AND id = $2`

// SetStorm writes storm mode. The pair is all-or-nothing (groups_storm_ck) and
// is written together, because storm collapse is a VISIBLE state and half of one
// renders as neither.
func (r *GroupRepository) SetStorm(ctx context.Context, s db.TenantScope, g domain.Group) error {
	if err := requireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, setStormSQL, s.OrgID(), g.ID(),
		g.StormMode(), nilTime(g.StormSince()), g.StateVersion())
	if err != nil {
		return mapErr(err, "write storm mode")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("group_not_found", "no such alert group")
	}
	return nil
}

const closeGroupSQL = `
UPDATE alert_groups SET
    state            = 'closed',
    closed_at        = GREATEST(first_seen_at, $3),
    last_activity_at = GREATEST(first_seen_at, last_activity_at, $3),
    state_version    = $4,
    storm_mode       = false,
    storm_since      = NULL,
    updated_at       = now()
WHERE org_id = $1 AND id = $2 AND state = 'open'`

// Close ends a generation and freezes its thread.
//
// The `state = 'open'` predicate makes it a compare-and-set: closing an
// already-closed generation affects zero rows and is reported as a precondition
// failure rather than silently rewriting closed_at.
func (r *GroupRepository) Close(ctx context.Context, s db.TenantScope, g domain.Group) error {
	if err := requireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, closeGroupSQL, s.OrgID(), g.ID(),
		g.ClosedAt().UTC(), g.StateVersion())
	if err != nil {
		return mapErr(err, "close alert group")
	}
	if tag.RowsAffected() == 0 {
		return errs.Precondition("group_already_closed",
			"this group generation does not exist or is already closed")
	}
	return nil
}

// Touch records activity so an idle generation's close clock restarts.
func (r *GroupRepository) Touch(ctx context.Context, s db.TenantScope, groupID uuid.UUID, at time.Time) error {
	if err := requireScope(s); err != nil {
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
	if err := requireScope(s); err != nil {
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
	if err := requireScope(s); err != nil {
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

// List is the groups list, keyset-paginated by (last_activity_at DESC, id DESC)
// — the default UI landing view (grp_list_idx).
func (r *GroupRepository) List(
	ctx context.Context, s db.TenantScope, states []string, p db.Keyset,
) ([]domain.Group, db.Cursor, error) {
	if err := requireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := clampLimit(p.Limit)

	sql := `SELECT ` + groupColumns + `
	          FROM alert_groups
	         WHERE org_id = $1
	           AND ($2::text[] IS NULL OR state = ANY($2))
	           AND ($3::timestamptz IS NULL OR (last_activity_at, id) < ($3, $4))
	         ORDER BY last_activity_at DESC, id DESC
	         LIMIT $5`

	var stateArg any
	if len(states) > 0 {
		stateArg = states
	}
	var cursorAt *time.Time
	var cursorID uuid.UUID
	if !p.Cursor.IsZero() {
		cursorAt = nilTime(p.Cursor.SortKey)
		cursorID = p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, sql, s.OrgID(), stateArg, cursorAt, cursorID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "list alert groups")
	}
	defer rows.Close()

	collected, err := collectGroups(rows, limit+1)
	if err != nil {
		return nil, db.Cursor{}, err
	}
	page, hasMore := pageOf(collected, limit)
	if len(page) == 0 {
		return nil, db.Cursor{Hash: p.Cursor.Hash}, nil
	}
	last := page[len(page)-1]
	return page, nextCursor(last.LastActivityAt(), last.ID(), p.Cursor.Hash, hasMore), nil
}

var closeCandidatesSQL = `
SELECT ` + groupColumns + `
  FROM alert_groups
 WHERE org_id = $1 AND state = 'open' AND last_activity_at < $2
 ORDER BY last_activity_at ASC
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
	if err := requireScope(s); err != nil {
		return nil, err
	}
	if idleBefore.IsZero() {
		return nil, errs.Internal("close_bound_missing", errsMissing("idleBefore is required"))
	}
	n := clampLimit(limit)

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
