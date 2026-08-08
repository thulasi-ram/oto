package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// HistoryStore reads the notification history of one alert, and the delivery
// roll-up of one alert, occurrence or group generation.
type HistoryStore interface {
	ListForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, page db.Keyset) ([]repository.Summary, db.Cursor, error)
	DeliveryRollupFor(ctx context.Context, s db.TenantScope, subject repository.RollupSubject, id uuid.UUID) (repository.DeliveryRollup, error)
}

// HistoryService answers "was anybody told about this alert, and did it land?"
//
// It exists as its own service because the answer is READ BY THE ALERT PAGE, not
// by the notification settings screen. `alerts/service` declares a
// consumer-side port for exactly this shape and deliberately does not import
// `notification` — alerts appends events and enqueues jobs, notification
// subscribes, and that direction is what lets oto run with notifications
// entirely disabled.
//
// The wiring in internal/app supplies the adapter between this service and that
// port; the two summary shapes are field-for-field identical so the adapter is a
// struct copy.
type HistoryService struct {
	store HistoryStore
}

// NewHistoryService builds the service.
func NewHistoryService(store HistoryStore) (*HistoryService, error) {
	if store == nil {
		return nil, errs.New(errs.KindInternal, "history_service_deps",
			"a history store is required")
	}
	return &HistoryService{store: store}, nil
}

// ListForAlert returns one alert's notification history, newest first.
//
// A SUPPRESSED NOTIFICATION IS IN THIS LIST. That is the point of the method: an
// operator asking "why did nobody hear about this?" gets a row with a reason —
// `snoozed`, `throttled`, `no_policy` — instead of an empty page they have to
// interpret. Filtering suppressions out here would recreate exactly the silence
// §B.6 forbids, one layer higher up.
func (s *HistoryService) ListForAlert(
	ctx context.Context, scope db.TenantScope, alertID uuid.UUID, page db.Keyset,
) ([]repository.Summary, db.Cursor, error) {
	if alertID == uuid.Nil {
		return nil, db.Cursor{}, errs.Validation("alert_required",
			"a notification history is about one alert",
			errs.Violation{Field: "alert_id", Code: "required", Message: "an alert id is required"})
	}
	return s.store.ListForAlert(ctx, scope, alertID, page)
}

// DeliveryRollup answers "was anybody told about this, and did it land?" for one
// alert, one occurrence or one group generation.
//
// ⭐ IT IS WHAT STOPS OTO'S SILENCE FROM LOOKING LIKE "NO ALERT". A user who sees
// nothing in Slack has two very different situations to tell apart — nothing
// fired, or four deliveries died — and from outside the database they look
// identical. The four detail responses that declare `delivery_summary` all get
// their counts from here.
//
// A subject with no deliveries returns ZEROES, which is an answer and not an
// absence: a suppressed notification, an alert no policy matched, and a
// deployment with notifications wired out all legitimately produce it, and the
// status beside it says which.
func (s *HistoryService) DeliveryRollup(
	ctx context.Context, scope db.TenantScope,
	subject repository.RollupSubject, id uuid.UUID,
) (repository.DeliveryRollup, error) {
	if id == uuid.Nil {
		return repository.DeliveryRollup{}, errs.Validation("subject_required",
			"a delivery roll-up is about one subject",
			errs.Violation{Field: "id", Code: "required", Message: "a subject id is required"})
	}
	return s.store.DeliveryRollupFor(ctx, scope, subject, id)
}
