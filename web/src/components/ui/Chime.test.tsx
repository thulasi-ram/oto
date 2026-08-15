/**
 * The fūrin's stroke, pinned per size — and the one signal its swing answers to.
 *
 * # The stroke
 *
 * This half of the file exists to fail a specific future refactor. The mark and the glyph
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
 *
 * # The swing
 *
 * The second half exists because the obvious trigger is wrong in a way that no
 * amount of staring at `Chime.tsx` reveals: `stream.ts` derives `connecting` vs
 * `reconnecting` from whether `sessionStorage` holds a resume point, so a bell
 * hung on `connecting → live` is hung on "this tab has never seen a sequenced
 * frame". The two cases below are the two halves of that mistake, driven through
 * the real `AlertStream` over a stubbed transport rather than described:
 *
 *   1. A **reload with a resume point** never enters `connecting` at all, so the
 *      old trigger was silent for the returning operator. The mark must ring
 *      anyway.
 *   2. A **quiet install** never gets a resume point, so every reconnect — tab
 *      focus, `online`, the 50 s stall check, a deploy — re-enters `connecting`.
 *      The mark must stay silent through all of them.
 */
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { render } from "@solidjs/testing-library";
import { createEffect, type Component } from "solid-js";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { LiveProvider, useLive } from "~/api/live";
import type { ConnectionState } from "~/api/stream";
import { until } from "~/test/harness";

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

/* -------------------------------------------------------------------------- */
/* A page load, and a socket                                                  */
/* -------------------------------------------------------------------------- */

/**
 * The class the mark carries when it rings.
 *
 * Written out in full, `motion-safe:` included, because the variant is half the
 * reduced-motion guard and dropping it would leave a passing test. The other
 * half — the `*` sweep that catches it even without the variant — is asserted in
 * `src/index.css.test.ts`, which is the only place that can see emitted CSS.
 */
const SWING = "motion-safe:oto-chime-swing";

/**
 * A fresh page load.
 *
 * The latch is a `data-` attribute on `<html>` because "once per document" is
 * what the swing means, and `setup.ts` replaces the body between tests but not
 * the document. Clearing it here is the only thing in this file that stands in
 * for something the browser does.
 */
beforeEach(() => {
  delete document.documentElement.dataset.otoChimeRung;
});

/** A stream that opens and then says nothing — enough to reach `live`. */
function openSocket(): void {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        // Opened and held open: `stream.ts` publishes `live` the moment the
        // response is ok, and never seeing a frame is the quiet install itself.
        body: new ReadableStream<Uint8Array>({
          start() {
            return undefined;
          },
        }),
      }),
    ),
  );
}

interface Wired {
  readonly svg: SVGSVGElement;
  /** Every connection state the provider has published, in order. */
  readonly states: readonly ConnectionState[];
  readonly reconnect: () => void;
}

/**
 * The mark, mounted under the real `LiveProvider` and the real `AlertStream`.
 *
 * The stream is not mocked: the point of these cases is that a component with no
 * connection input cannot be moved by one, and a stubbed provider would be a
 * test of the stub.
 */
