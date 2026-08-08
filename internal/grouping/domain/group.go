package domain

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// An AlertGroup is ONE GENERATION of ONE Alertmanager notification group, derived
// from (source, receiver, groupLabels). It OWNS EXACTLY ONE Slack thread.
//
// ⛔ "Group" here NEVER means a UI grouping. A UI grouping is a VIEW (§A.1), it
// has no row, no thread and no generation.
//
// ⭐ Why the unique key is (org_id, group_key, GENERATION) and not (org_id,
// group_key): when a closed group re-opens it gets a NEW generation and therefore
// a NEW Slack thread. Reusing the old thread would append tomorrow's incident to
// a conversation people stopped reading yesterday.

// MaxTitleBytes mirrors groups_title_ck.
const MaxTitleBytes = 500

// State is what an AlertGroup generation is doing. It is a PROJECTION of its
// members' occurrence states: `open` while at least one member is firing or
// suppressed, `closed` once none is and group_close_delay has elapsed (§B.2).
type State struct{ s string }

// The closed State set (groups_state_ck).
var (
	// StateOpen means at least one member occurrence is still live, or the
	// generation is inside its close delay.
	StateOpen = State{"open"}
	// StateClosed means the generation is finished and its thread is frozen.
	StateClosed = State{"closed"}
)

// NewState parses a persisted group state.
func NewState(s string) (State, error) {
	switch s {
	case StateOpen.s, StateClosed.s:
		return State{s: s}, nil
	default:
		return State{}, errs.Newf(errs.KindValidation, "enum",
			"group state must be one of: open, closed (got %q)", s)
	}
}

// String renders the state.
func (s State) String() string { return s.s }

// IsZero reports whether the state is unset.
func (s State) IsZero() bool { return s.s == "" }

// IsOpen reports whether the generation is still accepting members.
func (s State) IsOpen() bool { return s == StateOpen }

// GroupKey is the durable §C.4 identity of a notification group.
//
// It is STABLE ACROSS `alertmanager.yml` ROUTE EDITS, which is exactly what
// Alertmanager's own `groupKey` is not. It is parsed here and COMPUTED in the
// shared kernel: there is one implementation of §C.4 in this codebase and this is
// not it.
type GroupKey struct{ s string }

// NewGroupKey parses a group key, validating it against groups_key_ck.
func NewGroupKey(s string) (GroupKey, error) {
	if !validate.GroupKeyRe.MatchString(s) {
		return GroupKey{}, errs.Newf(errs.KindValidation, "group_key",
			"group_key must match %s", validate.PatternGroupKey)
	}
	return GroupKey{s: s}, nil
}

// String renders the group key.
func (k GroupKey) String() string { return k.s }

// IsZero reports whether the key is unset.
func (k GroupKey) IsZero() bool { return k.s == "" }

// Counts is the membership rollup carried on a generation.
//
// It is a PROJECTION of the member occurrences and is recomputed rather than
// incremented: an increment that misses one transition is wrong forever, while a
// recomputation is wrong until the next one.
type Counts struct {
	Firing     int
	Suppressed int
	Resolved   int
	Expired    int
	Total      int
	Acked      int
}

// Validate mirrors groups_counts_ck and groups_acked_ck.
func (c Counts) Validate() error {
	if c.Firing < 0 || c.Suppressed < 0 || c.Resolved < 0 || c.Expired < 0 ||
		c.Total < 0 || c.Acked < 0 {
		return errs.New(errs.KindValidation, "min", "group counts must be >= 0")
	}
	if c.Acked > c.Total {
		return errs.New(errs.KindValidation, "field_order",
			"acked_count must be <= total_count")
	}
	return nil
}

// Live is how many members are still firing or suppressed. A generation with a
// live member cannot close.
func (c Counts) Live() int { return c.Firing + c.Suppressed }

