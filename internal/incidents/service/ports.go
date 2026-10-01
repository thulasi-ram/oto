package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// The ports this module declares for itself (ADR 0002, CONTEXT.md §5.4).
// `internal/app` satisfies each one; nothing here names a concrete.

// Repository is the storage this service writes and reads through, satisfied by
// `incidents/repository.IncidentRepository`.
type Repository interface {
	List(ctx context.Context, s db.TenantScope, p db.Keyset) ([]domain.Incident, db.Cursor, error)
	Get(ctx context.Context, s db.TenantScope, number int64) (domain.Detail, error)
	GetByID(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Detail, error)
	Ref(ctx context.Context, s db.TenantScope, number int64) (domain.Ref, error)
	Lock(ctx context.Context, s db.TenantScope, ids []uuid.UUID) error
	OpenMembers(ctx context.Context, s db.TenantScope, incidentID uuid.UUID) (int, error)
	Holding(ctx context.Context, s db.TenantScope, caseIDs []uuid.UUID) ([]uuid.UUID, error)
	Cases(ctx context.Context, s db.TenantScope, ids []uuid.UUID) (map[uuid.UUID]domain.CaseRef, error)
	LiveMemberships(ctx context.Context, s db.TenantScope, caseIDs []uuid.UUID) (map[uuid.UUID]domain.Ref, error)
	ConversationHolding(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (domain.Ref, bool, error)
	Insert(ctx context.Context, s db.TenantScope, at time.Time, by domain.Attribution) (domain.Ref, error)
	AddMember(ctx context.Context, s db.TenantScope, incidentID, caseID uuid.UUID, at time.Time, by domain.Attribution) error
	RemoveMember(ctx context.Context, s db.TenantScope, incidentID, caseID uuid.UUID,
		at time.Time, by domain.Attribution, movedTo uuid.UUID) error
}

// TxRunner is the unit of work: everything a verb writes, including the
// timeline fact, commits together.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// CaseFact is one membership sentence for ONE member Case's timeline.
//
// The type is the kernel's closed `EventType`, so a typo is a compile error
// rather than a refused append at the moment the fact happened.
type CaseFact struct {
	Type    kernel.EventType
	CaseID  uuid.UUID
	AlertID uuid.UUID
	Summary string
	Payload map[string]any
	By      domain.Attribution
	// CorrelatorName is the deciding Correlator's name when By is a Correlator, so
	// the timeline can say WHICH one — "drawn by Correlator payments-storm" — rather
	// than "by a machine". Frozen into the event as its actor label, the way a
	// human's display name is; empty for a human.
	CorrelatorName string
}

// Timeline appends a CaseFact to `alert_events`, inside the caller's transaction.
//
// ⛔ THIS MODULE MAY NOT IMPORT `alerts/service` FOR IT. CONTEXT.md §4 draws no
// `incidents ──► alerts` edge, and a narration is not a reason to grow one, so the
// port is declared here and `internal/app`'s timelineRecorder — the adapter that
// already narrates for `rules` and `enrichment` over `AppendTimelineEvent` —
// satisfies it. One writer of request-shaped events, three doors onto it.
type Timeline interface {
	RecordIncidentFact(ctx context.Context, s db.TenantScope, f CaseFact) error
}

// Announcement is one Incident fact to declare outbound (ADR 0052 §5).
type Announcement struct {
	IncidentID uuid.UUID
	Fact       domain.Fact
	// Occasion is WHICH TIME this fact happened, minted in the transaction that made
	// it true. An Incident has no version, so this is what keeps one Case added,
	// removed and added again from collapsing into one notification.
	Occasion uuid.UUID
}

// Announcer hands Incident facts to the notification layer, INSIDE the caller's
// transaction (ADR 0001's outbox): a membership change and the job that declares
// it commit together, so a fact can neither be lost by a crash after the commit nor
// sent for a change that rolled back.
//
// ⛔ THIS MODULE MAY NOT IMPORT `notification` FOR IT. `internal/app` satisfies the
// port by enqueueing `notify.incident`; whether any policy routes the fact
// anywhere is entirely the notification layer's decision, and an org with no such
// policy sends nothing.
type Announcer interface {
	Announce(ctx context.Context, s db.TenantScope, facts []Announcement) error
}

// CorrelatorStore is the storage the Correlator path reads and writes through,
// satisfied by `incidents/repository.CorrelatorRepository` (migration 00085).
// Membership itself is still written through Repository: one writer of
// `incident_members`, whoever decided.
type CorrelatorStore interface {
	List(ctx context.Context, s db.TenantScope) ([]domain.Correlator, error)
	Live(ctx context.Context, s db.TenantScope) ([]domain.Correlator, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Correlator, error)
	Lock(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Correlator, error)
	Create(ctx context.Context, s db.TenantScope, c domain.Correlator, at time.Time) (domain.Correlator, error)
	Update(ctx context.Context, s db.TenantScope, c domain.Correlator, at time.Time) (domain.Correlator, error)
	Delete(ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time) error

	CorrelationCase(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (domain.CorrelationCase, error)
	RecordMatch(ctx context.Context, s db.TenantScope, correlatorID uuid.UUID, c domain.CorrelationCase, at time.Time) error
	WindowCandidates(ctx context.Context, s db.TenantScope, correlatorID uuid.UUID,
		from, to time.Time, except uuid.UUID) ([]domain.CaseAt, error)
	LatestDrawn(ctx context.Context, s db.TenantScope, correlatorID uuid.UUID) (domain.CorrelatorIncident, bool, error)
}
