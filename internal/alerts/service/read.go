package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The two sort keys §E.3 accepts, and nothing else. An unrecognised value is
// rejected rather than silently defaulted: a list that quietly ignores `sort` is
// a list that lies about its order.
const (
	// SortLastSeenDesc is the default: most recently heard from first.
	SortLastSeenDesc = "-last_seen_at"
	// SortFirstSeenDesc orders by when oto first saw the identity.
	SortFirstSeenDesc = "-first_seen_at"
)

// DefaultTimelineWindow bounds a timeline query whose caller supplied no lower
// bound.
//
// `recorded_at` is the partition key of `alert_events` and an unbounded query
// scans thirteen months of partitions (§D.12b). The API is expected to default
// the bound itself — to `group.first_seen_at` for a group timeline — and this is
// the backstop for when it does not.
const DefaultTimelineWindow = 30 * 24 * time.Hour

// ListQuery is the compiled form of `GET /api/v1/alerts` (§E.3).
type ListQuery struct {
	Filter domain.AlertFilter
	// Sort is "" (meaning SortLastSeenDesc), SortLastSeenDesc or
	// SortFirstSeenDesc.
	Sort string
	Page db.Keyset
}

// ListResult is one page of alerts plus the cursor for the next.
type ListResult struct {
	Alerts []domain.Alert
	// Snoozes is the ACTIVE snooze of each alert on this page that has one,
	// keyed by alert id. Absent means awake.
	//
	// ⭐ It is a MAP AND NOT A FIELD ON THE ALERT because a snooze is a row in
	// `alert_snoozes` and the Alert only carries the `snoozed_until` projection
	// (§D.8b) — who asked, why, and since when live on the side table, which is
	// what keeps the person reference off the signal row (§D.4.0). The list needs
	// all of it to draw the badge, so it is batch-loaded beside the page rather
	// than folded into the entity.
	Snoozes map[uuid.UUID]domain.Snooze
	Cursor  db.Cursor
}

// List serves `GET /api/v1/alerts`.
//
// The cursor is bound to the filter it was minted under. A cursor presented
// against a DIFFERENT filter is rejected rather than quietly producing a wrong
// page (§E.1) — the caller maps this to `400 cursor_filter_mismatch`.
//
// ⭐ `?snoozed=` IS NOW A TAB SELECTOR, AND ITS ABSENCE STILL MEANS "BOTH".
// `false` is the main tab and `true` is **Quiet**; a caller that sends neither
// gets every alert, because a caller that asked no question must not be handed a
// filtered answer. Either way the question is put to `alert_snoozes`, never to a
// column on `alerts`.
//
// §B.8.6 refused to hide snoozed alerts from the default list because a snooze
// badge on row 400 of a scrolling list is already invisible, so hiding the row
// loses the incident. The Quiet tab is what makes hiding safe: it is present even
// at zero, and its badge carries the worst state inside it, so something live
// being held back is legible in a way the badge never was.
func (s *Service) List(ctx context.Context, scope db.TenantScope, q ListQuery) (ListResult, error) {
	if !q.Page.Cursor.IsZero() && q.Page.Cursor.Hash != q.Filter.FilterHash {
		return ListResult{}, errs.Malformed("cursor_filter_mismatch",
			"this cursor was minted against a different set of filters")
	}
	switch q.Sort {
	case "", SortLastSeenDesc, SortFirstSeenDesc:
	default:
		return ListResult{}, errs.Validation("sort_invalid",
			"sort must be one of: -last_seen_at, -first_seen_at")
	}

	var (
		alerts []domain.Alert
		cur    db.Cursor
		err    error
	)
	switch {
	case s.lister != nil:
		alerts, cur, err = s.lister.ListSorted(ctx, scope, q.Filter, q.Sort, q.Page)
	case q.Sort == SortFirstSeenDesc:
		return ListResult{}, errs.Validation("sort_unsupported",
			"this deployment cannot sort by -first_seen_at")
	default:
		alerts, cur, err = s.alerts.List(ctx, scope, q.Filter, q.Page)
	}
	if err != nil {
		return ListResult{}, err
	}

	snoozes, err := s.activeSnoozes(ctx, scope, alerts)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Alerts: alerts, Snoozes: snoozes, Cursor: cur}, nil
}