// Equal reports whether two rollups are identical. It decides whether a
// membership change was MATERIAL, and therefore whether `state_version` moves —
// which in turn decides whether a new Notification can exist (§C.7).
func (c Counts) Equal(o Counts) bool { return c == o }

// Group is one generation of one notification group.
//
// Every field is unexported and reachable only through a constructor, so a
// generation that is `closed` without a `closed_at`, or `storm_mode` without a
// `storm_since`, cannot be built at all. Each invariant mirrors a §D.5 CHECK.
type Group struct {
	id        uuid.UUID
	orgID     uuid.UUID
	sourceID  uuid.UUID
	clusterID uuid.UUID

	// clusterKey is the human-readable identity of the failure domain, resolved
	// from clusterID. It is FIRST-CLASS rather than read out of groupLabels:
	// `group_labels["cluster"]` is whatever Alertmanager happened to group by
	// and is absent the moment an operator edits `group_by`, whereas cluster
	// participates in alert identity (§C.2) and is never absent.
	clusterKey string

	key            GroupKey
	generation     int
	sourceGroupKey string
	receiver       string
	groupLabels    map[string]string
	title          string

	state        State
	severity     string
	stateVersion int
	counts       Counts

	stormMode  bool
	stormSince time.Time

	lastNotificationReason string

	firstSeenAt    time.Time
	lastActivityAt time.Time
	closedAt       time.Time
}

// GroupParams is the full constructor input. It is also the rehydration shape: a
// repository maps a row into it and the constructor re-proves every invariant.
type GroupParams struct {
	ID        uuid.UUID
	OrgID     uuid.UUID
	SourceID  uuid.UUID
	ClusterID uuid.UUID
	// ClusterKey is the cluster's stable machine name, joined from `clusters`.
	// It may be empty only for a Group built in-flight before the join has run;
	// every Group a repository returns carries it.
	ClusterKey string

	Key        GroupKey
	Generation int
	// SourceGroupKey is Alertmanager's raw groupKey, kept verbatim for
	// observability. OPAQUE — it MUST NOT be parsed (C3).
	SourceGroupKey string
	Receiver       string
	GroupLabels    map[string]string
	Title          string

	State State
	// Severity is the highest member severity, RAW as the upstream wrote it.
	Severity     string
	StateVersion int
	Counts       Counts

	StormMode  bool
	StormSince time.Time

	LastNotificationReason string

	FirstSeenAt    time.Time
	LastActivityAt time.Time
	ClosedAt       time.Time
}

