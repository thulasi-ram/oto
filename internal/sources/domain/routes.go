package domain

import (
	"strings"
	"time"
)

// ⭐⭐ THE ROUTE TREE, RESOLVED — what actually governs the alerts oto is sent.
//
// `RouteTimings` (ports.go) reports the TOP-LEVEL route, which is what governs
// every alert matching no more specific route. That was never the whole story:
// `group_wait`, `group_interval` and `repeat_interval` are per-route and
// INHERITED, so the numbers governing a particular alert are the ones on the
// route that matched it, and a real Alertmanager overrides at least one of them
// on at least one child. Every tuning verdict oto gives is arithmetic over those
// three (docs/setup/tuning.md), so a verdict computed from the top-level route
// of a tree that overrides them is confident advice about a cluster that is not
// the operator's.
//
// This file is the tree walked properly. It resolves, per route:
//
//   - the RECEIVER it delivers to, inherited from the nearest ancestor that
//     names one;
//   - the three timings, inherited from the nearest ancestor that states each,
//     WITH the depth that stated it, so "5m because this route says so" stays
//     distinguishable from "5m because the root says so";
//   - `group_by`, inherited the same way, including the `...` form;
//   - the MATCHER PATH from the root, which is the only thing that lets a human
//     recognise the route in their own `alertmanager.yml`;
//   - whether the route is reachable at all.
//
// ⛔ IT DOES NOT EVALUATE MATCHERS AGAINST LABELS, AND IT MUST NOT. Deciding
// which route a PARTICULAR alert takes needs that alert's label set, and
// re-implementing `labels.Matchers.Matches` — regex dialect included — would be
// a second, invisible copy of somebody else's routing engine. Everything here is
// STRUCTURAL: it is derived from the shape of the tree alone and is true for
// every alert. The one place structure alone decides reachability is a
// matcher-less route, which matches everything by construction; see Shadowed.

// MaxResolvedRoutes caps how many delivering routes oto will resolve and carry.
//
// The tree is untrusted input of unbounded size and this list is stored on
// `source_health` and rendered on a settings screen. A config with more than this
// many delivery points is past the point where a list helps anybody, so the walk
// stops and reports how many it did not carry rather than truncating silently.
const MaxResolvedRoutes = 64

// RouteStep is one route on the path from the root, as a human would recognise
// it in their own file.
type RouteStep struct {
	// Matchers are this route's OWN matcher expressions, normalised into
	// Alertmanager's current `matchers` spelling, sorted for determinism. Empty
	// means the route matches everything its parent gives it.
	Matchers []string
	// Deprecated reports that the route spelled its matchers with `match` or
	// `match_re`. Both still work and both are deprecated upstream; oto renders
	// them in the current spelling and says where they came from rather than
	// quietly rewriting an operator's file in the display.
	Deprecated bool
	// Continue is this route's `continue`. It is why more than one route can
	// reach the same receiver: with it set, evaluation carries on to later
	// siblings instead of stopping at the first match.
	Continue bool
}

// CatchAll reports that this step states no matchers, so everything reaching its
// parent reaches it.
func (s RouteStep) CatchAll() bool { return len(s.Matchers) == 0 }

// InheritedTiming is one route timing together with WHERE ON THE PATH it was
// stated.
//
// Value is nil when no route on the path from the root states it at all — which
// is not "unknown", it is "Alertmanager's documented default governs", exactly as
// for the top-level route. `RouteTiming`/`TimingProvenance` (timings.go) is where
// that becomes a rendered answer.
type InheritedTiming struct {
	Value *time.Duration
	// FromDepth is the index into ReceiverRoute.Path of the route that stated
	// this value, and is -1 when Value is nil. 0 is the top-level route, so
	// `FromDepth == len(Path)-1` means "this route states it itself" and anything
	// smaller means "inherited from an ancestor".
	FromDepth int
}

// Stated reports whether any route on the path stated this timing.
func (t InheritedTiming) Stated() bool { return t.Value != nil }

// Own reports whether the route itself stated this timing, rather than
// inheriting it. pathLen is len(ReceiverRoute.Path).
func (t InheritedTiming) Own(pathLen int) bool {
	return t.Value != nil && t.FromDepth == pathLen-1
}