function mountUnderLiveStream(): Wired {
  const states: ConnectionState[] = [];
  let live: ReturnType<typeof useLive> | undefined;

  const Probe: Component = () => {
    const ctx = useLive();
    live = ctx;
    createEffect(() => states.push(ctx.state()));
    return null;
  };

  const { container } = render(() => (
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <LiveProvider>
        <Chime size="mark" class="text-accent" />
        <Probe />
      </LiveProvider>
    </QueryClientProvider>
  ));

  const svg = container.querySelector("svg");
  if (svg === null) throw new Error("the chime rendered no <svg>");
  return { svg, states, reconnect: () => live?.reconnect() };
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

describe("the chime's swing", () => {
  it("rings when the mark first mounts, which is the only page load a component can see", () => {
    const cls = draw("mark").svg.getAttribute("class") ?? "";
    expect(cls).toContain(SWING);
  });

  it("rings at most once per document, so a soft navigation is silent", () => {
    // The shell can unmount and remount — signing in, an error boundary, a route
    // that replaces the layout. None of those is a page load, and a bell that
    // rang on each of them would be a tic rather than a greeting.
    expect(draw("mark").svg.getAttribute("class") ?? "").toContain(SWING);
    expect(draw("mark").svg.getAttribute("class") ?? "").not.toContain("oto-chime-swing");
    expect(draw("mark").svg.getAttribute("class") ?? "").not.toContain("oto-chime-swing");

    // A new document — a reload, a second tab — rings again. This is exactly the
    // case `connecting` could not express, because the resume point outlives it.
    delete document.documentElement.dataset.otoChimeRung;
    expect(draw("mark").svg.getAttribute("class") ?? "").toContain(SWING);
  });

  it("never rings the glyph, which hangs in an empty state and in the door", () => {
    expect(draw("glyph").svg.getAttribute("class") ?? "").not.toContain("oto-chime-swing");
    // And the glyph must not consume the document's one ring on its way past.
    expect(draw("mark").svg.getAttribute("class") ?? "").toContain(SWING);
  });

  it("rings on a reload whose resume point means the connection never says `connecting`", async () => {
    // 412 is what a tab that has received a sequenced frame leaves behind
    // (`stream.ts` writes `oto.stream.seq`). With it present the very first
    // connect is `reconnecting`, so a bell hung on `connecting → live` would be
    // silent for the returning operator — the person the mark is for.
    sessionStorage.setItem("oto.stream.seq", "412");
    openSocket();

    const { svg, states } = mountUnderLiveStream();
    const atMount = svg.getAttribute("class") ?? "";
    expect(atMount).toContain(SWING);

    await until(() => expect(states).toContain("live"));
    expect(states).toContain("reconnecting");
    expect(states, "the premise of this case: the old trigger never fires here").not.toContain(
      "connecting",
    );

    // Reaching `live` neither rang it nor took the ring away: the class is the
    // one the mount wrote, byte for byte.
    expect(svg.getAttribute("class")).toBe(atMount);
  });

  it("stays silent through the reconnects a quiet install makes all day", async () => {
    // No resume point: nothing sequenced has ever arrived, and heartbeats carry
    // no sequence, so `#lastSeq` stays null and EVERY reconnect re-enters
    // `connecting` — tab focus, `online`, the 50 s stall check, each deploy.
    // That is the install with the least going on, ringing the most.
    openSocket();

    const { svg, states, reconnect } = mountUnderLiveStream();
    await until(() => expect(states).toContain("live"));
    const rung = svg.getAttribute("class") ?? "";
    expect(rung, "it rang at mount, before any connection state existed").toContain(SWING);

    reconnect();
    await until(() =>
      expect(states.filter((s) => s === "connecting").length).toBeGreaterThanOrEqual(2),
    );
    await until(() => expect(states.at(-1)).toBe("live"));

    // A second `connecting → live` in the same document changes nothing on the
    // mark, and a mark mounted after it does not ring either.
    expect(svg.getAttribute("class")).toBe(rung);
    expect(draw("mark").svg.getAttribute("class") ?? "").not.toContain("oto-chime-swing");
  });

  it("carries the `motion-safe:` guard, not the bare utility", () => {
    // Two guards, and this is the one visible from the DOM: under `reduce` the
    // variant's rule is never generated, so there is nothing to suppress. The
    // `*` sweep in `@layer base` is the other, and it is asserted where the
    // built CSS can be read — `src/index.css.test.ts`.
    const cls = draw("mark").svg.getAttribute("class") ?? "";
    expect(cls).toContain("oto-chime-swing");
    expect(cls.split(/\s+/)).toContain(SWING);
    expect(cls.split(/\s+/), "an unguarded copy of the utility").not.toContain("oto-chime-swing");
  });
});
