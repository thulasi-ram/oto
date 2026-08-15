package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// orgStore is an in-memory OrgReader. It counts reads, which is what the
// no-stale-cache test asserts on.
type orgStore struct {
	org   domain.Org
	reads int
}

func (o *orgStore) Get(context.Context, db.TenantScope) (domain.Org, error) {
	o.reads++
	return o.org, nil
}

func (o *orgStore) UpdateSettings(
	_ context.Context, _ db.TenantScope, p domain.SettingsPatch,
) (domain.Org, error) {
	o.org.Overrides = p
	o.org.Settings = p.Settings()
	return o.org, nil
}

// ListLive serves the one org as one keyset page: the first call returns it,
// and a call carrying its id as the cursor returns the empty page MaxRetention
// reads as the end of the table.
func (o *orgStore) ListLive(_ context.Context, after uuid.UUID, _ int) ([]domain.Org, error) {
	if after != uuid.Nil {
		return nil, nil
	}
	return []domain.Org{o.org}, nil
}

func newFixture(t *testing.T) (*service.Service, *orgStore, db.TenantScope) {
	t.Helper()

	orgID := uuid.New()
	store := &orgStore{org: domain.Org{
		ID: orgID, Slug: "acme", Name: "Acme",
		Settings: domain.DefaultSettings(),
	}}
	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return service.New(service.Deps{Orgs: store}), store, scope
}

func intp(v int) *int { return &v }

// TestTheWriteRefusesAnOutOfRangeValue — the service is the boundary, not the
// form. This is the same rule the domain enforces, asserted through the door a
// request actually comes in.
func TestTheWriteRefusesAnOutOfRangeValue(t *testing.T) {
	t.Parallel()

	svc, store, scope := newFixture(t)

	_, err := svc.UpdateOrgSettings(context.Background(), scope,
		domain.SettingsPatch{RefireGraceS: intp(0)}, nil)
	if err == nil {
		t.Fatal("a refire_grace of 0 was accepted; that is a Slack thread per transition")
	}
	if !errs.IsKind(err, errs.KindValidation) {
		t.Fatalf("kind is not validation: %v", err)
	}
	if store.org.Overrides.RefireGraceS != nil {
		t.Fatal("the rejected value was written anyway")
	}
}

// TestTheWriteValidatesTheMergedState is the subtle one.
//
// Validating only the incoming fragment would let a caller slip a value past by
// relying on a key it did not send. The merged state is what will be in force, so
// the merged state is what is checked.
func TestTheWriteValidatesTheMergedState(t *testing.T) {
	t.Parallel()

	svc, store, scope := newFixture(t)
	ctx := context.Background()

	// A legal first write.
	if _, err := svc.UpdateOrgSettings(ctx, scope,
		domain.SettingsPatch{StormThreshold: intp(40)}, nil); err != nil {
		t.Fatalf("a legal write failed: %v", err)
	}
	if store.org.Settings.StormThreshold != 40 {
		t.Fatalf("storm_threshold = %d, want 40", store.org.Settings.StormThreshold)
	}

	// A second, partial write must not disturb the first.
	if _, err := svc.UpdateOrgSettings(ctx, scope,
		domain.SettingsPatch{FlapThreshold: intp(7)}, nil); err != nil {
		t.Fatalf("the second write failed: %v", err)
	}
	if store.org.Settings.StormThreshold != 40 {
		t.Fatalf("the omitted key reverted: storm_threshold = %d", store.org.Settings.StormThreshold)
	}
	if store.org.Settings.FlapThreshold != 7 {
		t.Fatalf("flap_threshold = %d, want 7", store.org.Settings.FlapThreshold)
	}
}

// TestOriginSurvivesTheRoundTrip — a value written must report `org`, and a key
// reset must go back to reporting `default`.
func TestOriginSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	svc, _, scope := newFixture(t)
	ctx := context.Background()

	org, err := svc.UpdateOrgSettings(ctx, scope,
		domain.SettingsPatch{RefireGraceS: intp(900)}, nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := org.Overrides.Origin(domain.KeyRefireGrace); got != domain.OriginOrg {
		t.Fatalf("origin %q after a write, want org", got)
	}
	if org.Settings.RefireGrace != 900*time.Second {
		t.Fatalf("effective %v, want 900s", org.Settings.RefireGrace)
	}
	if got := org.Overrides.Origin(domain.KeyStormThreshold); got != domain.OriginDefault {
		t.Fatalf("an untouched key reports %q, want default", got)
	}

	org, err = svc.UpdateOrgSettings(ctx, scope,
		domain.SettingsPatch{}, []domain.SettingKey{domain.KeyRefireGrace})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := org.Overrides.Origin(domain.KeyRefireGrace); got != domain.OriginDefault {
		t.Fatalf("origin %q after a reset, want default", got)
	}
	if org.Settings.RefireGrace != domain.DefaultRefireGrace {
		t.Fatalf("effective %v after a reset, want the shipped default", org.Settings.RefireGrace)
	}
}

// TestSettingsAreReadLiveOnEveryLookup is the "no restart" property, asserted the
// only way it can be: by proving nothing memoises.
//
// ⭐ WHY THIS TEST EXISTS. The hot path (`internal/app`'s orgSettings adapter)
// calls GetOrg once per lifecycle evaluation, once per storm evaluation and once
// per notification evaluation. If a cache is ever slipped in front of it without
// a bounded TTL, an operator who raises `storm_threshold` during an incident will
// watch nothing change and have no way to tell a wrong setting from a stale one.
// This test fails the moment a read stops reaching the store.
func TestSettingsAreReadLiveOnEveryLookup(t *testing.T) {
	t.Parallel()

	svc, store, scope := newFixture(t)
	ctx := context.Background()

	first, err := svc.GetOrg(ctx, scope)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if first.Settings.StormThreshold != domain.DefaultStormThreshold {
		t.Fatalf("storm_threshold = %d, want the default", first.Settings.StormThreshold)
	}
	readsAfterFirst := store.reads

	// Somebody changes the setting — through the API, or in another pod.
	if _, err := svc.UpdateOrgSettings(ctx, scope,
		domain.SettingsPatch{StormThreshold: intp(60)}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	second, err := svc.GetOrg(ctx, scope)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if second.Settings.StormThreshold != 60 {
		t.Fatalf("the change did not take effect on the next read: storm_threshold = %d",
			second.Settings.StormThreshold)
	}
	if store.reads <= readsAfterFirst {
		t.Fatal("a read was served without reaching the store — something is caching " +
			"org settings, and a cached tuning value that cannot expire is a setting " +
			"an operator can change and never see take effect")
	}
}

// TestASoftDeletedOrgCannotBeTuned. A deleted tenant is not a tenant.
func TestASoftDeletedOrgCannotBeTuned(t *testing.T) {
	t.Parallel()

	svc, store, scope := newFixture(t)
	now := time.Now()
	store.org.DeletedAt = &now

	_, err := svc.UpdateOrgSettings(context.Background(), scope,
		domain.SettingsPatch{RefireGraceS: intp(900)}, nil)
	if err == nil {
		t.Fatal("a soft-deleted org accepted a settings write")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Kind != errs.KindUnauthorized {
		t.Fatalf("error %v, want unauthenticated", err)
	}
}
