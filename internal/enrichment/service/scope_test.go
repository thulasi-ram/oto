package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// WithScope/ScopeFrom exist because domain.Enricher.Enrich(ctx, *Subject) has no
// scope parameter and MUST NOT gain one. The Subject carries OrgID as a STRING
// for display; it is not, and must never be treated as, authorisation.

func TestScopeRoundTripsThroughTheContext(t *testing.T) {
	t.Parallel()

	orgID := id.New()
	scope, err := db.NewTenantScope(orgID)
	require.NoError(t, err)

	got, err := service.ScopeFrom(service.WithScope(context.Background(), scope))
	require.NoError(t, err)
	assert.Equal(t, orgID, got.OrgID())
	assert.True(t, got.Valid())
}

// TestAnEnricherWithoutAScopeMustFailRatherThanQueryUnscoped: an unscoped query
// in a multi-tenant table is a cross-tenant read, so the error is not
// decorative.
func TestAnEnricherWithoutAScopeMustFailRatherThanQueryUnscoped(t *testing.T) {
	t.Parallel()

	_, err := service.ScopeFrom(context.Background())
	require.Error(t, err)
	assert.Equal(t, "enrichment_no_tenant_scope", errs.CodeOf(err))
	assert.Equal(t, errs.KindInternal, errs.KindOf(err))
}

// TestAnInvalidScopeInTheContextIsNoScopeAtAll: a zero TenantScope is not a
// tenant, and finding one in the context is worse than finding none.
func TestAnInvalidScopeInTheContextIsNoScopeAtAll(t *testing.T) {
	t.Parallel()

	ctx := service.WithScope(context.Background(), db.TenantScope{})

	_, err := service.ScopeFrom(ctx)
	require.Error(t, err)
	assert.Equal(t, "enrichment_no_tenant_scope", errs.CodeOf(err))
}
