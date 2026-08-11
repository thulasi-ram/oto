package alertmanager

import (
	"sort"
	"strconv"
	"strings"

	"github.com/thulasiram/oto/internal/sources/domain"
)

// The route tree, walked with Alertmanager's OWN semantics.
//
// ⭐⭐ WHY THIS EXISTS AT ALL. `routeTimings` (amconfig.go) reads the top-level
// route, which is what governs every alert matching no more specific route. On a
// real Alertmanager that is usually not the number in force for oto: the three
// timings are per-route and inherited, so a config that overrides
// `group_interval` on the route oto's receiver hangs off makes every tuning
// verdict — `refire_grace`, the flap window and threshold, the storm burst window
// — arithmetic about a cluster that is not the operator's.
//
// ⛔ IT IS A STRUCTURAL WALK, NOT A MATCHER ENGINE. Nothing here evaluates a
// matcher against a label set; there is no label set. What it computes is true
// for EVERY alert:
//
//   - inheritance — `receiver`, `group_by` and the three timings flow down from
//     the nearest ancestor that states them (`dispatch.NewRoute` copies the
//     parent's options and overrides only the fields the child states);
//   - delivery — `dispatch.Route.Match` returns a node as the answer only when
//     NO child matched, so a route with a matcher-less child never delivers;
//   - order and `continue` — children are evaluated in order and evaluation
//     STOPS at the first match unless that child sets `continue: true`, which is
//     precisely why several routes can reach one receiver;
//   - shadowing — a matcher-less sibling without `continue` consumes everything,
//     so every sibling after it is unreachable. That is the only unreachability
//     provable without labels, and it is a real misconfiguration.
//
// Anything that would need labels is left as a set for the reader to see, never
// collapsed into one number.

// MaxRouteDepth bounds the recursion over an untrusted tree.
//
// It is not a statement about what Alertmanager allows — it allows any depth —
// but about what a hostile or corrupt `config.original` can do to oto's stack. A
// tree deeper than this stops being walked and the routes below the cut are
// counted as dropped, which is visible, rather than being silently absent.
const MaxRouteDepth = 16

// webhookConfigsKey is the receiver integration oto is delivered through. It is
// the ONLY integration that can feed oto, which is what makes it the basis for
// identifying oto's own receiver.
const webhookConfigsKey = "webhook_configs"

// resolveRoutes walks the whole tree and returns every delivering route with its
// timings resolved.
func resolveRoutes(root map[string]any, receivers []map[string]any) domain.RouteResolution {
	out := domain.RouteResolution{}
	out.Receiver, out.Basis, out.WebhookReceivers = identifyReceiver(receivers)
	if len(root) == 0 {
		// A config with no `route:` block routes nothing. There is no answer to
		// give, and an empty Routes is how Observed() reports that.
		return out
	}

	w := &walker{}
	w.visit(root, nil, inherited{
		groupWait:      domain.InheritedTiming{FromDepth: -1},
		groupInterval:  domain.InheritedTiming{FromDepth: -1},
		repeatInterval: domain.InheritedTiming{FromDepth: -1},
	}, false)

	out.Routes = w.routes
	out.Dropped = w.dropped
	return out
}

// inherited is what flows DOWN the tree: the options a child starts from before
// it overrides any of them, mirroring `dispatch.NewRoute`'s copy of its parent's
// RouteOpts.
type inherited struct {
	receiver       string
	groupWait      domain.InheritedTiming
	groupInterval  domain.InheritedTiming
	repeatInterval domain.InheritedTiming
	groupBy        []string
	groupByAll     bool
}

// walker accumulates the delivering routes under the cap.
type walker struct {
	routes  []domain.ReceiverRoute
	dropped int
}

