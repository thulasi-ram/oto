package scope

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// ADR 0054 §3, owner ruling 2026-10-05 on git-bug eb4f21b, REFINED BY OWNER RULING O3
// (2026-10-06): NO HTTP ROUTE WRITES A REMEDY RISK RULE DIRECTLY.
//
// The rules say whether a Remedy needs one approval or two. A rule saying one lets one grant
// holder approve alone, so writing one is the same authority as granting a second approver —
// which is given from the host shell only (remedy_approver_routes_test.go). 5ace8f3 let any
// member `PUT` the rules, and that loophole is what this test was written to keep closed.
//
// O3 reopened the Settings screen on one condition: a change from the app is PROPOSED by one
// member and CONFIRMED by a DIFFERENT member's browser session, and only the confirmation writes
// (from internal/app, behind a CHECK that refuses a self-confirmed row). So the allowed set is
// exact: the read, the proposal (which changes no rule), the confirmation, and the discard. Any
// other route that names the rules — a `PUT` on them, a `POST` that applies without confirming, a
// second path — is the loophole again, and fails here.
//
// ⭐ THE SUBJECT IS THE MOUNTED TRIE, for AC-51's reason: a grep for a literal misses a
// route assembled from a constant or mounted through a sub-router. The predicate is
// broad — any segment naming risk, or a remedy rule.
//
// ⚠️ WHAT THIS CANNOT SEE: a rule written as a side effect of some other route's body.
// That is held by the shape of the code instead — investigator/repository's
// RemedyRiskRepository and RiskChangeRepository, and the service's ports, never write
// `remedy_risk_rules` or `remedy_risk_settings`; the only INSERT/DELETE on them is in
// internal/app (remedyrules.go, remedyrulechanges.go), reached from cmd/oto and from the
// confirmation route's service.
// ---------------------------------------------------------------------------

// riskWords are the segment fragments a route about the risk rules would carry.
var riskWords = []string{"risk", "remedy-rule", "remedy_rule", "approval-rule", "approval_rule"}

// allowedRiskRoutes is exact. The read, and the three verbs of a two-person change.
var allowedRiskRoutes = map[string]string{
	"GET /api/v1/remedy-risk-rules":                       "the rules, and the change waiting for a second person",
	"POST /api/v1/remedy-risk-rules/changes":              "a PROPOSAL: writes no rule, takes a browser session",
	"POST /api/v1/remedy-risk-rules/changes/{id}/confirm": "the one that writes, by a DIFFERENT member's browser session",
	"POST /api/v1/remedy-risk-rules/changes/{id}/discard": "withdraws or refuses a proposal; writes nothing",
}

func riskRoutes(rs []route) []route {
	var bad []route
	for _, r := range rs {
		if _, ok := allowedRiskRoutes[r.String()]; ok {
			continue
		}
		for _, seg := range strings.Split(strings.ToLower(r.pattern), "/") {
			if seg == "" || strings.HasPrefix(seg, "{") || seg == "*" {
				continue
			}
			hit := false
			for _, w := range riskWords {
				if strings.Contains(seg, w) {
					hit = true
				}
			}
			if hit {
				bad = append(bad, r)
				break
			}
		}
	}
	return bad
}

// TestNoMountedRouteWritesARemedyRiskRule walks the REAL router.
func TestNoMountedRouteWritesARemedyRiskRule(t *testing.T) {
	rs := walkRoutes(t, mountedRouter(t))
	assertWalkReachedTheWholeTree(t, rs)

	have := map[string]bool{}
	for _, r := range rs {
		have[r.String()] = true
	}
	for key := range allowedRiskRoutes {
		if !have[key] {
			t.Errorf("%q is not mounted: the walk did not reach the investigator's routes, "+
				"or the exemption is stale", key)
		}
	}
	if bad := riskRoutes(rs); len(bad) > 0 {
		t.Errorf("the mounted router serves %d route(s) on the Remedy risk rules beyond the read and the "+
			"two-person change: %v\n\nOwner rulings 2026-10-05 and O3 (2026-10-06): the rules are written "+
			"from the host shell (`oto remedy-rules apply`) or by a change a DIFFERENT member confirmed. A "+
			"route that writes one directly lets one grant holder write a one-approval rule and approve "+
			"alone.", len(bad), routeStrings(bad))
	}
}

// TestTheRiskRuleRouteGateFires plants the routes that must never exist — among them the
// PUT that 5ace8f3 shipped.
func TestTheRiskRuleRouteGateFires(t *testing.T) {
	noop := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	const noun = "remedy-" + "risk-rules"

	v1 := chi.NewRouter()
	v1.Get("/"+noun, noop)
	v1.Post("/"+noun+"/changes", noop) // an allowed route: not reported
	v1.Put("/"+noun, noop)
	v1.Post("/"+noun+"/{name}", noop)
	v1.Post("/"+noun+"/changes/{id}/apply", noop) // a confirm by another name
	v1.Patch("/settings/remedy-rules", noop)
	v1.Get("/remedies/{id}", noop)
	root := chi.NewRouter()
	root.Mount("/api/v1", v1)

	got := routeStrings(riskRoutes(walkRoutes(t, root)))
	sort.Strings(got)
	want := []string{
		"PATCH /api/v1/settings/remedy-rules",
		"POST /api/v1/remedy-risk-rules/changes/{id}/apply",
		"POST /api/v1/remedy-risk-rules/{name}",
		"PUT /api/v1/remedy-risk-rules",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planted %v, gate reported %v", want, got)
	}
}