// activeSnoozes batch-loads the §B.8 snooze rows behind one page of alerts.
//
// ⭐ IT IS ONE QUERY, NEVER ONE PER ROW, and it is one query rather than
// sometimes none because the Alert no longer carries a hint about whether to ask.
// It used to skip the round trip for a page whose alerts all had an empty
// `alerts.snoozed_until`; that projection is gone (00048), and reading it to
// decide whether to read the truth was the shape of the defect, not an
// optimisation worth rebuilding.
//
// The cost of asking anyway is small and bounded: `alert_snoozes_active_idx` is
// UNIQUE (alert_id) WHERE ended_at IS NULL, so this is one indexed probe per page
// over the snoozes CURRENTLY IN FORCE — dozens of rows for a whole tenant.
//
// A snooze whose clock has run out but which the 60-second `snooze.expire` job
// has not swept yet is still returned: `ended_at` is still null, so it is still a
// row, and the mapper decides how to render it. Filtering it here would make the
// list and the detail page disagree for up to a minute.
func (s *Service) activeSnoozes(
	ctx context.Context, scope db.TenantScope, alerts []domain.Alert,
) (map[uuid.UUID]domain.Snooze, error) {
	if len(alerts) == 0 {
		return map[uuid.UUID]domain.Snooze{}, nil
	}
	ids := make([]uuid.UUID, 0, len(alerts))
	for _, a := range alerts {
		ids = append(ids, a.ID())
	}
	return s.snoozes.ActiveByAlerts(ctx, scope, ids)
}

// ActiveSnoozeResult is one page of the §B.8.6 org-wide view.
type ActiveSnoozeResult struct {
	Snoozes []domain.Snooze
	// Alerts is the Alert each snooze mutes, keyed by `alert_key`. A snooze row
	// carries the key denormalised precisely so the audit trail survives, which
	// makes it the join key here and costs one batched read rather than one per
	// row. An absent entry renders as a snooze without its alert rather than as
	// a dropped row: a quiet period whose alert cannot be read is still a quiet
	// period somebody has to know about.
	Alerts map[string]domain.Alert
	Cursor db.Cursor
}

// ActiveSnoozes serves the ORG-WIDE list of §B.8.6: what oto is currently quiet
// about, and until when.
//
// ⭐ IT IS NOT `GET /alerts?snoozed=true`. That pages ALERTS — it answers "which
// alerts are quiet" and structurally cannot answer "who asked, why, and until
// when", because those live on `alert_snoozes` and one alert has a whole history
// of them. §B.8.6 asks for a persistent banner enumerating every active snooze
// with its expiry "so a snooze cannot be forgotten"; a list of alerts is not that
// list, and a snooze nobody can enumerate is the silent suppression §B.6 forbids
// arriving by the back door.
//
// "Active" is evaluated against the SERVICE CLOCK, never the caller's: a client
// whose clock is wrong must not be able to disagree with the server about what is
// currently muted.
func (s *Service) ActiveSnoozes(
	ctx context.Context, scope db.TenantScope, p db.Keyset,
) (ActiveSnoozeResult, error) {
	rows, cur, err := s.snoozes.ListActive(ctx, scope, s.Now(), p)
	if err != nil {
		return ActiveSnoozeResult{}, err
	}

	out := ActiveSnoozeResult{Snoozes: rows, Alerts: map[string]domain.Alert{}, Cursor: cur}
	if len(rows) == 0 || s.alertBatch == nil {
		return out, nil
	}

	keys := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		k := r.AlertKey().String()
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	alerts, err := s.alertBatch.GetByAlertKeys(ctx, scope, keys)
	if err != nil {
		return ActiveSnoozeResult{}, err
	}
	out.Alerts = alerts
	return out, nil
}

// AlertDetail is `GET /api/v1/alerts/{id}`: the identity, the episode currently
// running (if any), and the snooze currently in force (if any).
//
// The three are returned side by side and never collapsed, because they are the
// three orthogonal axes of §B.1: what the world is doing, what humans have done,
// and whether oto is notifying.
type AlertDetail struct {
	Alert domain.Alert
	// CurrentCase is the open episode, or nil when none is running.
	CurrentCase *domain.Case
	// LatestCase is the most recent episode, open or ended. It is what the
	// UI shows when nothing is running.
	LatestCase *domain.Case
	// Snooze is the ACTIVE snooze row, or nil when the alert is awake.
	Snooze *domain.Snooze
	// SnoozedNow answers "is oto holding its tongue right now", evaluated against
	// the service clock rather than the caller's.
	SnoozedNow bool
}

