/**
 * The measured contrast tables of §M.4 and §M.5, actually measured — SPEC §M.7,
 * AC-45.
 *
 * ⭐⭐ THE TABLES WERE WRONG. When this gate was first run against HEAD, THIRTEEN
 * of the thirty-nine tabulated ratios disagreed with the WCAG 2.x formula the
 * SPEC says it used, by up to 0.68 — `--oto-state-expired-solid` on `--oto-bg`
 * was published as 4.3:1 and is 5.0:1, and `--oto-text` on `--oto-bg` (dark) was
 * published as 16.4:1 and is 15.6:1. Every one of them still cleared its
 * requirement, which is exactly why nobody caught it: the numbers were wrong in
 * a direction that changed no decision, and a number that changes no decision is
 * a number nobody re-derives. They were corrected in the commit that added this
 * file. That is the whole argument for computing them here rather than reading
 * them: a hand-measured ratio is a claim, and this repository has a rule about
 * claims (README.md).
 *
 * Each row is checked three ways:
 *
 *   1. the hex the row quotes IS what `tokens.css` declares for that token, in
 *      that theme — otherwise the row measures a colour the product does not use;
 *   2. the ratio the row states is what the formula gives, ±0.05 (one decimal
 *      place is what the table publishes, so the tolerance is rounding and
 *      nothing else);
 *   3. the ratio clears the requirement in the row, and the ✅/n/a the row
 *      carries agrees with that verdict.
 *
 * ⚠️ WHAT THIS IS NOT. It proves the PALETTE is sound. It cannot prove the
 * product puts those pairs together: a component that sets `--oto-text-subtle`
 * on `--oto-state-firing-fill` composes two verified tokens into an unverified
 * pair, and only something that reads rendered DOM — the axe row of §M.7, still
 * unwritten — can see that.
 */
import { describe, expect, it } from "vitest";

import {
  SPEC_PATH,
  TOKENS_CSS,
  type Theme,
  contrastRatio,
  specSection,
  themeTokens,
} from "~/test/design-sources";

interface Pair {
  readonly theme: Theme;
  readonly fgToken: string;
  readonly fg: string;
  readonly bgToken: string | null;
  readonly bg: string;
  readonly stated: number;
  /** `null` for the decorative hairline row, which states `—`. */
  readonly required: number | null;
  readonly pass: string;
}

/** `| `--oto-text` `#1E1B2E` | `--oto-surface` `#FFFFFF` | **16.8:1** | 4.5 | ✅ |` */
const ROW =
  /^\|\s*`(--oto-[a-z0-9-]+)`\s*`(#[0-9A-Fa-f]{6})`\s*\|\s*(?:`(--oto-[a-z0-9-]+)`\s*)?`(#[0-9A-Fa-f]{6})`\s*\|\s*\*\*([0-9]+(?:\.[0-9]+)?):1\*\*\s*\|([^|]*)\|([^|]*)\|/;

/** A line that means to be a measured pair, whether or not `ROW` can read it. */
const looksLikeARow = (line: string): boolean => line.startsWith("|") && line.includes(":1**");

interface Table {
  /** Lines that mean to be a measured pair. */
  readonly candidates: readonly string[];
  /** Those of them this file could actually read. */
  readonly pairs: readonly Pair[];
}

function tableOf(theme: Theme, section: string): Table {
  const candidates = specSection(section).split("\n").filter(looksLikeARow);

  const pairs = candidates.flatMap((line): readonly Pair[] => {
    const m = ROW.exec(line);
    if (m === null) return [];
    const [, fgToken, fg, bgToken, bg, stated, requirement, pass] = m;
    if (fgToken === undefined || fg === undefined || bg === undefined || stated === undefined) {
      return [];
    }
    const req = /^\s*([0-9]+(?:\.[0-9]+)?)/.exec(requirement ?? "");
    return [
      {
        theme,
        fgToken,
        fg,
        bgToken: bgToken ?? null,
        bg,
        stated: Number.parseFloat(stated),
        required: req?.[1] === undefined ? null : Number.parseFloat(req[1]),
        pass: (pass ?? "").trim(),
      },
    ];
  });

  return { candidates, pairs };
}

const TOKENS: Record<Theme, ReadonlyMap<string, string>> = {
  light: themeTokens(TOKENS_CSS, "light"),
  dark: themeTokens(TOKENS_CSS, "dark"),
};

function describeTable(theme: Theme, section: string): void {
  const { candidates, pairs } = tableOf(theme, section);

  describe(`measured contrast ratios (${theme}, ${section})`, () => {
    // Guards the guard, twice over. A table this file cannot read is a table this
    // file does not check, and the failure mode of a parser that drops everything
    // is silence — an empty table produces an empty suite and a green tick. So:
    // the section must contain rows, and EVERY line that looks like a measurement
    // must have been read as one.
    it("reads every row of the table", () => {
      expect(
        candidates.length,
        `no measured-ratio rows found in ${section} of ${SPEC_PATH}`,
      ).toBeGreaterThan(10);
      expect(
        pairs.length,
        `${candidates.length - pairs.length} row(s) in ${section} look like measurements and did ` +
          "not parse. Either the table's shape changed, or a row was written by hand in a shape " +
          "the rest of the table does not use — and an unparsed row is an unchecked row.",
      ).toBe(candidates.length);
    });

    it.each([...pairs])("$fgToken on $bg", (pair: Pair) => {
      for (const [token, hex] of [
        [pair.fgToken, pair.fg],
        [pair.bgToken, pair.bg],
      ] as const) {
        if (token === null) continue;
        expect(
          TOKENS[pair.theme].get(token),
          `${section} measures ${token} as \`${hex}\`, but the ${pair.theme} theme declares ` +
            `\`${String(TOKENS[pair.theme].get(token))}\`. The row measures a colour nobody ships.`,
        ).toBe(hex.toLowerCase());
      }

      const actual = contrastRatio(pair.fg, pair.bg);

      // `toBeCloseTo(…, 1)` IS the ±0.05 §M.7 asks for: the table publishes one
      // decimal place, so rounding is the only difference it may tolerate.
      expect(
        actual,
        `${pair.fgToken} on ${pair.bgToken ?? pair.bg} computes to ${actual.toFixed(3)}:1 by the ` +
          `WCAG 2.x formula, and ${section} publishes ${pair.stated}:1. The formula is the SPEC's ` +
          "own (§M.4), so the published number is the one that is wrong.",
      ).toBeCloseTo(pair.stated, 1);

      if (pair.required === null) {
        expect(
          pair.pass,
          `${pair.fgToken} states no requirement, so its Pass column must say n/a rather than ` +
            "claim a pass it never defined",
        ).toBe("n/a");
        return;
      }
      expect(
        actual,
        `${pair.fgToken} on ${pair.bgToken ?? pair.bg} is ${actual.toFixed(2)}:1 and must clear ` +
          `${pair.required}:1 (AC-45). This is an accessibility failure, not a documentation one.`,
      ).toBeGreaterThanOrEqual(pair.required);
      expect(pair.pass, "the row clears its requirement and its Pass column does not say so").toBe(
        "✅",
      );
    });
  });
}

describeTable("light", "### M.4");
describeTable("dark", "### M.5");
