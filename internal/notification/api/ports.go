package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/db"
)

// Every interface in this file is a PORT DECLARED BY THE CONSUMER (CONTEXT.md
// §5.4). This layer says exactly what it calls; `internal/app/container.go`
// decides what satisfies it.

// PolicyStore is the settings-side persistence of notification policies,
// satisfied by `*notification/repository.ConfigRepository`.
//
// ⚠️ ARCHITECTURAL NOTE. `notification/service.PolicyService` is deliberately
// read-only — it EVALUATES policies and giving the evaluator a write path would
// hand the thing that reads the rules a way to change them. The settings write
// path is therefore its own port, injected from the composition root. `api` still
// does not IMPORT `repository`, so depguard's rule holds.
//
// ⛔ AND `CreatePolicy` IS NO LONGER ON IT. See PolicyCreator.
type PolicyStore interface {
	ListPolicies(ctx context.Context, s db.TenantScope, p db.Keyset) ([]domain.Policy, db.Cursor, error)
	GetPolicy(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Policy, error)
	UpdatePolicy(ctx context.Context, s db.TenantScope, id uuid.UUID, p domain.PolicyPatch) (domain.Policy, error)
	SoftDeletePolicy(ctx context.Context, s db.TenantScope, id uuid.UUID) error
}

// PolicyCreator registers a routing policy, satisfied by
// `*notification/service.PolicyWriter`.
//
// ⭐⭐ IT IS A SEPARATE PORT BECAUSE IT IS SATISFIED BY A DIFFERENT LAYER. An
// `Idempotency-Key` claim has to join the insert's own transaction, so a handler
// wired straight to the repository had nowhere to take one — which is why
// `createNotificationPolicy` answered a same-body retry with a `policies_name_uniq`
// conflict naming nothing rather than with the policy the caller already made.
// It is NOT `PolicyService`: that one evaluates policies, and the evaluator must
// not gain a way to change the rules it reads.
type PolicyCreator interface {
	CreatePolicy(ctx context.Context, s db.TenantScope, in domain.PolicyDraft, idem service.Idempotency) (domain.Policy, error)
}

// Compile-time proof that the writer satisfies the port this layer declares.
var _ PolicyCreator = (*service.PolicyWriter)(nil)

// AuditStore serves the two read-only audit lists and the delivery retry,
// satisfied by the same `ConfigRepository`.
type AuditStore interface {
	ListNotifications(ctx context.Context, s db.TenantScope, f domain.NotificationFilter, p db.Keyset) ([]domain.Notification, db.Cursor, error)
	ListDeliveries(ctx context.Context, s db.TenantScope, f domain.DeliveryFilter, p db.Keyset) ([]domain.Delivery, map[uuid.UUID]domain.DeliveryContext, db.Cursor, error)
	DeliveriesFor(ctx context.Context, s db.TenantScope, notificationID uuid.UUID) ([]domain.Delivery, error)
	ChannelContextFor(ctx context.Context, s db.TenantScope, channelIDs []uuid.UUID) (map[uuid.UUID]domain.DeliveryContext, error)
	// RequeueDead re-queues a delivery that has been given up on. The second
	// result is false when the row was not `dead`, which the handler turns into a
	// `412`: pending and failed deliveries are already on their own backoff
	// schedule and nudging one would double-send.
	RequeueDead(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Delivery, bool, error)
}

// NotificationReader reads one intent, satisfied by
// `*notification/repository.NotificationRepository`.
type NotificationReader interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Notification, error)
}

// DeliveryReader reads one materialisation, satisfied by
// `*notification/repository.DeliveryRepository`.
type DeliveryReader interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Delivery, error)
}

// Previewer runs the REAL policy matcher without creating anything, satisfied by
// `*notification/service.PolicyService`.
//
// ⛔ IT SENDS NOTHING, WRITES NO ROW AND ENQUEUES NO JOB. That is the whole
// contract: an operator must be able to ask "where would this go?" of a
// production system, repeatedly, at no cost and with no risk of a message
// appearing in a channel.
type Previewer interface {
	Preview(ctx context.Context, s db.TenantScope, req service.PreviewRequest) (service.Preview, error)
}

// Compile-time proof that the policy service satisfies the port this layer
// declares.
var _ Previewer = (*service.PolicyService)(nil)

// ViewBuilder projects the world into the renderer's read model, satisfied by
// `*notification/service.ViewService`.
//
// The preview uses it for TWO things: the group labels the matcher runs against,
// and the view the renderer turns into the exact payload that would be sent. Both
// come from one read, so the preview cannot show a card built from labels other
// than the ones it matched on.
type ViewBuilder interface {
	Build(ctx context.Context, s db.TenantScope, req service.ViewRequest) (*service.NotificationView, error)
}

// Compile-time proof that the view service satisfies the port this layer
// declares.
var _ ViewBuilder = (*service.ViewService)(nil)

// RendererSource resolves the real Renderer for a destination.
//
// It is the SAME renderer a delivery uses, which is what makes the preview's
// `rendered` payload trustworthy: a preview rendered by a simplified path would
// answer a question nobody asked.
//
// It is OPTIONAL. When it is absent the preview still answers the important
// question — who is told, where, and what would suppress it — and simply omits
// the payload, which the contract types as nullable.
type RendererSource interface {
	Renderer(t service.ProviderType, id service.RendererID) (service.MessageRenderer, error)
}

// SubjectResolver maps an alert or occurrence onto the group generation whose
// card would carry the fact.
//
// It exists because routing is about the GROUP: the thing being routed is the
// group's card, and routing two members of one group to two different channels
// would split one conversation across two rooms. A preview asked about an alert
// therefore has to resolve that alert's group before it can answer.
//
// It is OPTIONAL. Without it, `alert_id` and `occurrence_id` are refused with a
// field violation telling the caller to supply `group_id`, which is honest — far
// better than previewing against the wrong subject.
type SubjectResolver interface {
	GroupIDForAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID) (uuid.UUID, error)
	GroupIDForOccurrence(ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID) (uuid.UUID, error)
}

// Requeuer re-enqueues the dispatch job for a manually retried delivery.
//
// The row transition alone is not enough: `RequeueDead` moves the delivery back
// to `pending`, but nothing would pick it up until the retry sweep noticed. This
// port makes the retry immediate, which is what an operator who has just rotated
// a revoked token is asking for.
//
// It is OPTIONAL: without it the delivery still becomes `pending` and the sweep
// collects it on its own schedule.
type Requeuer interface {
	Enqueue(ctx context.Context, args db.JobArgs, opts ...db.JobOption) (db.EnqueueResult, error)
}
