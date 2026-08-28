package scope

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The SPA catch-all must not be able to answer for the API.
//
// ⛔ THIS IS A DISPATCH QUESTION AND NOT A ROUTE-PATTERN ONE, which is why it
// sends requests instead of walking the trie like its neighbour. `GET /*` is
// mounted for the UI (internal/app/ui.go). If chi ever resolved it ahead of the
// `/api/v1` mount — a refactor, a middleware that rewrites paths, a chi major
// version — then `/api/v1/anything-mistyped` would answer 200 with HTML.
//
// ⭐ AND THAT DEFECT IS INVISIBLE TO EVERY HUMAN CHECK. Someone opens the app,
// the UI works perfectly, every page loads. The damage lands on programmatic
// callers: a client asks for a slightly wrong path and gets HTML with a success
// status, so its JSON decoder fails somewhere downstream with an error naming
// neither oto nor the route. Nobody looks at the server.
//
// ⚠️ IT MUST HOLD WHETHER OR NOT `npm run build` HAS RUN, and getting that wrong
// once already cost this gate its meaning. The catch-all has two faces: with a
// built `web/dist` it serves index.html with a 200, and without one it answers
// 503 explaining that no UI is embedded. A developer's machine usually has the
// first and a fresh clone always has the second, so a gate keyed to either
// single status passes vacuously in half the world. `answeredByUI` recognises
// both, and the tests below are phrased entirely in terms of it.
// ---------------------------------------------------------------------------

func getPath(t *testing.T, h http.Handler, path string) (int, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, rec.Header().Get("Content-Type"), string(body)
}

// answeredByUI reports whether a response came from the SPA catch-all, in either
// of its two states.
func answeredByUI(status int, ctype, body string) bool {
	if status == http.StatusServiceUnavailable && strings.Contains(body, "no web UI is embedded") {
		return true
	}
	return status == http.StatusOK && strings.Contains(strings.ToLower(ctype), "text/html")
}

// TestTheCatchAllIsActuallyMounted is the anchor guard for the two tests below.
//
// Without it they pass vacuously: if `/*` were not mounted at all, no path could
// possibly be swallowed by it, and the gate would report success while the
// feature it guards had been deleted.
func TestTheCatchAllIsActuallyMounted(t *testing.T) {
	h := mountedHandler(t)

	for _, p := range []string{"/", "/alerts", "/settings/notifications"} {
		status, ctype, body := getPath(t, h, p)
		if !answeredByUI(status, ctype, body) {
			t.Fatalf("GET %s answered %d (%s) and not the SPA catch-all.\n\n"+
				"Either the catch-all is no longer mounted — in which case the two "+
				"gates below prove nothing and every client-side route is a 404 in "+
				"production — or the handler's two responses changed and "+
				"answeredByUI needs to follow them.\nbody: %s",
				p, status, ctype, truncate(body))
		}
	}
}

// TestTheCatchAllNeverAnswersAnAPIPath is the gate.
func TestTheCatchAllNeverAnswersAnAPIPath(t *testing.T) {
	h := mountedHandler(t)

	// Each of these is a path a real client gets wrong: a typo, a stale route
	// from an older client, a trailing segment, a path that only exists in
	// somebody's notes.
	for _, p := range []string{
		"/api/v1/definitely-not-a-route",
		"/api/v1/alerts/not-a-uuid/nope",
		"/api/v1/",
		"/api/v1/version/extra",
		"/api/v2/alerts",
	} {
		t.Run(p, func(t *testing.T) {
			status, ctype, body := getPath(t, h, p)

			if answeredByUI(status, ctype, body) {
				t.Fatalf("GET %s was answered by the SPA catch-all (%d, %s).\n\n"+
					"An API path must never reach the UI handler. `/api/**` is the "+
					"API's namespace whether or not a version of it is mounted — "+
					"`/api/v2/alerts` reached this handler exactly this way and was "+
					"served index.html with a 200, so a client pinned to a version "+
					"oto does not serve got HTML where it expected JSON, and "+
					"nothing in oto reported a problem.", p, status, ctype)
			}
			if strings.Contains(strings.ToLower(ctype), "text/html") {
				t.Fatalf("GET %s answered with Content-Type %q.\n\n"+
					"No path under /api/ may ever answer HTML.", p, ctype)
			}
		})
	}
}

// TestTheCatchAllNeverAnswersAnOpsPath keeps the four unauthenticated
// operational routes out of the SPA's reach.
//
// ⛔ EACH OF THESE IS LOAD-BEARING FOR SOMETHING THAT IS NOT A BROWSER. /healthz
// is a liveness probe — answered by the SPA it would keep a broken pod alive, or
// (worse, when the UI is absent) a 503 would make Kubernetes restart every pod in
// the deployment. /readyz gates the load balancer, /metrics feeds Prometheus, and
// /openapi.json is the published contract a generated client reads.
func TestTheCatchAllNeverAnswersAnOpsPath(t *testing.T) {
	h := mountedHandler(t)

	// Metrics is disabled in this container (see mountedHandler), so it is
	// deliberately not in this list: asserting it here would be asserting the
	// test's own configuration.
	for _, p := range []string{"/healthz", "/readyz", "/openapi.json"} {
		t.Run(p, func(t *testing.T) {
			status, ctype, body := getPath(t, h, p)
			if answeredByUI(status, ctype, body) {
				t.Fatalf("GET %s was answered by the SPA catch-all (%d). "+
					"An operational route answered by the UI is a probe that "+
					"reports on the wrong thing.", p, status)
			}
			if status != http.StatusOK {
				t.Fatalf("GET %s answered %d, wanted 200. body: %s", p, status, truncate(body))
			}
		})
	}
}

func truncate(s string) string {
	const limit = 200
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
