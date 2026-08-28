#!/usr/bin/env node
// Every internal link in the built site resolves to a page the build produced.
//
// ⛔ THIS EXISTS BECAUSE THE DOCS HAVE NO OTHER READER. `just lint-vocabulary`
// does not read this directory, nothing type-checks Markdown, and the sync
// script deliberately leaves a link it cannot resolve alone rather than guessing
// — which is right, and means an unresolvable link reaches the site silently.
// Until this ran, "is the mirror correct" was answered by somebody clicking.
//
// It catches two failures that look nothing alike:
//
//   1. A LINK TO A PAGE THAT IS NOT THERE. A doc renamed under docs/ takes its
//      route with it, and every other doc pointing at the old name keeps
//      pointing at it — in the repository the relative *.md link still resolves,
//      so nothing local complains.
//
//   2. AN ABSOLUTE PATH MISSING THE SITE'S BASE. `base` prefixes what Astro
//      generates and not what sync-docs.mjs writes into content, so getting only
//      half of it produces a site whose nav works and whose in-content links all
//      404. That is the specific bug this gate was written after; see
//      site.config.mjs.
//
// Run after a build: `npm run check-links`, or `npm run verify` for both.
import { readdirSync, readFileSync, existsSync } from "node:fs";
import { join, relative, sep } from "node:path";

import { base } from "../site.config.mjs";

const DIST = join(import.meta.dirname, "..", "dist");

if (!existsSync(DIST)) {
	console.error("check-links: no dist/. Run `npm run build` first.");
	process.exit(1);
}

function walk(dir) {
	return readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
		e.isDirectory() ? walk(join(dir, e.name)) : [join(dir, e.name)],
	);
}

const pages = walk(DIST).filter((f) => f.endsWith(".html"));
if (pages.length === 0) {
	console.error("check-links: dist/ has no HTML. Did the build fail?");
	process.exit(1);
}

const prefix = base === "/" || base === "" ? "" : base;

// ⛔ AN EXPLICIT SET OF WHAT THE BUILD WROTE, NOT `existsSync`, AND THE
// DIFFERENCE IS THE WHOLE VALUE OF THIS GATE. A case-insensitive filesystem —
// every default macOS one — answers `existsSync("dist/design/SPEC")` for a
// directory actually named `spec`. So a link whose case does not match the route
// Starlight generated passes on a laptop and 404s on Linux, which is to say it
// passes everywhere except CI and production.
//
// That is not hypothetical: Starlight lower-cases its slugs, `docs/design/SPEC.md`
// is served at `/design/spec/`, and the sync script pointed ten references at
// `/design/SPEC/` from the day the mirror existed. This set is compared exactly,
// so the laptop and the runner agree.
const built = new Set(
	walk(DIST).map((f) => "/" + relative(DIST, f).split(sep).join("/")),
);

/** Does an absolute site path resolve to something the build wrote? */
function resolves(path) {
	// Astro does not nest dist/ under `base` — dist IS the base root — so the
	// prefix is stripped before looking it up.
	let rel = path;
	if (prefix !== "") {
		if (rel === prefix) rel = "/";
		else rel = rel.slice(prefix.length);
	}
	if (rel === "") rel = "/";
	if (built.has(rel)) return true;
	return built.has(rel.endsWith("/") ? `${rel}index.html` : `${rel}/index.html`);
}

const problems = [];

