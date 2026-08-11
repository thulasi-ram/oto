/**
 * The fūrin's stroke, pinned per size.
 *
 * This file exists to fail a specific future refactor. The mark and the glyph
 * share one 40-unit `viewBox` but *cannot* share one `stroke-width`, because a
 * hairline is a device-pixel fact: the same SVG number rendered at 16px and at
 * 32px is two different lines. The two hand-copied SVGs this component replaced
 * had already been tuned for that, and the tuning is the easiest thing in the
 * codebase to mistake for an inconsistency and "clean up".
 *
 * So the assertions are written in device pixels rather than in SVG units — the
 * unit the reader of a diff would be tempted to normalise. Both numbers below
 * are what the pre-consolidation glyphs rendered:
 *
 *   header  viewBox 20 at 16px, stroke 1.5 → 1.20px
 *   states  viewBox 40 at 32px, stroke 1.6 → 1.28px
 */
import { render } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { Chime } from "./Chime";

/** The rendered edge of each size, in CSS pixels. Tailwind: size-4, size-8. */
const RENDERED_PX: Readonly<Record<"mark" | "glyph", number>> = { mark: 16, glyph: 32 };

/** The SVG user-space box the component draws in. */
const VIEWBOX_UNITS = 40;

interface Drawn {
  readonly svg: SVGSVGElement;
  /** Every stroked element's `stroke-width`, in SVG units. */
  readonly strokes: readonly number[];
}

function draw(size: "mark" | "glyph"): Drawn {
  const { container } = render(() => <Chime size={size} />);
  const svg = container.querySelector("svg");
  if (svg === null) throw new Error("the chime rendered no <svg>");

  const strokes = [...svg.querySelectorAll("[stroke-width]")].map((el) =>
    Number(el.getAttribute("stroke-width")),
  );
  return { svg, strokes };
}

/** What a stroke actually measures on screen at this size. */
function devicePx(width: number, size: "mark" | "glyph"): number {
  return width * (RENDERED_PX[size] / VIEWBOX_UNITS);
}

describe("the chime's stroke", () => {
  it("renders the header mark at the 1.20px hairline it had at viewBox 20", () => {
    const { svg, strokes } = draw("mark");

    // The premise of the arithmetic: 40 units drawn into a 16px box.
    expect(svg.getAttribute("viewBox")).toBe("0 0 40 40");
    expect(svg.getAttribute("class")).toContain("size-4");

    // 3.0 looks wrong next to the glyph's 1.6, and it is not. 16/40 = 0.4.
    expect(strokes.length).toBeGreaterThan(0);
    for (const w of strokes) {
      expect(w).toBe(3);
      expect(devicePx(w, "mark")).toBeCloseTo(1.2, 5);
    }
  });

  it("renders the empty-state glyph at the 1.28px hairline it already had", () => {
    const { svg, strokes } = draw("glyph");

    expect(svg.getAttribute("viewBox")).toBe("0 0 40 40");
    expect(svg.getAttribute("class")).toContain("size-8");

    expect(strokes.length).toBeGreaterThan(0);
    for (const w of strokes) {
      expect(w).toBe(1.6);
      expect(devicePx(w, "glyph")).toBeCloseTo(1.28, 5);
    }
  });

  it("keeps the two sizes on different numbers, which is the whole point", () => {
    // Stated as its own case so a diff that unifies them fails with a message
    // about the intent rather than about an unexplained magic number.
    const mark = draw("mark").strokes[0];
    const glyph = draw("glyph").strokes[0];
    expect(mark).not.toBe(glyph);
  });

  it("draws the zetsu only at glyph size, where it is not sub-pixel", () => {
    // At 16px the clapper's hairline collides with the mouth arc and reads as a
    // smudge, so the mark is two strokes and the glyph is three plus the weight.
    const mark = draw("mark");
    expect(mark.svg.querySelectorAll("path")).toHaveLength(2);
    expect(mark.svg.querySelector("circle")).toBeNull();

    const glyph = draw("glyph");
    expect(glyph.svg.querySelectorAll("path")).toHaveLength(3);
    expect(glyph.svg.querySelector("circle")).not.toBeNull();
  });

  it("is decorative at both sizes and names no colour of its own", () => {
    for (const size of ["mark", "glyph"] as const) {
      const { svg } = draw(size);
      expect(svg.getAttribute("aria-hidden")).toBe("true");
      // Colour belongs to the call site; a fill or stroke literal here would be
      // the first hex in `web/src` outside design/.
      for (const el of svg.querySelectorAll("[stroke]")) {
        expect(el.getAttribute("stroke")).toBe("currentColor");
      }
    }
  });

  it("still lets the call site own colour and layout", () => {
    const { container } = render(() => <Chime size="glyph" class="text-line-strong" />);
    const cls = container.querySelector("svg")?.getAttribute("class") ?? "";
    expect(cls).toContain("size-8");
    expect(cls).toContain("text-line-strong");
  });
});
