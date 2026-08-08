package domain

import (
	"testing"
	"time"
)

// The three provenance states.
//
// ⭐ THE POINT OF THIS FILE is that "unknown" used to be the answer for a stock
// Alertmanager, which is the commonest install there is. Alertmanager does not
// publish its own defaults — that is proven against a real
// `prom/alertmanager:v0.28.1` capture in
// `client/alertmanager/testdata/minimal_child_routes_v0.28.1.yaml` — so a config
// that states none of the three reports none of the three. Reporting that as
// "oto does not know" was true about the read and false about the world, and it
// made every piece of tuning guidance inert for the majority of deployments.

func d(v time.Duration) *time.Duration { return &v }

// TestAnUnparsedConfigIsUnknownInEveryField. `unknown` is reserved for exactly
// one situation: oto has never managed to read this source's configuration, so it
// cannot say whether a value was stated OR that a default governs.
func TestAnUnparsedConfigIsUnknownInEveryField(t *testing.T) {
	t.Parallel()

	got := RouteTimings{ChildRoutes: 3}.Resolve(false, "0.28.1")

	for name, f := range map[string]RouteTiming{
		"group_wait": got.GroupWait, "group_interval": got.GroupInterval,
		"repeat_interval": got.RepeatInterval,
	} {
		if f.Provenance != TimingUnknown {
			t.Fatalf("%s provenance = %q, want unknown", name, f.Provenance)
		}
		if f.Known() {
			t.Fatalf("%s reports a usable value from a config oto never read", name)
		}
	}
	if got.AnyDefaulted() {
		t.Fatal("a config oto never read was given Alertmanager's defaults; oto cannot know " +
			"the config even exists, let alone that it states nothing")
	}
	if got.DefaultsFromVersion != "" {
		t.Fatalf("defaults attributed to %q when nothing defaulted", got.DefaultsFromVersion)
	}
	// The caveat count is a fact about the last parse and survives regardless.
	if got.ChildRoutes != 3 {
		t.Fatalf("child_routes = %d, want 3", got.ChildRoutes)
	}
}

// TestAnAbsentKeyMeansTheDocumentedDefaultGoverns.
//
// ⛔ THIS IS THE BEHAVIOUR CHANGE. A parsed config that states nothing is not
// ignorance: Alertmanager applies 30s / 5m / 4h in `dispatch.NewRoute`, so those
// values ARE what governs. They are carried, and they are LABELLED, because the
// operator's next action differs.
func TestAnAbsentKeyMeansTheDocumentedDefaultGoverns(t *testing.T) {
	t.Parallel()

	got := RouteTimings{}.Resolve(true, "0.28.1")

	cases := []struct {
		name string
		got  RouteTiming
		want time.Duration
	}{
		{"group_wait", got.GroupWait, DefaultGroupWait},
		{"group_interval", got.GroupInterval, DefaultGroupInterval},
		{"repeat_interval", got.RepeatInterval, DefaultRepeatInterval},
	}
	for _, c := range cases {
		if c.got.Provenance != TimingDefaultApplies {
			t.Fatalf("%s provenance = %q, want default_applies", c.name, c.got.Provenance)
		}
		if c.got.Value != c.want {
			t.Fatalf("%s = %v, want %v", c.name, c.got.Value, c.want)
		}
		if c.got.Observed() {
			t.Fatalf("%s claims to have been observed; it was derived, and an operator "+
				"acting on it has no line in alertmanager.yml to edit", c.name)
		}
	}
	if got.DefaultsFromVersion != "0.28.1" {
		t.Fatalf("defaults attributed to %q, want the version the source reported", got.DefaultsFromVersion)
	}
	if !got.DefaultsVerified {
		t.Fatal("0.28.1 is the release oto verified these constants against and is reported unverified")
	}
}

