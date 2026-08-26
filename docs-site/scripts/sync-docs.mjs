#!/usr/bin/env node
// Pulls docs/ and the root-level Markdown (README.md, CONTEXT.md,
// CONTRIBUTING.md) into src/content/docs/ as real Starlight pages: adds the
// frontmatter Starlight's schema requires (derived from each file's first H1),
// strips that now-redundant H1 from the body, and rewrites internal *.md links
// to the clean URLs Starlight serves them at. Re-run after any change to a
// source file; the generated pages are committed, so they go stale otherwise.
//
// DESTRUCTIVE — READ THIS FIRST. This script rmSync()s adr/, design/, setup/
// and runbooks/ under src/content/docs/ before regenerating them, so ANY page
// living in one of those four directories that has NO source under docs/ is
// destroyed, not merely stale. Give such a page a home under docs/ before
// running this, or copy it aside and put it back afterwards.
//
// This comment deliberately does NOT list the orphans. The list it used to
// carry named two files that no longer existed and missed the one that did:
// a hand-maintained inventory of generated output rots by construction. The
// script now finds them itself and prints them, under ORPHANS, before it
// deletes anything — so the warning cannot go out of date.
//
// Everything outside those four directories is written in place: architecture.md,
// concepts.md, contributing.md, orchestration.md and guides/overview.md are
// generated and safe to lose; index.mdx and the rest of guides/ are never
// touched.
import { readFileSync, writeFileSync, mkdirSync, readdirSync, rmSync, existsSync, cpSync } from "node:fs";
import { join, dirname, basename, extname, posix } from "node:path";

const REPO_ROOT = join(import.meta.dirname, "..", "..");
const OUT_ROOT = join(import.meta.dirname, "..", "src", "content", "docs");

// The four directories this script fully owns: wiped and rebuilt on every run.
const OWNED_DIRS = ["adr", "design", "setup", "runbooks"];

// [source path relative to repo root, destination relative to content/docs root]
//
// The root-level pages are optional: a source that is not on disk is skipped
// rather than crashing the sync, so this list can name a file before it is
// written. The four directory mirrors below are read from disk and need no entry
// per file — nor a sidebar entry, since astro.config.mjs autogenerates them.
const SOURCES = [
	...optional([
		["README.md", "guides/overview.md"],
		["CONTEXT.md", "architecture.md"],
		["CONTRIBUTING.md", "contributing.md"],
		["docs/releasing.md", "releasing.md"],
		["docs/concepts.md", "concepts.md"],
		["docs/ORCHESTRATION.md", "orchestration.md"],
	]),
	...listDir("docs/adr").map((f) => [`docs/adr/${f}`, `adr/${f}`]),
	...listDir("docs/design").map((f) => [`docs/design/${f}`, `design/${f}`]),
	...listDir("docs/setup").map((f) => [`docs/setup/${f}`, `setup/${f}`]),
	...listDir("docs/runbooks")
		.map((f) => [`docs/runbooks/${f}`, `runbooks/${f === "README.md" ? "index.md" : f}`]),
];

function optional(pairs) {
	return pairs.filter(([src]) => {
		if (existsSync(join(REPO_ROOT, src))) return true;
		console.warn(`  skipped (no source yet): ${src}`);
		return false;
	});
}

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

// Image references need their own rewrite, and NOT the *.md one above. A source
// doc points at docs/assets/… relative to its own location, which is not where
// the generated page sits — and Astro resolves an image in Markdown at BUILD
// time, so a path that does not resolve is a hard build failure, not a broken
// image. Anything under docs/assets/ is therefore copied to public/assets/ and
// referenced from the site root, where a missing file degrades to a 404 instead.
//
// public/assets/ is generated output and is gitignored: the source of truth for
// every one of these files is docs/assets/.
const ASSET_SRC = join(REPO_ROOT, "docs", "assets");
const ASSET_OUT = join(import.meta.dirname, "..", "public", "assets");
const missingAssets = [];

function rewriteImages(body, srcDir) {
	return body.replace(/!\[([^\]]*)\]\(([^)\s]+)([^)]*)\)/g, (whole, alt, path, rest) => {
		if (/^https?:\/\//.test(path) || path.startsWith("/") || path.startsWith("data:")) return whole;
		const resolved = posix.normalize(posix.join(srcDir, path));
		if (!resolved.startsWith("docs/assets/")) return whole; // not ours to move
		if (!existsSync(join(REPO_ROOT, resolved))) missingAssets.push(resolved);
		return `![${alt}](/${resolved.slice("docs/".length)}${rest})`;
	});
}

function yamlEscape(s) {
	return s.includes(":") || s.includes('"') ? JSON.stringify(s) : s;
}

// Mirror docs/assets/ wholesale rather than per-reference: it is small, and a
// partial copy would make "the image is missing" depend on which doc mentions it.
if (existsSync(ASSET_SRC)) {
	rmSync(ASSET_OUT, { recursive: true, force: true });
	cpSync(ASSET_SRC, ASSET_OUT, { recursive: true });
}

// Name what is about to be destroyed. Anything in an owned directory that this
// run would not write back has no source under docs/ and is not coming back —
// so say which file, by name, rather than trusting a comment to stay accurate.
const willWrite = new Set(SOURCES.map(([, dest]) => posix.normalize(dest)));
const orphans = OWNED_DIRS.flatMap((dir) =>
	(existsSync(join(OUT_ROOT, dir)) ? readdirSync(join(OUT_ROOT, dir)) : [])
		.map((f) => `${dir}/${f}`)
		.filter((rel) => !willWrite.has(rel)),
);
if (orphans.length > 0) {
	console.warn(
		`ORPHANS — ${orphans.length} generated page(s) have no source under docs/ and are being DELETED:`,
	);
	for (const rel of orphans) console.warn(`  ${rel}`);
	console.warn("Give each a home under docs/ if it should survive the next sync.");
}

// Only wipe the directories this script fully owns (adr/, design/, setup/,
// runbooks/), so a stale file removed from docs/ doesn't linger — and so
// hand-authored pages elsewhere in content/docs (index.mdx, guides/) are
// never touched.
for (const dir of OWNED_DIRS) {
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
	const srcDir = posix.normalize(dirname(srcRel));
	const body = rewriteImages(rewriteLinks(bodyLines.join("\n"), srcDir), srcDir);
	const frontmatterTitle = title ?? basename(destRel, ".md");
	const out = `---\ntitle: ${yamlEscape(frontmatterTitle)}\n---\n${body}`;

	const outPath = join(OUT_ROOT, destRel);
	mkdirSync(join(outPath, ".."), { recursive: true });
	writeFileSync(outPath, out);
}

if (missingAssets.length > 0) {
	const unique = [...new Set(missingAssets)].sort();
	console.warn(`MISSING ASSETS — ${unique.length} image(s) are referenced but not in docs/assets/:`);
	for (const rel of unique) console.warn(`  ${rel}`);
	console.warn("Each renders as a broken image on the site. Add the file, or drop the reference.");
}

console.log(`synced ${SOURCES.length} pages into ${OUT_ROOT}`);
