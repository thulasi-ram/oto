package app

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/thulasiram/oto/web"
)

// Serving the single-page application, and the four rules that make a fallback
// safe rather than merely convenient.
//
// ⭐ A SPA NEEDS A CATCH-ALL because client-side routes are real URLs. `/alerts`
// is a page in the browser and not a file on disk, so a deep link — a bookmark,
// a pasted link, a refresh — must answer with index.html and let the app router
// take it from there. Without that, every URL except `/` is a 404 and the UI
// works only if you never reload.
//
// ⛔ AND A CATCH-ALL IS A LOADED GUN POINTED AT THE API. The failure mode is not
// a broken page: it is `/api/v1/anything-mistyped` answering 200 with HTML, so a
// programmatic caller's JSON parser fails somewhere downstream with an error that
// names neither oto nor the route. That defect is invisible to every human check
// — the browser looks perfect — which is why the rules below are stated as
// refusals and pinned by tests rather than left to the handler's shape.
const (
	// assetPrefix is Vite's output directory for hashed bundles
	// (web/vite.config.ts `build.outDir` + Rollup's default `assets/`).
	assetPrefix = "assets/"
	// indexFile is the SPA shell, and the target of every fallback.
	indexFile = "index.html"
)

// mountUI registers the SPA at `/`, or a handler that explains its absence.
//
// ⚠️ IT IS REGISTERED LAST AND ON `/*`, WHICH IS WHAT KEEPS THE API SAFE. chi
// matches the more specific pattern, so `/api/v1/...` reaches the v1 sub-router
// and an unknown path there gets the v1 router's own 404 — never this handler.
// The ops routes (`/healthz`, `/readyz`, `/metrics`, `/openapi.json`) are exact
// registrations and win for the same reason. The test file asserts both, because
// "chi does the right thing" is a claim about a dependency and not a property of
// this code.
func (c *Container) mountUI(r chi.Router) {
	sub, err := web.FS()
	if err != nil || !web.Present() {
		// ⭐ A SENTENCE, NOT A 404, AND NOT A PANIC. `web/dist` is a build output
		// that is not committed, so a `go build` without `npm run build` is the
		// ordinary state of a developer's machine and must still boot. Answering
		// `404 page not found` here is what sent a reader looking for a missing
		// route when the truth was a missing build step — the exact confusion
		// this feature exists to end. 503 rather than 500: nothing is broken, a
		// component is absent.
		r.Method(http.MethodGet, "/*", http.HandlerFunc(uiAbsent))
		r.Method(http.MethodHead, "/*", http.HandlerFunc(uiAbsent))
		return
	}
	h := newUIHandler(sub)
	// GET and HEAD only. A POST to an unknown path answering 200 index.html
	// tells a client its write succeeded.
	r.Method(http.MethodGet, "/*", h)
	r.Method(http.MethodHead, "/*", h)
}

func uiAbsent(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, "oto: no web UI is embedded in this binary.\n\n"+
		"The API is unaffected — every surface under /api/v1 works, and so do\n"+
		"/healthz, /readyz, /metrics and /openapi.json.\n\n"+
		"A released image always carries the UI (the release workflow refuses to\n"+
		"publish one that does not). A binary built with `go build` does not,\n"+
		"until `just ui-build` has written web/dist.\n")
}

// reservedRoots are the path roots the SPA may never answer for, whether or not
// a route is currently mounted under them.
//
// ⚠️ IT IS A LIST OF ROOTS AND NOT OF ROUTES, because the danger is precisely the
// path that is NOT mounted: a mounted route never reaches this handler at all.
var reservedRoots = []string{"api", "healthz", "readyz", "metrics", "openapi.json"}

// isReserved reports whether name lives in a namespace that belongs to the API or
// to an operational probe.
func isReserved(name string) bool {
	for _, root := range reservedRoots {
		if name == root || strings.HasPrefix(name, root+"/") {
			return true
		}
	}
	return false
}

// uiHandler serves the embedded SPA.
type uiHandler struct{ fsys fs.FS }

func newUIHandler(fsys fs.FS) *uiHandler { return &uiHandler{fsys: fsys} }