// Get serves `GET /api/v1/alerts/{id}`.
func (s *Service) Get(ctx context.Context, scope db.TenantScope, alertID uuid.UUID) (AlertDetail, error) {
	alert, err := s.alerts.GetByID(ctx, scope, alertID)
	if err != nil {
		return AlertDetail{}, err
	}
	out := AlertDetail{Alert: alert}

	if latest, ok, err := s.cases.GetLatestByAlert(ctx, scope, alertID); err != nil {
		return AlertDetail{}, err
	} else if ok {
		o := latest
		out.LatestCase = &o
		if o.IsOpen() {
			cur := o
			out.CurrentCase = &cur
		}
	}

	// ⭐ `SnoozedNow` IS DECIDED FROM THE ROW THAT WAS JUST READ. It used to be
	// `alert.IsSnoozedAt(now)` — the mirrored timestamp — two lines away from the
	// authoritative record, which is how the two could ever disagree.
	if snz, ok, err := s.snoozes.GetActive(ctx, scope, alertID); err != nil {
		return AlertDetail{}, err
	} else if ok {
		v := snz
		out.Snooze = &v
		out.SnoozedNow = v.IsActiveAt(s.Now())
	}
	return out, nil
}

// GetAlert returns the Alert row alone — identity, labels, projections — with
// none of the detail page's companion reads.
//
// It exists for MACHINE callers. `Get` above is the detail PAGE: beside the
// alert it re-reads the latest case and the active snooze, because a
// human opening the page is owed all three §B.1 axes at once. The enrichment
// pipeline needs only the frozen identity of the subject it is enriching — it
// already holds the case it was dispatched for — and a worker that
// consumed the page paid two extra reads per run whose results were discarded.
// A machine that wants one fact asks for one fact.
func (s *Service) GetAlert(ctx context.Context, scope db.TenantScope, alertID uuid.UUID) (domain.Alert, error) {
	return s.alerts.GetByID(ctx, scope, alertID)
}

// GetByKey resolves an Alert by its §C.2 identity key — the human-copyable
// handle that appears in Slack buttons and URLs.
func (s *Service) GetByKey(ctx context.Context, scope db.TenantScope, alertKey string) (AlertDetail, error) {
	alert, err := s.alerts.GetByAlertKey(ctx, scope, alertKey)
	if err != nil {
		return AlertDetail{}, err
	}
	return s.Get(ctx, scope, alert.ID())
}

// CaseResult is one page of episode history.
type CaseResult struct {
	Cases  []domain.Case
	Cursor db.Cursor
}

// CaseListQuery is the compiled form of `GET /api/v1/cases` (§E.3b).
type CaseListQuery struct {
	Filter domain.CaseFilter
	Page   db.Keyset
}

// CaseListResult is one page of the ORG-WIDE case list, plus the identities its
// rows belong to.
type CaseListResult struct {
	Cases []domain.Case
	// Alerts is the identity behind each row, keyed by alert id.
	//
	// ⭐ IT IS A MAP BESIDE THE PAGE AND NOT A FIELD ON THE CASE, for the same
	// reason ListResult.Snoozes is: `alertname`, `severity`, `cluster_key` and
	// `namespace` are columns of `alerts` and describe the IDENTITY. An episode
	// that carried a copy would be asserting them for as long as the row lives,
	// and a severity relabelled last week would still be rendered on every
	// episode from before the change. The list needs them to be readable, so
	// they are batch-loaded — ONE query for the whole page — rather than folded
	// into the entity.
	//
	// An entry is always present for every row: the repository's `EXISTS` proved
	// the alert is in the caller's org before the case was returned.
	Alerts map[uuid.UUID]domain.Alert
	Cursor db.Cursor
}

