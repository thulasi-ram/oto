/**
 * The half of §M.9's contract that lives in the DOM rather than in the CSS —
 * SPEC §M.7.
 *
 * `ink.test.ts` proves the tints, the assets and the `oto-ink` rule. This file
 * proves the three things only a rendered tree (and the shape of the tree that
 * is *not* rendered) can say:
 *
 *   1. the ink is `aria-hidden` and points at a real motif, so a decorative mask
 *      never reaches assistive tech and never resolves to a flat rectangle;
 *   2. a carve-out compiles to `mask-composite: intersect` — the geometry §M.9
 *      relies on instead of a carefully chosen opacity;
 *   3. **the six-at-once trap.** The shared `EmptyState` has eighteen call sites
 *      and six of them are sub-panels on ONE alert-detail page. A wash on the
 *      shared component renders six of them on one quiet alert, which is the
 *      exact failure mode §M.9 was written to avoid. That is a fact about the
 *      tree and not about one render, so it is read off disk.
 */
import { render } from "@solidjs/testing-library";
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { EmptyState, PageEmptyState } from "./states";
import { Ink, clearColumn } from "./Ink";

const SRC = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

/** The one element `Ink` renders, whichever wrapper it was handed to. */
const inkIn = (container: HTMLElement): HTMLElement | null => container.querySelector(".oto-ink");

describe("decorative ink in the DOM", () => {
  it("is hidden from assistive tech and inert to the pointer", () => {
    const { container } = render(() => <Ink motif="kumo" />);
    const ink = inkIn(container);

    expect(ink, "no `.oto-ink` element rendered").not.toBeNull();
    // ⛔ `aria-hidden` is the one guarantee no stylesheet can make, which is why
    // `Ink.tsx` exists at all rather than the utility being applied by hand.
    expect(ink?.getAttribute("aria-hidden")).toBe("true");
    // Nothing to read: a motif is a second channel beside a sentence (U1), never
    // a substitute for it, so it must contribute no text of its own.
    expect(ink?.textContent).toBe("");
  });

  it("points at the motif it was asked for", () => {
    const { container } = render(() => <Ink motif="sakura" />);
    expect(inkIn(container)?.style.getPropertyValue("--oto-ink-motif")).toBe(
      "url(/motifs/sakura.svg)",
    );
  });

  it("intersects a carve-out with the art rather than fading the ink", () => {
    const { container } = render(() => (
      <Ink motif="enso" carve={clearColumn("400px")} size="28rem 28rem, 100% 100%" />
    ));
    const style = inkIn(container)?.style;

    // ⭐ THE VALUE IS `intersect`, AND `add` WOULD BE SILENT. With the default
    // compositing the carve-out layer UNIONS with the art — every pixel the
    // gradient covers becomes ink instead of every pixel it clears becoming
    // transparent — so the guarantee inverts into a slab across the whole box
    // while every other assertion here still passes.
    expect(style?.getPropertyValue("--oto-ink-composite")).toBe("intersect");
    expect(style?.getPropertyValue("--oto-ink-motif")).toMatch(
      /^url\(\/motifs\/enso\.svg\), linear-gradient\(/,
    );
    // The transparent band is stated as a width and centred with `calc`, so it is
    // the same width at every viewport size — no media query, nothing to tune.
    expect(style?.getPropertyValue("--oto-ink-motif")).toContain("calc(50% - 400px / 2)");
  });

  it("takes no tint override unless one is asked for, so the default cannot drift", () => {
    const plain = render(() => <Ink motif="kumo" />);
    expect(inkIn(plain.container)?.style.getPropertyValue("--oto-ink-tint")).toBe("");

    const heading = render(() => <Ink motif="swipe" tint="heading" />);
    expect(inkIn(heading.container)?.style.getPropertyValue("--oto-ink-tint")).toBe(
      "var(--oto-wash-heading)",
    );
  });
});

describe("the six-at-once trap", () => {
  it("leaves the shared EmptyState with no ink on it", () => {
    const { container } = render(() => (
      <EmptyState title="No episodes recorded." body="Nothing has happened to this alert yet." />
    ));
    expect(
      inkIn(container),
      "`EmptyState` has grown a wash. Six of its eighteen call sites are sub-panels on ONE " +
        "alert-detail page — delivery, enrichment, occurrences, timeline, rule drift, snoozes — so " +
        "this renders six washes on one quiet alert, and at six a gesture becomes a texture. The " +
        "full-page component is `PageEmptyState`.",
    ).toBeNull();
  });

  it("puts exactly one motif on the full-page one", () => {
    const { container } = render(() => <PageEmptyState motif="kumo" title="No groups match." />);
    expect(
      container.querySelectorAll(".oto-ink"),
      "one motif per state, never both: the moment a panel carries clouds AND petals the " +
        "distinction they exist to draw is gone (§M.9).",
    ).toHaveLength(1);
    // U1: the sentence still carries the whole fact with the ink removed.
    expect(container.textContent).toContain("No groups match.");
  });

  it("keeps every alert-detail sub-panel off the full-page component", () => {
    // ⛔ READ OFF THE TREE, NOT ASSERTED ABOUT A RENDER. The rule is about which
    // files may reach for `PageEmptyState`, and a render can only ever prove the
    // one panel it was pointed at. A seventh sub-panel added next year is covered
    // by this and would not be by a fixture.
    const dir = path.join(SRC, "features", "alerts", "detail");
    const offenders = readdirSync(dir)
      .filter((f) => /\.tsx?$/.test(f) && !/\.(test|spec)\./.test(f))
      .filter((f) => /\bPageEmptyState\b/.test(readFileSync(path.join(dir, f), "utf8")));

    expect(
      offenders,
      "these are sub-panels on a single alert-detail page, and each one reaching for the " +
        "full-page empty state is one more wash on the same quiet alert. They take the shared " +
        "`EmptyState` (§M.9).",
    ).toEqual([]);
  });
});
