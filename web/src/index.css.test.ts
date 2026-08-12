/**
 * The stylesheet's first-party utilities, checked against the CSS the build
 * actually emits.
 *
 * ⛔ THIS TEST CANNOT BE REPLACED BY A COMPONENT TEST. The failure mode it
 * guards is *absence*, and it is invisible from the DOM: a class written into
 * markup is present whether or not any rule was generated for it. Tailwind v4
 * only registers a custom utility declared with `@utility`. A rule hand-written
 * inside a plain `@layer utilities` block is pass-through CSS — the name is
 * never registered, so when the scanner meets `motion-safe:oto-enter` it
 * resolves the variant, fails to resolve the utility, and discards the whole
 * candidate without a warning. `class` contains `motion-safe:oto-enter`, every
 * component assertion passes, and no `.motion-safe\:oto-enter` rule exists.
 * That is exactly how the dead variant survived review in two files.
 *
 * So this compiles `index.css` the way the build does and asserts on the
 * generated selectors. It derives both halves from the tree rather than from a
 * list kept here:
 *
 *   1. every utility name `index.css` declares locally, and
 *   2. every variant use of one of those names anywhere in `src/`.
 *
 * A new hand-written utility given a variant therefore fails here on the day it
 * is written, without anyone remembering to extend this file.
 *
 * ⛔ IT COMPILES THROUGH `tailwindcss` ITSELF, WHICH IS THE ONE COMPILER THE
 * BUILD ALSO USES. `@tailwindcss/vite` is a thin wrapper: it depends on this
 * exact `tailwindcss` version and calls this exact `compile`, adding only source
 * scanning (which this file replaces by naming its own candidates) and Node
 * module resolution (which is `loadStylesheet` below). It would have been shorter
 * to import `@tailwindcss/node`, and that is what this test did — but that
 * package is nowhere in `web/package.json`, so it resolved only by npm hoisting
 * it up out of `@tailwindcss/vite`'s own dependencies, and a clean install that
 * hoisted differently would have failed here for a reason having nothing to do
 * with CSS.
 */
import { readFileSync, readdirSync } from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { compile } from "tailwindcss";
import { describe, expect, it } from "vitest";

const SRC = path.dirname(fileURLToPath(import.meta.url));
const ENTRYPOINT = path.join(SRC, "index.css");
const CSS = readFileSync(ENTRYPOINT, "utf8");

/** `.foo` escaped the way a selector must be to survive a `:` in a class name. */
const selectorFor = (candidate: string): string => `.${candidate.replace(/[:.[\]/%]/g, (c) => `\\${c}`)}`;

/** A pattern's first capture group over every match in the stylesheet. */
const captures = (re: RegExp): readonly string[] =>
  [...CSS.matchAll(re)].flatMap((m) => (m[1] === undefined ? [] : [m[1]]));

/** Utility names registered the supported way — the only ones variants compose with. */
const registered = (): ReadonlySet<string> => new Set(captures(/@utility\s+([a-z][a-zA-Z0-9_-]*)/g));

/**
 * Class rules written by hand, i.e. a selector that starts a rule with `.name`.
 * These are the pass-through ones: emitted verbatim, invisible to variants.
 */
const handWritten = (): ReadonlySet<string> => new Set(captures(/^[ \t]*\.([a-z][a-zA-Z0-9_-]*)/gm));

/** Every `.ts`/`.tsx` under `src/` that is not itself a test. */
function sourceFiles(dir: string = SRC): readonly string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name !== "generated") out.push(...sourceFiles(full));
    } else if (/\.tsx?$/.test(entry.name) && !/\.(test|spec)\.tsx?$/.test(entry.name)) {
      out.push(full);
    }
  }
  return out;
}

/**
 * Candidates in the tree that hang one or more variants off a locally declared
 * utility — `motion-safe:oto-enter`, `open:oto-enter`, and anything added later.
 */
function variantUsesOfLocalUtilities(local: ReadonlySet<string>): ReadonlyMap<string, string> {
  const found = new Map<string, string>();
  for (const file of sourceFiles()) {
    const text = readFileSync(file, "utf8");
    for (const m of text.matchAll(/\b((?:[a-z][a-z0-9-]*:)+)([a-z][a-zA-Z0-9-]*)\b/g)) {
      const [variants, base] = [m[1], m[2]];
      if (base !== undefined && local.has(base)) {
        found.set(`${variants ?? ""}${base}`, path.relative(SRC, file));
      }
    }
  }
  return found;
}

/**
 * The two forms of `@import` `index.css` actually uses: a bare package specifier
 * (`"tailwindcss"`, whose `exports` publish `./index.css`) and a path relative to
 * the importing sheet (`"./design/tokens.css"`). Anything else throws rather than
 * resolving to something plausible-but-wrong, because a silently mis-resolved
 * import here would look exactly like a missing utility.
 */