// NewGroup builds a generation, enforcing every §D.5 invariant.
func NewGroup(p GroupParams) (Group, error) {
	if err := requireID("group id", p.ID); err != nil {
		return Group{}, err
	}
	if err := requireID("org_id", p.OrgID); err != nil {
		return Group{}, err
	}
	if err := requireID("source_id", p.SourceID); err != nil {
		return Group{}, err
	}
	if err := requireID("cluster_id", p.ClusterID); err != nil {
		return Group{}, err
	}
	if p.Key.IsZero() {
		return Group{}, errs.New(errs.KindValidation, "required", "group_key is required")
	}
	if p.Generation < 1 {
		return Group{}, errs.New(errs.KindValidation, "min", "generation must be >= 1")
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return Group{}, errs.New(errs.KindValidation, "not_blank", "a group needs a title")
	}
	if len(title) > MaxTitleBytes {
		return Group{}, errs.Newf(errs.KindValidation, "max_length",
			"group title must have at most %d characters", MaxTitleBytes)
	}
	if p.State.IsZero() {
		return Group{}, errs.New(errs.KindValidation, "required", "group state is required")
	}
	if p.StateVersion < 1 {
		return Group{}, errs.New(errs.KindValidation, "min", "state_version must be >= 1")
	}
	if err := p.Counts.Validate(); err != nil {
		return Group{}, err
	}
	if p.FirstSeenAt.IsZero() {
		return Group{}, errs.New(errs.KindValidation, "required", "first_seen_at is required")
	}
	if p.LastActivityAt.Before(p.FirstSeenAt) {
		return Group{}, errs.New(errs.KindValidation, "field_order",
			"last_activity_at must be >= first_seen_at")
	}
	// groups_closed_ck: closed exactly when closed_at is set.
	if (p.State == StateClosed) != !p.ClosedAt.IsZero() {
		return Group{}, errs.New(errs.KindValidation, "field_order",
			"closed_at is set exactly when the group is closed")
	}
	if !p.ClosedAt.IsZero() && p.ClosedAt.Before(p.FirstSeenAt) {
		return Group{}, errs.New(errs.KindValidation, "field_order",
			"closed_at must be >= first_seen_at")
	}
	// groups_storm_ck: storm_mode and storm_since are all-or-nothing. Storm
	// collapse is a VISIBLE state and half of one would render as neither.
	if p.StormMode != !p.StormSince.IsZero() {
		return Group{}, errs.New(errs.KindValidation, "field_order",
			"storm_since is set exactly when storm_mode is on")
	}

	labels := map[string]string{}
	for k, v := range p.GroupLabels {
		labels[k] = v
	}

	return Group{
		id:                     p.ID,
		orgID:                  p.OrgID,
		sourceID:               p.SourceID,
		clusterID:              p.ClusterID,
		clusterKey:             p.ClusterKey,
		key:                    p.Key,
		generation:             p.Generation,
		sourceGroupKey:         p.SourceGroupKey,
		receiver:               p.Receiver,
		groupLabels:            labels,
		title:                  title,
		state:                  p.State,
		severity:               p.Severity,
		stateVersion:           p.StateVersion,
		counts:                 p.Counts,
		stormMode:              p.StormMode,
		stormSince:             utcOrZero(p.StormSince),
		lastNotificationReason: p.LastNotificationReason,
		firstSeenAt:            p.FirstSeenAt.UTC(),
		lastActivityAt:         p.LastActivityAt.UTC(),
		closedAt:               utcOrZero(p.ClosedAt),
	}, nil
}

// ID is the generation's uuidv7.
func (g Group) ID() uuid.UUID { return g.id }

// OrgID is the tenant this generation belongs to.
func (g Group) OrgID() uuid.UUID { return g.orgID }

// SourceID is the AlertSource whose notification group this is.
func (g Group) SourceID() uuid.UUID { return g.sourceID }

// ClusterID is the Cluster the group's members belong to.
func (g Group) ClusterID() uuid.UUID { return g.clusterID }

// ClusterKey is that Cluster's stable machine name — the value the contract
// calls `cluster_key` and the one that participates in alert identity (§C.2).
//
// ⛔ It is NOT `group_labels["cluster"]`. That entry exists only while
// Alertmanager happens to group by `cluster`, so reading it there makes the
// field vanish on the day someone edits a route — and a group whose cluster is
// silently unknown is a group nobody can trust during a multi-cluster incident.
func (g Group) ClusterKey() string { return g.clusterKey }

// Key is the durable §C.4 group identity, stable across route edits.
func (g Group) Key() GroupKey { return g.key }

// Generation increments when a closed group re-opens, and is the reason the
// re-opened group starts a fresh Slack thread.
func (g Group) Generation() int { return g.generation }

// SourceGroupKey is Alertmanager's raw groupKey. OPAQUE — never parse it.
func (g Group) SourceGroupKey() string { return g.sourceGroupKey }

// Receiver is the Alertmanager receiver name, "" for a reconciler-sourced group.
func (g Group) Receiver() string { return g.receiver }

// GroupLabels is a copy of the labels Alertmanager grouped by.
func (g Group) GroupLabels() map[string]string {
	out := make(map[string]string, len(g.groupLabels))
	for k, v := range g.groupLabels {
		out[k] = v
	}
	return out
}

// Title is the pre-rendered group title for the Slack card and the UI.
func (g Group) Title() string { return g.title }

// State is open or closed — a projection of the members' occurrence states.
func (g Group) State() State { return g.state }