// ReceiverRoute is ONE route that delivers to a receiver, with everything
// resolved.
//
// "Delivers" is Alertmanager's own rule, from `dispatch.Route.Match`: a route is
// the delivery point for an alert only when NO child of it matched. So a route
// with a matcher-less child never delivers anything itself — the child takes
// everything — and a route with only matcher-bearing children may or may not,
// depending on labels oto does not have. The conservative direction is taken:
// such a route IS listed, because for some alert it is the answer.
type ReceiverRoute struct {
	// Receiver is the receiver this route delivers to, after inheritance.
	Receiver string
	// Path is the route chain from the top-level route (index 0) to this one.
	// It is never empty: the top-level route is itself a path of length one.
	Path []RouteStep
	// The three timings, each resolved along Path.
	GroupWait      InheritedTiming
	GroupInterval  InheritedTiming
	RepeatInterval InheritedTiming
	// GroupBy is the effective grouping labels, inherited. GroupByAll is the
	// `group_by: ['...']` form, which groups by every label and therefore makes
	// storm collapse unreachable at any threshold — worth surfacing beside the
	// numbers because no number captures it.
	GroupBy    []string
	GroupByAll bool
	// Shadowed reports that this route can NEVER be reached: an EARLIER sibling
	// somewhere on its path states no matchers and no `continue`, so it consumes
	// everything before evaluation gets this far. This is the only unreachability
	// oto can prove from structure alone, and it is a real misconfiguration
	// rather than a display detail.
	Shadowed bool
}

// Depth is how far below the top-level route this route sits. 0 is the top-level
// route itself.
func (r ReceiverRoute) Depth() int { return len(r.Path) - 1 }

// TopLevel reports whether this IS the top-level route.
func (r ReceiverRoute) TopLevel() bool { return len(r.Path) == 1 }

// Timings flattens the three back into the observation shape the rest of the
// module already speaks. Child counts are not this route's business and stay
// zero: they describe a tree, not a route.
func (r ReceiverRoute) Timings() RouteTimings {
	return RouteTimings{
		GroupWait:      r.GroupWait.Value,
		GroupInterval:  r.GroupInterval.Value,
		RepeatInterval: r.RepeatInterval.Value,
	}
}

// SameTimingsAs reports whether two routes would produce identical tuning
// arithmetic. It compares what is STATED, not what a default would supply,
// because two routes that both state nothing agree by inheriting the same
// answer.
func (r ReceiverRoute) SameTimingsAs(o ReceiverRoute) bool {
	return sameDuration(r.GroupWait.Value, o.GroupWait.Value) &&
		sameDuration(r.GroupInterval.Value, o.GroupInterval.Value) &&
		sameDuration(r.RepeatInterval.Value, o.RepeatInterval.Value)
}

func sameDuration(a, b *time.Duration) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// Label renders the route's matcher path as one string an operator can search
// their own file for: `{} > {severity="critical"} > {team=~"a|b"}`.
func (r ReceiverRoute) Label() string {
	parts := make([]string, 0, len(r.Path))
	for _, step := range r.Path {
		parts = append(parts, "{"+strings.Join(step.Matchers, ",")+"}")
	}
	return strings.Join(parts, " > ")
}

// ReceiverBasis is HOW oto decided which receiver is its own.
//
// ⛔⛔ OTO CANNOT READ ITS OWN WEBHOOK URL OUT OF AN ALERTMANAGER CONFIG, AND
// THAT IS NOT A GAP IN THE PARSER. `GET /api/v2/status` returns `config.original`
// with every secret replaced by the literal string `<secret>`, and
// `webhook_config.url` is a `SecretURL` — so the one field that would identify
// oto's receiver unambiguously (it contains oto's own source id, the ingest path
// being `/api/v1/ingest/alertmanager/{source_id}`) is redacted before oto ever
// sees it. This is verified, not assumed: the checked-in
// `client/alertmanager/testdata/compose_v0.28.1.yaml` is a real capture and reads
// `url: <secret>`.
//
// So identification is an INFERENCE, and it is labelled as one. The inference is
// deliberately narrow: the shape SPEC §J.1 tells every operator to deploy — one
// webhook receiver, which is the entire Alertmanager change oto requires — is
// identified exactly, and anything more ambiguous says so and shows the operator
// every candidate instead of guessing between them.
type ReceiverBasis string

