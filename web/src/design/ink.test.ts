/**
 * §M.9's decorative ink, against the stylesheet, the assets and the compiler —
 * SPEC §M.7.
 *
 * ⭐⭐ THE ❌ ROW IS THE REASON THIS FILE EXISTS. §M.9 justifies a whole design
 * decision — the carve-out geometry — with one number: `--oto-text-subtle` drops
 * from 4.90:1 to **4.37:1** under a flat 6% wash, an AA failure in light and not
 * in dark. Nothing else in the tree can see that. `contrast.test.ts` measures
 * token PAIRS and a tint behind text is a third colour; the axe row of §M.7 that
 * would see it is UNWRITTEN. A number that justifies a decision and is checked by
 * nobody is exactly the kind of claim this repository has a rule about
 * (README.md) — so every composite §M.9 tabulates is recomputed here, including
 * the one that fails, and a row whose ✅/❌ disagrees with the formula fails the
 * build.
 *
 * Four assertions, and they fail on four different mistakes:
 *
 *   1. a tabulated composite that is not what the WCAG formula gives, or whose
 *      verdict column disagrees with it;
 *   2. a tint §M.9 quotes that `tokens.css` does not derive that way — the row
 *      would then measure a colour nobody ships;
 *   3. a tint that has drifted INTO a `[data-theme]` block, where it would owe
 *      `contrast.test.ts` a measured pair it has no way to supply;
 *   4. a motif the `Motif` union names with no asset behind it, or an asset
 *      without `preserveAspectRatio="none"` — the failure that presents as the
 *      content having vanished rather than as a scaling bug.
 *
 * ⛔ NOTHING BELOW NAMES A TOKEN, A HEX, A RATIO OR A MOTIF. All four sides are
 * read off disk, so a tint added to §M.9 and not to the stylesheet, or a motif
 * added to the union and not to `web/public/motifs/`, fails on the day it is
 * written with nobody having to remember this file exists.
 */
import { readFileSync, readdirSync } from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { compile } from "tailwindcss";
import { describe, expect, it } from "vitest";

import {
  REPO_ROOT,
  SPEC_PATH,
  TOKENS_CSS,
  TOKENS_PATH,
  type Theme,
  contrastRatio,
  specSection,
  themeTokens,
} from "~/test/design-sources";

const SRC = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const INDEX_CSS = path.join(SRC, "index.css");
const INK_TSX = path.join(SRC, "components", "ui", "Ink.tsx");
const MOTIF_DIR = path.join(REPO_ROOT, "web", "public", "motifs");
const M9 = specSection("### M.9");

/* -------------------------------------------------------------------------- */
/* The tints, as `tokens.css` actually derives them                           */
/* -------------------------------------------------------------------------- */

interface Derived {
  /** The themed token the tint is mixed from. */
  readonly from: string;
  /** Its share of the mix, as a percentage. */
  readonly pct: number;
}

/**
 * `--oto-wash-heading: color-mix(in oklab, var(--oto-border-strong) 12%, transparent)`
 * → `["--oto-wash-heading", { from: "--oto-border-strong", pct: 12 }]`.
 *
 * The shape is asserted rather than assumed: a tint written any other way (a
 * literal `rgba()`, a mix in a different space, a mix with something other than
 * `transparent`) simply does not parse, and the count assertion below catches
 * that rather than letting the suite go quietly green over an empty map.
 */
const DERIVATION =
  /(--oto-wash[a-z-]*)\s*:\s*color-mix\(\s*in\s+oklab\s*,\s*var\(\s*(--oto-[a-z-]+)\s*\)\s*([0-9.]+)%\s*,\s*transparent\s*\)/g;

const derived = new Map<string, Derived>(
  [...TOKENS_CSS.matchAll(DERIVATION)].flatMap(([, name, from, pct]) =>
    name === undefined || from === undefined || pct === undefined
      ? []
      : [[name, { from, pct: Number.parseFloat(pct) }] as const],
  ),
);

/* -------------------------------------------------------------------------- */
/* §M.9's composite table                                                     */
/* -------------------------------------------------------------------------- */

