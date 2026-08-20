package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/tuning"
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

// ⛔ THERE IS NO `AlertProjectionWriter` PORT ANY MORE. It existed so a snooze
// could write its own mirror onto `alerts` without being able to move state —
// a narrow port around a projection that should never have existed. A snooze
// writes `alert_snoozes` and stops; the list, the card and the suppression
// decision all read that row (§B.1, §D.8b, migration 00048).

// CaseBatchReader reads the latest episode of many Alerts in one round
// trip. A 200-alert webhook must not become 200 round trips (§G.4).
type CaseBatchReader interface {
	LatestByAlerts(ctx context.Context, s db.TenantScope, alertIDs []uuid.UUID) (map[uuid.UUID]domain.Case, error)
}

// CaseSourceResolver answers which AlertSource a case came from.
//
// It exists for one caller: the §B.4 reaper guard, which must load the owning
// source's health before it may expire anything. A case absent from the
// result has no resolvable source, and the reaper reads that as "cannot prove
// healthy" and HOLDS it.
type CaseSourceResolver interface {
	SourceIDs(ctx context.Context, s db.TenantScope, caseIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
}

// ⛔ THERE IS NO `EventCounter` PORT ANY MORE. It counted lifecycle transitions per
// Alert inside `flap_window` for the `flap.score` job, and the job is retired: the
// case retention window W (migration 00057) damps a flap at case formation, so a
// damped episode appends neither of the two events the count lived on and the score
// read false exactly when the alert was flapping (ADR 0041, Amendment 1). See the
// tombstone in `sweep.go`.
//
// ⚠️ `EventRepository.StateChangeCounts` and `stateChangeCountsSQL` DELIBERATELY
// SURVIVE in `alerts/repository`: `test/arch/eventtype_test.go` registers that
// statement as one of the three SQL sites that must spell both the canonical and the
// pre-ADR-0036 form of a lifecycle type, and the query is a truthful read over
// history. What it no longer has is a consumer.

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
	// HealthyFor reports, for each named AlertSource, whether it is currently
	// healthy — one round trip for a whole sweep tick, because the guard is per
	// SOURCE and a tick's candidates share a handful of them.
	//
	// ABSENCE IS A VERDICT: a source missing from the result — never probed, not
	// this org's, unresolvable — is one the implementation cannot vouch for, and
	// the caller must read absence exactly as it reads false: not proven healthy,
	// so every case it owns is held.
	HealthyFor(ctx context.Context, s db.TenantScope, sourceIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

// The `ui_events.kind` values this module publishes (§E.4). They are plain
// strings so that the port below needs no type from `streaming/domain`, which
// depguard keeps out of `alerts`; the one-line adapter onto
// `streaming/service.Append` lives in the wiring layer.
const (
	// StreamAlertUpserted announces that an Alert row changed.
	StreamAlertUpserted = "alert.upserted"
	// StreamCaseUpserted announces that an episode changed.
	StreamCaseUpserted = "case.upserted"
	// StreamEventAppended announces a new timeline entry.
	StreamEventAppended = "event.appended"
)

// StreamFrame is one §E.4 change notice, encoded and held until a batch flush.
// The payload is the SMALL envelope of §E.4 — a change notice, not a resource.
type StreamFrame struct {
	Kind       string
	ResourceID uuid.UUID
	Payload    []byte
}

// StreamAppender publishes a user-visible change onto the SSE spine.
//
// It is called INSIDE the caller's transaction so the frame and the row it
// describes commit together: the UI can never be told about something that rolled
// back, and can never miss something that committed (§E.4).
type StreamAppender interface {
	Append(ctx context.Context, s db.TenantScope, kind string, resourceID uuid.UUID, payload []byte) error
	// AppendBatch publishes many frames in ONE round trip, in slice order — the
	// implementation must assign stream positions in that order, because the
	// observe path relies on it to keep the batched flush indistinguishable from
	// the per-frame appends it replaced. A 200-alert webhook produces hundreds of
	// frames and must not produce hundreds of round trips (§G.4).
	AppendBatch(ctx context.Context, s db.TenantScope, frames []StreamFrame) error
}

// EnrichmentReader reads the provenanced enrichment results for one subject.
//
// The read model is declared HERE, by the consumer, so that `alerts` never
// imports `enrichment`. A failed enrichment and a missing one are deliberately
// distinguishable — that is what Status is for.
type EnrichmentReader interface {
	ListForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, caseID *uuid.UUID) ([]EnrichmentSummary, error)
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
	// DeliveryRollupForCase is the same question narrowed to one episode.
	DeliveryRollupForCase(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (DeliveryRollup, error)
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
	ID uuid.UUID
	// ⛔ `GroupID uuid.UUID` WAS HERE AND IS DELETED (git-bug `7570090`). It was the
	// intent's delivery target, and it was ALSO doing duty as the subject in the DTO
	// mapper below — two questions on one field, which is the confusion the
	// `(subject_kind, subject_id)` pair exists to end.
	//
	// ⭐ `SubjectKind`/`SubjectID` are carried explicitly now instead of being
	// synthesised. The mapper used to hardcode `"alert_group"` and pass `GroupID`;
	// a Case-shaped notification has a subject that varies (`alert` when a Reason is
	// alert-scoped, `case` otherwise), so guessing it in the mapper would be wrong
	// for every acked, refired or rule_changed intent.
	SubjectKind string
	SubjectID   uuid.UUID
	AlertID     *uuid.UUID
	CaseID      *uuid.UUID
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

// ⛔ `GroupVersionReader` WAS HERE AND IS DELETED (git-bug `7570090`). It answered
// the current `state_version` of one `alert_groups` generation, and it was a PORT
// for two reasons that both expired with the table: `alert_groups` belonged to
// `grouping`, which `alerts` may not import, and the version was hashed into
// `notifications.idempotency_key` (§C.7) so that a notify job enqueued against a
// stale version could not mint a duplicate intent. There is no generation, no
// version column to read, and `internal/grouping` no longer exists to implement it.
//
// ⚠️ THE GUARANTEE IT SERVED IS STILL OWED AND IS CURRENTLY UNMET, which is why
// this note is longer than a deletion needs. A Case carries no monotonic version,
// so every `enriched` evaluation for one Case now hashes IDENTICALLY forever — the
// first amendment is minted and every later one collides on the unique index and
// is swallowed as the mechanism working (§L.9). `enrichment/service.Loaded.StateVersion`
// carries the full account with the numbers on it. Restoring this needs a version
// on `alert_cases`, which is a migration; re-adding a port here would only move the
// missing column somewhere it is harder to see.

// Settings is the per-org tuning of the lifecycle machine (`orgs.settings`,
// §D.1). Every value is a duration or a count the SPEC names; nothing here
// changes what a transition MEANS, only when it fires.
type Settings struct {
	// ResolveGrace is how long past `source_ends_at` the reaper waits before an
	// case may expire (§B.4).
	ResolveGrace time.Duration

	// ⛔⛔ `FlapThreshold` AND `FlapWindow` CONFIGURE NOTHING IN THIS MODULE ANY MORE,
	// AND THE RESOLVE GRACE IS THE WHOLE LIVE LIFECYCLE POLICY. They tuned the
	// `flap.score` job, which is RETIRED: the case retention window W damps a flap at
	// CASE FORMATION (migration 00057), which left the score counting lifecycle events
	// a damped flap does not append — false exactly when the alert was flapping (ADR
	// 0041, Amendment 1). `alerts.flap_score` / `alerts.is_flapping` keep their last
	// value and stay readable, and nothing recomputes them, so no code in `alerts`
	// reads either number.
	//
	// ⚠️ THEY SURVIVE AS THE MIRROR OF `orgs.settings`, NOT AS POLICY. The keys
	// `flap_threshold` and `flap_window_s` are still declared in `identity/domain`, and
	// `identity/domain/defaults_derivation_test.go` reads `DefaultSettings()` below to
	// prove the two copies of the SHIPPED DEFAULT cannot drift. Deleting these two
	// fields is the identity-side half of the retirement and has deliberately not been
	// taken here; when those keys go, these go with them.
	FlapThreshold int
	FlapWindow    time.Duration
}

// DefaultSettings are the §D.1 defaults, used when no SettingsReader is wired.
func DefaultSettings() Settings {
	return Settings{
		ResolveGrace: domain.DefaultResolveGrace,
		// ⚠️ RETIRED-BUT-MIRRORED (see the struct above): the shipped flap defaults,
		// referenced rather than copied so ADR 0026's window move cannot be made in one
		// place and missed in the other. Nothing in `alerts` reads them any more;
		// `identity/domain/defaults_derivation_test.go` does, to prove the copies agree.
		FlapThreshold: tuning.DefaultFlapThreshold,
		FlapWindow:    tuning.DefaultFlapWindow,
	}
}

// normalise fills any zero field from the defaults, so a partially-configured org
// cannot produce a zero grace period and a state machine that fires on every
// observation.
func (s Settings) normalise() Settings {
	d := DefaultSettings()
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
