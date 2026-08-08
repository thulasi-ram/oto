package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The precedence rules of the declarative tuning layer, asserted through the one
// door a request actually comes in.
//
// ⭐ THE THREE PROPERTIES BELOW ARE THE WHOLE FEATURE:
//
//  1. declarative config BEATS an org override, so an install driven by Helm
//     values cannot be quietly contradicted from the UI;
//  2. a PATCH on a declaratively-managed key is REFUSED with 409 naming the
//     config key, rather than accepted and reverted on the next deploy;
//  3. a shadowed override STAYS VISIBLE, so "you have 900 stored and
//     configuration is forcing 600" is a thing an operator can be told.
//
// Each is a failure somebody would otherwise spend an afternoon on.

// declFixture builds a service whose deployment forces the named keys.
func declFixture(t *testing.T, entries ...domain.DeclaredEntry) (*service.Service, *orgStore, db.TenantScope) {
	t.Helper()

	decl, err := domain.NewDeclarative(entries)
	if err != nil {
		t.Fatalf("resolve the declarative layer: %v", err)
	}
	_, store, scope := newFixture(t)
	// Rebuilt over the SAME store, so the fixture's org identity is preserved and
	// only the declarative layer differs from the plain fixture.
	return service.New(service.Deps{Orgs: store, Declarative: decl}), store, scope
}

// TestDeclarativeConfigBeatsAnOrgOverride is the precedence rule itself.
func TestDeclarativeConfigBeatsAnOrgOverride(t *testing.T) {
	t.Parallel()

	svc, store, scope := declFixture(t, domain.DeclaredEntry{
		Key: "refire_grace_s", ConfigKey: "OTO_TUNING_REFIRE_GRACE_S", Value: "600",
	})
	ctx := context.Background()

	// The org wrote 900 before the deployment started stating a value.
	store.org.Overrides = domain.SettingsPatch{RefireGraceS: intp(900)}
	store.org.Settings = store.org.Overrides.Settings()

	org, err := svc.GetOrg(ctx, scope)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if org.Settings.RefireGrace != 600*time.Second {
		t.Fatalf("effective refire_grace = %v, want 600s: the deployment's configuration "+
			"must beat the org override, or the value reverts on every deploy",
			org.Settings.RefireGrace)
	}
	if got := org.Origin(domain.KeyRefireGrace); got != domain.OriginConfig {
		t.Fatalf("origin %q, want config", got)
	}
	if got := org.ConfigKey(domain.KeyRefireGrace); got != "OTO_TUNING_REFIRE_GRACE_S" {
		t.Fatalf("config key %q: a badge with no key to go and edit is a wall", got)
	}
	// An untouched key is unaffected in both directions.
	if got := org.Origin(domain.KeyStormThreshold); got != domain.OriginDefault {
		t.Fatalf("an unmanaged, unwritten key reports %q, want default", got)
	}
	if org.Settings.StormThreshold != domain.DefaultStormThreshold {
		t.Fatalf("an unmanaged key drifted: storm_threshold = %d", org.Settings.StormThreshold)
	}
}

// TestAShadowedOverrideStaysVisible. The 900 is still in `orgs.settings` and the
// API can still show it. Hiding it is how somebody spends an afternoon working
// out why the number in the database is not the number in force — and it is what
// comes back the moment the config key is deleted.
func TestAShadowedOverrideStaysVisible(t *testing.T) {
	t.Parallel()

	svc, store, scope := declFixture(t, domain.DeclaredEntry{
		Key: "refire_grace_s", ConfigKey: "tuning.refire_grace_s", Value: 600,
	})
	store.org.Overrides = domain.SettingsPatch{RefireGraceS: intp(900), FlapThreshold: intp(9)}
	store.org.Settings = store.org.Overrides.Settings()

	org, err := svc.GetOrg(context.Background(), scope)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	shadowed := org.Shadowed()
	if shadowed.RefireGraceS == nil || *shadowed.RefireGraceS != 900 {
		t.Fatalf("the shadowed override is not reported: %+v — an operator has to be able "+
			"to see both numbers", shadowed.RefireGraceS)
	}
	// A key the deployment does NOT manage is not shadowed: it is simply in force.
	if shadowed.FlapThreshold != nil {
		t.Fatal("an override that is in force was reported as shadowed")
	}
	if org.Settings.FlapThreshold != 9 {
		t.Fatalf("flap_threshold = %d, want the org's own 9", org.Settings.FlapThreshold)
	}
	// And the stored override is untouched: nothing deleted it on the org's behalf.
	if store.org.Overrides.RefireGraceS == nil || *store.org.Overrides.RefireGraceS != 900 {
		t.Fatal("the shadowed override was removed from storage; nothing asked for that")
	}
}