interface Composite {
  readonly what: string;
  readonly theme: Theme;
  readonly textToken: string;
  readonly inkToken: string;
  readonly pct: number;
  readonly bgToken: string;
  readonly stated: number;
  readonly floor: number;
  readonly verdict: string;
}

/** `| page heading, muted brush | light | `--oto-text` | `--oto-border-strong` 12% on `--oto-surface` | **12.95:1** | 4.5 | ✅ |` */
const ROW =
  /^\|\s*([^|]+?)\s*\|\s*(light|dark)\s*\|\s*`(--oto-[a-z-]+)`\s*\|\s*`(--oto-[a-z-]+)`\s*([0-9.]+)%\s*on\s*`(--oto-[a-z-]+)`\s*\|\s*\*\*([0-9.]+):1\*\*\s*\|\s*([0-9.]+)\s*\|\s*(\S+)\s*\|/;

/** A line that MEANS to be a measured composite, whether or not `ROW` can read it. */
const looksLikeARow = (line: string): boolean => line.startsWith("|") && line.includes(":1**");

const candidates = M9.split("\n").filter(looksLikeARow);

const composites: readonly Composite[] = candidates.flatMap((line): readonly Composite[] => {
  const m = ROW.exec(line);
  if (m === null) return [];
  const [, what, theme, textToken, inkToken, pct, bgToken, stated, floor, verdict] = m;
  if (
    what === undefined ||
    theme === undefined ||
    textToken === undefined ||
    inkToken === undefined ||
    pct === undefined ||
    bgToken === undefined ||
    stated === undefined ||
    floor === undefined ||
    verdict === undefined
  ) {
    return [];
  }
  return [
    {
      what,
      theme: theme as Theme,
      textToken,
      inkToken,
      pct: Number.parseFloat(pct),
      bgToken,
      stated: Number.parseFloat(stated),
      floor: Number.parseFloat(floor),
      verdict,
    },
  ];
});

const TOKENS: Record<Theme, ReadonlyMap<string, string>> = {
  light: themeTokens(TOKENS_CSS, "light"),
  dark: themeTokens(TOKENS_CSS, "dark"),
};

const rgb = (hex: string): readonly [number, number, number] => {
  const n = Number.parseInt(hex.slice(1), 16);
  return [(n >> 16) & 0xff, (n >> 8) & 0xff, n & 0xff];
};

/** `hex` at `alpha` over `bg`, composited in sRGB, back to a six-digit hex. */
function over(hex: string, bg: string, alpha: number): string {
  const [f, b] = [rgb(hex), rgb(bg)];
  const channel = (i: number): string =>
    Math.round(alpha * (f[i] ?? 0) + (1 - alpha) * (b[i] ?? 0))
      .toString(16)
      .padStart(2, "0");
  return `#${channel(0)}${channel(1)}${channel(2)}`;
}

