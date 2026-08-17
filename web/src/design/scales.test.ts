/**
 * The type and radius scales of §M.8, enforced — SPEC §M.7.
 *
 * ⭐ THE AXIS WITH NO VOCABULARY IS THE ONE THAT ROTS. Colour has had names and a
 * gate since the first commit, so nobody has ever written `bg-[#fbfaff]`. Type
 * and radius had neither, and the tree accumulated **309** `text-[Npx]` across 34
 * files and **59** `rounded-[Npx]` — which collapsed to seven font sizes and five
 * radii, four of those nine values appearing once or twice each. Nothing said
 * whether `text-[15px]` beside 183 uses of `text-[11px]` was a decision or a
 * typo, because nothing could: there was no scale for it to be off.
 *
 * §M.8 is that scale, derived from the census rather than invented, and this file
 * is what stops the census restarting. It rejects three spellings of the same
 * mistake:
 *
 *   1. a bracket — `text-[13px]`, `rounded-[4px]`, `[font-size:13px]`;
 *   2. Tailwind's OWN ladder — `text-sm`, `rounded-md`, bare `rounded`. These
 *      compile, which is what makes them worse than a bracket: a second complete
 *      vocabulary for the same axis, in a namespace this stylesheet extends
 *      rather than replaces, and no reviewer's eye separates `rounded-md` from
 *      `rounded-control` in a 200-character class string;
 *   3. a raw declaration in a stylesheet — `border-radius: 3px`. That is where
 *      this rot started: `index.css`'s focus ring carried a hand-written `3px`
 *      that agreed with the chip step by coincidence and with the controls it
 *      draws around by nothing at all.
 *
 * ⛔ THE SCALE IS READ OUT OF `index.css`, NOT LISTED HERE. A step added to
 * `@theme inline` is permitted here the moment it exists, and — the other
 * direction, which matters more — a step that no call site uses fails, because a
 * scale that grows ahead of its call sites is how the six honest steps become
 * eleven aspirational ones.
 *
 * ⚠️ COMMENTS ARE STRIPPED BEFORE SCANNING, for the same reason
 * `TestNoOtoTokenReachesTheSlackRenderer` reads string literals rather than
 * source text: the sentences above name every banned form, and a substring
 * search over raw bytes would fail on the file that exists to prevent them.
 */
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { SPEC_PATH, TOKENS_CSS, TOKENS_PATH, specSection } from "~/test/design-sources";

const HERE = path.dirname(fileURLToPath(import.meta.url));
/** `web/src`, the tree §M.8 governs. */
const SRC = path.resolve(HERE, "..");
const INDEX_CSS = path.join(SRC, "index.css");
const THEME = readFileSync(INDEX_CSS, "utf8");

/**
 * The names published into one of Tailwind's theme namespaces by `index.css` —
 * `--text-meta` → `meta`, `--radius-chip` → `chip`.
 */
const published = (namespace: string): readonly string[] =>
  [...THEME.matchAll(new RegExp(`^\\s*--${namespace}-([a-z0-9-]+)\\s*:`, "gm"))].flatMap((m) =>
    m[1] === undefined ? [] : [m[1]],
  );

const typeSteps = published("text");
const radiusSteps = published("radius");

/**
 * The spacing step names — `2xs`, `sm`, `lg`, … — read out of `index.css` the
 * same way, because they are also the list of width utilities this stylesheet
 * has quietly broken. See the assertion at the foot of this file.
 *
 * Longest first, so the alternation cannot let `xs` claim the tail of `2xs`.
 */
const spacingSteps = [...published("spacing")].sort((a, b) => b.length - a.length);

/**
 * The side and corner variants a radius utility composes with. `rounded-t-surface`
 * is the same decision as `rounded-surface`, and a rule that only knew the
 * unqualified form would miss both the legal use and the illegal one.
 */
const SIDES = "t|r|b|l|s|e|tl|tr|br|bl|ss|se|ee|es";

/** Tailwind's built-in font-size ladder — the second vocabulary, banned by (2). */
const BUILTIN_TEXT = ["xs", "sm", "base", "lg", "xl", "2xl", "3xl", "4xl", "5xl", "6xl", "7xl", "8xl", "9xl"];

/**
 * Tailwind's built-in radius ladder. `none` and `full` are deliberately absent:
 * they are shapes, not steps — a square corner and a pill/circle say something
 * the three-step scale cannot, and `rounded-full` has 17 honest call sites on
 * status dots.
 */
const BUILTIN_RADIUS = ["xs", "sm", "md", "lg", "xl", "2xl", "3xl", "4xl"];

/** A CSS length, which is what makes a bracket a *size* bracket rather than a colour one. */
const LENGTH = String.raw`\d[\d.]*(?:px|rem|em|pt|ch|ex|vh|vw|vmin|vmax|%)`;

interface Rule {
  readonly what: string;
  readonly find: RegExp;
  readonly fix: string;
}

