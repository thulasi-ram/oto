package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/thulasiram/oto/test/contract/schema"
)

/*
Gate G2 of SPEC §L.8.1 — the running server against api/openapi/openapi.yaml.

⛔ NOTHING IN THIS FILE MAY NAME AN operationId AS A STRING LITERAL, and the
reason is `test/contract/coverage_test.go`. That ratchet decides an operation is
covered by looking for its id, quoted, in any `_test.go` file. A table here that
listed all eighty-five ids would satisfy the ratchet for the whole API in one
stroke and quietly retire it. Probes are therefore keyed by METHOD and PATH
TEMPLATE, and the operationId is resolved from the contract at run time — which
is also the honest direction: a client drives a path, not an id.
*/

// probe is one request the gate makes and one answer it expects.
type probe struct {
	// method is the HTTP method.
	method string
	// tmpl is the path exactly as `paths:` spells it, including {placeholders}.
	// It is the key the operation is resolved by, so it must match the contract
	// character for character.
	tmpl string
	// url overrides the request target when the probe needs query parameters or
	// a fixture id in place of a {placeholder}. `{name}` is expanded from the
	// fixture table.
	url string
	// body is marshalled to JSON. rawBody is sent verbatim, for the probes that
	// are ABOUT bytes the encoder would never produce.
	body    any
	rawBody string
	header  map[string]string
	auth    authMode
	// want is the status this probe expects. Every probe states one: a gate whose
	// probes accept "any status the contract declares" is satisfied by
	// eighty-five 404s.
	want int
	// stream marks a response that never ends, so only its headers are read.
	stream bool
	// capture records members of the response body into the fixture table, as
	// `fixture name -> member path`, so a later probe can address what this one
	// created.
	capture map[string][]string
	// keepsCookie stores the response's Set-Cookie for the probes that must
	// present a SESSION rather than a token.
	keepsCookie bool
	// why documents a probe whose expectation is not the obvious one.
	why string
}

// ⭐ TestTheRunningServerMatchesTheContract is gate G2.
//
// It is ONE test function rather than eighty-five, because the probes share a
// database and build on each other: `createChannel` is what gives `getChannel`
// an id to ask for. Each probe is a subtest, so one failure is one line and the
// rest of the run still reports.
func TestTheRunningServerMatchesTheContract(t *testing.T) {
	w := newWorld(t)

	byRoute := routeIndex(t)
	reached := map[string]int{}

	for _, p := range plan() {
		key := p.method + " " + p.tmpl
		op, ok := byRoute[key]
		if !ok {
			t.Errorf("the probe table drives %s, which the contract does not declare", key)
			continue
		}

		t.Run(key+wantSuffix(p), func(t *testing.T) {
			res := w.call(t, p)

			if res.status != p.want {
				t.Fatalf("%s answered %d, want %d%s\nbody = %s",
					key, res.status, p.want, whySuffix(p), res.body)
			}
			if !op.Declares(res.status) {
				t.Fatalf("%s answered %d, which the contract does not declare for %s (declared: %v)",
					key, res.status, op.ID, op.Statuses)
			}

			assertConformance(t, op, res)

			if p.keepsCookie {
				if res.setCookie == "" {
					t.Fatal("a successful login set no cookie; nothing can hold a session")
				}
				w.cookie = res.setCookie
			}
			for name, path := range p.capture {
				w.record(t, name, res.body, path...)
			}
			if res.status/100 == 2 {
				reached[op.ID] = res.status
			}
		})
	}

	// A run in which the seed collapsed leaves every later probe answering 404,
	// and every 404 validates against the Problem schema. That is a green that
	// means nothing, so the gate states its own floor.
	t.Logf("%d of %d declared operations answered 2xx", len(reached), len(byRoute))
	if len(reached) < minimumSuccessfulOperations {
		t.Errorf("only %d operation(s) answered 2xx; at least %d must, or this gate is "+
			"validating the shape of its own failures", len(reached), minimumSuccessfulOperations)
	}
}

