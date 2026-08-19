package domain_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

func intp(v int) *int       { return &v }
func strp(v string) *string { return &v }
func boolp(v bool) *bool    { return &v }

// TestBoundsAreEnforcedServerSide is the property the settings UI must not be
// trusted with. The request that sets `refire_grace_s` to 0 arrives from `curl`
// long before it arrives from a form.
func TestBoundsAreEnforcedServerSide(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		patch domain.SettingsPatch
		field string
	}{
		{
			// The headline refusal: zero is a Slack thread per transition.
			name:  "refire_grace of zero",
			patch: domain.SettingsPatch{RefireGraceS: intp(0)},
			field: "refire_grace_s",
		},
		{
			// Below a minute the window is shorter than any useful group_interval,
			// so it is unreachable and every re-fire opens a new root message.
			name:  "refire_grace below the floor",
			patch: domain.SettingsPatch{RefireGraceS: intp(30)},
			field: "refire_grace_s",
		},
		{
			// Two incidents a day apart would merge into one case, and the
			// history would lie about how many times this happened.
			name:  "refire_grace above the ceiling",
			patch: domain.SettingsPatch{RefireGraceS: intp(90000)},
			field: "refire_grace_s",
		},
		{
			// Below 3 a single rolling deploy is mislabelled as flapping.
			name:  "flap threshold of two",
			patch: domain.SettingsPatch{FlapThreshold: intp(2)},
			field: "flap_threshold",
		},
		{
			// A window shorter than one group_interval cannot contain two
			// transitions oto is able to observe.
			name:  "flap window below one group_interval",
			patch: domain.SettingsPatch{FlapWindowS: intp(60)},
			field: "flap_window_s",
		},
		{
			name:  "unknown verbosity",
			patch: domain.SettingsPatch{DefaultVerbosity: strp("very_loud")},
			field: "default_verbosity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.patch.Validate()
			if err == nil {
				t.Fatalf("accepted an out-of-range write; the bound is not enforced")
			}
			var e *errs.Error
			if !errors.As(err, &e) {
				t.Fatalf("error %v is not an errs.Error, so no field reaches the caller", err)
			}
			if e.Kind != errs.KindValidation {
				t.Fatalf("kind %v, want a validation failure (a 422, not a 500)", e.Kind)
			}
			found := false
			for _, v := range e.Violations {
				if v.Field == tc.field {
					found = true
					if v.Message == "" {
						t.Errorf("violation on %s has no reason; a caller told only "+
							"\"invalid\" tries a different wrong number", tc.field)
					}
				}
			}
			if !found {
				t.Fatalf("no violation named %s; got %+v", tc.field, e.Violations)
			}
		})
	}
}

// TestInRangeWritesAreAccepted — the bounds must not be so tight that the
// documented worked example in docs/setup/tuning.md is refused.
func TestInRangeWritesAreAccepted(t *testing.T) {
	t.Parallel()

	worked := domain.SettingsPatch{
		RefireGraceS:        intp(900),
		ResolveGraceS:       intp(300),
		GroupCloseDelayS:    intp(300),
		FlapThreshold:       intp(5),
		FlapWindowS:         intp(10800),
		FlapDigestIntervalS: intp(900),
	}
	if err := worked.Validate(); err != nil {
		t.Fatalf("the tuning guide's own worked example was refused: %v", err)
	}
}

// TestOriginDistinguishesAnOverrideFromTheDefault is the reporting property.
//
// An effective value with no origin cannot be acted on: "600 because we chose it"
// and "600 because that is what oto ships" behave identically today and diverge
// the moment the default moves.
func TestOriginDistinguishesAnOverrideFromTheDefault(t *testing.T) {
	t.Parallel()

	var empty domain.SettingsPatch
	for _, k := range domain.AllSettingKeys() {
		if got := empty.Origin(k); got != domain.OriginDefault {
			t.Errorf("%s on an org that wrote nothing: origin %q, want default", k, got)
		}
	}
	if n := len(empty.Overridden()); n != 0 {
		t.Fatalf("an org that wrote nothing reports %d overrides", n)
	}

	// ⭐ THE CASE THAT MATTERS: writing the SAME NUMBER as the default must still
	// report `org`. It is a different fact — that value will not follow oto's
	// default when oto's default moves — and collapsing the two is the whole
	// failure this reporting exists to prevent.
	shipped := int(domain.DefaultRefireGrace / time.Second)
	same := domain.SettingsPatch{RefireGraceS: intp(shipped)}
	if got := same.Origin(domain.KeyRefireGrace); got != domain.OriginOrg {
		t.Fatalf("writing the default value reports origin %q; it is still an override", got)
	}
	if v, origin, ok := same.EffectiveInt(domain.KeyRefireGrace); !ok || v != shipped || origin != domain.OriginOrg {
		t.Fatalf("effective (%d, %q, %v), want (%d, org, true)", v, origin, ok, shipped)
	}

	// A key nobody touched, alongside one that was.
	if got := same.Origin(domain.KeyFlapThreshold); got != domain.OriginDefault {
		t.Fatalf("flap_threshold origin %q, want default", got)
	}
	if v, origin, _ := same.EffectiveInt(domain.KeyFlapThreshold); v != domain.DefaultFlapThreshold ||
		origin != domain.OriginDefault {
		t.Fatalf("flap_threshold effective (%d, %q), want (%d, default)", v, origin, domain.DefaultFlapThreshold)
	}
}

