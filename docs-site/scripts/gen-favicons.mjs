#!/usr/bin/env node
// The raster favicons, rendered from the committed mark.
//
// ⛔ THE OUTPUTS ARE COMMITTED AND THIS SCRIPT IS NOT IN THE BUILD. That is
// deliberate, and it is the opposite of how sync-docs.mjs treats its mirrors.
// A favicon is how a person finds oto's tab in a row of twenty, so it has to be
// STABLE — regenerating it on every build makes the one asset whose whole job is
// not changing depend on a toolchain that can. `sharp` is also the only reason
// this is possible at all, and it is a build dependency that a docs deploy should
// be able to lose without losing the site's icons.
//
// Run it by hand when the mark changes, and commit what it writes:
//   node scripts/gen-favicons.mjs
import { writeFileSync, readFileSync } from "node:fs";
import { join } from "node:path";
import sharp from "sharp";

const PUBLIC = join(import.meta.dirname, "..", "public");
const ASSETS = join(import.meta.dirname, "..", "src", "assets");

// Kincha on transparent, for the browser tab. `favicon.svg` carries a
// prefers-color-scheme swap to kindei and is served to anything that can read an
// SVG icon; these PNGs are the fallback, and a PNG cannot switch on the theme.
// Kincha is the value that reads on both a light and a dark tab strip.
const MARK = readFileSync(join(ASSETS, "oto-mark-light.svg"));

// The iOS home-screen icon. It gets an OPAQUE ground because iOS composites a
// transparent touch icon onto black, which would put a dark-theme gold on a
// ground it was never measured against. Konshi indigo with the gold mark is the
// treatment assets/logo/oto-icon-dark.svg already uses, so the home screen and
// the repo's own icon art agree.
const KONSHI = "#1b2333";
const KINDEI = "#c9a668";
const TOUCH = Buffer.from(
	readFileSync(join(ASSETS, "oto-mark-dark.svg"), "utf8").replace(
		'viewBox="0 0 128 128"',
		// Inset the mark to ~72% and sit it on a rounded konshi tile. iOS masks
		// the corners itself, but Android and the PWA manifest do not, so the
		// radius is drawn rather than assumed.
		'viewBox="-24 -24 176 176"',
	),
);

const jobs = [
	["favicon-32.png", MARK, 32, null],
	["favicon-192.png", MARK, 192, null],
	["apple-touch-icon.png", TOUCH, 180, KONSHI],
];

for (const [name, svg, size, ground] of jobs) {
	let img = sharp(svg, { density: 384 }).resize(size, size, {
		fit: "contain",
		background: { r: 0, g: 0, b: 0, alpha: 0 },
	});
	if (ground) img = img.flatten({ background: ground });
	const out = await img.png({ compressionLevel: 9 }).toBuffer();
	writeFileSync(join(PUBLIC, name), out);
	console.log(`${name} — ${size}×${size}, ${ground ? `on ${ground}` : "transparent"}, ${out.length} bytes`);
}
console.log(`gold: kincha for the tab, kindei ${KINDEI} on the touch icon`);