for (const page of pages) {
	const html = readFileSync(page, "utf8");
	const where = relative(DIST, page);

	for (const m of html.matchAll(/(?:href|src)="([^"]+)"/g)) {
		const raw = m[1];
		// Not ours to check: off-site, in-page, and non-http schemes.
		if (/^(https?:)?\/\//.test(raw)) continue;
		if (/^(#|mailto:|tel:|data:|javascript:)/.test(raw)) continue;
		// ⭐ A SURVIVING `.md` IS THE THIRD FAILURE, AND IT IS THE QUIET ONE.
		// `rewriteLinks` leaves a link it cannot resolve exactly as written rather
		// than guessing — correctly — so a pointer at a doc that is NOT synced
		// (docs/design/architect-proposal.md's neighbours, a file under docs/ that
		// no SOURCES entry covers) reaches the site as a relative `foo.md` href.
		// It still resolves when read in the repository, which is why nobody
		// notices, and it is a guaranteed 404 on the site.
		if (/\.md($|[#?])/.test(raw)) {
			problems.push({
				where,
				raw,
				why: "still points at a .md file — the sync script found no route for it",
			});
			continue;
		}

		// Other relative links are resolved by the browser against the current
		// page and are not a failure mode this gate can judge; the sync script
		// emits absolute ones.
		if (!raw.startsWith("/")) continue;

		const path = raw.split("#")[0].split("?")[0];
		if (path === "") continue;

		if (prefix !== "" && path !== prefix && !path.startsWith(`${prefix}/`)) {
			problems.push({ where, raw, why: `missing the site base \`${prefix}\`` });
			continue;
		}
		if (!resolves(path)) {
			problems.push({ where, raw, why: "no page was built at that path" });
		}
	}
}

if (problems.length > 0) {
	// Grouped by target: one renamed doc produces one line here rather than the
	// forty pages that happen to link to it.
	const byLink = new Map();
	for (const p of problems) {
		const key = `${p.raw}\t${p.why}`;
		if (!byLink.has(key)) byLink.set(key, []);
		byLink.get(key).push(p.where);
	}
	console.error(`check-links: ${byLink.size} broken link target(s) across ${problems.length} reference(s)\n`);
	for (const [key, wheres] of [...byLink].sort()) {
		const [raw, why] = key.split("\t");
		console.error(`  ${raw}`);
		console.error(`    ${why}`);
		console.error(`    linked from: ${wheres.slice(0, 5).join(", ")}${wheres.length > 5 ? ` (+${wheres.length - 5} more)` : ""}`);
	}
	console.error(
		"\nA link into docs/ is fixed at the SOURCE, not in src/content/docs/ — the next sync overwrites the copy.",
	);
	process.exit(1);
}

console.log(`check-links: ${pages.length} pages, every internal link resolves`);

// ---------------------------------------------------------------------------
// Every url() in the built CSS points at something that exists.
//
// ⛔ THIS IS A SEPARATE GATE FROM THE ONE ABOVE BECAUSE IT CATCHES A FAILURE
// HTML CANNOT SHOW. The link check reads hrefs; a mask, a font and a background
// image are reached from CSS, and Vite does not treat an unresolvable one as an
// error. It warns — `didn't resolve at build time, it will remain unchanged to
// be resolved at runtime` — and writes the path through verbatim. The build is
// green, the page renders, and the asset 404s.
//
// For custom.css's decorative ink that is the worst possible shape of bug: a
// mask that fails to load is not a broken image, it is an element with NO mask,
// which paints as a flat opaque block of tint over whatever is behind it — or,
// if the tint is subtle enough, as decoration that simply never appears. Neither
// announces itself. `src/assets/motifs/` is generated by sync-docs.mjs, so the
// path this protects is a real one: build without syncing and the ink breaks.
const cssFiles = walk(DIST).filter((f) => f.endsWith(".css"));
const assetProblems = [];

for (const file of cssFiles) {
	const where = relative(DIST, file).split(sep).join("/");
	const css = readFileSync(file, "utf8");
	for (const m of css.matchAll(/url\(\s*(['"]?)([^'")]+)\1\s*\)/g)) {
		const raw = m[2].trim();
		// Inlined (data:), remote, and in-document fragment references are not
		// files on disk and are not this gate's business.
		if (/^(data:|https?:|\/\/|#)/i.test(raw)) continue;
		// A path that still looks like the SOURCE tree is the signature of the
		// Vite warning above: it means the reference was never rewritten to a
		// hashed asset, so nothing was emitted for it.
		if (raw.includes("../") || raw.startsWith("src/")) {
			assetProblems.push({ where, raw, why: "unresolved at build time — no asset was emitted for it" });
			continue;
		}
		if (!raw.startsWith("/")) continue; // relative-to-stylesheet; Vite owns it
		if (prefix && !raw.startsWith(`${prefix}/`)) {
			assetProblems.push({ where, raw, why: `absolute path missing the site base (${prefix})` });
			continue;
		}
		const onDisk = join(DIST, raw.slice(prefix.length));
		if (!existsSync(onDisk)) {
			assetProblems.push({ where, raw, why: "no file was built at that path" });
		}
	}
}

if (assetProblems.length > 0) {
	console.error(`\ncheck-links: ${assetProblems.length} unresolvable CSS asset reference(s)\n`);
	for (const p of assetProblems) {
		console.error(`  ${p.raw}`);
		console.error(`    ${p.why}`);
		console.error(`    from: ${p.where}`);
	}
	console.error("\nIf these are motifs, run `npm run sync-docs` — src/assets/motifs/ is generated, not committed.");
	process.exit(1);
}

console.log(`check-links: ${cssFiles.length} stylesheet(s), every url() resolves`);
