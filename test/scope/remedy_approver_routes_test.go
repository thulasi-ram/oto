package scope

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// ADR 0054 §4 (git-bug 47f67c8): NO HTTP ROUTE WRITES A REMEDY APPROVER GRANT.
//
// A grant is given and taken by `oto grant remedy-approver` / `oto revoke
// remedy-approver` from the host shell, and by nothing else. Double approval means
// two DIFFERENT holders; a route that could create a grant would let one holder mint
// a second approver (an alt account) and approve alone. The ruling follows
// bootstrap's own argument: no HTTP route may ever create one.
//
// ⭐ THE SUBJECT IS THE MOUNTED TRIE, for AC-51's reason (forbidden_routes_test.go):
// a grep for a literal misses a route assembled from a constant or mounted through
// a sub-router. The predicate is deliberately broad — any segment naming an
// approver, a grant or a permission — and the one route it may find is the read.
//
// ⚠️ WHAT THIS CANNOT SEE: a grant written as a side effect of some other route's
// body. That is held by the shape of the code instead — the identity repository and
// service have no write method on `remedy_approver_grants`, and the only INSERT is in
// internal/app/remedyapprover.go, called only from cmd/oto.
// ---------------------------------------------------------------------------

// grantWords are the segment fragments a route about a grant would carry.
var grantWords = []string{"approver", "grant", "permission", "authz", "role"}

// allowedGrantRoutes is exact: the one read.
var allowedGrantRoutes = map[string]string{
	"GET /api/v1/tool-servers/{id}/remedy-approvers": "the read-only list of a ToolServer's grant holders",
}

func grantRoutes(rs []route) []route {
	var bad []route
	for _, r := range rs {
		if _, ok := allowedGrantRoutes[r.String()]; ok {
			continue
		}
		for _, seg := range strings.Split(strings.ToLower(r.pattern), "/") {
			if seg == "" || strings.HasPrefix(seg, "{") || seg == "*" {
				continue
			}
			hit := false
			for _, w := range grantWords {
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

// TestNoMountedRouteWritesARemedyApproverGrant walks the REAL router.
func TestNoMountedRouteWritesARemedyApproverGrant(t *testing.T) {
	rs := walkRoutes(t, mountedRouter(t))
	assertWalkReachedTheWholeTree(t, rs)

	have := map[string]bool{}
	for _, r := range rs {
		have[r.String()] = true
	}
	for key := range allowedGrantRoutes {
		if !have[key] {
			t.Errorf("the read %q is not mounted: the walk did not reach the investigator's routes, "+
				"or the exemption is stale", key)
		}
	}
	if bad := grantRoutes(rs); len(bad) > 0 {
		t.Errorf("the mounted router serves %d route(s) on a grant beyond the one read: %v\n\n"+
			"ADR 0054 §4: a Remedy approver is granted and revoked from the host shell only "+
			"(`oto grant` / `oto revoke`). A route that writes one lets a holder mint a second "+
			"approver and defeat double approval.", len(bad), routeStrings(bad))
	}
}

// TestTheGrantRouteGateFires plants the routes that must never exist.
func TestTheGrantRouteGateFires(t *testing.T) {
	noop := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	const noun = "remedy-" + "approvers"

	ts := chi.NewRouter()
	ts.Get("/{id}/"+noun, noop)
	ts.Post("/{id}/"+noun, noop)
	ts.Delete("/{id}/"+noun+"/{user_id}", noop)
	ts.Get("/{id}/tools", noop)
	v1 := chi.NewRouter()
	v1.Mount("/tool-servers", ts)
	v1.Put("/users/{id}/grants", noop)
	root := chi.NewRouter()
	root.Mount("/api/v1", v1)

	got := routeStrings(grantRoutes(walkRoutes(t, root)))
	sort.Strings(got)
	want := []string{
		"DELETE /api/v1/tool-servers/{id}/remedy-approvers/{user_id}",
		"POST /api/v1/tool-servers/{id}/remedy-approvers",
		"PUT /api/v1/users/{id}/grants",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planted %v, gate reported %v", want, got)
	}
}
