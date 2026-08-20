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

// ⛔ IT WAS `SELECT org_id FROM alert_groups WHERE id = $1` (git-bug `7570090`,
// migration `00069`). A conversation is a Case, so the id a notify job carries is a
// Case id and `alert_cases` is the table that owns it.
const orgOfCaseSQL = `SELECT org_id FROM alert_cases WHERE id = $1`

// ForCase resolves the tenant that owns a Case.
//
// ⭐ THIS IS THE ONE READ THAT RUNS BEFORE A TENANT SCOPE EXISTS, which is why it is
// a bare `WHERE id = $1` with no `org_id` predicate — there is no org to predicate on
// yet. Its answer is what every subsequent query is scoped BY, so it is also the one
// place a wrong row would silently cross a tenant boundary; the id comes from oto's
// own job args and never from a request.
func (r *ScopeResolver) ForCase(ctx context.Context, caseID uuid.UUID) (db.TenantScope, error) {
	var orgID uuid.UUID
	if err := r.db(ctx).QueryRow(ctx, orgOfCaseSQL, caseID).Scan(&orgID); err != nil {
		return db.TenantScope{}, mapErr(err, "case_not_found", "alert case")
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
