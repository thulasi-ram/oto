package scope

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// ADR 0054 §3, owner ruling 2026-10-05 on git-bug eb4f21b: NO HTTP ROUTE WRITES A
// REMEDY RISK RULE.
//
// The rules say whether a Remedy needs one approval or two. A rule saying one lets one
// grant holder approve alone, so writing one is the same authority as granting a second
// approver — and that is given from the host shell only (remedy_approver_routes_test.go).
// The rules are applied by `oto remedy-rules apply --org SLUG -f rules.yaml`, and the
// one route on them is the read.
//
// ⭐ THE SUBJECT IS THE MOUNTED TRIE, for AC-51's reason: a grep for a literal misses a
// route assembled from a constant or mounted through a sub-router. The predicate is
// broad — any segment naming risk, or a remedy rule — and the one route it may find is
// `GET /api/v1/remedy-risk-rules`.
//
// ⚠️ WHAT THIS CANNOT SEE: a rule written as a side effect of some other route's body.
// That is held by the shape of the code instead — investigator/repository's
// RemedyRiskRepository and the service's RemedyRiskStore port only read, and the only
// INSERT/DELETE on `remedy_risk_rules` and `remedy_risk_settings` is in
// internal/app/remedyrules.go, called only from cmd/oto.
// ---------------------------------------------------------------------------

// riskWords are the segment fragments a route about the risk rules would carry.
var riskWords = []string{"risk", "remedy-rule", "remedy_rule", "approval-rule", "approval_rule"}

// allowedRiskRoutes is exact: the one read.
var allowedRiskRoutes = map[string]string{
	"GET /api/v1/remedy-risk-rules": "the read-only rules, shown as managed by `oto remedy-rules`",
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
			t.Errorf("the read %q is not mounted: the walk did not reach the investigator's routes, "+
				"or the exemption is stale", key)
		}
	}
	if bad := riskRoutes(rs); len(bad) > 0 {
		t.Errorf("the mounted router serves %d route(s) on the Remedy risk rules beyond the one read: %v\n\n"+
			"Owner ruling 2026-10-05 (git-bug eb4f21b): the rules are applied from the host shell only "+
			"(`oto remedy-rules apply`). A route that writes one lets one grant holder write a "+
			"one-approval rule and approve alone.", len(bad), routeStrings(bad))
	}
}

// TestTheRiskRuleRouteGateFires plants the routes that must never exist — among them the
// PUT that 5ace8f3 shipped.
func TestTheRiskRuleRouteGateFires(t *testing.T) {
	noop := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	const noun = "remedy-" + "risk-rules"

	v1 := chi.NewRouter()
	v1.Get("/"+noun, noop)
	v1.Put("/"+noun, noop)
	v1.Post("/"+noun+"/{name}", noop)
	v1.Patch("/settings/remedy-rules", noop)
	v1.Get("/remedies/{id}", noop)
	root := chi.NewRouter()
	root.Mount("/api/v1", v1)

	got := routeStrings(riskRoutes(walkRoutes(t, root)))
	sort.Strings(got)
	want := []string{
		"PATCH /api/v1/settings/remedy-rules",
		"POST /api/v1/remedy-risk-rules/{name}",
		"PUT /api/v1/remedy-risk-rules",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planted %v, gate reported %v", want, got)
	}
}
