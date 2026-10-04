package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// WHO MAY APPROVE A REMEDY (ADR 0054 §4, git-bug 47f67c8), without a database. The SQL
// half — the join, the cascade, the CLI's writes — is cmd/oto/remedyapprover_test.go's.

// memGrants is RemedyApproverReader over a map, keyed by (ToolServer, user). Revoking is
// deleting the key, exactly as `oto revoke` deletes the row.
type memGrants struct {
	rows map[[2]uuid.UUID]domain.RemedyApprover
	fail error
}

func (m *memGrants) ListForToolServer(_ context.Context, _ db.TenantScope, ts uuid.UUID) ([]domain.RemedyApprover, error) {
	out := []domain.RemedyApprover{}
	for k, g := range m.rows {
		if k[0] == ts {
			out = append(out, g)
		}
	}
	return out, m.fail
}

func (m *memGrants) Grant(_ context.Context, _ db.TenantScope, ts, user uuid.UUID) (domain.RemedyApprover, error) {
	if m.fail != nil {
		return domain.RemedyApprover{}, m.fail
	}
	g, ok := m.rows[[2]uuid.UUID{ts, user}]
	if !ok {
		return domain.RemedyApprover{}, errs.NotFound("remedy_approver_not_found", "no such remedy approver")
	}
	return g, nil
}

func TestOnlyAHolderWhoseGrantCountsMayApproveARemedy(t *testing.T) {
	ctx := context.Background()
	scope, err := db.NewTenantScope(uuid.New())
	require.NoError(t, err)
	write, other := uuid.New(), uuid.New()
	holder, stranger, disabled, revoked := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	email, err := domain.NewEmail("ada@example.test")
	require.NoError(t, err)
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	grant := func(user uuid.UUID) domain.RemedyApprover {
		return domain.RemedyApprover{UserID: user, Email: email, DisplayName: "Ada",
			GrantedAt: at, GrantedBy: domain.RemedyApproverGrantedByCLI}
	}
	off := grant(disabled)
	off.DisabledAt = &at
	grants := &memGrants{rows: map[[2]uuid.UUID]domain.RemedyApprover{
		{write, holder}:   grant(holder),
		{write, disabled}: off,
		{write, revoked}:  grant(revoked),
	}}
	svc := New(Deps{RemedyApprovers: grants})

	// `oto revoke` deletes the row.
	delete(grants.rows, [2]uuid.UUID{write, revoked})

	for _, c := range []struct {
		name       string
		toolServer uuid.UUID
		user       uuid.UUID
		want       bool
	}{
		{"a holder", write, holder, true},
		{"a holder, on ANOTHER ToolServer", other, holder, false},
		{"a non-holder", write, stranger, false},
		{"a revoked holder", write, revoked, false},
		{"a disabled holder", write, disabled, false},
		{"no user at all", write, uuid.Nil, false},
	} {
		got, err := svc.CanApproveRemedies(ctx, scope, c.toolServer, c.user)
		require.NoError(t, err, c.name)
		require.Equal(t, c.want, got, c.name)

		rerr := svc.RequireRemedyApprover(ctx, scope, c.toolServer, c.user)
		if c.want {
			require.NoError(t, rerr, c.name)
			continue
		}
		require.True(t, errs.IsKind(rerr, errs.KindForbidden), "%s: %v", c.name, rerr)
		require.Equal(t, domain.RemedyApproverRequiredCode, errs.CodeOf(rerr), c.name)
	}

	// The list shows the disabled holder, marked as not counting, rather than hiding them.
	list, err := svc.RemedyApprovers(ctx, scope, write)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

// TestTheRemedyApproverCheckFailsClosed — an unreadable grant is an error, never a yes,
// and a service built without the reader refuses rather than letting anyone approve.
func TestTheRemedyApproverCheckFailsClosed(t *testing.T) {
	ctx := context.Background()
	scope, err := db.NewTenantScope(uuid.New())
	require.NoError(t, err)

	broken := New(Deps{RemedyApprovers: &memGrants{fail: errs.New(errs.KindInternal, "boom", "boom")}})
	ok, err := broken.CanApproveRemedies(ctx, scope, uuid.New(), uuid.New())
	require.Error(t, err)
	require.False(t, ok)
	require.Error(t, broken.RequireRemedyApprover(ctx, scope, uuid.New(), uuid.New()))

	unwired := New(Deps{})
	ok, err = unwired.CanApproveRemedies(ctx, scope, uuid.New(), uuid.New())
	require.Error(t, err)
	require.False(t, ok)
	require.False(t, errs.IsKind(unwired.RequireRemedyApprover(ctx, scope, uuid.New(), uuid.New()), errs.KindForbidden),
		"an unwired reader must not look like an ordinary refusal")
}
