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
	Ref(ctx context.Context, s db.TenantScope, number int64) (domain.Ref, error)
	Cases(ctx context.Context, s db.TenantScope, ids []uuid.UUID) (map[uuid.UUID]domain.CaseRef, error)
	LiveMemberships(ctx context.Context, s db.TenantScope, caseIDs []uuid.UUID) (map[uuid.UUID]domain.Ref, error)
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
