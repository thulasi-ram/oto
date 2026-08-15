package db

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// RequireScope refuses a scope that names no tenant. A missing org_id predicate
// is a data leak, so it is refused here rather than defended against downstream.
// A scope can only be minted by platform/authn from an authenticated principal,
// so one that names no tenant is a programmer error, not an authorization
// decision — which is why this is Internal and never Forbidden.
func RequireScope(s TenantScope) error {
	if !s.Valid() {
		return errs.Internal("missing_tenant_scope", ErrNoTenant)
	}
	return nil
}

// RequireID refuses a zero UUID reaching a NOT NULL column. §L.9(1): catch a
// mapper bug at the boundary rather than as an opaque 23502.
func RequireID(field string, id uuid.UUID) error {
	if id == uuid.Nil {
		return errs.Internal("missing_"+field, fmt.Errorf("repository: %s is required", field))
	}
	return nil
}