// ListCases serves `GET /api/v1/cases` — the ORG-WIDE episode list.
//
// ⭐⭐ THIS IS THE ACK SURFACE, AND IT IS WHY IT IS NOT A MODE OF `GET /alerts`.
// The alert list pages IDENTITIES, and since 00049 `alerts` carries no ack
// column at all: a receipt belongs to the firing it was given for, so
// `?ack=acked` over that table asked whether a closed episode had been
// acknowledged and was removed rather than fixed. "What is firing that nobody
// has looked at" is a question about `alert_cases`, and this is where it is
// asked — `?state=open&ack=unacked`, which is the one shape case_ack_idx exists
// to serve. (It was `?open=true` until ADR 0040 collapsed the two axes into one;
// the repository still emits the liveness LITERAL, because that is the only
// spelling a partial index can be matched against.)
//
// The cursor is bound to the filter it was minted under; a cursor presented
// against a DIFFERENT filter is rejected rather than quietly producing a wrong
// page (§E.1), exactly as on the alert list.
func (s *Service) ListCases(
	ctx context.Context, scope db.TenantScope, q CaseListQuery,
) (CaseListResult, error) {
	if !q.Page.Cursor.IsZero() && q.Page.Cursor.Hash != q.Filter.FilterHash {
		return CaseListResult{}, errs.Malformed("cursor_filter_mismatch",
			"this cursor was minted against a different set of filters")
	}

	rows, cur, err := s.cases.ListCases(ctx, scope, q.Filter, q.Page)
	if err != nil {
		return CaseListResult{}, err
	}
	if len(rows) == 0 {
		return CaseListResult{Alerts: map[uuid.UUID]domain.Alert{}, Cursor: cur}, nil
	}

	// One query for the page, never one per row. The ids are de-duplicated
	// first: an alert with two episodes on one page is one identity to read.
	ids := make([]uuid.UUID, 0, len(rows))
	seen := make(map[uuid.UUID]struct{}, len(rows))
	for _, c := range rows {
		if _, dup := seen[c.AlertID()]; dup {
			continue
		}
		seen[c.AlertID()] = struct{}{}
		ids = append(ids, c.AlertID())
	}
	alerts, err := s.alerts.GetByIDs(ctx, scope, ids)
	if err != nil {
		return CaseListResult{}, err
	}
	return CaseListResult{Cases: rows, Alerts: alerts, Cursor: cur}, nil
}

// Cases serves `GET /api/v1/alerts/{id}/cases` — the episode
// history, newest first.
func (s *Service) Cases(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, p db.Keyset,
) (CaseResult, error) {
	acs, cur, err := s.cases.ListByAlert(ctx, scope, alertID, p)
	if err != nil {
		return CaseResult{}, err
	}
	return CaseResult{Cases: acs, Cursor: cur}, nil
}

// GetCase serves `GET /api/v1/cases/{id}`.
func (s *Service) GetCase(
	ctx context.Context, scope db.TenantScope, caseID uuid.UUID,
) (domain.Case, error) {
	return s.cases.GetByID(ctx, scope, caseID)
}

// PreviousCaseWithRule resolves the episode that fired BEFORE the given
// one and had a rule snapshot bound to it.
//
// ⭐ IT EXISTS SO THAT DRIFT CAN BE MEASURED BETWEEN TWO EPISODES. "What changed
// about this rule since the last time this alert fired" is a question about two
// FIRES, and the only reads this service had — the current episode and the
// newest captured version of the rule — answer neither half of it. Comparing an
// episode against the newest version upstream answers a different question, and
// answers it `null` in the case that matters: an alert that fired under a text
// somebody had just edited is an alert whose bound snapshot IS the newest one.
//
// The episode is addressed by `seq`, the ordinal the state machine mints, and an
// episode with no snapshot is stepped over rather than stopped at — see
// `repository.PreviousWithRuleSnapshot`, which is where both choices are argued.
func (s *Service) PreviousCaseWithRule(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, beforeSeq int,
) (domain.Case, bool, error) {
	// The first episode of an alert has no predecessor, and asking the database
	// to prove that on every rule panel read is a query with a known answer.
	if beforeSeq <= 1 {
		return domain.Case{}, false, nil
	}
	return s.cases.PreviousWithRuleSnapshot(ctx, scope, alertID, beforeSeq)
}

// TimelineResult is one page of the append-only timeline.
type TimelineResult struct {
	Events []domain.Event
	Cursor db.Cursor
}

// AlertTimeline serves `GET /api/v1/alerts/{id}/events`.
func (s *Service) AlertTimeline(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, w db.TimeWindow, p db.Keyset,
) (TimelineResult, error) {
	evs, cur, err := s.events.ListByAlert(ctx, scope, alertID, s.window(w), p)
	if err != nil {
		return TimelineResult{}, err
	}
	return TimelineResult{Events: evs, Cursor: cur}, nil
}

