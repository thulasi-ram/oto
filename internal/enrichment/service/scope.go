package service

import (
	"context"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

type scopeKey struct{}

// WithScope puts the caller's TenantScope into ctx.
//
// It exists because domain.Enricher.Enrich(ctx, *Subject) has no scope
// parameter and MUST NOT gain one — §F.3 is a fixed port. An enricher that
// talks to storage still has to prove which tenant it is acting for, so the
// pipeline places the scope it was called with into the context it hands down,
// and an enricher's own ports take it back out. The Subject carries OrgID as a
// STRING for display; it is not, and must never be treated as, authorisation.
func WithScope(ctx context.Context, s db.TenantScope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom returns the TenantScope travelling in ctx.
//
// The error is not decorative: an enricher that cannot find a scope must fail
// its own result rather than fall back to an unscoped query, because an
// unscoped query in a multi-tenant table is a cross-tenant read.
func ScopeFrom(ctx context.Context) (db.TenantScope, error) {
	s, ok := ctx.Value(scopeKey{}).(db.TenantScope)
	if !ok || !s.Valid() {
		return db.TenantScope{}, errs.New(errs.KindInternal, "enrichment_no_tenant_scope",
			"this enricher was called without a tenant scope in its context")
	}
	return s, nil
}
