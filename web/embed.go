// Package web carries the built single-page application into the oto binary.
//
// ⭐ IT EXISTS BECAUSE THE UI IS PART OF v1 AND WAS NOT PART OF THE ARTIFACT.
// SPEC §0 positions oto as "self-hosted, with a UI"; SPEC §J's acceptance
// criteria have a subsection headed "Timeline and UI", and criterion 23 — "with
// the browser asleep for 20 minutes, reopening the tab replays the missed
// changes via Last-Event-ID and the UI is correct without a manual refresh" — is
// not satisfiable by an API at all. Until this package existed the published
// image served `/` as `404 page not found`, so the one step README's own setup
// instructions give an operator ("create a source in the UI") was impossible on
// a real deployment.
//
// ⛔ THE SPA IS SAME-ORIGIN-ONLY, WHICH IS WHY THE BINARY SERVES IT RATHER THAN
// A CDN. `web/src/api/endpoints.ts` hard-codes a relative `/api/v1` and
// `client.ts` calls `fetch(path + query)`: there is no base-URL surface and no
// CORS story in the happy path — vite.config.ts proxies the backend in
// development precisely "so the browser never learns there are two origins".
// Serving the assets from a second hostname therefore does not work, and it
// would look like it did until the first authenticated fetch. Embedding also
// keeps SPEC criterion 31 true: `helm install` plus a Postgres URL is the ENTIRE
// install, and an install that needs a static bucket is not that.
//
// ⚠️ `dist` IS A BUILD OUTPUT AND IS NOT COMMITTED, so a bare checkout embeds
// only the placeholder and Present() reports false. That is deliberate: a
// committed `dist` is a stale UI nobody notices, and a build tag would be one
// more thing a release could forget to pass. What closes the loop instead is the
// assertion on the PUBLISHED image — `oto version` reports whether the UI is
// embedded, and the release workflow fails when it is not.
package web

import (
	"embed"
	"io/fs"
)

// ⛔ `all:` IS LOAD-BEARING. The default embed patterns skip files beginning with
// a dot, and `dist/.gitkeep` is the only thing in the directory on a checkout
// that has never run `npm run build` — without `all:` the pattern matches
// nothing and the whole module fails to COMPILE, on a fresh clone, for everyone.
//
//go:embed all:dist
var dist embed.FS

// FS returns the built SPA rooted at its own directory, so a request path maps
// to an entry name without the caller knowing about `dist`.
func FS() (fs.FS, error) { return fs.Sub(dist, "dist") }

// Present reports whether a real UI was built into this binary.
//
// It asks for `index.html` rather than counting entries, because the placeholder
// alone is an empty UI and every fallback in the handler serves index.html.
func Present() bool {
	f, err := dist.Open("dist/index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// Files counts the embedded entries, for `oto version` to report. A number is
// worth more than a boolean in a release log: "embedded (1 file)" is a
// placeholder somebody shipped by accident.
func Files() int {
	n := 0
	sub, err := FS()
	if err != nil {
		return 0
	}
	_ = fs.WalkDir(sub, ".", func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
