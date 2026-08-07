package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// HistoryStore reads the notification history of one alert.
type HistoryStore interface {
	ListForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID, page db.Keyset) ([]repository.Summary, db.Cursor, error)
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
