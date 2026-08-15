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
// Note what this method does NOT do: it never hides snoozed alerts. §B.8.6 is
// explicit that the default list includes them, because hiding a snoozed alert
// is how an incident is lost. `?snoozed=` is an explicit, visible filter chip.
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
// ⭐ IT IS ONE QUERY OR NONE, NEVER ONE PER ROW. `alerts.snoozed_until` is the
// projection of the active snooze, so an alert with no projection provably has no
// snooze row to read: the id list is narrowed by that before the query is issued,
// and a page with nothing snoozed — which is the overwhelmingly common case —
// costs nothing at all.
//
// The projection is read as a fact about the ROW rather than about the clock. A
// projection that is set but has already passed belongs to a snooze the
// 60-second `snooze.expire` job has not swept yet; it is still fetched, and the
// mapper decides how to render it, because the alternative is a list that
// silently disagrees with the detail page for up to a minute.
func (s *Service) activeSnoozes(
	ctx context.Context, scope db.TenantScope, alerts []domain.Alert,
) (map[uuid.UUID]domain.Snooze, error) {
	ids := make([]uuid.UUID, 0, len(alerts))
	for _, a := range alerts {
		if !a.SnoozedUntil().IsZero() {
			ids = append(ids, a.ID())
		}
	}
	if len(ids) == 0 {
		return map[uuid.UUID]domain.Snooze{}, nil
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
	// CurrentOccurrence is the open episode, or nil when none is running.
	CurrentOccurrence *domain.Occurrence
	// LatestOccurrence is the most recent episode, open or ended. It is what the
	// UI shows when nothing is running.
	LatestOccurrence *domain.Occurrence
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
	out := AlertDetail{Alert: alert, SnoozedNow: alert.IsSnoozedAt(s.Now())}

	if latest, ok, err := s.occurrences.GetLatestByAlert(ctx, scope, alertID); err != nil {
		return AlertDetail{}, err
	} else if ok {
		o := latest
		out.LatestOccurrence = &o
		if o.IsOpen() {
			cur := o
			out.CurrentOccurrence = &cur
		}
	}

	if snz, ok, err := s.snoozes.GetActive(ctx, scope, alertID); err != nil {
		return AlertDetail{}, err
	} else if ok {
		v := snz
		out.Snooze = &v
	}
	return out, nil
}

// GetAlert returns the Alert row alone — identity, labels, projections — with
// none of the detail page's companion reads.
//
// It exists for MACHINE callers. `Get` above is the detail PAGE: beside the
// alert it re-reads the latest occurrence and the active snooze, because a
// human opening the page is owed all three §B.1 axes at once. The enrichment
// pipeline needs only the frozen identity of the subject it is enriching — it
// already holds the occurrence it was dispatched for — and a worker that
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

// OccurrenceResult is one page of episode history.
type OccurrenceResult struct {
	Occurrences []domain.Occurrence
	Cursor      db.Cursor
}

// Occurrences serves `GET /api/v1/alerts/{id}/occurrences` — the episode
// history, newest first.
func (s *Service) Occurrences(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, p db.Keyset,
) (OccurrenceResult, error) {
	occs, cur, err := s.occurrences.ListByAlert(ctx, scope, alertID, p)
	if err != nil {
		return OccurrenceResult{}, err
	}
	return OccurrenceResult{Occurrences: occs, Cursor: cur}, nil
}

// GetOccurrence serves `GET /api/v1/occurrences/{id}`.
func (s *Service) GetOccurrence(
	ctx context.Context, scope db.TenantScope, occurrenceID uuid.UUID,
) (domain.Occurrence, error) {
	return s.occurrences.GetByID(ctx, scope, occurrenceID)
}

// PreviousOccurrenceWithRule resolves the episode that fired BEFORE the given
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
func (s *Service) PreviousOccurrenceWithRule(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, beforeSeq int,
) (domain.Occurrence, bool, error) {
	// The first episode of an alert has no predecessor, and asking the database
	// to prove that on every rule panel read is a query with a known answer.
	if beforeSeq <= 1 {
		return domain.Occurrence{}, false, nil
	}
	return s.occurrences.PreviousWithRuleSnapshot(ctx, scope, alertID, beforeSeq)
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

// OccurrenceTimeline serves `GET /api/v1/occurrences/{id}/events`.
func (s *Service) OccurrenceTimeline(
	ctx context.Context, scope db.TenantScope, occurrenceID uuid.UUID, w db.TimeWindow, p db.Keyset,
) (TimelineResult, error) {
	evs, cur, err := s.events.ListByOccurrence(ctx, scope, occurrenceID, s.window(w), p)
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
	var occID *uuid.UUID
	if alert.HasOpenOccurrence() {
		occID = ptr(alert.CurrentOccurrenceID())
	}
	return s.enrichments.ListForAlert(ctx, scope, alertID, occID)
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

// DeliveryRollupForOccurrence serves the `delivery_summary` of
// `GET /occurrences/{id}` — the same question narrowed to one firing episode.
func (s *Service) DeliveryRollupForOccurrence(
	ctx context.Context, scope db.TenantScope, occurrenceID uuid.UUID,
) (DeliveryRollup, error) {
	if s.notifications == nil {
		return DeliveryRollup{}, nil
	}
	return s.notifications.DeliveryRollupForOccurrence(ctx, scope, occurrenceID)
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
	ctx context.Context, scope db.TenantScope, occurrenceID, snapshotID uuid.UUID,
) error {
	return s.tx.InTx(ctx, func(ctx context.Context) error {
		return s.occurrences.BindRuleSnapshot(ctx, scope, occurrenceID, snapshotID)
	})
}
