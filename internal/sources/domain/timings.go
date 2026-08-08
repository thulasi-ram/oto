package domain

import (
	"strconv"
	"strings"
	"time"
)

// TimingProvenance is where one Alertmanager route timing's number came from.
//
// ⭐⭐ WHY THIS IS THREE STATES AND NOT A NULLABLE NUMBER.
//
// Alertmanager does NOT publish its own defaults. `group_wait`, `group_interval`
// and `repeat_interval` are `omitempty` pointers on `config.Route`, and the
// 30s / 5m / 4h values are applied much later, in `dispatch.NewRoute`, where
// `GET /api/v2/status` cannot see them. A stock `alertmanager.yml` therefore
// reports NONE of the three — which was verified against a live
// `prom/alertmanager:v0.28.1` and is preserved as
// `client/alertmanager/testdata/minimal_child_routes_v0.28.1.yaml`.
//
// Rendering that as "unknown" was honest about the READ and dishonest about the
// WORLD. When `group_interval` is absent from the config, Alertmanager's
// documented default is not a guess about what might be in force: it is what IS
// in force, derived from an observation oto actually made. Calling it unknown
// made the tuning guidance useless for the common case — a stock install — while
// claiming to be the careful answer.
//
// So the answer carries its own provenance, per field:
//
//	observed        — the value was present in the source's configuration;
//	default_applies — the key was absent, so Alertmanager's documented default
//	                  governs; the value is carried AND labelled;
//	unknown         — oto could not read or parse the configuration at all, so it
//	                  cannot even say whether a value was stated.
//
// ⛔ `default_applies` MUST NEVER RENDER AS `observed`. The arithmetic is equally
// valid for both — a 2m re-fire grace is just as unreachable under a defaulted 5m
// `group_interval` as under a configured one — but what the operator does about
// it is different. Under `observed` they edit their `alertmanager.yml` value;
// under `default_applies` there is no line to edit, and the fix is on oto's side
// or is a new line in their route.
type TimingProvenance string

// The three provenance states. They are stable wire strings: the UI keys off
// them and an operator greps for them.
const (
	// TimingObserved means the source's own configuration stated this value.
	TimingObserved TimingProvenance = "observed"
	// TimingDefaultApplies means the configuration stated nothing, so
	// Alertmanager's documented default is what governs.
	TimingDefaultApplies TimingProvenance = "default_applies"
	// TimingUnknown means the configuration could not be read or parsed, so oto
	// cannot say what governs. It is the only state that carries no value.
	TimingUnknown TimingProvenance = "unknown"
)

// Alertmanager's documented per-route defaults.
//
// ⚠️⚠️ THESE ARE UPSTREAM CONSTANTS AND THEY COULD CHANGE. They are
// `DefaultRouteOpts` in `github.com/prometheus/alertmanager/dispatch/route.go`,
// applied in `NewRoute` to any field the configuration leaves nil, and they have
// been 30s / 5m / 4h for every release oto has been pointed at. oto copies them
// because the status endpoint structurally cannot report them — see
// TimingProvenance — and a copy of somebody else's constant is a copy that can
// silently go stale.
//
// Two things make a drift detectable rather than silent:
//
//   - DefaultsVerifiedUpTo pins the newest release whose source these were read
//     against. A source running anything newer is still given these numbers —
//     they are the best answer available — but `Verified` reports false and the
//     API says so, so a screen can qualify the claim instead of asserting it.
//   - The checked-in `client/alertmanager/testdata/*_v0.28.1.yaml` fixtures are
//     real `config.original` captures. Re-capturing them from a newer image and
//     re-running `go test ./internal/sources/...` is the whole verification
//     procedure: if a future Alertmanager starts PUBLISHING these three, the
//     fixture gains the keys and the "an unset timing is absent" test fails,
//     which is exactly the signal that this table has stopped being needed.
const (
	// DefaultGroupWait is `route.group_wait` when the configuration omits it.
	DefaultGroupWait = 30 * time.Second
	// DefaultGroupInterval is `route.group_interval` when the configuration
	// omits it. It is the clock rate of oto's whole view of the world, which is
	// why leaving it "unknown" made most of the tuning screen inert.
	DefaultGroupInterval = 5 * time.Minute
	// DefaultRepeatInterval is `route.repeat_interval` when the configuration
	// omits it.
	DefaultRepeatInterval = 4 * time.Hour
)

// DefaultsVerifiedUpTo is the newest Alertmanager release whose `dispatch`
// defaults oto has actually checked the three constants above against.
const DefaultsVerifiedUpTo = "0.28.1"

// RouteTiming is one route timing together with where its number came from.
//
// Value is meaningless — and is zero — when Provenance is TimingUnknown. Zero is
// a legal Alertmanager setting (`group_wait: 0s` means "notify at once"), which
// is precisely why the provenance is carried beside the number rather than being
// inferred from it.
type RouteTiming struct {
	Provenance TimingProvenance
	Value      time.Duration
}

// Known reports whether this field carries a usable number at all.
func (t RouteTiming) Known() bool { return t.Provenance != TimingUnknown }

