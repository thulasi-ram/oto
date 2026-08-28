package scope

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// ---------------------------------------------------------------------------
// AC-51 (SPEC.md:3904-3905, §P-19b)
//
//	There is no route matching `/resolve`, `/close`, `/merge`, `/dismiss` or
//	`/reopen` in the mounted router, asserted by walking `chi`'s route tree at
//	test time (§E.1.1).
//
// ⭐ THE SUBJECT IS THE TRIE, NOT THE SOURCE. SPEC §E.1.1 is blunt about the
// property (SPEC.md:1816): "There is no `POST /api/v1/alerts/{id}/resolve`,
// ever. Nor `/close`, `/merge`, `/dismiss`…". oto has three human verbs — ack,
// unack, snooze — and a fourth would make it an incident tool.
//
// A grep for `"/resolve"` in `internal/*/api/router.go` answers a question about
// string literals. `chi.Walk` answers the question the AC asks, and the gap
// between them is where a fourth verb would actually arrive:
//
//   - `r.Post("/"+verb, h)` where `verb` is a constant, or a `map[string]http.HandlerFunc`
//     range, or a `fmt.Sprintf`. No literal `/resolve` exists anywhere in the tree
//     and the route is served.
//   - a route mounted through a sub-router in a package the grep's path filter
//     never visited, or added by a middleware that installs its own mux.
//   - a route whose literal lives in a file the grep excluded — a second copy in
//     another package is exactly the failure mode a text match cannot see.
//
// Conversely a grep FIRES on things that are not routes at all: the word
// `resolve` appears in this repo constantly and legitimately (`resolveSource`,
// "group resolution", `ResolveGraceS`). A gate that fires on prose is a gate that
// gets switched off.
//
// ⚠️ THE ROUTER IS THE REAL ONE. `app.Container.Router()` with a real database
// behind it, so every module's `Mount` actually runs. A hand-assembled router in
// the test would be a gate on the test's own wiring.
// ---------------------------------------------------------------------------

// resolutionVerbs are the five AC-51 names. They are the verbs that CLOSE
// something on a human's say-so; oto's signals close because the signal stopped
// firing, and never because somebody clicked.
var resolutionVerbs = []string{"resolve", "close", "merge", "dismiss", "reopen"}

// allowedResolutionRoutes is the exact-match exemption list, and it is EXACT for
// a reason: one (method, pattern) pair per entry, with the argument for it.
//
// ⭐⭐ THE VERB MATCHER IS NOT LOOSENED, AND THAT IS THE POINT. AC-51 forbids
// five verbs because they CLOSE A SIGNAL ON A HUMAN'S SAY-SO — oto has three
// human verbs and a fourth would make it an incident tool. `resolutionVerbs`
// matches on segment-contains precisely so that `resolve-all` and `bulk-close`
// cannot slip past it, and that breadth is what makes it worth having. Teaching
// the predicate to understand "resolve, but the other sense of resolve" is how a
// gate stops meaning anything: the next `/resolve` would have to argue with a
// regex instead of with a reviewer.
//
// So the breadth stays and the exception is named, one route at a time, here —
// where a reviewer sees it in the diff.
//
// ⛔ THE ONE ENTRY IS NOT A SIGNAL VERB AT ALL. `POST
// /api/v1/channel-connections/{id}/slack/resolve` (ADR 0047) resolves a Slack
// channel NAME to its ID, or the reverse — it is a metadata lookup at
// configuration time, on a `channel_connections` row, triggered by an admin
// typing into a settings form. It closes nothing, it touches no Alert and no
// Case, and the noun it is mounted under is a credential, not a signal. §E.1.1's
// property — "there is no POST /api/v1/alerts/{id}/resolve, ever" — is untouched
// by it. The English collision is total and the semantic overlap is zero.
var allowedResolutionRoutes = map[string]string{
	"POST /api/v1/channel-connections/{id}/slack/resolve": "ADR 0047: resolves a Slack " +
		"channel name to its id at configuration time. Not a signal verb — it closes " +
		"nothing and is mounted on a connection, not on an alert or a case.",
}

// route is one entry in the mounted trie.
type route struct {
	method  string
	pattern string
}

func (r route) String() string { return r.method + " " + r.pattern }

// walkRoutes flattens the mounted router into every (method, pattern) pair it
// actually serves, descending through every Mount and Group.
func walkRoutes(t *testing.T, routes chi.Routes) []route {
	t.Helper()

	var out []route
	err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, route{method: method, pattern: pattern})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the route tree: %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// resolutionRoutes is the predicate.
