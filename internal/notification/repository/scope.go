package repository

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
)

// ScopeResolver derives a TenantScope from a job payload.
//
// ⚠ THESE ARE THE ONLY TWO QUERIES IN THIS MODULE THAT ARE NOT ORG-SCOPED, and
// they are the queries that PRODUCE the scope, so they cannot be. Everything
// after them takes a db.TenantScope, which is unforgeable outside platform/db —
// that is the property that makes "no query forgets its org_id" checkable rather
// than aspirational.
//
// They exist because SPEC §G.3's job payloads name a group or a delivery and not
// an org. A worker cannot take the tenant on trust from a payload anyway: a job
// row is data, and data that decided its own authorisation would be the whole
// tenancy boundary undone. Resolving the org from the SUBJECT means the scope is
// always the one the row actually belongs to.
type ScopeResolver struct {
	q db.Querier
}

// NewScopeResolver builds the resolver over a fallback querier.
func NewScopeResolver(q db.Querier) *ScopeResolver { return &ScopeResolver{q: q} }

func (r *ScopeResolver) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const orgOfGroupSQL = `SELECT org_id FROM alert_groups WHERE id = $1`

// ForGroup resolves the tenant that owns an alert group.
func (r *ScopeResolver) ForGroup(ctx context.Context, groupID uuid.UUID) (db.TenantScope, error) {
	var orgID uuid.UUID
	if err := r.db(ctx).QueryRow(ctx, orgOfGroupSQL, groupID).Scan(&orgID); err != nil {
		return db.TenantScope{}, mapErr(err, "group_not_found", "alert group")
	}
	return db.NewTenantScope(orgID)
}

const orgOfDeliverySQL = `SELECT org_id FROM notification_deliveries WHERE id = $1`

// ForDelivery resolves the tenant that owns a delivery.
func (r *ScopeResolver) ForDelivery(ctx context.Context, deliveryID uuid.UUID) (db.TenantScope, error) {
	var orgID uuid.UUID
	if err := r.db(ctx).QueryRow(ctx, orgOfDeliverySQL, deliveryID).Scan(&orgID); err != nil {
		return db.TenantScope{}, mapErr(err, "delivery_not_found", "delivery")
	}
	return db.NewTenantScope(orgID)
}
