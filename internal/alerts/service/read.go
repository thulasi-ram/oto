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
	Cursor db.Cursor
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

	if s.lister != nil {
		alerts, cur, err := s.lister.ListSorted(ctx, scope, q.Filter, q.Sort, q.Page)
		if err != nil {
			return ListResult{}, err
		}
		return ListResult{Alerts: alerts, Cursor: cur}, nil
	}

	if q.Sort == SortFirstSeenDesc {
		return ListResult{}, errs.Validation("sort_unsupported",
			"this deployment cannot sort by -first_seen_at")
	}
	alerts, cur, err := s.alerts.List(ctx, scope, q.Filter, q.Page)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Alerts: alerts, Cursor: cur}, nil
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
