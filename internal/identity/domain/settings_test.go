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
			// Two incidents a day apart would merge into one occurrence, and the
			// history would lie about how many times this happened.
			name:  "refire_grace above the ceiling",
			patch: domain.SettingsPatch{RefireGraceS: intp(90000)},
			field: "refire_grace_s",
		},
		{
			// A threshold of 1 puts every group into permanent storm mode and
			// suppresses every per-alert reply forever: silence wearing a
			// damper's name.
			name:  "storm threshold of one",
			patch: domain.SettingsPatch{StormThreshold: intp(1)},
			field: "storm_threshold",
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
			// Mirrors policies_reminder_ck exactly. A value this accepted and that
			// CHECK rejected would be a 23514 at reminder time.
			name:  "reminder delay below policies_reminder_ck",
			patch: domain.SettingsPatch{UnackedReminderAfterS: intp(30)},
			field: "unacked_reminder_after_s",
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
		StormThreshold:      intp(25),
		StormWindowS:        intp(60),
		StormCooldownS:      intp(600),
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
	same := domain.SettingsPatch{RefireGraceS: intp(int(domain.DefaultRefireGrace / time.Second))}
	if got := same.Origin(domain.KeyRefireGrace); got != domain.OriginOrg {
		t.Fatalf("writing the default value reports origin %q; it is still an override", got)
	}
	if v, origin, ok := same.EffectiveInt(domain.KeyRefireGrace); !ok || v != 600 || origin != domain.OriginOrg {
		t.Fatalf("effective (%d, %q, %v), want (600, org, true)", v, origin, ok)
	}

	// A key nobody touched, alongside one that was.
	if got := same.Origin(domain.KeyStormThreshold); got != domain.OriginDefault {
		t.Fatalf("storm_threshold origin %q, want default", got)
	}
	if v, origin, _ := same.EffectiveInt(domain.KeyStormThreshold); v != 25 || origin != domain.OriginDefault {
		t.Fatalf("storm_threshold effective (%d, %q), want (25, default)", v, origin)
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

	stored := domain.SettingsPatch{RefireGraceS: intp(900), StormThreshold: intp(40)}
	merged := stored.Merge(domain.SettingsPatch{StormThreshold: intp(50)})

	if merged.RefireGraceS == nil || *merged.RefireGraceS != 900 {
		t.Fatalf("an omitted key was reverted: refire_grace_s = %v", merged.RefireGraceS)
	}
	if merged.StormThreshold == nil || *merged.StormThreshold != 50 {
		t.Fatalf("the written key did not take: storm_threshold = %v", merged.StormThreshold)
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
		RefireGraceS:   intp(0),       // would be a Slack thread per transition
		StormThreshold: intp(1),       // would be permanent storm mode
		FlapWindowS:    intp(9999999), // beyond a day
	}
	s := legacy.Settings()

	if s.RefireGrace < 60*time.Second {
		t.Fatalf("refire_grace clamped to %v, want at least the floor", s.RefireGrace)
	}
	if s.StormThreshold < 2 {
		t.Fatalf("storm_threshold clamped to %d, want at least 2", s.StormThreshold)
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

	// ⭐ THE MENTION DEFAULT IS `none`, AND IT IS ASSERTED HERE RATHER THAN LEFT
	// TO THE STRUCT COMPARISON, because it is the one default in this file that is
	// a RESEARCH RESULT: Slack documents that @here and @channel do not notify
	// when used in threads, and oto's unacked reminder is a thread reply. A
	// default of `here` would be a control that silently does nothing, which is
	// worse than no default at all (ADR 0020).
	if got.UnackedReminderMention != domain.MentionNone {
		t.Fatalf("the shipped reminder mention is %q, want none", got.UnackedReminderMention)
	}
	// And mentions are gated on severity, critical only: @here on every unacked
	// warning is how a channel gets muted, and a muted channel hides the real
	// incident.
	if got.UnackedReminderMentionMinSeverity != domain.MentionSeverityCritical {
		t.Fatalf("the shipped mention severity gate is %q, want critical",
			got.UnackedReminderMentionMinSeverity)
	}
	if len(got.UnackedReminderMentionList) != 0 {
		t.Fatalf("the shipped reminder mention list is %v, want empty", got.UnackedReminderMentionList)
	}

	// Every default must sit inside its own bound.
	for _, k := range domain.IntKeys() {
		b, ok := domain.Bounds(k)
		if !ok {
			t.Fatalf("%s has no bound and so cannot be validated at all", k)
		}
		v, _, _ := empty.EffectiveInt(k)
		if k == domain.KeyUnackedReminder {
			// Its default is deliberately ZERO — "this org sets no default" — which
			// is below the floor and is the one value outside the range on purpose.
			// Anything else would turn reminders on for every install that upgrades.
			if v != 0 {
				t.Fatalf("the shipped unacked reminder default is %d, want 0 (no default)", v)
			}
			continue
		}
		if !b.Contains(v) {
			t.Errorf("%s ships %d, outside its own bound %d..%d", k, v, b.Min, b.Max)
		}
	}
}