const RULES: readonly Rule[] = [
  {
    what: "a font size written as a bracket",
    find: new RegExp(String.raw`(?:^|[^\w-])(text-\[[^\]]*${LENGTH}[^\]]*\])`, "g"),
    fix: `use a §M.8 type step — ${typeSteps.map((n) => `text-${n}`).join(", ")}`,
  },
  {
    what: "a corner radius written as a bracket",
    find: new RegExp(String.raw`(?:^|[^\w-])(rounded(?:-(?:${SIDES}))?-\[[^\]]*\])`, "g"),
    fix: `use a §M.8 radius step — ${radiusSteps.map((n) => `rounded-${n}`).join(", ")}`,
  },
  {
    what: "a size set through Tailwind's arbitrary-property syntax",
    find: /(\[(?:font-size|border-radius)\s*:[^\]]*\])/g,
    fix: "a bracket is a bracket whichever side of the colon the property is on; use the token",
  },
  {
    what: "Tailwind's own font-size ladder, which is a second vocabulary for the same axis",
    find: new RegExp(String.raw`(?:^|[^\w-])(text-(?:${BUILTIN_TEXT.join("|")}))(?![\w-])`, "g"),
    fix: `the product's sizes are ${typeSteps.map((n) => `text-${n}`).join(", ")} and nothing else`,
  },
  {
    what: "Tailwind's own radius ladder, or the unqualified `rounded`",
    find: new RegExp(
      String.raw`(?:^|[^\w-])(rounded(?:(?:-(?:${SIDES}))?-(?:${BUILTIN_RADIUS.join("|")}))?)(?![\w-])`,
      "g",
    ),
    fix: `the product's radii are ${radiusSteps.map((n) => `rounded-${n}`).join(", ")}, plus rounded-full and rounded-none`,
  },
];

/**
 * Source with its comments removed. The `//` case refuses to fire after a colon
 * so that a URL in a string keeps the rest of its line — a stripper that ate
 * `"https://…"` would blind this gate to whatever followed on that line.
 */
function stripComments(text: string): string {
  return text.replace(/\/\*[\s\S]*?\*\//g, " ").replace(/(^|[^:])\/\/[^\n]*/g, "$1");
}

/**
 * Every `.ts`/`.tsx`/`.css` under `src/` that is not generated and not itself a
 * test — the same corpus `index.css.test.ts` walks, for the same two reasons.
 * The rule is about markup the product renders, and a `.test.ts` is neither
 * rendered nor shipped; and this file's own rules are five regex sources that
 * spell out every banned form, so a gate that read them would be the only thing
 * it ever failed on.
 */
function sourceFiles(dir: string = SRC): readonly string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name !== "generated") out.push(...sourceFiles(full));
    } else if (/\.(tsx?|css)$/.test(entry.name) && !/\.(test|spec)\.tsx?$/.test(entry.name)) {
      out.push(full);
    }
  }
  return out;
}

const corpus: ReadonlyMap<string, string> = new Map(
  sourceFiles().map((file) => [path.relative(SRC, file), stripComments(readFileSync(file, "utf8"))]),
);

/**
 * The corpus MINUS the two stylesheets that declare the scale, for the "is this
 * step used" question only.
 *
 * ⛔ WITHOUT THIS, EVERY STEP IS ALWAYS IN USE. `--text-micro: …` in `index.css`
 * contains the string `text-micro`, so a declaration would answer the question
 * about its own call sites, and the unused-step assertion would be green over a
 * scale nothing in the product had ever asked for.
 */
const MARKUP = [...corpus]
  .filter(([file]) => !file.endsWith(".css"))
  .map(([, text]) => text)
  .join("\n");