// CaseTimeline serves `GET /api/v1/cases/{id}/events`.
func (s *Service) CaseTimeline(
	ctx context.Context, scope db.TenantScope, caseID uuid.UUID, w db.TimeWindow, p db.Keyset,
) (TimelineResult, error) {
	evs, cur, err := s.events.ListByCase(ctx, scope, caseID, s.window(w), p)
	if err != nil {
		return TimelineResult{}, err
	}
	return TimelineResult{Events: evs, Cursor: cur}, nil
}

// GroupTimeline serves `GET /api/v1/alert-groups/{id}/timeline` — §D.12(b), the
// MERGED, ORDERED lifecycle timeline and the signature view of the product.
//
// It lives in the alerts module because `alert_events` does, and it is exposed as
// a port for `grouping` rather than the other way round: the timeline is the
// truth, and the group is one of three subjects it can be read by.
func (s *Service) GroupTimeline(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID, w db.TimeWindow, p db.Keyset,
) (TimelineResult, error) {
	evs, cur, err := s.events.ListByGroup(ctx, scope, groupID, s.window(w), p)
	if err != nil {
		return TimelineResult{}, err
	}
	return TimelineResult{Events: evs, Cursor: cur}, nil
}

// window supplies the mandatory lower bound when the caller omitted one, so that
// a timeline query can never degenerate into a thirteen-month partition scan.
func (s *Service) window(w db.TimeWindow) db.TimeWindow {
	now := s.Now()
	if w.From.IsZero() {
		w.From = now.Add(-DefaultTimelineWindow)
	}
	if w.To.IsZero() {
		w.To = now
	}
	return w
}

// Enrichments serves `GET /api/v1/alerts/{id}/enrichments`: every enrichment
// result with its provenance.
//
// A FAILED enrichment and a MISSING one are deliberately distinguishable — a
// result that cannot say who computed it, at which version and whether it
// succeeded is a rumour. When no reader is wired the answer is an empty list, not
// an error: oto runs with enrichment disabled.
func (s *Service) Enrichments(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID,
) ([]EnrichmentSummary, error) {
	if s.enrichments == nil {
		return nil, nil
	}
	alert, err := s.alerts.GetByID(ctx, scope, alertID)
	if err != nil {
		return nil, err
	}
	var caseID *uuid.UUID
	if alert.HasOpenCase() {
		caseID = ptr(alert.CurrentCaseID())
	}
	return s.enrichments.ListForAlert(ctx, scope, alertID, caseID)
}

// NotificationResult is one page of notification intents for an Alert.
type NotificationResult struct {
	Notifications []NotificationSummary
	Cursor        db.Cursor
}

// Notifications serves `GET /api/v1/alerts/{id}/notifications`: was anybody told
// about this alert, and did it land.
//
// ⭐ Delivery failure MUST be visible per alert. oto's silence must never be
// indistinguishable from "no alert" — that is why the summary carries the
// per-status delivery counts and the suppression reason rather than a single
// boolean.
//
// It goes through a PORT. §I.1 binds `alerts` never to import `notification`,
// which is what lets oto run with notifications entirely disabled; in that
// configuration this returns an empty page.
func (s *Service) Notifications(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, p db.Keyset,
) (NotificationResult, error) {
	if s.notifications == nil {
		return NotificationResult{}, nil
	}
	items, cur, err := s.notifications.ListForAlert(ctx, scope, alertID, p)
	if err != nil {
		return NotificationResult{}, err
	}
	return NotificationResult{Notifications: items, Cursor: cur}, nil
}

// DeliveryRollupForAlert serves the `delivery_summary` of `GET /alerts/{id}`.
//
// ⭐ THE FIELD WAS DECLARED AND NEVER EMITTED. It is optional in the schema, so
// every contract validator passed and the gap was invisible for as long as it
// existed. What it costs is the one thing oto sells: a user who sees no Slack
// message cannot otherwise tell "nothing fired" from "four deliveries died".
//
// With no notification module wired, the answer is an all-zero roll-up rather
// than an omitted field. That is honest — this deployment delivers nothing, so
// nothing was delivered — and it keeps the field's presence unconditional, which
// is what stops it from silently disappearing again.
func (s *Service) DeliveryRollupForAlert(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID,
) (DeliveryRollup, error) {
	if s.notifications == nil {
		return DeliveryRollup{}, nil
	}
	return s.notifications.DeliveryRollupForAlert(ctx, scope, alertID)
}