// TestClearReturnsAKeyToTheDefault — an override must be undoable, and after it
// the origin must say so.
func TestClearReturnsAKeyToTheDefault(t *testing.T) {
	t.Parallel()

	p := domain.SettingsPatch{RefireGraceS: intp(900), BroadcastOnResolved: boolp(true)}
	if p.Origin(domain.KeyRefireGrace) != domain.OriginOrg {
		t.Fatal("setup: the override did not register")
	}

	p = p.Clear(domain.KeyRefireGrace)
	if got := p.Origin(domain.KeyRefireGrace); got != domain.OriginDefault {
		t.Fatalf("after clear, origin %q, want default", got)
	}
	if got := p.Settings().RefireGrace; got != domain.DefaultRefireGrace {
		t.Fatalf("after clear, effective %v, want the shipped default %v", got, domain.DefaultRefireGrace)
	}
	// Clearing one key must not disturb another.
	if p.Origin(domain.KeyBroadcastOnResolved) != domain.OriginOrg {
		t.Fatal("clearing refire_grace also cleared broadcast_on_resolved")
	}
}

// TestMergeLeavesOmittedKeysAlone. A settings API where an omitted key silently
// reverted to the default would revert nine settings every time somebody changed
// one.
func TestMergeLeavesOmittedKeysAlone(t *testing.T) {
	t.Parallel()

	stored := domain.SettingsPatch{RefireGraceS: intp(900), FlapThreshold: intp(4)}
	merged := stored.Merge(domain.SettingsPatch{FlapThreshold: intp(6)})

	if merged.RefireGraceS == nil || *merged.RefireGraceS != 900 {
		t.Fatalf("an omitted key was reverted: refire_grace_s = %v", merged.RefireGraceS)
	}
	if merged.FlapThreshold == nil || *merged.FlapThreshold != 6 {
		t.Fatalf("the written key did not take: flap_threshold = %v", merged.FlapThreshold)
	}
}

// TestOutOfRangeStoredValuesAreClampedNotRejected is the read-path rule.
//
// A row written before a bound existed must never produce a pathological runtime
// value, and must never fail an alert either. Rejecting on read would turn a
// legacy row into a 500 on every notification — validating the wrong direction.
func TestOutOfRangeStoredValuesAreClampedNotRejected(t *testing.T) {
	t.Parallel()

	legacy := domain.SettingsPatch{
		RefireGraceS:  intp(0),       // would be a Slack thread per transition
		FlapThreshold: intp(1),       // below the floor: one deploy reads as flapping
		FlapWindowS:   intp(9999999), // beyond a day
	}
	s := legacy.Settings()

	if s.RefireGrace < 60*time.Second {
		t.Fatalf("refire_grace clamped to %v, want at least the floor", s.RefireGrace)
	}
	if s.FlapThreshold < 3 {
		t.Fatalf("flap_threshold clamped to %d, want at least the floor", s.FlapThreshold)
	}
	if s.FlapWindow > 86400*time.Second {
		t.Fatalf("flap_window clamped to %v, want at most the ceiling", s.FlapWindow)
	}
	// The origin still reports `org`: the value WAS written by the org, and
	// clamping does not change who wrote it.
	if legacy.Origin(domain.KeyRefireGrace) != domain.OriginOrg {
		t.Fatal("clamping erased the origin")
	}
}

// TestTheShippedDefaultsAreUnchanged. Every bound was chosen to contain the value
// oto already ships; a bound that excluded its own default would make a
// brand-new org unconfigurable.
func TestTheShippedDefaultsAreUnchanged(t *testing.T) {
	t.Parallel()

	var empty domain.SettingsPatch
	got, want := empty.Settings(), domain.DefaultSettings()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("an empty patch yields %+v, want the shipped defaults %+v", got, want)
	}

	// Every default must sit inside its own bound.
	for _, k := range domain.IntKeys() {
		b, ok := domain.Bounds(k)
		if !ok {
			t.Fatalf("%s has no bound and so cannot be validated at all", k)
		}
		v, _, _ := empty.EffectiveInt(k)
		if !b.Contains(v) {
			t.Errorf("%s ships %d, outside its own bound %d..%d", k, v, b.Min, b.Max)
		}
	}
}