// visit resolves one route node and recurses into its children.
//
// `path` is the chain of steps from the top-level route DOWN TO BUT NOT
// INCLUDING this node; this node's own step is appended here. `shadowed` says
// that an ancestor-or-sibling already consumed everything that could have
// reached this node, so nothing here can ever fire.
func (w *walker) visit(node map[string]any, path []domain.RouteStep, inh inherited, shadowed bool) {
	if len(path) >= MaxRouteDepth {
		w.dropped++
		return
	}

	step := routeStep(node)
	// Copy rather than append in place: sibling subtrees must not share the
	// backing array, or the last one written wins for every route already
	// recorded. This is the bug that would make every path in the output look
	// like the deepest one.
	here := make([]domain.RouteStep, len(path), len(path)+1)
	copy(here, path)
	here = append(here, step)
	depth := len(here) - 1

	inh = inherit(node, inh, depth)

	kids := childRoutes(node)

	// Which children are unreachable, in evaluation order. `blocked` flips the
	// moment a matcher-less child without `continue` is seen: it takes
	// everything, so no later sibling is ever evaluated.
	//
	// `deliversItself` is computed from SIBLING STRUCTURE ALONE and deliberately
	// ignores `shadowed`. Whether this node can be reached at all is a separate
	// question from whether it would be the delivery point if it were, and mixing
	// them would make every node inside a shadowed subtree look like a delivery
	// point and fill the cap with routes that cannot fire.
	childShadowed := make([]bool, len(kids))
	blocked := false
	deliversItself := true
	for i, kid := range kids {
		childShadowed[i] = shadowed || blocked
		if blocked {
			continue
		}
		s := routeStep(kid)
		if s.CatchAll() {
			// Alertmanager's `Match` appends this child's result and therefore
			// never falls back to the parent. The parent stops being a delivery
			// point entirely — not "usually", ever.
			deliversItself = false
			if !s.Continue {
				blocked = true
			}
		}
	}

	if deliversItself {
		w.record(domain.ReceiverRoute{
			Receiver:       inh.receiver,
			Path:           here,
			GroupWait:      inh.groupWait,
			GroupInterval:  inh.groupInterval,
			RepeatInterval: inh.repeatInterval,
			GroupBy:        inh.groupBy,
			GroupByAll:     inh.groupByAll,
			Shadowed:       shadowed,
		})
	}

	for i, kid := range kids {
		w.visit(kid, here, inh, childShadowed[i])
	}
}

// record appends one delivering route, or counts it as dropped once the cap is
// reached. The cap is never silent: `Dropped` is carried to the wire.
func (w *walker) record(r domain.ReceiverRoute) {
	if len(w.routes) >= domain.MaxResolvedRoutes {
		w.dropped++
		return
	}
	w.routes = append(w.routes, r)
}

// inherit applies one node's own settings on top of what it inherited, which is
// exactly what `dispatch.NewRoute` does: copy the parent's options, then override
// only the fields this route states.
func inherit(node map[string]any, inh inherited, depth int) inherited {
	if name := strings.TrimSpace(asString(node["receiver"])); name != "" {
		inh.receiver = name
	}
	inh.groupWait = inheritTiming(node, "group_wait", inh.groupWait, depth)
	inh.groupInterval = inheritTiming(node, "group_interval", inh.groupInterval, depth)
	inh.repeatInterval = inheritTiming(node, "repeat_interval", inh.repeatInterval, depth)

	if labels, all, present := routeGroupBy(node); present {
		inh.groupBy, inh.groupByAll = labels, all
	}
	return inh
}

// inheritTiming overrides one timing when this route states it, and records the
// depth that did so.
//
// ⛔ AN UNPARSEABLE VALUE INHERITS RATHER THAN CLEARING. `promDurationField`
// reports false for both "absent" and "not a duration", and in both cases
// Alertmanager's own behaviour is the parent's value (an unparseable duration
// would have failed the config load, so a live Alertmanager never has one). Zeroing
// the inherited value here would invent an override that does not exist.
func inheritTiming(node map[string]any, field string, cur domain.InheritedTiming, depth int) domain.InheritedTiming {
	d, ok := promDurationField(node, field)
	if !ok {
		return cur
	}
	v := d
	return domain.InheritedTiming{Value: &v, FromDepth: depth}
}

// routeStep normalises one route's own matchers and its `continue`.
func routeStep(node map[string]any) domain.RouteStep {
	matchers, deprecated := routeMatchers(node)
	cont, _ := node["continue"].(bool)
	return domain.RouteStep{Matchers: matchers, Deprecated: deprecated, Continue: cont}
}