//
// ⭐ IT MATCHES PATH SEGMENTS, NOT THE WHOLE PATTERN, and that is the decision
// worth stating. A substring test over the full pattern would fire on
// `/api/v1/alerts/{id}/enrichments` for `rich`-shaped verbs and, worse, would
// let `/api/v1/alerts/{id}/resolve-all` through if it insisted on an exact
// match. Splitting on `/` and asking whether any segment CONTAINS a resolution
// verb catches `resolve`, `resolve-all`, `bulk-close` and `reopen_group` alike,
// while `{id}` and `/rollups` are ordinary segments that contain none of them.
//
// Path parameters are skipped: `/{closed_at}` names a field, not a verb.
func resolutionRoutes(rs []route) []route {
	var bad []route
	for _, r := range rs {
		if _, allowed := allowedResolutionRoutes[r.String()]; allowed {
			continue
		}
		for _, seg := range strings.Split(r.pattern, "/") {
			if seg == "" || strings.HasPrefix(seg, "{") || seg == "*" {
				continue
			}
			seg = strings.ToLower(seg)
			if slices.ContainsFunc(resolutionVerbs, func(v string) bool { return strings.Contains(seg, v) }) {
				bad = append(bad, r)
				break
			}
		}
	}
	return bad
}

// anchorRoutes are routes the mounted tree MUST contain. They are the
// anti-vacuity guard: `chi.Walk` that fails to descend into `Mount` returns two
// ops routes and no error, and `resolutionRoutes` over two routes is empty —
// green, forever, having examined none of the versioned API.
//
// ⛔ THESE ARE NOT A CONTRACT TEST. `test/contract` owns whether the served
// surface matches `api/openapi/`. These four exist only to prove the walk
// reached the ingest surface, the identity surface, a domain under the
// authenticated group, and the ops root — the four separately-mounted regions of
// `internal/app/routes.go`. If one is renamed, replace it with its neighbour.
var anchorRoutes = []route{
	{method: "GET", pattern: "/healthz"},
	{method: "POST", pattern: "/api/v1/cases/{id}/ack"},
	{method: "GET", pattern: "/api/v1/me"},
	{method: "POST", pattern: "/api/v1/ingest/alertmanager/{source_id}"},
}

// missingAnchors is the anti-vacuity predicate, kept separate from the assertion
// so its own companion test can feed it a truncated walk.
func missingAnchors(rs []route) []route {
	have := map[string]bool{}
	for _, r := range rs {
		have[r.String()] = true
	}
	var missing []route
	for _, want := range anchorRoutes {
		if !have[want.String()] {
			missing = append(missing, want)
		}
	}
	return missing
}

func assertWalkReachedTheWholeTree(t *testing.T, rs []route) {
	t.Helper()

	if missing := missingAnchors(rs); len(missing) > 0 {
		t.Fatalf("the route walk did not find %v among the %d routes it saw.\n\n"+
			"AC-51 is asserted by walking the MOUNTED tree, so a walk that misses a "+
			"whole region reports \"no forbidden route\" about a region it never "+
			"visited. Either chi.Walk stopped descending at a Mount, or an anchor "+
			"was renamed and `anchorRoutes` was not updated with it.\n\nsaw:\n  %s",
			routeStrings(missing), len(rs), strings.Join(routeStrings(rs), "\n  "))
	}
}

func routeStrings(rs []route) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.String())
	}
	return out
}

// forbiddenRouteFailure is the message a violation earns.
func forbiddenRouteFailure(bad []route) string {
	return fmt.Sprintf(
		"the mounted router serves %d resolution route(s): %s\n\n"+
			"AC-51 (SPEC §E.1.1, SPEC.md:1816): there is no `/resolve`, `/close`, `/merge`, "+
			"`/dismiss` or `/reopen` in the mounted router, ever.\n\n"+
			"oto has THREE human verbs — ack, unack, snooze — and every one of them writes "+
			"NOTIFICATION state. A signal's state is a fact about the signal: it closes when "+
			"it stops firing, not when somebody decides it is handled. A fourth verb that "+
			"closes an alert on a human's say-so is the first endpoint of an incident tool, "+
			"and the endpoint is the cheapest part of it to add.\n\n"+
			"If that is genuinely the product now, it is an ADR and a SPEC amendment (§N) — "+
			"not a route.",
		len(bad), strings.Join(routeStrings(bad), ", "))
}

