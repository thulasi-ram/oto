package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/web"
)

// The SPA fallback's rules, pinned.
//
// ⛔ THESE ARE NOT COSMETIC ASSERTIONS. An over-greedy catch-all is the defect
// this feature invites, and it is invisible to every human check: the browser
// looks perfect while `/api/v1/mistyped` answers 200 with HTML and somebody
// else's JSON parser fails downstream. A stale cached index.html asking for a
// deleted bundle is the other one, and it presents as a blank page with no
// server-side symptom at all.
//
// The handler is constructed over an fstest.MapFS rather than the real embed, so
// these run on a checkout that has never built the UI — which is most checkouts,
// and certainly every fresh clone in ci.
func testUIFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":            &fstest.MapFile{Data: []byte("<!doctype html><title>oto</title>")},
		"assets/app-abc123.js":  &fstest.MapFile{Data: []byte("console.log('oto')")},
		"assets/app-abc123.css": &fstest.MapFile{Data: []byte(".a{}")},
		"favicon.ico":           &fstest.MapFile{Data: []byte("\x00\x00\x01\x00")},
	}
}

func TestUIServesTheShellForClientSideRoutes(t *testing.T) {
	h := newUIHandler(testUIFS())

	// Every one of these is a real URL a person can arrive at: a bookmark, a
	// pasted link, a refresh. The app router resolves them in the browser, so the
	// server has to answer with the shell and not a 404.
	for _, p := range []string{"/", "/alerts", "/alerts/9f8e7d6c-0000-4000-8000-000000000001", "/settings/notifications", "/index.html"} {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))

			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
			body, _ := io.ReadAll(rec.Body)
			require.Contains(t, string(body), "<!doctype html>")

			// ⛔ THE SHELL IS NEVER CACHED. It names hashed bundles that the next
			// build deletes, so a cached copy asks for assets that no longer
			// exist — a blank page for as long as the cache lives, on a
			// deployment that is otherwise healthy.
			require.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
		})
	}
}

func TestUIServesHashedAssetsImmutablyAndNeverFallsBackForThem(t *testing.T) {
	h := newUIHandler(testUIFS())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app-abc123.js", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "javascript")
	// The filename carries a content hash, so this URL can never mean different
	// bytes.
	require.Contains(t, rec.Header().Get("Cache-Control"), "immutable")

	// ⛔ THE ONE THAT MATTERS. A missing bundle answered with index.html hands the
	// browser HTML with a 200 and a text/html type for a <script> — a blank page
	// and a console syntax error, with nothing anywhere saying "that file is
	// gone". This is precisely what a stale cached shell requests after a deploy.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app-DELETED.js", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	body, _ := io.ReadAll(rec.Body)
	require.NotContains(t, string(body), "<!doctype html>",
		"a missing asset fell back to the SPA shell")
}

func TestUIRefusesToInventFilesThatLookLikeFiles(t *testing.T) {
	h := newUIHandler(testUIFS())

	// A browser asks for these unprompted. Answering the shell means it keeps
	// asking, and means a monitoring check for "is the favicon there" passes
	// while it is not.
	for _, p := range []string{"/robots.txt", "/manifest.webmanifest", "/sw.js", "/apple-touch-icon.png"} {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			require.Equal(t, http.StatusNotFound, rec.Code)
		})
	}

	// ...while a file that IS there is served on its own terms, revalidated
	// rather than immutable, because its name is stable across builds.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
}