// minimumSuccessfulOperations is the floor described above, and it is the number
// the table actually reaches, not a comfortable margin below it. It is a
// RATCHET: raise it when the gate drives more of the API for real, never lower
// it. The ones that do not reach 2xx each carry their reason in the probe's
// `why` — a fixture with no rule provenance, a drill that cannot finish in a
// container that works no jobs, a Slack callback nobody signed.
//
// ⛔⛔ IT WAS 80 AND IS NOW 77, AND THAT IS THE ONE MOVE THE COMMENT ABOVE FORBIDS,
// SO HERE IS THE ARITHMETIC RATHER THAN AN ASSURANCE. git-bug `7570090` DELETED NINE
// OPERATIONS — the whole `/api/v1/alert-groups*` surface — so the DENOMINATOR fell
// from 94 to 85. The floor is an absolute count, so a smaller API lowers it by
// construction without the gate having weakened at all.
//
//	before   >= 80 of 94 reached 2xx   =>  at most 14 structurally unprobeable
//	after       77 of 85 reached 2xx   =>           8 structurally unprobeable
//
// The unprobeable set SHRANK from at most fourteen to eight, so proportionally this
// gate drives MORE of the API than it did. And if every one of the nine deleted
// operations had been reaching 2xx, the mechanical expectation now would be 71; the
// table reaches 77, six better.
//
// ⚠️ THE RATCHET RULE STILL STANDS AND IS NOT SUSPENDED BY THIS. 77 is again the
// number the table actually reaches. Raise it when the gate drives more; lower it
// ONLY with this shape of arithmetic showing the API itself got smaller, never
// because probes started failing.
const minimumSuccessfulOperations = 77

/* -------------------------------------------------------------------------- */
/* The three assertions                                                       */
/* -------------------------------------------------------------------------- */

// assertConformance is points (2) and (3) of the package doc: the media type the
// server chose, and the bytes it wrote.
func assertConformance(t *testing.T, op schema.Operation, res response) {
	t.Helper()

	declared, err := schema.MediaTypes(op.ID, res.status)
	if err != nil {
		t.Fatalf("%s %d: %v", op.ID, res.status, err)
	}

	if len(declared) == 0 {
		// The contract declares no body. `schema.AssertNoBody` proves both halves:
		// that the contract really says so, and that the handler agreed.
		schema.AssertNoBody(t, op.ID, res.status, res.body)
		return
	}

	if !contains(declared, res.mediaType) {
		t.Fatalf("%s %d: the server wrote Content-Type %q; the contract declares %v.\n"+
			"A body that validates but arrives as the wrong media type is one no generated "+
			"client will parse.\nbody = %s",
			op.ID, res.status, res.mediaType, declared, res.body)
	}

	switch res.mediaType {
	case "application/problem+json":
		// AssertProblem adds the two cross-checks a schema cannot express: the
		// `status` member must agree with the status line, and `code` must be
		// there for a client to branch on.
		schema.AssertProblem(t, op.ID, res.status, res.body)
	case schema.MediaTypeJSON:
		if res.status >= 400 {
			schema.AssertProblem(t, op.ID, res.status, res.body)
			return
		}
		schema.Assert(t, op.ID, res.status, res.body)
	default:
		// text/plain, text/event-stream. There is no JSON to validate; what the
		// contract promises about these is the media type, and that has just been
		// checked. Assert the contract really declares a schema for it, so that a
		// media type nobody described cannot slip through as "not JSON".
		if _, err := schema.Response(op.ID, res.status, res.mediaType); err != nil {
			t.Fatalf("%s %d: %s: %v", op.ID, res.status, res.mediaType, err)
		}
	}
}

/* -------------------------------------------------------------------------- */
/* Exhaustiveness                                                             */
/* -------------------------------------------------------------------------- */

// notDriven is the honest record of what the probe table does NOT call, with the
// reason. It may only ever SHRINK — an entry that stops matching a real route is
// an error, so a route that is finally driven cannot keep its exemption.
var notDriven = map[string]string{}

