#!/usr/bin/env node
// Pulls docs/, README.md and CONTEXT.md from the repo root into
// src/content/docs/ as real Starlight pages: adds the frontmatter Starlight's
// schema requires (derived from each file's first H1), strips that now-
// redundant H1 from the body, and rewrites internal *.md links to the clean
// URLs Starlight serves them at. Re-run after any change under docs/.
//
// ⛔ DESTRUCTIVE — READ THIS FIRST. This script rmSync()s adr/, design/, setup/
// and runbooks/ under src/content/docs/ before regenerating them, so ANY page
// living in one of those four directories that has NO source under docs/ is
// destroyed, not merely stale. Two such pages exist today and are NOT generated
// from anything:
//
//     src/content/docs/design/case-and-grouping.md
//     src/content/docs/design/case-and-grouping-handoff.md
//
// Give them a home under docs/design/ before running this, or copy them aside
// and put them back afterwards. Everything else under those four directories,
// plus architecture.md, orchestration.md and guides/overview.md, is generated
// and safe to lose; index.mdx and the rest of guides/ are never touched.
import { readFileSync, writeFileSync, mkdirSync, readdirSync, rmSync } from "node:fs";
import { join, dirname, basename, extname, posix } from "node:path";

const REPO_ROOT = join(import.meta.dirname, "..", "..");
const OUT_ROOT = join(import.meta.dirname, "..", "src", "content", "docs");

// [source path relative to repo root, destination relative to content/docs root]
const SOURCES = [
	["README.md", "guides/overview.md"],
	["CONTEXT.md", "architecture.md"],
	["docs/ORCHESTRATION.md", "orchestration.md"],
	...listDir("docs/adr").map((f) => [`docs/adr/${f}`, `adr/${f}`]),
	...listDir("docs/design").map((f) => [`docs/design/${f}`, `design/${f}`]),
	...listDir("docs/setup").map((f) => [`docs/setup/${f}`, `setup/${f}`]),
	...listDir("docs/runbooks")
		.map((f) => [`docs/runbooks/${f}`, `runbooks/${f === "README.md" ? "index.md" : f}`]),
];

function listDir(rel) {
	return readdirSync(join(REPO_ROOT, rel)).filter((f) => extname(f) === ".md");
}

// repo-root-relative source path -> clean Starlight route.
const ROUTES = new Map(
	SOURCES.map(([src, dest]) => [
		posix.normalize(src),
		"/" + dest.replace(/\.md$/, "/").replace(/index\/$/, ""),
	]),
);

function extractTitle(lines) {
	let inFence = false;
	for (let i = 0; i < lines.length; i++) {
		const line = lines[i];
		if (/^(```|~~~)/.test(line)) inFence = !inFence;
		else if (!inFence && /^#\s+/.test(line)) {
			return { title: line.replace(/^#\s+/, "").trim(), headingLine: i };
		}
	}
	return { title: null, headingLine: -1 };
}

// srcDir is the source file's own directory, relative to REPO_ROOT (posix, no leading slash).
function rewriteLinks(body, srcDir) {
	return body.replace(/\]\(([^)\s]+?)\.md(#[^)]*)?\)/g, (whole, path, anchor = "") => {
		if (/^https?:\/\//.test(path) || path.startsWith("/")) return whole;
		const resolved = posix.normalize(posix.join(srcDir, path + ".md"));
		const route = ROUTES.get(resolved);
		if (!route) return whole; // unknown target: leave as-is rather than guess
		return `](${route}${anchor})`;
	});
}

function yamlEscape(s) {
	return s.includes(":") || s.includes('"') ? JSON.stringify(s) : s;
}

// Only wipe the directories this script fully owns (adr/, design/, setup/,
// runbooks/), so a stale file removed from docs/ doesn't linger — and so
// hand-authored pages elsewhere in content/docs (index.mdx, guides/) are
// never touched.
for (const dir of ["adr", "design", "setup", "runbooks"]) {
	rmSync(join(OUT_ROOT, dir), { recursive: true, force: true });
}
mkdirSync(OUT_ROOT, { recursive: true });

for (const [srcRel, destRel] of SOURCES) {
	const raw = readFileSync(join(REPO_ROOT, srcRel), "utf8");
	const lines = raw.split("\n");
	const { title, headingLine } = extractTitle(lines);
	const bodyLines = [...lines];
	if (headingLine >= 0) {
		bodyLines.splice(headingLine, 1);
		if (bodyLines[headingLine] === "") bodyLines.splice(headingLine, 1);
	}
	const body = rewriteLinks(bodyLines.join("\n"), posix.normalize(dirname(srcRel)));
	const frontmatterTitle = title ?? basename(destRel, ".md");
	const out = `---\ntitle: ${yamlEscape(frontmatterTitle)}\n---\n${body}`;

	const outPath = join(OUT_ROOT, destRel);
	mkdirSync(join(outPath, ".."), { recursive: true });
	writeFileSync(outPath, out);
}

console.log(`synced ${SOURCES.length} pages into ${OUT_ROOT}`);
