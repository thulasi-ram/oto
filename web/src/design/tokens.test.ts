/**
 * `tokens.css` against §M.4 and §M.5 — SPEC §M.7.
 *
 * Two assertions, and they fail on two different mistakes.
 *
 * ⭐ THEME PARITY. Every token must be declared in BOTH themes. A token defined
 * in light only does not fall back to a default in dark: the custom property is
 * simply undefined, `var(--oto-state-storm-fill)` resolves to nothing, and the
 * element renders with a transparent background against whatever is behind it.
 * In dark that is usually still legible enough to survive review, which is how a
 * one-theme token gets shipped. Nothing in the type system, the build or the
 * browser reports it; a set difference does.
 *
 * ⭐ SPEC PARITY. §M.4/§M.5 quote the stylesheet verbatim and CI now asserts the
 * contrast ratios they tabulate — so if the stylesheet drifts from the quoted
 * block, every ratio in the SPEC is a measurement of a palette nobody ships.
 * That is worse than an unmeasured palette, because it reads as verified.
 *
 * ⛔ NEITHER ASSERTION NAMES A TOKEN. Both sides are read off disk (see
 * `~/test/design-sources`), so a token added to §M.4 and to one theme fails here
 * on the day it is written, with nobody having to remember this file exists.
 */
import { describe, expect, it } from "vitest";

import { SPEC_PATH, TOKENS_CSS, TOKENS_PATH, specSection, themeTokens } from "~/test/design-sources";

const light = themeTokens(TOKENS_CSS, "light");
const dark = themeTokens(TOKENS_CSS, "dark");

/** In `a` and not in `b`, sorted so a failure message is stable. */
const missing = (a: ReadonlyMap<string, string>, b: ReadonlyMap<string, string>): readonly string[] =>
  [...a.keys()].filter((name) => !b.has(name)).sort();

describe("design tokens", () => {
  // Guards the guard. Every assertion below is a comparison, and a comparison of
  // two empty sets passes. If the block reader stops matching — a selector
  // rewritten, `[data-theme="dark"]` replaced by a media query — this is the one
  // that says so instead of the suite going quietly green.
  it("reads both theme blocks out of the stylesheet at all", () => {
    expect(light.size, `no light-theme tokens parsed out of ${TOKENS_PATH}`).toBeGreaterThan(30);
    expect(dark.size, `no dark-theme tokens parsed out of ${TOKENS_PATH}`).toBeGreaterThan(30);
  });

  it("declares exactly the same token names in light and in dark", () => {
    expect(
      missing(light, dark),
      "declared for light and missing from dark. An undeclared custom property does not fall " +
        "back — it resolves to nothing, and the element renders unstyled in that theme.",
    ).toEqual([]);
    expect(
      missing(dark, light),
      "declared for dark and missing from light. Same failure, other way round — and harder to " +
        "see, because dark is the default theme (§M.3 U3) and light is the one nobody opens.",
    ).toEqual([]);
  });

  it("carries the values §M.4 and §M.5 declare, token for token", () => {
    for (const [theme, section, actual] of [
      ["light", "### M.4", light],
      ["dark", "### M.5", dark],
    ] as const) {
      const declared = themeTokens(specSection(section), theme);

      expect(
        [...actual.keys()].sort(),
        `the ${theme} token NAMES in the stylesheet and in ${section} of ${SPEC_PATH} differ. ` +
          "A token the SPEC declares and the stylesheet does not is unimplemented; one the " +
          "stylesheet declares and the SPEC does not was never specified. (An empty set on the " +
          "SPEC side means the section moved and this gate is reading nothing.)",
      ).toEqual([...declared.keys()].sort());

      for (const [name, value] of declared) {
        expect(
          actual.get(name),
          `${name} is \`${value}\` in ${section} and \`${String(actual.get(name))}\` in the ` +
            "stylesheet. §M.4/§M.5 quote this file verbatim and the measured ratios beside them " +
            "are computed from these values; one of the two edits is unfinished.",
        ).toBe(value);
      }
    }
  });
});
