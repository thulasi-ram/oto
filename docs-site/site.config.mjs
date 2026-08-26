// Where the built site is served from — the ONE place that fact is written.
//
// ⛔ TWO THINGS NEED THIS AND THEY FAIL DIFFERENTLY, WHICH IS WHY IT IS SHARED
// RATHER THAN REPEATED. `astro.config.mjs` uses it to prefix every URL Astro
// generates: assets, the sidebar, the search index. `scripts/sync-docs.mjs`
// uses it to prefix every link it writes INTO page content, because Astro does
// not touch raw hrefs in Markdown — it has no way to tell a route it produced
// from a string somebody typed.
//
// Get them out of step and the failure is the confusing kind: the nav works, the
// stylesheet loads, the page renders, and all 281 in-content links 404. Setting
// `base` in astro.config.mjs alone does exactly that, which is how this file
// came to exist.
//
// A move to a custom domain served at the root is a one-line change here —
// `base: "/"` — plus re-running the sync.

/** The origin the site is published under. Also what the sitemap is built from. */
export const site = "https://thulasi-ram.github.io";

/**
 * The path prefix, with a leading and no trailing slash — Astro's own
 * convention, and what `withBase` below assumes.
 *
 * GitHub Pages serves a PROJECT site at `<owner>.github.io/<repo>`, so this is
 * the repository name. A user site (`<owner>.github.io`) or a custom domain is
 * served at the root and wants `"/"`.
 */
export const base = "/oto";

/**
 * One absolute site path, prefixed. Idempotent, so a path that already carries
 * the prefix survives a second pass unharmed.
 *
 * `path` is expected to start with `/`. The `base === "/"` case returns it
 * untouched rather than producing a doubled slash.
 */
export function withBase(path) {
	if (base === "/" || base === "") return path;
	if (path === base || path.startsWith(`${base}/`)) return path;
	return `${base}${path}`;
}
