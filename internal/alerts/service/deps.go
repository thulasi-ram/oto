package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// This file holds the ports `alerts/service` declares for itself BEYOND the four
// repository interfaces §F.5 names in ports.go.
//
// Each one exists because the alerts module needs a capability it must not
// import: §I.1 binds `alerts` never to import `notification` or `channels`, and
// CONTEXT.md §5.4 binds every cross-domain call to be service → service through
// an interface declared by the CONSUMER. The consumer is this package, so the
// interfaces are here.
//
// Every one of them is OPTIONAL. A nil port degrades to the safest behaviour,
// which is what lets oto run with notifications, enrichment and grouping
// entirely disabled — the configuration the first correctness tests use (§I.1).

// TxRunner runs a unit of work inside ONE transaction.
//
// It is a port rather than a *pgxpool.Pool because a service that names pgx has
// stopped being testable without a database. `internal/alerts/repository`
// supplies the concrete.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// AlertLister is the sorted form of the §F.5 alert list.
//
// domain.AlertFilter carries no sort field while §E.3 exposes two sort keys, so
// the sort travels as its own argument. It is a plain string, from the closed set
// {"", "-last_seen_at", "-first_seen_at"}, so that the repository implementing it
// needs no type from this package.
type AlertLister interface {
	ListSorted(ctx context.Context, s db.TenantScope, f domain.AlertFilter, sort string, p db.Keyset) ([]domain.Alert, db.Cursor, error)
}

// AlertBatchReader reads many Alerts by identity key in one round trip.
//
// It serves the T2 material-change probe, which must see an Alert as it was
// BEFORE the §D.12(c) upsert overwrote its annotations and severity — after the
// upsert the question can no longer be asked.
type AlertBatchReader interface {
	GetByAlertKeys(ctx context.Context, s db.TenantScope, alertKeys []string) (map[string]domain.Alert, error)
}

// AlertProjectionWriter writes the §B.8 snooze projection and nothing else.
//
// It is separate from SetProjection because a snooze MUST NOT be able to move
// state, ack_state or severity: the three axes are independent (§B.1), and a
// method that could write all of them is a method that one day will.
type AlertProjectionWriter interface {
	SetSnoozedUntil(ctx context.Context, s db.TenantScope, alertID uuid.UUID, until *time.Time) error
}

// OccurrenceBatchReader reads the latest episode of many Alerts in one round
// trip. A 200-alert webhook must not become 200 round trips (§G.4).
type OccurrenceBatchReader interface {
	LatestByAlerts(ctx context.Context, s db.TenantScope, alertIDs []uuid.UUID) (map[uuid.UUID]domain.Occurrence, error)
}

// OccurrenceSourceResolver answers which AlertSource an occurrence came from.
//
// It exists for one caller: the §B.4 reaper guard, which must load the owning
// source's health before it may expire anything. An occurrence absent from the
// result has no resolvable source, and the reaper reads that as "cannot prove
// healthy" and HOLDS it.
type OccurrenceSourceResolver interface {
	SourceIDs(ctx context.Context, s db.TenantScope, occurrenceIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
}

// EventCounter counts lifecycle transitions per Alert in a window, for the
// `flap.score` job (§B.6).
type EventCounter interface {
	StateChangeCounts(ctx context.Context, s db.TenantScope, w db.TimeWindow) (map[uuid.UUID]int, error)
}

// SnoozeHistoryReader reads an Alert's snooze history. Membership of a snooze is
// history, not a boolean (§B.8.6).
type SnoozeHistoryReader interface {
	ListByAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, limit int) ([]domain.Snooze, error)
}