// TestPatchOnAConfigManagedKeyIs409 — the refusal, and the config key in it.
func TestPatchOnAConfigManagedKeyIs409(t *testing.T) {
	t.Parallel()

	svc, store, scope := declFixture(t, domain.DeclaredEntry{
		Key: "refire_grace_s", ConfigKey: "OTO_TUNING_REFIRE_GRACE_S", Value: "600",
	})

	_, err := svc.UpdateOrgSettings(context.Background(), scope,
		domain.SettingsPatch{RefireGraceS: intp(900)}, nil)
	if err == nil {
		t.Fatal("a write to a config-managed key was accepted; it would revert on the next deploy")
	}
	if !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("kind %q, want conflict (409): %v", errs.KindOf(err), err)
	}
	if errs.CodeOf(err) != service.CodeSettingManagedByConfig {
		t.Fatalf("code %q, want %q", errs.CodeOf(err), service.CodeSettingManagedByConfig)
	}

	vs := errs.ViolationsOf(err)
	if len(vs) != 1 {
		t.Fatalf("violations %+v, want exactly the offending key", vs)
	}
	if vs[0].Field != "refire_grace_s" {
		t.Fatalf("violation field %q, want the settings key", vs[0].Field)
	}
	if !strings.Contains(vs[0].Message, "OTO_TUNING_REFIRE_GRACE_S") {
		t.Fatalf("the detail does not name the config key: %q — without it the caller is "+
			"told they cannot change the value and nothing about where they can",
			vs[0].Message)
	}
	if store.org.Overrides.RefireGraceS != nil {
		t.Fatal("the refused value was written anyway")
	}
}

// TestResetOnAConfigManagedKeyIs409. A reset cannot take effect either, and
// unlike a write it would also destroy the shadowed override underneath — the one
// value that comes back if the config key is ever removed.
func TestResetOnAConfigManagedKeyIs409(t *testing.T) {
	t.Parallel()

	svc, store, scope := declFixture(t, domain.DeclaredEntry{
		Key: "refire_grace_s", ConfigKey: "tuning.refire_grace_s", Value: 600,
	})
	store.org.Overrides = domain.SettingsPatch{RefireGraceS: intp(900)}

	_, err := svc.UpdateOrgSettings(context.Background(), scope,
		domain.SettingsPatch{}, []domain.SettingKey{domain.KeyRefireGrace})
	if err == nil {
		t.Fatal("a reset of a config-managed key was accepted")
	}
	if !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("kind %q, want conflict: %v", errs.KindOf(err), err)
	}
	if store.org.Overrides.RefireGraceS == nil {
		t.Fatal("the reset destroyed the shadowed override despite being refused")
	}
}

// TestAnUnmanagedKeyIsStillWritable. The refusal is per key, not per request:
// a deployment that states one value must not freeze the whole settings screen.
func TestAnUnmanagedKeyIsStillWritable(t *testing.T) {
	t.Parallel()

	svc, _, scope := declFixture(t, domain.DeclaredEntry{
		Key: "refire_grace_s", ConfigKey: "OTO_TUNING_REFIRE_GRACE_S", Value: "600",
	})

	org, err := svc.UpdateOrgSettings(context.Background(), scope,
		domain.SettingsPatch{StormThreshold: intp(40)}, nil)
	if err != nil {
		t.Fatalf("a write to an unmanaged key was refused: %v", err)
	}
	if org.Settings.StormThreshold != 40 {
		t.Fatalf("storm_threshold = %d, want 40", org.Settings.StormThreshold)
	}
	// And the managed key is still forced in the returned org.
	if org.Settings.RefireGrace != 600*time.Second {
		t.Fatalf("the write dropped the declarative overlay: refire_grace = %v", org.Settings.RefireGrace)
	}
}

// TestAllKeysCanBeSetDeclaratively is the "every existing knob" requirement,
// asserted against the closed key set rather than a hand-written list — a new
// knob that cannot be configured declaratively fails this the day it is added.
func TestAllKeysCanBeSetDeclaratively(t *testing.T) {
	t.Parallel()

	values := map[domain.SettingKey]any{
		domain.KeyRefireGrace:                       600,
		domain.KeyResolveGrace:                      300,
		domain.KeyGroupCloseDelay:                   300,
		domain.KeyFlapThreshold:                     5,
		domain.KeyFlapWindow:                        1800,
		domain.KeyFlapDigestInterval:                900,
		domain.KeyStormThreshold:                    25,
		domain.KeyStormWindow:                       60,
		domain.KeyStormCooldown:                     600,
		domain.KeyRawRetention:                      14,
		domain.KeyEventRetention:                    13,
		domain.KeyUnackedReminder:                   900,
		domain.KeyDefaultVerbosity:                  "firing_only",
		domain.KeyBroadcastOnResolved:               true,
		domain.KeyUnackedReminderMention:            "list",
		domain.KeyUnackedReminderMentionList:        []string{"<@U012AB3CD>"},
		domain.KeyUnackedReminderMentionMinSeverity: "warning",
	}

	entries := make([]domain.DeclaredEntry, 0, len(values))
	for _, k := range domain.AllSettingKeys() {
		v, ok := values[k]
		if !ok {
			t.Fatalf("%s has no declarative test value: a new tuning key must be settable "+
				"from configuration on the day it is added, or an IaC install cannot state it", k)
		}
		entries = append(entries, domain.DeclaredEntry{
			Key: string(k), ConfigKey: "tuning." + string(k), Value: v,
		})
	}

	decl, err := domain.NewDeclarative(entries)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, k := range domain.AllSettingKeys() {
		if !decl.Manages(k) {
			t.Fatalf("%s was not resolved from configuration", k)
		}
	}
}