describe("the type and radius scales", () => {
  // Guards the guard, three ways. Every assertion below is "does the tree contain
  // one of these", and the answer over an empty scale, an empty corpus or a
  // corpus the comment-stripper has emptied is always no.
  it("has a scale, a tree to check it against, and a stripper that left the code behind", () => {
    expect(typeSteps.length, `no --text-* steps parsed out of ${INDEX_CSS}`).toBeGreaterThanOrEqual(3);
    expect(radiusSteps.length, `no --radius-* steps parsed out of ${INDEX_CSS}`).toBeGreaterThanOrEqual(2);
    expect(corpus.size, "walked src/ and found nothing to read").toBeGreaterThan(20);

    expect(
      typeSteps.some((step) => new RegExp(String.raw`(?<![\w-])text-${step}(?![\w-])`).test(MARKUP)),
      "not one type step survives in the stripped corpus, so the stripper has removed the markup " +
        "rather than the comments and every rule below now scans a blank page.",
    ).toBe(true);
  });

  for (const rule of RULES) {
    it(`rejects ${rule.what}`, () => {
      const offences: string[] = [];
      for (const [file, text] of corpus) {
        for (const m of text.matchAll(rule.find)) {
          offences.push(`${file}: ${m[1] ?? m[0]}`);
        }
      }
      expect(
        offences,
        `§M.8 gives this axis a vocabulary; ${rule.fix}. If the value genuinely is not on the ` +
          "scale, that is an amendment (§N): add the step to `tokens.css` and `index.css`, " +
          "tabulate it in §M.8, and say what it is for — do not spell it out at the call site, " +
          "which is how 368 of these accumulated in the first place.",
      ).toEqual([]);
    });
  }

  it("rejects a font size or a corner radius hand-written in a stylesheet", () => {
    const offences: string[] = [];
    for (const [file, text] of corpus) {
      if (!file.endsWith(".css")) continue;
      for (const m of text.matchAll(/(?:^|[;{])\s*(font-size|border-radius)\s*:\s*([^;}]+)/g)) {
        const [, property, value] = m;
        if (value !== undefined && /var\(\s*--oto-(?:type|radius)-/.test(value)) continue;
        offences.push(`${file}: ${property ?? ""}: ${(value ?? "").trim()}`);
      }
    }
    expect(
      offences,
      "a declaration in a stylesheet is the one place a size can hide from the class-name rules " +
        "above — and it is where the focus ring's `border-radius: 3px` sat for months, agreeing " +
        "with the chip step by coincidence. Point it at `var(--oto-type-*)` or `var(--oto-radius-*)`.",
    ).toEqual([]);
  });

  // The parity §M.4/§M.5 get from `tokens.test.ts`, for the axes they do not cover.
  // §M.8 quotes the stylesheet, and a quote nothing compares is a claim.
  it("declares in §M.8 exactly the steps `tokens.css` declares, with the same values", () => {
    const steps = (css: string): ReadonlyMap<string, string> =>
      new Map(
        [...css.matchAll(/(--oto-(?:type|radius)-[a-z0-9-]+)\s*:\s*([^;]+);/g)].flatMap((m) =>
          m[1] === undefined || m[2] === undefined ? [] : [[m[1], m[2].trim()] as const],
        ),
      );

    const declared = steps(specSection("### M.8"));
    const actual = steps(TOKENS_CSS);

    // Guards the guard: over an empty SPEC side every comparison below is vacuous,
    // which is what a renamed or renumbered heading would produce.
    expect(
      declared.size,
      `no --oto-type-*/--oto-radius-* declarations found in §M.8 of ${SPEC_PATH} — either the ` +
        "section moved and this gate is reading nothing, or the CSS block it quotes was removed.",
    ).toBeGreaterThanOrEqual(6);

    expect(
      [...actual.keys()].sort(),
      `the step NAMES in ${TOKENS_PATH} and in §M.8 of ${SPEC_PATH} differ. A step §M.8 declares ` +
        "and the stylesheet does not is unimplemented; one the stylesheet declares and §M.8 does " +
        "not was never specified, and U10 sends a reader to §M.8 to find out what the scale is.",
    ).toEqual([...declared.keys()].sort());

    for (const [name, value] of declared) {
      expect(
        actual.get(name),
        `${name} is \`${value}\` in §M.8 and \`${String(actual.get(name))}\` in the stylesheet. ` +
          "§M.8 tabulates what each step is for beside its value; one of the two edits is unfinished.",
      ).toBe(value);
    }
  });

  it("rejects a width utility whose name a spacing step has shadowed", () => {
    const shadowed = new RegExp(
      String.raw`(?<![\w-])(max-w-(?:${spacingSteps.join("|")}))(?![\w-])`,
      "g",
    );
    const offences: string[] = [];
    for (const [file, text] of corpus) {
      for (const m of text.matchAll(shadowed)) offences.push(`${file}: ${m[1] ?? m[0]}`);
    }
    expect(
      offences,
      "in Tailwind v4 a NAMED width key is resolved against the SPACING namespace before the " +
        "container namespace, and `index.css` publishes spacing steps called 2xs, xs, sm, md, lg " +
        "and xl. So `max-w-sm` does not mean 24rem here — it compiles to `max-width: " +
        "var(--oto-space-sm)`, 8px, and `max-w-lg` to 16px. Nothing warns: the class exists, it " +
        "is spelled correctly, and it renders a dialog as a sliver. State the width as a NUMERIC " +
        "multiple instead, which has no name for a spacing step to collide with — max-w-80 for " +
        "the old xs, 96 for sm, 112 for md, 128 for lg, 144 for xl. Renaming the spacing steps is " +
        "not the fix: `px-md`/`gap-lg` are the product's spacing vocabulary and §M.8 tabulates " +
        "them under exactly these names.",
    ).toEqual([]);
  });

  it("keeps every step it declares in use, so the scale cannot outgrow the product", () => {
    const unused = [
      ...typeSteps.filter((step) => !new RegExp(String.raw`(?<![\w-])text-${step}(?![\w-])`).test(MARKUP)),
      ...radiusSteps.filter(
        (step) =>
          !new RegExp(String.raw`(?<![\w-])rounded(?:-(?:${SIDES}))?-${step}(?![\w-])`).test(MARKUP),
      ),
    ];
    expect(
      unused,
      "these §M.8 steps are declared and used nowhere. Every step in the scale was derived from " +
        "call sites that already existed (see the ADR); one with no call site is a size somebody " +
        "expected the product to need, and the next reader cannot tell it from a step whose last " +
        "use was deleted by accident.",
    ).toEqual([]);
  });
});