describe("§M.9 measured composites", () => {
  // Guards the guard, twice. A table this file cannot read is a table this file
  // does not check, and the failure mode of a parser that drops everything is
  // silence — so the section must contain rows, and EVERY line that means to be a
  // measurement must have been read as one.
  it("reads every row of the table", () => {
    expect(
      candidates.length,
      `no measured composites found in §M.9 of ${SPEC_PATH} — either the section moved or its ` +
        "table was removed, and this whole file is then asserting nothing.",
    ).toBeGreaterThan(4);
    expect(
      composites.length,
      `${candidates.length - composites.length} row(s) in §M.9 look like measurements and did not ` +
        "parse. An unparsed row is an unchecked row.",
    ).toBe(candidates.length);
  });

  it.each([...composites])("$what ($theme)", (c: Composite) => {
    const text = TOKENS[c.theme].get(c.textToken);
    const ink = TOKENS[c.theme].get(c.inkToken);
    const bg = TOKENS[c.theme].get(c.bgToken);

    for (const [name, value] of [
      [c.textToken, text],
      [c.inkToken, ink],
      [c.bgToken, bg],
    ] as const) {
      expect(
        value,
        `§M.9 composites ${name}, which the ${c.theme} theme does not declare. The row measures a ` +
          "colour nobody ships.",
      ).toBeDefined();
    }

    // The tint has to be one `tokens.css` actually derives, from this token at
    // this percentage — otherwise the row is arithmetic about a wash that does
    // not exist.
    const match = [...derived.values()].some((d) => d.from === c.inkToken && d.pct === c.pct);
    expect(
      match,
      `§M.9 measures ${c.inkToken} at ${c.pct}%, and no --oto-wash* token in ${TOKENS_PATH} is ` +
        `derived that way. Declared: ${[...derived]
          .map(([n, d]) => `${n} = ${d.from} ${d.pct}%`)
          .join("; ")}.`,
    ).toBe(true);

    const actual = contrastRatio(text ?? "#000000", over(ink ?? "#000000", bg ?? "#ffffff", c.pct / 100));

    // ±0.05 — the table publishes one decimal place, so rounding is the only
    // difference it may tolerate. Same tolerance §M.4/§M.5 get.
    expect(
      actual,
      `${c.textToken} over ${c.inkToken} ${c.pct}% on ${c.bgToken} computes to ` +
        `${actual.toFixed(3)}:1 and §M.9 publishes ${c.stated}:1. The formula is the SPEC's own, ` +
        "so the published number is the one that is wrong.",
    ).toBeCloseTo(c.stated, 1);

    // ⛔ BOTH DIRECTIONS. A ✅ that fails is an accessibility defect; a ❌ that
    // passes is worse in its own way — §M.9 spends a ❌ row arguing for the
    // carve-out geometry, and if the number stopped failing the argument would be
    // fiction while still reading as evidence.
    if (c.verdict === "✅") {
      expect(
        actual,
        `${c.what} (${c.theme}) is marked ✅ and computes to ${actual.toFixed(2)}:1, under its ` +
          `${c.floor}:1 floor. This is an accessibility failure, not a documentation one.`,
      ).toBeGreaterThanOrEqual(c.floor);
      return;
    }
    expect(c.verdict, `unknown verdict "${c.verdict}" — §M.9's Pass column is ✅ or ❌`).toBe("❌");
    expect(
      actual,
      `${c.what} (${c.theme}) is marked ❌ but computes to ${actual.toFixed(2)}:1, which CLEARS ` +
        `${c.floor}:1. §M.9 cites this row as the reason the ambient wash relies on geometry ` +
        "rather than on being faint — if it now passes, that argument needs rewriting rather than " +
        "leaving in place.",
    ).toBeLessThan(c.floor);
  });
});

describe("the tints themselves", () => {
  it("derives every one of them from a themed token, in one declaration", () => {
    expect(
      [...derived.keys()],
      `no --oto-wash* token in ${TOKENS_PATH} parses as ` +
        "`color-mix(in oklab, var(--oto-*) N%, transparent)`. Either they are gone, or one is " +
        "written some other way — a literal `rgba()` is correct in exactly one theme, and a mix " +
        "with anything but `transparent` is not a tint.",
    ).not.toHaveLength(0);
  });

  it("keeps them out of every [data-theme] block", () => {
    // ⛔ THIS IS NOT TIDINESS. A tint inside a palette block is a token
    // `contrast.test.ts` demands a measured §M.4/§M.5 pair for — and a decorative
    // wash has no pair to give, because its obligation is a composite. It is also
    // the only way the two themes could disagree about a value that is supposed
    // to be derived from exactly one declaration.
    const themed = [...TOKENS.light.keys(), ...TOKENS.dark.keys()].filter((n) =>
      n.startsWith("--oto-wash"),
    );
    expect(
      [...new Set(themed)].sort(),
      "these decorative tints are declared inside a `[data-theme]` block. They must sit in the " +
        "bare `:root` block beside `--oto-row-h`, where they inherit the themed token they mix " +
        "and are therefore correct in both palettes from one line (§M.9).",
    ).toEqual([]);
  });
});