const requireFrom = createRequire(ENTRYPOINT);

async function loadStylesheet(id: string, base: string) {
  const file = id.startsWith(".") ? path.resolve(base, id) : requireFrom.resolve(`${id}/index.css`);
  return { path: file, base: path.dirname(file), content: readFileSync(file, "utf8") };
}

async function buildCss(candidates: readonly string[]): Promise<string> {
  const compiler = await compile(CSS, { base: SRC, loadStylesheet });
  return compiler.build([...candidates]);
}

/**
 * The body of the first at-rule whose prelude matches, brace-matched rather than
 * regex-matched so the assertion survives a change in how the output is
 * formatted (or minified).
 */
function atRuleBody(css: string, prelude: RegExp): string | null {
  const at = css.match(prelude);
  if (at?.index === undefined) return null;
  const open = css.indexOf("{", at.index);
  if (open === -1) return null;
  let depth = 0;
  for (let i = open; i < css.length; i++) {
    if (css[i] === "{") depth++;
    else if (css[i] === "}" && --depth === 0) return css.slice(open + 1, i);
  }
  return null;
}

describe("first-party utilities", () => {
  it("declares every one of them with @utility, never as a bare class rule", () => {
    const bare = [...handWritten()];
    expect(
      bare,
      `\`index.css\` declares ${bare.join(", ")} as plain class rule(s) inside a layer. ` +
        "Tailwind v4 copies those out verbatim without registering the name, so no variant " +
        "of them can ever be generated. Declare them with \`@utility <name> { … }\` instead.",
    ).toEqual([]);
  });

  it("generates a real rule for every variant use of one", async () => {
    const local = new Set([...registered(), ...handWritten()]);
    const uses = variantUsesOfLocalUtilities(local);

    // Guards the guard, and nothing more: the set itself is scanned out of `src/`,
    // so naming its members here would be the maintained list this file claims not
    // to keep — and every one of them would have to be added by hand on the day it
    // was written, which is the failure mode being tested. What must not happen
    // silently is the set becoming EMPTY (a regex that stops matching, a `sourceFiles`
    // that walks the wrong tree), because then the loop below asserts nothing.
    expect(
      [...uses.keys()],
      "no variant use of a first-party utility was found anywhere in `src/`, so the " +
        "assertions below are vacuous — either every such call site was removed, or " +
        "the scan above is broken.",
    ).not.toHaveLength(0);

    const css = await buildCss([...local, ...uses.keys()]);
    for (const [candidate, file] of uses) {
      expect(
        css,
        `\`${candidate}\` is used in ${file} but the build emits no ${selectorFor(candidate)} rule — ` +
          "the class is dead in the DOM. Its base utility must be declared with `@utility`.",
      ).toContain(selectorFor(candidate));
    }
    // The bare uses must keep working too: a typo'd `@utility` would drop them.
    for (const name of local) {
      expect(css, `\`${name}\` produces no CSS at all`).toContain(selectorFor(name));
    }
  });
});

describe("reduced motion", () => {
  it("suppresses motion by sweeping every element, not by naming classes", async () => {
    const css = await buildCss([
      "oto-pulse",
      "oto-enter",
      "open:oto-enter",
      "motion-safe:oto-enter",
      "motion-safe:oto-chime-swing",
    ]);

    // `motion-safe:` is a real guard: the rule lives inside no-preference, so
    // under `reduce` it is not applied at all.
    const safe = atRuleBody(css, /@media\s*\(prefers-reduced-motion:\s*no-preference\)/);
    expect(safe, "`motion-safe:` no longer compiles to a no-preference media query").not.toBeNull();
    expect(safe ?? "").toContain(".motion-safe\\:oto-enter");
    // The fūrin's swing (U9, ADR 0028) is the one animation whose only reason to
    // exist is decoration, so its guard is the one worth naming: under `reduce`
    // no rule for it is generated at all, before the sweep below is even reached.
    expect(safe ?? "").toContain(".motion-safe\\:oto-chime-swing");

    // `open:` carries no guard of its own, so the sweep is what stops it — and
    // the sweep has to reach `*`. A rule naming `.oto-enter` would not match
    // `.open\:oto-enter`; it is a different selector.
    const body = atRuleBody(css, /@media\s*\(prefers-reduced-motion:\s*reduce\)/);
    expect(body, "no prefers-reduced-motion: reduce block in the built CSS").not.toBeNull();
    // `*, *::before, *::after`, tolerating the minifier's `*,:before,:after`.
    expect(body ?? "").toMatch(/\*\s*,\s*\*?::?before\s*,\s*\*?::?after/);
    expect(body ?? "").toMatch(/animation-duration:\s*0?\.01ms\s*!important/);
    expect(body ?? "").toMatch(/animation-iteration-count:\s*1\s*!important/);
    expect(body ?? "").toMatch(/transition-duration:\s*0?\.01ms\s*!important/);
  });
});