// ⛔ THE DEFECT A GATE CAUGHT, PINNED AT THE UNIT LEVEL TOO.
//
// The first version of this handler reasoned that chi resolves the `/api/v1`
// mount ahead of `/*`, which is true, and concluded the API was safe, which was
// false: `/api/v2/alerts` matches no mount at all, fell through to the SPA, has
// no dot in its last segment, and was served index.html with a 200. A client
// pinned to an API version oto does not serve got HTML where it expected JSON.
//
// The rule is about the NAMESPACE, not about what happens to be mounted — which
// is why `/metrics` is here as well: `telemetry.metrics_enabled: false`
// unregisters it, and a Prometheus scrape answered 200 text/html is a target that
// looks healthy while reporting nothing.
// ⚠️ BOTH STATES OF THE MOUNT, AND THE SECOND ONE IS WHY THIS TABLE EXISTS.
// The refusal was first written inside the serving handler only, so a binary with
// no UI — every fresh clone, and any image whose node stage broke — answered
// `/api/v2/alerts` with 503 and "no web UI is embedded". ci caught it, because
// its checkout has no `web/dist` while a developer's usually does: the same code
// path, opposite defaults, and the local run was the one that lied.
func TestUINeverAnswersForReservedNamespaces(t *testing.T) {
	states := map[string]http.Handler{
		"ui embedded": refuseReserved(newUIHandler(testUIFS())),
		"ui absent":   refuseReserved(http.HandlerFunc(uiAbsent)),
	}

	for state, h := range states {
		for _, p := range []string{
			"/api", "/api/", "/api/v1/anything", "/api/v2/alerts", "/api/v99/x",
			"/healthz", "/readyz", "/metrics", "/openapi.json",
		} {
			t.Run(state+" "+p, func(t *testing.T) {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))

				require.Equal(t, http.StatusNotFound, rec.Code,
					"a reserved path was answered by the UI mount")
				body, _ := io.ReadAll(rec.Body)
				require.NotContains(t, string(body), "<!doctype html>",
					"a reserved path was served the SPA shell")
				require.NotContains(t, string(body), "no web UI is embedded",
					"a reserved path was answered by the no-UI handler")
			})
		}
	}
}

func TestUIAnswersHeadAndServesNoBodyForIt(t *testing.T) {
	h := newUIHandler(testUIFS())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/alerts", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
}

func TestUIDoesNotServeADirectoryListing(t *testing.T) {
	h := newUIHandler(testUIFS())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets", nil))

	// `/assets` is a directory in the FS. http.FileServer would list it; this
	// handler must not, because the listing is an inventory of a deployment's
	// exact build.
	body, _ := io.ReadAll(rec.Body)
	require.NotContains(t, string(body), "app-abc123.js", "the asset directory was listed")
}

// TestTheRealEmbeddedUIServesThroughTheRealMount closes the one seam the tests
// above cannot: they run the handler over an fstest.MapFS, which proves the
// RULES and says nothing about `web/embed.go`'s `fs.Sub`, the actual index.html
// Vite emits, or the two being wired together.
//
// ⚠️ IT SKIPS WHERE THERE IS NOTHING TO TEST, which is ci and every fresh clone.
// That is not a hole: the same property is asserted on the PUBLISHED image, by
// `oto version` reporting `ui: embedded (N files)` in both workflows — which is
// the environment where being wrong actually costs something.
func TestTheRealEmbeddedUIServesThroughTheRealMount(t *testing.T) {
	if !web.Present() {
		t.Skip("no web/dist in this checkout; run `just ui-build`. The published image is asserted instead.")
	}

	h := uiRoot()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	body, _ := io.ReadAll(rec.Body)
	require.Contains(t, strings.ToLower(string(body)), "<!doctype html")
	require.Contains(t, string(body), "/assets/",
		"the real index.html should reference at least one hashed bundle")

	// A deep link, through the real FS.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/alerts", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")

	// And the namespace refusal, through the real mount rather than a wrapper the
	// test assembled.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/alerts", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestUIWithNoBuildSaysSoInsteadOf404(t *testing.T) {
	// The ordinary state of a developer's machine: a Go binary built without
	// `npm run build`. Answering `404 page not found` here is what sends a reader
	// hunting for a missing route when the truth is a missing build step.
	h := http.HandlerFunc(uiAbsent)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	body, _ := io.ReadAll(rec.Body)
	require.Contains(t, string(body), "no web UI is embedded")
	require.Contains(t, string(body), "/api/v1", "the message must say the API is unaffected")
	require.Contains(t, string(body), "just ui-build", "the message must say how to fix it")
}