// Observed reports whether the number came from the source's own configuration
// rather than from Alertmanager's documented default.
func (t RouteTiming) Observed() bool { return t.Provenance == TimingObserved }

// ResolvedTimings is RouteTimings with each field's provenance decided: what oto
// read, plus what Alertmanager's own defaults supply for what it did not.
type ResolvedTimings struct {
	GroupWait      RouteTiming
	GroupInterval  RouteTiming
	RepeatInterval RouteTiming

	// ChildRoutes and ChildrenWithTimings are carried through unchanged. They are
	// the v1 boundary made countable: only the top-level route is evaluated, and
	// a non-zero ChildrenWithTimings says out loud that the three above do not
	// govern every alert.
	ChildRoutes         int
	ChildrenWithTimings int

	// DefaultsFromVersion is the Alertmanager version any `default_applies` field
	// is attributed to: the version the source itself reported when oto has one,
	// and DefaultsVerifiedUpTo when it does not. It is "" when no field defaulted.
	DefaultsFromVersion string
	// DefaultsVerified is false when the source is newer than the release oto
	// checked the constants against, or when the version could not be read. It is
	// the difference between "Alertmanager 0.28.1's default" and "the default oto
	// last checked, which was for 0.28.1".
	DefaultsVerified bool
}

// AnyDefaulted reports whether at least one field is riding a documented default.
func (r ResolvedTimings) AnyDefaulted() bool {
	return r.GroupWait.Provenance == TimingDefaultApplies ||
		r.GroupInterval.Provenance == TimingDefaultApplies ||
		r.RepeatInterval.Provenance == TimingDefaultApplies
}

// Resolve decides each field's provenance.
//
// `parsed` is whether oto has EVER successfully read this source's running
// configuration — `SourceHealth.RouteTimingsAt != nil`. It is the whole
// difference between the two negative answers: when it is false oto cannot say
// whether a value was stated, so every field is `unknown`; when it is true a nil
// field means the configuration stated nothing, so the documented default
// governs.
//
// ⛔ IT IS A READ-TIME DERIVATION AND IS NEVER STORED. `source_health` keeps only
// what was observed, NULL included, so the day this table is wrong the stored
// rows are still right and one deploy fixes every source at once. Writing a
// derived default into the column would make the two facts indistinguishable
// forever.
func (t RouteTimings) Resolve(parsed bool, amVersion string) ResolvedTimings {
	out := ResolvedTimings{
		ChildRoutes:         t.ChildRoutes,
		ChildrenWithTimings: t.ChildrenWithTimings,
	}
	if !parsed {
		out.GroupWait = RouteTiming{Provenance: TimingUnknown}
		out.GroupInterval = RouteTiming{Provenance: TimingUnknown}
		out.RepeatInterval = RouteTiming{Provenance: TimingUnknown}
		return out
	}

	out.GroupWait = resolveOne(t.GroupWait, DefaultGroupWait)
	out.GroupInterval = resolveOne(t.GroupInterval, DefaultGroupInterval)
	out.RepeatInterval = resolveOne(t.RepeatInterval, DefaultRepeatInterval)

	if out.AnyDefaulted() {
		out.DefaultsFromVersion, out.DefaultsVerified = attributeDefaults(amVersion)
	}
	return out
}

// resolveOne is the per-field rule: a stated value is observed, an absent one is
// governed by Alertmanager's documented default.
func resolveOne(observed *time.Duration, fallback time.Duration) RouteTiming {
	if observed != nil {
		return RouteTiming{Provenance: TimingObserved, Value: *observed}
	}
	return RouteTiming{Provenance: TimingDefaultApplies, Value: fallback}
}

// attributeDefaults pins the documented defaults to a version, and says whether
// that pin is one oto has actually checked.
//
// A version oto has not verified still gets the constants — they are the best
// answer anyone has, and withholding them would put the operator back on
// "unknown" for no gain — but it is reported as unverified so the claim can be
// qualified where it is rendered.
func attributeDefaults(amVersion string) (string, bool) {
	v := strings.TrimSpace(amVersion)
	if v == "" {
		return DefaultsVerifiedUpTo, false
	}
	return v, compareVersions(v, DefaultsVerifiedUpTo) <= 0
}

// compareVersions orders two Alertmanager version strings by their numeric
// components, ignoring a leading `v` and anything after the third number.
//
// It is deliberately not a semver library: the only question asked of it is "is
// this source newer than the release oto checked", the answer only qualifies a
// sentence in the UI, and an unparseable component sorts as 0 rather than
// producing an error nobody could act on.
func compareVersions(a, b string) int {
	av, bv := versionParts(a), versionParts(b)
	for i := range 3 {
		switch {
		case av[i] < bv[i]:
			return -1
		case av[i] > bv[i]:
			return 1
		}
	}
	return 0
}

// versionParts splits `v0.28.1-rc.0` into {0, 28, 1}.
func versionParts(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return out
		}
		out[i] = n
	}
	return out
}