// SourceHealth is ⭐ THE REAPER GUARD (§B.4), the highest-value correctness rule
// in the system.
//
//	Losing sight of an alert is NOT the same as the alert resolving.
//
// It is implemented by `sources/service`. When it is NOT wired, or when it cannot
// answer, the sweep HOLDS every candidate — the reaper defaults to silence, never
// to a fabricated ending.
type SourceHealth interface {
	// Healthy reports whether the named AlertSource is currently healthy.
	Healthy(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (bool, error)
}

// The `ui_events.kind` values this module publishes (§E.4). They are plain
// strings so that the port below needs no type from `streaming/domain`, which
// depguard keeps out of `alerts`; the one-line adapter onto
// `streaming/service.Append` lives in the wiring layer.
const (
	// StreamAlertUpserted announces that an Alert row changed.
	StreamAlertUpserted = "alert.upserted"
	// StreamOccurrenceUpserted announces that an episode changed.
	StreamOccurrenceUpserted = "occurrence.upserted"
	// StreamEventAppended announces a new timeline entry.
	StreamEventAppended = "event.appended"
)

// StreamAppender publishes a user-visible change onto the SSE spine.
//
// It is called INSIDE the caller's transaction so the frame and the row it
// describes commit together: the UI can never be told about something that rolled
// back, and can never miss something that committed (§E.4).
type StreamAppender interface {
	Append(ctx context.Context, s db.TenantScope, kind string, resourceID uuid.UUID, payload []byte) error
}

// EnrichmentReader reads the provenanced enrichment results for one subject.
//
// The read model is declared HERE, by the consumer, so that `alerts` never
// imports `enrichment`. A failed enrichment and a missing one are deliberately
// distinguishable — that is what Status is for.
type EnrichmentReader interface {
	ListForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, occurrenceID *uuid.UUID) ([]EnrichmentSummary, error)
}

// EnrichmentSummary is the alerts-side view of one enrichment result.
type EnrichmentSummary struct {
	ID              uuid.UUID
	SubjectKind     string
	SubjectID       uuid.UUID
	Enricher        string
	EnricherVersion int
	Phase           int
	// Status is ok | partial | skipped | failed | timeout. A failure is RECORDED,
	// never discarded.
	Status     string
	Payload    map[string]any
	Warnings   []string
	Error      string
	DurationMS int
	FromCache  bool
	ComputedAt time.Time
	ExpiresAt  *time.Time
}

// NotificationReader answers "was anybody told about this alert, and did it
// land".
//
// ⛔ It is a PORT and not an import. SPEC §I.1 binds `alerts` never to import
// `notification`: alerts appends events and enqueues jobs, and notification
// subscribes. That is what makes it possible to run oto with notifications
// entirely disabled, which is how the first correctness tests run.
type NotificationReader interface {
	ListForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset) ([]NotificationSummary, db.Cursor, error)
	// DeliveryRollupForAlert counts every delivery anybody made about this alert
	// — the intents that name the alert AND the intents about the group
	// generations it has been part of, because oto notifies about generations.
	DeliveryRollupForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (DeliveryRollup, error)
	// DeliveryRollupForOccurrence is the same question narrowed to one episode.
	DeliveryRollupForOccurrence(ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID) (DeliveryRollup, error)
}

// DeliveryRollup is the alerts-side view of one subject's fan-out health.
//
// ⭐ IT IS THE FIELD THAT STOPS OTO'S SILENCE FROM LOOKING LIKE "NO ALERT".
// `delivery_summary` was declared on four detail responses and emitted by none of
// them; being optional in the schema, every validator passed and the absence was
// invisible. A user who sees no Slack message must be able to tell "nothing
// fired" from "four deliveries died", and nothing else on the page can say it.
type DeliveryRollup struct {
	Total   int
	Sent    int
	Failed  int
	Dead    int
	Skipped int
	Pending int
	// LastErrorClass turns "one died" into "one died because the token expired".
	LastErrorClass string
	// LastSentAt is when anything about this subject last reached a destination.
	LastSentAt *time.Time
}

