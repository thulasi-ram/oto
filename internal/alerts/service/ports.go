package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// This file is SPEC §F.5 and §F.5.2: the repository ports the alerts service
// declares FOR ITSELF. The parameter and result types live in
// `internal/alerts/domain` (contracts.go, snooze.go); only the interfaces live
// here, because §F.5's binding rule is that a repository interface is declared
// by its CONSUMER and implemented in `internal/alerts/repository`.
//
// SnoozeRepository appears in §F.5.2's code block alongside its parameter types,
// but its methods take `db.TenantScope`, and `internal/*/domain` may not import
// `platform/db` (CONTEXT.md §5.2, enforced by depguard). It is declared here with
// the other three, which is also where §F.5.2's own prose puts it: "declared by
// alerts/service, implemented in alerts/repository".
//
// The binding rules, from §F.5:
//
//  1. `ctx` first, `db.TenantScope` second, always. TenantScope has an unexported
//     field and can only be built from an authenticated principal, so there is no
//     repository method that can forget which tenant it is in.
//  2. Return domain types. Never row types, never pgx types.
//  3. List methods take a `db.Keyset` and return a `db.Cursor`. THERE IS NO
//     OFFSET IN THIS CODEBASE.
//  4. No repository method calls another domain's repository. Cross-domain access
//     is service -> service, through an interface declared by the consumer.
//  5. A method that participates in a caller's transaction takes it from `ctx`
//     via `db.FromContext(ctx)`. There are no `WithTx(tx)` variants.
//
// And one from CONTEXT.md §5b: the repository NEVER validates a business rule —
// that is the service's job — but it does reject malformed row models and it is
// the single place SQLSTATEs become `errs.Kind`.

// AlertRepository owns `alerts`. All writes are ON CONFLICT upserts (C.2): dedup
// is enforced by alerts_key_uniq, never by a read-then-write check.
type AlertRepository interface {
	UpsertBatch(ctx context.Context, s db.TenantScope, in []domain.AlertUpsert) ([]domain.AlertUpsertResult, error)
	GetByID(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Alert, error)
	GetByAlertKey(ctx context.Context, s db.TenantScope, alertKey string) (domain.Alert, error)
	List(ctx context.Context, s db.TenantScope, f domain.AlertFilter, p db.Keyset) ([]domain.Alert, db.Cursor, error)
	SetProjection(ctx context.Context, s db.TenantScope, alertID uuid.UUID, p domain.AlertProjection) error
	SetFlap(ctx context.Context, s db.TenantScope, alertID uuid.UUID, score float32, flapping bool) error
	// The two discovery reads return the alert COUNT alongside each name and
	// value. The contract has declared `alert_count` on both DTOs since the
	// first draft; returning a bare []string is what left it permanently absent.
	DistinctLabelNames(ctx context.Context, s db.TenantScope, prefix string, limit int) ([]domain.LabelCount, error)
	DistinctLabelValues(ctx context.Context, s db.TenantScope, name, prefix string, limit int) ([]domain.LabelCount, error)
	// Rollup is §E.3a: the alert list aggregated onto one axis, honouring EVERY
	// filter the list itself honours. Keyset position is the bucket key, which is
	// a total order because a bucket key appears exactly once.
	Rollup(ctx context.Context, s db.TenantScope, f domain.AlertFilter, key domain.RollupKey, after string, limit int) ([]domain.AlertRollup, bool, error)
}

// OccurrenceRepository owns `alert_occurrences` — the table the authoritative
// §B.3 state machine runs on.
type OccurrenceRepository interface {
	OpenOccurrence(ctx context.Context, s db.TenantScope, in domain.OpenOccurrence) (domain.Occurrence, error)
	GetOpenByAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (domain.Occurrence, bool, error)
	GetByID(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Occurrence, error)
	GetLatestByAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (domain.Occurrence, bool, error)
	ListByAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset) ([]domain.Occurrence, db.Cursor, error)
	Observe(ctx context.Context, s db.TenantScope, id uuid.UUID, o domain.Observation) error
	Transition(ctx context.Context, s db.TenantScope, id uuid.UUID, t domain.Transition) error
	SetAck(ctx context.Context, s db.TenantScope, id uuid.UUID, a domain.AckChange) error
	BindRuleSnapshot(ctx context.Context, s db.TenantScope, id, snapshotID uuid.UUID) error
	// ReapCandidates feeds T6. THE REAPER GUARD (§B.4) IS THE CALLER'S: an
	// occurrence whose AlertSource is not healthy is HELD, never expired. Losing
	// sight of an alert is not the same as the alert resolving.
	ReapCandidates(ctx context.Context, s db.TenantScope, before time.Time, limit int) ([]domain.Occurrence, error)
}

// EventRepository is APPEND ONLY. There is no Update and there is no Delete —
// alert_events is the truth and everything else is a projection. Events age out
// by dropping a partition, never by a statement.
type EventRepository interface {
	// Append returns written=false when the C.8 dedupe key had already been
	// recorded, which is the idempotency mechanism working, not an error.
	Append(ctx context.Context, s db.TenantScope, e domain.Event) (domain.Event, bool, error)
	AppendBatch(ctx context.Context, s db.TenantScope, e []domain.Event) (int, error)
	// The db.TimeWindow is REQUIRED on every event query: recorded_at is the
	// partition key, and an unbounded event query scans thirteen months.
	ListByAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, w db.TimeWindow, p db.Keyset) ([]domain.Event, db.Cursor, error)
	ListByOccurrence(ctx context.Context, s db.TenantScope, occID uuid.UUID, w db.TimeWindow, p db.Keyset) ([]domain.Event, db.Cursor, error)
	ListByGroup(ctx context.Context, s db.TenantScope, groupID uuid.UUID, w db.TimeWindow, p db.Keyset) ([]domain.Event, db.Cursor, error)
}

// SnoozeRepository owns `alert_snoozes` (§B.8, §F.5.2).
//
// "Exactly one active snooze per alert" is enforced by the partial unique index
// alert_snoozes_active_idx, NOT by application code: Create is expected to run in
// the same transaction as the End that supersedes the previous one.
type SnoozeRepository interface {
	Create(ctx context.Context, s db.TenantScope, in domain.SnoozeRequest) (domain.Snooze, error)
	GetActive(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (domain.Snooze, bool, error)
	// ActiveByAlerts is GetActive for a whole page of alerts in one round trip.
	// It is what keeps `AlertDTO.snooze` off the N+1 path: the list renders the
	// countdown badge for two hundred rows in ONE extra query, or in none at all
	// when the page holds nothing snoozed.
	ActiveByAlerts(ctx context.Context, s db.TenantScope, alertIDs []uuid.UUID) (map[uuid.UUID]domain.Snooze, error)
	// ListActive is the ORG-WIDE §B.8.6 view: every quiet period in force,
	// soonest wake-up first. It is what the persistent banner is built from, and
	// it is a different question from `GET /alerts?snoozed=true` — that pages
	// alerts and can never say who asked, why, or until when.
	ListActive(ctx context.Context, s db.TenantScope, now time.Time, p db.Keyset) ([]domain.Snooze, db.Cursor, error)
	End(ctx context.Context, s db.TenantScope, in domain.SnoozeEnd) (domain.Snooze, error)
	// ExpiredCandidates feeds the 60-second `snooze.expire` job (§B.8.3, §G.3).
	ExpiredCandidates(ctx context.Context, s db.TenantScope, before time.Time, limit int) ([]domain.Snooze, error)
}