func (u *uiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/")), "/")

	// ⛔ RESERVED SPACE IS REFUSED BEFORE ANYTHING ELSE, AND A GATE CAUGHT THIS
	// BEING WRONG. The first version of this handler relied on chi resolving the
	// `/api/v1` mount ahead of `/*`, which it does — and concluded the API was
	// therefore safe, which it was not: `/api/v2/alerts` matches NO mount, fell
	// through to here, has no dot in its last segment, and was served index.html
	// with a 200. So a client pinned to an API version oto does not serve, or one
	// typing `/api/v2` from a newer client's docs, received HTML where it expected
	// JSON — the exact failure this handler's own comments warned about, arriving
	// through a path those comments had not considered.
	//
	// The rule is therefore about the NAMESPACE and not about which routes happen
	// to be mounted: `/api/**` belongs to the API whether a version of it exists
	// or not, and the operational paths belong to probes. `/metrics` matters even
	// though it is normally registered — `telemetry.metrics_enabled: false`
	// unregisters it, and a Prometheus scrape answered with 200 text/html is a
	// target that looks up while it is reporting nothing.
	if isReserved(name) {
		http.NotFound(w, r)
		return
	}

	if name == "" || name == indexFile {
		u.serveIndex(w, r)
		return
	}

	f, err := u.fsys.Open(name)
	if err == nil {
		defer func() { _ = f.Close() }()
		if st, serr := f.Stat(); serr == nil && !st.IsDir() {
			u.serveFile(w, r, name, f, st)
			return
		}
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrInvalid) {
		// A path that is not a valid FS name (a traversal attempt, a NUL) lands
		// here. It is not a client-side route and must not be answered with the
		// shell.
		http.NotFound(w, r)
		return
	}

	// The file does not exist. Whether that is a deep link or a genuine 404 is
	// the whole question this feature can get wrong.
	if u.isMissingFile(name) {
		http.NotFound(w, r)
		return
	}
	u.serveIndex(w, r)
}

// isMissingFile reports whether a path that resolved to nothing was ASKING for a
// file rather than naming a client-side route.
//
// ⛔ `/assets/*` IS NEVER A ROUTE. A missing hashed bundle answered with
// index.html gives the browser HTML with a 200 and a text/html Content-Type for
// a `<script>` — and the symptom is a blank page with a syntax error in the
// console, not anything that says "that file is gone". This is the case a stale
// cached index.html produces on every deploy, so it is not hypothetical.
//
// ⚠️ A DOT IN THE LAST SEGMENT MEANS A FILE, and that heuristic is deliberate
// rather than clever. `/favicon.ico`, `/robots.txt` and `/manifest.webmanifest`
// must 404 when absent so a browser stops asking; `/alerts/abc-123` must not.
// Nothing in oto's UI routes a segment containing a dot — an alert is addressed
// by a UUID, a group by a key — so the heuristic has no false positive here, and
// if a route ever needs one this comment is where to change the rule.
func (u *uiHandler) isMissingFile(name string) bool {
	if strings.HasPrefix(name, assetPrefix) {
		return true
	}
	return strings.Contains(path.Base(name), ".")
}

func (u *uiHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := u.fsys.Open(indexFile)
	if err != nil {
		// Present() proved this file existed at mount time, so reaching here
		// means the embedded FS is not what it claimed.
		http.Error(w, "oto: the embedded UI has no "+indexFile, http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "oto: the embedded UI is unreadable", http.StatusInternalServerError)
		return
	}
	// ⛔ NEVER CACHED, AND THIS IS THE HEADER THAT MAKES A DEPLOY SAFE. index.html
	// names hashed bundles that the NEXT build deletes. A cached shell therefore
	// asks for `/assets/index-OLD.js`, which no longer exists — a blank page for
	// exactly as long as the cache lives, on a deployment that is otherwise
	// healthy. The shell is small; re-fetching it costs nothing worth having.
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	u.serveContent(w, r, indexFile, f, st)
}

func (u *uiHandler) serveFile(w http.ResponseWriter, r *http.Request, name string, f fs.File, st fs.FileInfo) {
	if strings.HasPrefix(name, assetPrefix) {
		// ⭐ IMMUTABLE IS HONEST HERE AND NOWHERE ELSE. Vite puts a content hash
		// in every bundle's filename, so this exact URL can never mean different
		// bytes: a change ships a new name. The pair — immutable assets, an
		// uncacheable shell — is what makes the SPA both fast and correct across
		// a deploy.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Everything else (favicon, manifest, a file from web/public) is served
		// under its own name across builds, so it may only be revalidated.
		w.Header().Set("Cache-Control", "no-cache")
	}
	u.serveContent(w, r, name, f, st)
}

// serveContent hands off to net/http, which owns Range, If-Modified-Since, HEAD
// and content sniffing — none of which is worth reimplementing here.
func (u *uiHandler) serveContent(w http.ResponseWriter, r *http.Request, name string, f fs.File, st fs.FileInfo) {
	if rs, ok := f.(io.ReadSeeker); ok {
		// ⚠️ A ZERO modtime IS DELIBERATE, NOT AN OVERSIGHT. Every file in an
		// embed.FS reports the zero time, so passing st.ModTime() would emit
		// `Last-Modified: Thu, 01 Jan 1970` and invite a conditional request that
		// can never be satisfied. http.ServeContent omits the header entirely for
		// a zero time, which is the correct answer for bytes whose identity is
		// their name.
		http.ServeContent(w, r, name, st.ModTime(), rs)
		return
	}
	// An fs.FS whose files are not seekable cannot serve a Range request. Copying
	// is correct for a whole-file GET, which is every request the SPA makes.
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}