// TestAStatedValueIsObservedAndNeverOverwritten. A mixed config — some keys
// stated, some not — must not smear one provenance across all three.
func TestAStatedValueIsObservedAndNeverOverwritten(t *testing.T) {
	t.Parallel()

	got := RouteTimings{
		GroupWait:      d(10 * time.Second),
		RepeatInterval: d(12 * time.Hour),
	}.Resolve(true, "0.28.1")

	if got.GroupWait.Provenance != TimingObserved || got.GroupWait.Value != 10*time.Second {
		t.Fatalf("group_wait = %+v, want an observed 10s", got.GroupWait)
	}
	if got.RepeatInterval.Provenance != TimingObserved || got.RepeatInterval.Value != 12*time.Hour {
		t.Fatalf("repeat_interval = %+v, want an observed 12h", got.RepeatInterval)
	}
	if got.GroupInterval.Provenance != TimingDefaultApplies ||
		got.GroupInterval.Value != DefaultGroupInterval {
		t.Fatalf("group_interval = %+v, want a defaulted 5m", got.GroupInterval)
	}
}

// TestAZeroTimingStaysObserved. `group_wait: 0s` means "notify at once" and is a
// real, deliberate setting. Collapsing it into the default would replace an
// operator's choice with a number three hundred times larger.
func TestAZeroTimingStaysObserved(t *testing.T) {
	t.Parallel()

	got := RouteTimings{GroupWait: d(0)}.Resolve(true, "0.28.1")
	if got.GroupWait.Provenance != TimingObserved {
		t.Fatalf("group_wait: 0s reported as %q", got.GroupWait.Provenance)
	}
	if got.GroupWait.Value != 0 {
		t.Fatalf("group_wait = %v, want 0", got.GroupWait.Value)
	}
}

// TestTheDefaultsArePinnedToTheObservedVersion. The constants are UPSTREAM and
// could move; attributing them to the version that is actually running is what
// lets a screen say whose default it is quoting, and `Verified` is what stops it
// asserting a claim oto has not checked.
func TestTheDefaultsArePinnedToTheObservedVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		version      string
		wantFrom     string
		wantVerified bool
	}{
		{"0.28.1", "0.28.1", true},
		{"0.27.0", "0.27.0", true},
		{"v0.28.1", "v0.28.1", true},
		{"0.28.1-rc.0", "0.28.1-rc.0", true},
		// Newer than anything oto has read the source of: still the best answer
		// available, but reported as unverified rather than asserted.
		{"0.29.0", "0.29.0", false},
		{"1.0.0", "1.0.0", false},
		// No version at all: the pin falls back to the release oto checked.
		{"", DefaultsVerifiedUpTo, false},
	}
	for _, c := range cases {
		got := RouteTimings{}.Resolve(true, c.version)
		if got.DefaultsFromVersion != c.wantFrom {
			t.Fatalf("version %q → defaults_from_version %q, want %q",
				c.version, got.DefaultsFromVersion, c.wantFrom)
		}
		if got.DefaultsVerified != c.wantVerified {
			t.Fatalf("version %q → verified %v, want %v",
				c.version, got.DefaultsVerified, c.wantVerified)
		}
	}
}

// TestTheDocumentedDefaultsAreTheOnesAlertmanagerShips. A guard on the three
// constants themselves. They are copied from
// `github.com/prometheus/alertmanager/dispatch/route.go`, they are the whole
// reason `default_applies` can carry a number, and a typo here would put a
// confident wrong figure in front of every operator on a stock install.
func TestTheDocumentedDefaultsAreTheOnesAlertmanagerShips(t *testing.T) {
	t.Parallel()

	if DefaultGroupWait != 30*time.Second {
		t.Fatalf("group_wait default = %v, want 30s", DefaultGroupWait)
	}
	if DefaultGroupInterval != 5*time.Minute {
		t.Fatalf("group_interval default = %v, want 5m", DefaultGroupInterval)
	}
	if DefaultRepeatInterval != 4*time.Hour {
		t.Fatalf("repeat_interval default = %v, want 4h", DefaultRepeatInterval)
	}
	if DefaultsVerifiedUpTo == "" {
		t.Fatal("the defaults are pinned to no version, so nothing can ever detect them going stale")
	}
}