// ⭐ TestEveryDeclaredOperationIsDriven is what stops this gate ageing.
//
// It needs no server and no Docker: it compares two lists. An operation added to
// the contract tomorrow with a handler and no probe fails HERE, on the day it is
// written — which is the only moment the fix is cheap.
func TestEveryDeclaredOperationIsDriven(t *testing.T) {
	t.Parallel()

	driven := map[string]bool{}
	for _, p := range plan() {
		driven[p.method+" "+p.tmpl] = true
	}

	var missing, stale []string
	for _, op := range schema.Operations(t) {
		key := op.Method + " " + op.Path
		_, exempt := notDriven[key]
		switch {
		case driven[key] && exempt:
			stale = append(stale, key)
		case !driven[key] && !exempt:
			missing = append(missing, key)
		}
	}

	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d operation(s) are declared in api/openapi/openapi.yaml and driven by no G2 probe:\n  %s\n\n"+
			"Add a probe to plan() in conformance_test.go stating the status you expect. If one\n"+
			"genuinely cannot be driven, add it to notDriven WITH THE REASON.",
			len(missing), strings.Join(missing, "\n  "))
	}

	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d route(s) are listed in notDriven but ARE driven; delete their entries:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// TestTheExemptionListNamesRealRoutes, so an entry left behind after a rename
// cannot go on excusing a route that no longer exists.
func TestTheExemptionListNamesRealRoutes(t *testing.T) {
	t.Parallel()

	declared := map[string]bool{}
	for _, op := range schema.Operations(t) {
		declared[op.Method+" "+op.Path] = true
	}
	for key, why := range notDriven {
		if !declared[key] {
			t.Errorf("notDriven names %q (%q), which the contract does not declare", key, why)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("notDriven[%q] has no reason; an undriven operation costs an argument", key)
		}
	}
}

// TestTheProbeTableDrivesNoRouteTwiceUnannounced keeps the table readable: a
// route may appear more than once (a happy path and its negative twin), but the
// duplicates must differ in what they expect, or one of them is dead weight.
func TestTheProbeTableIsWellFormed(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, p := range plan() {
		if p.want == 0 {
			t.Errorf("%s %s states no expected status", p.method, p.tmpl)
		}
		// A `{placeholder}` left in the request target produces a 404 that reads
		// like a tenancy answer, so a templated route must state a `url`.
		if p.url == "" && strings.Contains(p.tmpl, "{") {
			t.Errorf("%s %s is templated and states no url; the request would ask for the literal %q",
				p.method, p.tmpl, p.tmpl)
		}
		sig := fmt.Sprintf("%s %s %d %v %s", p.method, p.tmpl, p.want, p.auth, p.url)
		if seen[sig] {
			t.Errorf("%s %s is probed twice with the same expectation (%d)", p.method, p.tmpl, p.want)
		}
		seen[sig] = true
	}
}

/* -------------------------------------------------------------------------- */
/* Plumbing                                                                   */
/* -------------------------------------------------------------------------- */

// routeIndex maps "METHOD /path/{template}" to the operation the contract
// declares there.
func routeIndex(t *testing.T) map[string]schema.Operation {
	t.Helper()
	out := map[string]schema.Operation{}
	for _, op := range schema.Operations(t) {
		out[op.Method+" "+op.Path] = op
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// wantSuffix disambiguates the subtest names of a route probed more than once.
func wantSuffix(p probe) string {
	switch {
	case p.auth == authNone && p.want == http.StatusUnauthorized:
		return " (no credential)"
	case strings.Contains(p.url, "{{stranger}}"):
		return " (another tenant's id)"
	case p.want >= 400:
		return fmt.Sprintf(" (%d)", p.want)
	default:
		return ""
	}
}

func whySuffix(p probe) string {
	if p.why == "" {
		return ""
	}
	return " — " + p.why
}

// idempotency is the header the mutating endpoints take. A fixed value per
// probe is deliberate: replaying the table must not depend on a clock.
func idempotency(key string) map[string]string {
	return map[string]string{"Idempotency-Key": "g2-" + key}
}