// The bases. They are stable wire strings: the UI keys off them.
const (
	// ReceiverSoleWebhook means exactly one receiver in the whole configuration
	// has a webhook integration, so that receiver is oto's. It is the shape the
	// setup guide produces and it is not a coin toss.
	ReceiverSoleWebhook ReceiverBasis = "sole_webhook"
	// ReceiverAmbiguous means several receivers have webhook integrations and the
	// URLs that would tell them apart are redacted. oto reports EVERY route and
	// names the candidates rather than picking one.
	ReceiverAmbiguous ReceiverBasis = "ambiguous"
	// ReceiverNoWebhook means no receiver has a webhook integration at all. oto
	// cannot be receiving pushes from this source, which is itself worth saying
	// out loud on a screen about how oto is fed.
	ReceiverNoWebhook ReceiverBasis = "no_webhook"
	// ReceiverUnknown means the configuration could not be read, so there is not
	// even a receiver list to reason about.
	ReceiverUnknown ReceiverBasis = "unknown"
)

// RouteResolution is the whole answer for one source's route tree.
type RouteResolution struct {
	// Routes is every DELIVERING route, in Alertmanager's own evaluation order,
	// capped at MaxResolvedRoutes.
	Routes []ReceiverRoute
	// Dropped is how many delivering routes the cap discarded. Non-zero means the
	// list below is incomplete and must be rendered as such.
	Dropped int
	// Receiver is oto's own receiver name, or "" when Basis is not
	// ReceiverSoleWebhook.
	Receiver string
	// Basis is how Receiver was decided — or why it could not be.
	Basis ReceiverBasis
	// WebhookReceivers is every receiver with a webhook integration, in
	// declaration order. It IS the candidate list when Basis is
	// ReceiverAmbiguous, and it is what makes the ambiguity actionable.
	WebhookReceivers []string
}

// Observed reports whether oto has a resolution at all. A configuration that
// parsed but declares no route is not observed — there is nothing to say.
func (r RouteResolution) Observed() bool { return len(r.Routes) > 0 }

// Reaching returns the routes that deliver to receiver and are actually
// reachable, in evaluation order.
//
// ⚠️ IT CAN LEGITIMATELY RETURN MORE THAN ONE, WITH DIFFERENT TIMINGS. A route
// with `continue: true` does not stop evaluation, so two sibling routes can both
// deliver to the same receiver under different matchers and different
// `group_interval`s. The answer for a receiver is a SET, and every caller must
// treat it as one rather than taking the first element.
func (r RouteResolution) Reaching(receiver string) []ReceiverRoute {
	if receiver == "" {
		return nil
	}
	out := make([]ReceiverRoute, 0, 2)
	for _, rt := range r.Routes {
		if rt.Receiver == receiver && !rt.Shadowed {
			out = append(out, rt)
		}
	}
	return out
}

// ForOto returns the routes reaching oto's own receiver. It is empty whenever
// oto could not identify its receiver, which is the caller's cue to fall back to
// the top-level route and SAY SO.
func (r RouteResolution) ForOto() []ReceiverRoute { return r.Reaching(r.Receiver) }

// Agree reports whether every route in the set resolves to the same three
// timings, so a single tuning verdict is honest. An empty set and a set of one
// both agree; a set of two that state different `group_interval`s does not, and
// the screen must say "these two routes reach oto and disagree" rather than
// silently picking one.
func Agree(routes []ReceiverRoute) bool {
	for i := 1; i < len(routes); i++ {
		if !routes[0].SameTimingsAs(routes[i]) {
			return false
		}
	}
	return true
}

// Governing returns the single route whose timings may be argued from, and
// whether there is one.
//
// There is one exactly when the set is non-empty and Agree. Anything else — no
// identified receiver, no reaching route, or a disagreement — has no single
// answer, and manufacturing one (first match, slowest, an average) would be the
// same lie the hand-typed form was.
func Governing(routes []ReceiverRoute) (ReceiverRoute, bool) {
	if len(routes) == 0 || !Agree(routes) {
		return ReceiverRoute{}, false
	}
	return routes[0], true
}