// DeliveryRollupForCase serves the `delivery_summary` of
// `GET /cases/{id}` — the same question narrowed to one firing episode.
func (s *Service) DeliveryRollupForCase(
	ctx context.Context, scope db.TenantScope, caseID uuid.UUID,
) (DeliveryRollup, error) {
	if s.notifications == nil {
		return DeliveryRollup{}, nil
	}
	return s.notifications.DeliveryRollupForCase(ctx, scope, caseID)
}

// SnoozeHistory serves the §B.8.6 snooze history for one Alert. Membership of a
// snooze is history, not a boolean: the org-wide banner that makes the feature
// safe is built from these rows.
func (s *Service) SnoozeHistory(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, limit int,
) ([]domain.Snooze, error) {
	if s.snoozeHist == nil {
		return nil, nil
	}
	return s.snoozeHist.ListByAlert(ctx, scope, alertID, limit)
}

// LabelNames serves `GET /api/v1/labels` — the filter bar's typeahead, each name
// with the number of alerts carrying it.
func (s *Service) LabelNames(
	ctx context.Context, scope db.TenantScope, prefix string, limit int,
) ([]domain.LabelCount, error) {
	return s.alerts.DistinctLabelNames(ctx, scope, prefix, limit)
}

// LabelValues serves `GET /api/v1/labels/{name}/values`.
func (s *Service) LabelValues(
	ctx context.Context, scope db.TenantScope, name, prefix string, limit int,
) ([]domain.LabelCount, error) {
	return s.alerts.DistinctLabelValues(ctx, scope, name, prefix, limit)
}

// RollupQuery is the compiled form of `GET /api/v1/alerts/rollups` (§E.3a).
type RollupQuery struct {
	Filter domain.AlertFilter
	// By is the axis to bucket on. Required; the domain constructor has already
	// refused anything outside the closed set.
	By domain.RollupKey
	// After is the keyset position: the bucket key of the last row of the
	// previous page, "" for the first page.
	After string
	Limit int
}

// RollupResult is one page of roll-up buckets.
type RollupResult struct {
	Rollups []domain.AlertRollup
	HasMore bool
}

// Rollups serves `GET /api/v1/alerts/rollups` — the alert list aggregated by
// alertname, namespace or source fingerprint.
//
// ⭐ WHY THIS EXISTS. "Group alerts by name/namespace/fingerprint" is a product
// requirement, and the only honest place to answer it is the server. A client
// rolling up the rows it has loaded is right for exactly as long as the result
// fits in one page and silently wrong afterwards — it reports "3 firing" for a
// bucket with 300, and nothing about the screen says so.
//
// ⛔ A roll-up bucket is NOT an AlertGroup. `/alert-groups` is one generation of
// one ALERTMANAGER NOTIFICATION GROUP; it has a row, a generation and a chat
// thread. A roll-up is a view over the alert list and has none of those (§A.1).
//
// Every filter the list honours is honoured here, unchanged and by construction:
// the aggregation wraps the same compiled filter the list passes down, so the
// two can never drift into two answers to one question.
func (s *Service) Rollups(
	ctx context.Context, scope db.TenantScope, q RollupQuery,
) (RollupResult, error) {
	if q.By.IsZero() {
		return RollupResult{}, errs.Validation("group_by_required",
			"group_by must be one of: alertname, namespace, fingerprint")
	}
	buckets, hasMore, err := s.alerts.Rollup(ctx, scope, q.Filter, q.By, q.After, q.Limit)
	if err != nil {
		return RollupResult{}, err
	}
	return RollupResult{Rollups: buckets, HasMore: hasMore}, nil
}

// BindRuleSnapshot attaches the RuleSnapshot captured at fire time to an episode
// (R6) and records it on the timeline.
//
// It is called by `rules`, which owns the snapshot, through a port that module
// declares. What the rule SAID at that moment is the differentiator, and the
// binding is what makes drift decidable later.
func (s *Service) BindRuleSnapshot(
	ctx context.Context, scope db.TenantScope, caseID, snapshotID uuid.UUID,
) error {
	return s.tx.InTx(ctx, func(ctx context.Context) error {
		return s.cases.BindRuleSnapshot(ctx, scope, caseID, snapshotID)
	})
}