// NotificationSummary is the alerts-side view of one notification intent and the
// health of its fan-out.
//
// Delivery failure must be VISIBLE PER ALERT: oto's silence must never be
// indistinguishable from "no alert" (CONTEXT.md §6).
// ⛔ EVERY FIELD HERE IS EITHER SUPPLIED OR ABSENT — NEVER SUBSTITUTED. A
// projection that reports a value it did not read is worse than one that reports
// nothing, because the caller cannot tell the two apart. `UpdatedAt` used to be
// rendered as `CreatedAt`, which made "this intent has never changed" and "this
// intent changed a minute ago" indistinguishable on the wire.
type NotificationSummary struct {
	ID           uuid.UUID
	GroupID      uuid.UUID
	AlertID      *uuid.UUID
	OccurrenceID *uuid.UUID
	// PolicyID is the notification_policy that routed this intent. It is nil when
	// no policy matched — which is itself a fact worth showing, and is why
	// SuppressedReason has a `no_policy` value.
	PolicyID *uuid.UUID
	Reason   string
	Status   string
	// SuppressedReason is oto's OWN vocabulary — no_policy, throttled, storm,
	// flapping, snoozed, verbosity, channel_disabled, duplicate_render — and never
	// Alertmanager's four suppression reasons (§B.8.2).
	SuppressedReason string
	StateVersion     int
	CreatedAt        time.Time
	// UpdatedAt is the last time the intent or its delivery roll-up moved. It is
	// ZERO when the producer does not track one, and the mapper renders zero as
	// JSON `null` rather than as CreatedAt.
	UpdatedAt time.Time

	DeliveriesTotal  int
	DeliveriesSent   int
	DeliveriesFailed int
	DeliveriesDead   int
	// DeliveriesSkipped is a coalesced no-op update — the destination already
	// shows exactly this content. It is NOT a failure and it is NOT silence.
	//
	// A producer that folds skipped into DeliveriesSent (as the v1 notification
	// read model does) leaves this zero, and zero is then an honest "none
	// separately recorded" rather than an invented breakdown.
	DeliveriesSkipped int
	// DeliveriesPending is queued plus in-flight. A producer that leaves it zero
	// gets the documented derivation Total-Sent-Failed-Dead, which is exact
	// because those four plus pending exhaust the delivery states.
	DeliveriesPending int
}

// GroupVersionReader answers the current `state_version` of one AlertGroup
// generation.
//
// It is a port because `alert_groups` belongs to `grouping`, and because
// `notifications.idempotency_key` hashes the version (§C.7): a notify job
// enqueued against the wrong version would mint a duplicate intent. When the
// port is not wired the job carries version 0 and the notify worker resolves it
// at evaluation time.
type GroupVersionReader interface {
	StateVersion(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (int, error)
}

// Settings is the per-org tuning of the lifecycle machine (`orgs.settings`,
// §D.1). Every value is a duration or a count the SPEC names; nothing here
// changes what a transition MEANS, only when it fires.
type Settings struct {
	// RefireGrace decides T8 from T7: a re-fire inside the window reopens the
	// existing occurrence, one after it opens a new episode (§B.5).
	RefireGrace time.Duration
	// ResolveGrace is how long past `source_ends_at` the reaper waits before an
	// occurrence may expire (§B.4).
	ResolveGrace time.Duration
	// FlapThreshold is the transition count above which an Alert is marked
	// flapping. Flapping is a VISIBLE state, never silent suppression (§B.6).
	FlapThreshold int
	// FlapWindow is the window FlapThreshold is counted over.
	FlapWindow time.Duration
}

// DefaultSettings are the §D.1 defaults, used when no SettingsReader is wired.
func DefaultSettings() Settings {
	return Settings{
		RefireGrace:   domain.DefaultRefireGrace,
		ResolveGrace:  domain.DefaultResolveGrace,
		FlapThreshold: 5,
		FlapWindow:    30 * time.Minute,
	}
}

// normalise fills any zero field from the defaults, so a partially-configured org
// cannot produce a zero grace period and a state machine that fires on every
// observation.
func (s Settings) normalise() Settings {
	d := DefaultSettings()
	if s.RefireGrace <= 0 {
		s.RefireGrace = d.RefireGrace
	}
	if s.ResolveGrace <= 0 {
		s.ResolveGrace = d.ResolveGrace
	}
	if s.FlapThreshold <= 0 {
		s.FlapThreshold = d.FlapThreshold
	}
	if s.FlapWindow <= 0 {
		s.FlapWindow = d.FlapWindow
	}
	return s
}

// SettingsReader reads one org's lifecycle tuning. Implemented by
// `identity/service`, which owns `orgs`.
type SettingsReader interface {
	Lifecycle(ctx context.Context, s db.TenantScope) (Settings, error)
}