describe("the motif assets", () => {
  /** The `Motif` union in `Ink.tsx`, read rather than restated. */
  const union = (): readonly string[] => {
    const m = /export type Motif =([^;]+);/.exec(readFileSync(INK_TSX, "utf8"));
    return [...(m?.[1] ?? "").matchAll(/"([a-z-]+)"/g)].flatMap((x) => (x[1] === undefined ? [] : [x[1]]));
  };

  const onDisk = (): readonly string[] =>
    readdirSync(MOTIF_DIR).filter((f) => f.endsWith(".svg"));

  it("has a union to check, so the assertions below are not vacuous", () => {
    expect(union().length, `no Motif members parsed out of ${INK_TSX}`).toBeGreaterThan(2);
  });

  it("ships an asset for every motif the union names, and names every asset it ships", () => {
    expect(
      [...union()].sort(),
      "the `Motif` union and `web/public/motifs/` disagree. A member with no file resolves to " +
        "nothing at runtime and paints a flat rectangle of wash over whatever it was positioned " +
        "against; a file with no member is art nothing can reach.",
    ).toEqual(onDisk().map((f) => f.replace(/\.svg$/, "")).sort());
  });

  it("carries preserveAspectRatio=\"none\" on every one of them", () => {
    const missing = onDisk().filter(
      (f) => !readFileSync(path.join(MOTIF_DIR, f), "utf8").includes('preserveAspectRatio="none"'),
    );
    expect(
      missing,
      "the SVG default is `xMidYMid meet`, which letterboxes the art inside the mask box — and " +
        "the mask then goes TRANSPARENT at the edges. That does not present as a scaling bug. It " +
        "presents as the content having vanished, which is the hardest possible symptom to trace " +
        "back to an attribute nobody set (§M.9, and the README beside the files).",
    ).toEqual([]);
  });
});

describe("the `oto-ink` utility", () => {
  const requireFrom = createRequire(INDEX_CSS);

  async function loadStylesheet(id: string, base: string) {
    const file = id.startsWith(".")
      ? path.resolve(base, id)
      : requireFrom.resolve(id.endsWith(".css") ? id : `${id}/index.css`);
    return { path: file, base: path.dirname(file), content: readFileSync(file, "utf8") };
  }

  /** The body of `.oto-ink`, brace-matched out of the CSS the build emits. */
  async function ruleBody(): Promise<string> {
    const compiler = await compile(readFileSync(INDEX_CSS, "utf8"), { base: SRC, loadStylesheet });
    const css = compiler.build(["oto-ink"]);
    const at = css.indexOf(".oto-ink");
    expect(at, "the build emits no `.oto-ink` rule at all").toBeGreaterThan(-1);
    const open = css.indexOf("{", at);
    let depth = 0;
    for (let i = open; i < css.length; i++) {
      if (css[i] === "{") depth++;
      else if (css[i] === "}" && --depth === 0) return css.slice(open + 1, i);
    }
    throw new Error("unbalanced braces in the emitted `.oto-ink` rule");
  }

  // ⛔ THIS CANNOT BE A COMPONENT TEST, FOR THE REASON `index.css.test.ts` GIVES
  // ABOUT ITS OWN SUBJECT: the failure mode is *absence*, and absence is
  // invisible from the DOM. `class="oto-ink"` is present in the markup whether or
  // not the rule behind it declares anything. What is at stake is the exemption
  // itself — decorative ink is outside WCAG 1.4.3/1.4.11 only for as long as it
  // is genuinely decorative, and each line below is part of what buys that.
  it("compiles with the whole accessibility contract in it", async () => {
    const body = await ruleBody();
    for (const [declaration, why] of [
      [
        /pointer-events:\s*none/,
        "ink that can be clicked is not decoration; it is an invisible target over the content " +
          "behind it",
      ],
      [
        /user-select:\s*none/,
        "a mask has no text, but a selection dragged across it still stops there",
      ],
      [
        /@media\s*\(forced-colors:\s*active\)[\s\S]*?display:\s*none/,
        "under forced colours the OS overrides the tint to a system colour at FULL strength, and " +
          "a 6% wash becomes an opaque slab across the panel. This is the one everybody misses, " +
          "and it is why the ink is a single utility rather than three lines per motif",
      ],
      [
        /mask-image:\s*var\(--oto-ink-motif\)/,
        "the art has to arrive as a mask; a `background-image` cannot take a token fill and " +
          "therefore needs one asset per theme",
      ],
    ] as const) {
      expect(body, `\`oto-ink\` no longer declares this, and §M.9 says why it must: ${why}.`).toMatch(
        declaration,
      );
    }
  });
});