// Severity is the highest member severity, raw as upstream wrote it.
func (g Group) Severity() string { return g.severity }

// StateVersion increments on every MATERIAL change and is hashed into
// notification.idempotency_key (§C.7), which is what makes "all_resolved at
// state_version 7" exist exactly once.
func (g Group) StateVersion() int { return g.stateVersion }

// Counts is the membership rollup.
func (g Group) Counts() Counts { return g.counts }

// StormMode reports whether storm collapse is holding this generation to one
// message. It is a VISIBLE state, never silent suppression (§B.6).
func (g Group) StormMode() bool { return g.stormMode }

// StormSince is when storm mode began, zero when it is off.
func (g Group) StormSince() time.Time { return g.stormSince }

// LastNotificationReason is the most recent Alertmanager notification_reason seen
// for this group, feeding the §H.6 decision table.
func (g Group) LastNotificationReason() string { return g.lastNotificationReason }

// FirstSeenAt is when this generation opened.
func (g Group) FirstSeenAt() time.Time { return g.firstSeenAt }

// LastActivityAt drives group.close: a generation idle past group_close_delay_s
// closes and freezes its thread.
func (g Group) LastActivityAt() time.Time { return g.lastActivityAt }

// ClosedAt is when the generation closed, zero while it is open.
func (g Group) ClosedAt() time.Time { return g.closedAt }

// IsOpen reports whether the generation still accepts members.
func (g Group) IsOpen() bool { return g.state.IsOpen() }

// CanCloseAt reports whether the generation may close: no live member, and idle
// for at least the close delay.
//
// The instant is a PARAMETER — the domain never calls time.Now().
func (g Group) CanCloseAt(now time.Time, closeDelay time.Duration) bool {
	if !g.IsOpen() || g.counts.Live() > 0 {
		return false
	}
	return !now.UTC().Before(g.lastActivityAt.Add(closeDelay))
}

// WithRollup returns the generation carrying a recomputed rollup, and reports
// whether the change was MATERIAL.
//
// Material means the counts moved, the state flipped or the severity changed. It
// is the trigger for bumping `state_version`, and therefore for a new
// Notification being mintable at all (§C.7). A repeat observation that changes no
// count is not news, and must not be able to re-notify.
func (g Group) WithRollup(counts Counts, severity string, now time.Time) (Group, bool, error) {
	if err := counts.Validate(); err != nil {
		return Group{}, false, err
	}
	state := StateOpen
	if !g.IsOpen() {
		state = g.state
	}
	material := !g.counts.Equal(counts) || g.severity != severity

	next := g
	next.counts = counts
	next.severity = severity
	next.state = state
	if material {
		next.stateVersion = g.stateVersion + 1
		next.lastActivityAt = maxTime(g.lastActivityAt, now.UTC())
	}
	return next, material, nil
}

// Close ends the generation and freezes its thread.
func (g Group) Close(now time.Time) (Group, error) {
	if !g.IsOpen() {
		return Group{}, errs.New(errs.KindPrecondition, "group_already_closed",
			"this group generation is already closed")
	}
	if g.counts.Live() > 0 {
		return Group{}, errs.New(errs.KindPrecondition, "group_has_live_members",
			"a group with a firing or suppressed member cannot close")
	}
	next := g
	next.state = StateClosed
	next.closedAt = maxTime(g.firstSeenAt, now.UTC())
	next.lastActivityAt = maxTime(g.lastActivityAt, next.closedAt)
	next.stateVersion = g.stateVersion + 1
	// Storm mode is a fact about a LIVE generation. Carrying it onto a closed one
	// would leave the UI showing a storm nobody can act on.
	next.stormMode = false
	next.stormSince = time.Time{}
	return next, nil
}

// Touch records activity without changing the rollup, so an idle group's close
// clock restarts when something happens in it.
func (g Group) Touch(now time.Time) Group {
	next := g
	next.lastActivityAt = maxTime(g.lastActivityAt, now.UTC())
	return next
}