// mountedRouter builds the WHOLE product's HTTP surface: a real container over a
// real, fully-migrated database, so that every module's `Mount` runs.
func mountedRouter(t *testing.T) chi.Routes {
	t.Helper()

	handler := mountedHandler(t)
	routes, ok := handler.(chi.Routes)
	if !ok {
		t.Fatalf("Router() returned %T, which does not implement chi.Routes.\n\n"+
			"AC-51 is asserted by WALKING the tree. A router that cannot be walked "+
			"cannot be asserted about, and swapping chi for something opaque is a "+
			"decision that has to take this gate with it.", handler)
	}
	return routes
}

// mountedHandler is the same real surface, kept as an http.Handler so a gate can
// SEND a request rather than only walk the trie. ui_catchall_test.go needs that:
// "does `/*` swallow an API path" is a question about dispatch at request time,
// and a route pattern cannot answer it.
func mountedHandler(t *testing.T) http.Handler {
	t.Helper()

	h := harness.New(t)

	cfg := config.Default()
	cfg.DB.URL = h.DSN
	cfg.HTTP.BaseURL = "http://oto.scope.test"
	// The metrics handler is not a domain route and its path is configurable; a
	// gate about the domain surface should not depend on it.
	cfg.Telemetry.MetricsEnabled = false
	// A generated keyring key so that every module builds. Without one the
	// container boots deliberately keyring-less, and a gate that asserts an
	// ABSENCE must never run against a partially-assembled tree.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("keyring key: %v", err)
	}
	cfg.Security.SecretKey = base64.StdEncoding.EncodeToString(key)

	pools, err := db.Open(h.Ctx, cfg.DB)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}

	c, err := app.New(h.Ctx, app.Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pools:  pools,
	})
	if err != nil {
		t.Fatalf("container: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	return c.Router()
}

// TestNoResolutionRouteIsMounted is AC-51.
func TestNoResolutionRouteIsMounted(t *testing.T) {
	rs := walkRoutes(t, mountedRouter(t))
	assertWalkReachedTheWholeTree(t, rs)

	if bad := resolutionRoutes(rs); len(bad) > 0 {
		t.Error(forbiddenRouteFailure(bad))
	}
}

// TestEveryResolutionExemptionIsRealAndNarrow keeps the allowlist honest.
//
// ⛔ AN ALLOWLIST NOBODY CHECKS IS HOW A GATE DIES QUIETLY. Two failure modes,
// both asserted here:
//
//   - a DEAD entry — the route was renamed or deleted and its exemption stayed,
//     so the next route that happens to match that pattern is pre-forgiven;
//   - a WIDE entry — a `{placeholder}` or a `*` where a literal segment should
//     be, which would exempt a subtree rather than one route.
//
// Every entry also has to carry a reason, for the same cause `notDriven` does in
// the conformance gate: an exemption costs an argument.
func TestEveryResolutionExemptionIsRealAndNarrow(t *testing.T) {
	mounted := map[string]bool{}
	for _, r := range walkRoutes(t, mountedRouter(t)) {
		mounted[r.String()] = true
	}

	for key, why := range allowedResolutionRoutes {
		if !mounted[key] {
			t.Errorf("allowedResolutionRoutes exempts %q, which the mounted router does not "+
				"serve. A stale exemption pre-forgives whatever arrives at that pattern next; "+
				"delete it with the route.", key)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("allowedResolutionRoutes[%q] has no reason. AC-51 is a decision, and an "+
				"exception to it costs an argument a reviewer can read.", key)
		}
		// The exempted route must itself be one the predicate WOULD have caught,
		// or the entry is noise that hides nothing and explains nothing.
		method, pattern, ok := strings.Cut(key, " ")
		if !ok {
			t.Errorf("allowedResolutionRoutes key %q is not `METHOD /pattern`", key)
			continue
		}
		bare := []route{{method: method, pattern: pattern}}
		withoutList := allowedResolutionRoutes
		allowedResolutionRoutes = nil
		caught := len(resolutionRoutes(bare)) == 1
		allowedResolutionRoutes = withoutList
		if !caught {
			t.Errorf("allowedResolutionRoutes exempts %q, which the predicate would not have "+
				"flagged anyway. An exemption that forgives nothing is a comment pretending "+
				"to be a gate.", key)
		}
	}
}

// TestTheExemptionDoesNotForgiveASignalVerb is the other half of the teeth.
//
// The allowlist is consulted by exact (method, pattern) match, so a real
// `/resolve` on an alert must still be caught while the connection lookup is
// waved through — the two live in the same walk and only one of them is allowed.
func TestTheExemptionDoesNotForgiveASignalVerb(t *testing.T) {
	rs := []route{
		{method: "POST", pattern: "/api/v1/channel-connections/{id}/slack/resolve"},
		{method: "POST", pattern: "/api/v1/alerts/{id}/resolve"},
		// The same lookup on a DIFFERENT noun is not the exempted route, and an
		// exact-match allowlist has to say so.
		{method: "POST", pattern: "/api/v1/alerts/{id}/slack/resolve"},
		// And not by method either.
		{method: "DELETE", pattern: "/api/v1/channel-connections/{id}/slack/resolve"},
	}

	got := routeStrings(resolutionRoutes(rs))
	sort.Strings(got)
	want := []string{
		"DELETE /api/v1/channel-connections/{id}/slack/resolve",
		"POST /api/v1/alerts/{id}/resolve",
		"POST /api/v1/alerts/{id}/slack/resolve",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the exemption forgave the wrong routes:\n got  %v\n want %v", got, want)
	}
}

// TestResolutionRouteGateFires plants the routes that must never exist and
// checks the real walk reports them.
//
// ⭐ IT IS THE ONLY PROOF THE GATE HAS TEETH, and it plants the two shapes a
// grep provably cannot see:
//
//   - the pattern is assembled from a constant at registration time, so no
//     literal `"/resolve"` exists in any file;
//   - it is registered on a sub-router that is Mounted into another sub-router
//     that is Mounted into the root, so only a walk that descends finds it.
//
// It builds a chi tree and points the REAL walkRoutes and resolutionRoutes at
// it, rather than hand-building a []route: the walk, the segment logic and the
// message are all the production ones.
func TestResolutionRouteGateFires(t *testing.T) {
	// THE VIOLATION, never spelled. Concatenated at registration time, exactly as
	// a fourth human verb would be if somebody generated the CRUD.
	const verb = "resolv" + "e"

	noop := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

	alerts := chi.NewRouter()
	alerts.Route("/{id}", func(r chi.Router) {
		r.Post("/ack", noop)
		r.Post("/"+verb, noop)
		// A second shape: a hyphenated bulk verb, to prove the segment test is not
		// an equality test in disguise.
		r.Post("/bulk-close", noop)
	})

	v1 := chi.NewRouter()
	v1.Mount("/alerts", alerts)
	// Controls. None of these is a resolution verb, and every one of them would
	// trip a careless substring test over the whole pattern or a careless
	// prefix test over the segment.
	v1.Get("/alerts/rollups", noop)
	v1.Post("/alerts/{id}/unsnooze", noop)
	v1.Get("/sources/{id}/reconcile", noop)

	root := chi.NewRouter()
	root.Get("/healthz", noop)
	root.Mount("/api/v1", v1)

	rs := walkRoutes(t, root)

	bad := resolutionRoutes(rs)
	got := routeStrings(bad)
	sort.Strings(got)

	want := []string{
		"POST /api/v1/alerts/{id}/bulk-close",
		"POST /api/v1/alerts/{id}/resolve",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planted exactly %v, gate reported %v\n\nfull tree:\n  %s",
			want, got, strings.Join(routeStrings(rs), "\n  "))
	}

	// The message has to survive too.
	msg := forbiddenRouteFailure(bad)
	for _, want := range []string{"/api/v1/alerts/{id}/resolve", "AC-51", "THREE human verbs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure message does not mention %q:\n%s", want, msg)
		}
	}
}

// TestRouteWalkAnchorGuardFires proves the anti-vacuity guard itself works: a
// walk that stops at the root — the exact failure mode of a chi.Walk that does
// not descend into Mount — must be reported, not accepted as a clean tree.
func TestRouteWalkAnchorGuardFires(t *testing.T) {
	shallow := []route{{method: "GET", pattern: "/healthz"}}

	if len(missingAnchors(shallow)) != len(anchorRoutes)-1 {
		t.Fatalf("a walk that found only /healthz was accepted as a complete tree "+
			"(%d anchors reported missing, want %d).\n\n"+
			"That is the shape of a chi.Walk that stops at the first Mount, and it is "+
			"how a route gate ends up reporting an absence it never looked for.",
			len(missingAnchors(shallow)), len(anchorRoutes)-1)
	}
	if len(resolutionRoutes(shallow)) != 0 {
		t.Fatal("the shallow tree was supposed to contain no forbidden route; " +
			"the point of this test is that its EMPTINESS is not a pass")
	}
}