// routeMatchers reads a route's matchers in ALL THREE spellings and renders them
// in the current one.
//
// `match` and `match_re` are deprecated upstream but are still accepted, still
// common in configs written years ago, and still routing production traffic —
// ignoring them would silently report a route as matcher-less, which is the one
// structural claim oto uses to decide reachability. Getting that wrong would
// print "this route can never fire" about a route that fires constantly.
//
// The rendering is Alertmanager's matcher syntax with the value quoted, so what
// oto shows can be pasted straight into `amtool`.
func routeMatchers(node map[string]any) ([]string, bool) {
	var out []string
	deprecated := false

	if list, ok := node["matchers"].([]any); ok {
		for _, item := range list {
			if s := strings.TrimSpace(asString(item)); s != "" {
				out = append(out, s)
			}
		}
	}
	if m, ok := node["match"].(map[string]any); ok {
		out = append(out, renderMatchMap(m, "=")...)
		deprecated = deprecated || len(m) > 0
	}
	if m, ok := node["match_re"].(map[string]any); ok {
		out = append(out, renderMatchMap(m, "=~")...)
		deprecated = deprecated || len(m) > 0
	}

	sort.Strings(out)
	return out, deprecated
}

// renderMatchMap turns a deprecated `match`/`match_re` map into matcher strings,
// key-sorted so the same config always renders the same way — a Go map iterated
// in its own order would make the stored route list churn on every probe.
func renderMatchMap(m map[string]any, op string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+op+strconv.Quote(asString(m[k])))
	}
	return out
}

// routeGroupBy reads `group_by`, including the `...` form.
//
// `group_by: ['...']` means "group by every label", which no timing captures and
// which makes storm collapse unreachable at any threshold, because no group ever
// accumulates a second member. The third return says whether the route stated
// anything at all, because an absent `group_by` INHERITS and an empty one does
// not.
func routeGroupBy(node map[string]any) ([]string, bool, bool) {
	raw, present := node["group_by"]
	if !present {
		return nil, false, false
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false, false
	}
	out := make([]string, 0, len(list))
	all := false
	for _, item := range list {
		s := strings.TrimSpace(asString(item))
		switch s {
		case "":
			continue
		case "...":
			all = true
		default:
			out = append(out, s)
		}
	}
	if all {
		return nil, true, true
	}
	return out, false, true
}

// childRoutes reads a route's `routes:` list, skipping anything that is not a
// mapping. A malformed entry is dropped rather than failing the whole parse: the
// config is a moving target and reporting "unknown" for one branch beats
// reporting nothing for the source.
func childRoutes(node map[string]any) []map[string]any {
	list, ok := node["routes"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if child, ok := item.(map[string]any); ok {
			out = append(out, child)
		}
	}
	return out
}

// identifyReceiver works out which receiver is OTO'S OWN.
//
// ⛔⛔ THE OBVIOUS METHOD IS CLOSED AND IT IS WORTH KNOWING WHY. oto's ingest path
// is `/api/v1/ingest/alertmanager/{source_id}`, so the webhook URL in an
// operator's config literally contains the id of the source oto is probing — it
// would be an exact, unambiguous identification. It is unavailable:
// `webhook_config.url` is a `SecretURL`, and `config.original` is the MARSHALLED
// config, so every secret comes back as the literal string `<secret>`. The
// checked-in `testdata/compose_v0.28.1.yaml` is a real capture of oto's own
// compose Alertmanager and reads `url: <secret>`.
//
// So this is an inference, it is deliberately narrow, and its basis travels with
// its answer:
//
//   - exactly one webhook receiver — that is oto's. This is the shape SPEC §J.1
//     tells every operator to deploy ("the receiver block is the ENTIRE
//     Alertmanager change oto requires"), so the common install is answered
//     exactly rather than hedged into uselessness;
//   - several — oto says AMBIGUOUS and hands back every candidate. Picking one
//     would be a coin toss presented as a reading, which is the same failure as
//     the hand-typed form this whole feature replaced;
//   - none — no receiver in this config can reach oto at all, which is a real
//     finding about a source, not a parser shortfall.
func identifyReceiver(receivers []map[string]any) (string, domain.ReceiverBasis, []string) {
	var webhooks []string
	for _, r := range receivers {
		name := strings.TrimSpace(asString(r["name"]))
		if name == "" {
			continue
		}
		if list, ok := r[webhookConfigsKey].([]any); ok && len(list) > 0 {
			webhooks = append(webhooks, name)
		}
	}

	switch len(webhooks) {
	case 0:
		return "", domain.ReceiverNoWebhook, nil
	case 1:
		return webhooks[0], domain.ReceiverSoleWebhook, webhooks
	default:
		return "", domain.ReceiverAmbiguous, webhooks
	}
}