// Title renders the pre-computed group title from its groupLabels.
//
// Rendering it ONCE, at write time, is what stops the Slack card and the UI
// disagreeing about what a group is called — and what stops the timeline reading
// differently after somebody edits a label.
func Title(groupLabels map[string]string, fallback string) string {
	if len(groupLabels) == 0 {
		return truncateTitle(fallbackTitle(fallback))
	}
	names := make([]string, 0, len(groupLabels))
	for k := range groupLabels {
		names = append(names, k)
	}
	sort.Strings(names)

	// `alertname` leads when present: it is what a human recognises the group by.
	if _, ok := groupLabels["alertname"]; ok {
		names = append([]string{"alertname"}, without(names, "alertname")...)
	}

	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(n)
		b.WriteString("=")
		b.WriteString(groupLabels[n])
	}
	return truncateTitle(b.String())
}

func fallbackTitle(fallback string) string {
	if strings.TrimSpace(fallback) == "" {
		return "Alert group"
	}
	return fallback
}

func truncateTitle(s string) string {
	if len(s) <= MaxTitleBytes {
		return s
	}
	return s[:MaxTitleBytes-1] + "…"
}

func without(xs []string, drop string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x != drop {
			out = append(out, x)
		}
	}
	return out
}

// The two sort keys `GET /api/v1/alert-groups` accepts, and nothing else.
//
// Sorting is restricted for the same reason it is on the alert list: a keyset
// cursor is only sound over a total ordering, and an unrecognised value is
// rejected rather than silently defaulted. A list that quietly ignores `sort` is
// a list that lies about its order.
const (
	// SortLastActivityDesc is the default: the busiest conversation first.
	SortLastActivityDesc = "-last_activity_at"
	// SortFirstSeenDesc orders by when the generation opened.
	SortFirstSeenDesc = "-first_seen_at"
)

// GroupFilter is the compiled, validated form of the `listAlertGroups` query.
//
// ⭐ EVERY DIMENSION HERE IS PUSHED DOWN TO SQL. It exists because the handler
// used to post-filter an already-fetched keyset page in memory, which is not a
// filter but a truncation: the page came back holding 50 groups, the filter
// removed 43, and the caller was handed 7 rows plus a `has_more` cursor that had
// already skipped past everything it never got to see. A filter that runs after
// pagination is silently wrong on every page but the last.
//
// A nil or empty value means "no constraint on this dimension".
type GroupFilter struct {
	// States filters over open|closed. Empty means both — a closed generation is
	// the record of a conversation that happened, and hiding it would make the
	// group list disagree with the chat channel it mirrors.
	States []string
	// Severities matches the group's MAXIMUM member severity, RAW as upstream
	// wrote it (§L.4.2).
	Severities []string
	// ClusterKeys matches the joined `clusters.cluster_key`, not a group label.
	ClusterKeys []string
	SourceID    *uuid.UUID
	Receiver    string
	// Storm restricts to generations collapsed into storm mode, or excludes
	// them. Storm collapse is a VISIBLE state, which is why it is filterable at
	// all (§B.6).
	Storm *bool
	// FullyAcked is true for "every member has a receipt", false for "at least
	// one does not". Orthogonal to state: an acked group is still firing (§B.1).
	FullyAcked *bool
	// Since is a lower bound on `last_activity_at`.
	Since *time.Time
	// Query is free text over the title and the group labels.
	Query string
	// FilterHash must equal Cursor.Hash or the cursor is rejected (§E.1).
	FilterHash string
}

// GenerationLabel renders "gk_… #3" for logs and debugging.
func GenerationLabel(k GroupKey, generation int) string {
	return k.String() + " #" + strconv.Itoa(generation)
}

func requireID(field string, v uuid.UUID) error {
	if v == uuid.Nil {
		return errs.Newf(errs.KindValidation, "required", "%s is required", field)
	}
	return nil
}

func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
