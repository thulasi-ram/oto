/**
 * The empty state — SPEC.md §8.
 *
 * This is the ONE place the prototype is allowed full-saturation Bauhaus. The
 * three-hue rule (§1) exists because saturated primaries vibrate against the
 * `#0f0f11` canvas and destroy scannability — but here density is zero, nothing
 * is being scanned, and the composition is the only thing on screen. So the
 * literal primaries §0 names (`#f00`, `#00f`, `#ff0`) appear here and nowhere
 * else in the app.
 *
 * The arrangement is deliberately asymmetric, in the Bauhaus manner: mass is
 * carried by the blue square at the bottom-left, answered by the red circle
 * pushed off the right edge, with the small yellow triangle at the top-left
 * closing the diagonal. Blue is the weakest of the three on a near-black canvas,
 * so it is given the largest area; yellow is the brightest, so it is given the
 * smallest. Balance comes from area, not from symmetry.
 *
 * The circle is stroked in the canvas colour rather than outlined — an invisible
 * stroke on the background that cuts a crisp hairline gap where it crosses the
 * square, so two flat fills read as two shapes without either one gaining an
 * outline. Flat fills only: no gradient, no shadow, no radius.
 */
import type { JSX } from "solid-js";
import { Show } from "solid-js";

import { cn } from "~/lib/cn";

import { Keycap } from "./primitives/Keycap";

export function EmptyState(props: { onClearFilters?: () => void }): JSX.Element {
  return (
    <div class="flex h-full flex-col items-center justify-center gap-6 px-6 py-20 text-center">
      <svg
        width="120"
        height="120"
        viewBox="0 0 120 120"
        fill="none"
        shape-rendering="geometricPrecision"
        aria-hidden="true"
      >
        {/* Mass: the blue square, anchored to the bottom-left corner. */}
        <rect x="0" y="54" width="66" height="66" fill="#0000ff" />
        {/* Counterweight: the yellow triangle, closing the diagonal top-left. */}
        <polygon points="2,2 46,2 2,46" fill="#ffff00" />
        {/* Foreground: the red circle, cropped by the right edge of the frame. */}
        <circle
          cx="86"
          cy="50"
          r="34"
          fill="#ff0000"
          stroke="var(--lp-canvas)"
          stroke-width="3"
        />
      </svg>

      {/* `text-balance` so the caption breaks into even lines instead of leaving
          one orphaned word under a centred composition. */}
      <p class="text-item max-w-[42ch] text-balance text-[var(--lp-text-2)]">
        Nothing here. No issue matches this view.
      </p>

      <div class="flex flex-col items-center gap-3">
        <p class="text-meta flex items-center gap-1.5 text-[var(--lp-text-3)]">
          <span>Press</span>
          <Keycap>⌘K</Keycap>
          <span>for commands</span>
        </p>

        <Show when={props.onClearFilters}>
          <button
            type="button"
            onClick={() => props.onClearFilters?.()}
            class={cn(
              "text-micro font-medium uppercase tracking-[0.08em] h-7 px-3 border border-[var(--lp-border)] outline-none",
              "text-[var(--lp-text-2)] active:scale-[0.97]",
              "transition-[color,background-color,border-color,transform] duration-150",
              "ease-[var(--lp-ease)] motion-reduce:transition-none",
              "hover:bg-white/[0.04] hover:text-[var(--lp-text)]",
              "focus-visible:border-[var(--lp-accent)]",
            )}
          >
            Clear all filters
          </button>
        </Show>
      </div>
    </div>
  );
}
