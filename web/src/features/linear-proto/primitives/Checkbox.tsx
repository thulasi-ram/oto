/**
 * 14px square checkbox on `@kobalte/core/checkbox`.
 *
 * Square, radius 0, accent fill when checked. The check mark is drawn in the
 * canvas colour with butt caps and miter joins so it reads as cut out of the
 * accent square rather than laid on top of it — the same construction as the
 * Done status glyph.
 *
 * Kobalte's `Input` is visually hidden (1x1 clipped), so focus is made visible
 * on the `Control` via `peer-focus-visible`; an outline on the real focus target
 * would be clipped to nothing.
 */
import type { Component } from "solid-js";
import { Show } from "solid-js";

import * as CheckboxPrimitive from "@kobalte/core/checkbox";

import { cn } from "~/lib/cn";

export const Checkbox: Component<{
  checked: boolean;
  onChange: (v: boolean) => void;
  class?: string;
  label?: string;
  /**
   * Keeps the accessible name but takes the label out of flow. A list row's
   * `COL.select` is a 20px box; a visible label plus the `gap-2` beside it makes
   * min-content 22px, which shoves the 14px square ~3px off centre and out of
   * column with the priority and status glyphs one cell over. Default is a
   * visible label — the side panel's filter rows need it.
   */
  labelHidden?: boolean;
  /**
   * Roving-tabindex passthrough (SPEC.md §6). The real `<input>` is a tab stop
   * of its own, so a list that ropes 60 rows into one stop has to be able to
   * pull every row's checkbox out of the sequence.
   */
  tabIndex?: number;
}> = (props) => (
  <CheckboxPrimitive.Root
    checked={props.checked}
    onChange={props.onChange}
    class={cn(
      "group inline-flex items-center",
      // A hidden label is out of flow and contributes no gap, but stating it
      // keeps the intrinsic width honest at a glance.
      props.labelHidden ? undefined : "gap-2",
      props.class,
    )}
  >
    <CheckboxPrimitive.Input class="peer" aria-label={props.label} tabindex={props.tabIndex} />
    <CheckboxPrimitive.Control
      class={cn(
        "relative size-3.5 shrink-0 rounded-none border border-white/[0.2]",
        "flex items-center justify-center",
        /*
         * The visible box is 14px, well under the 40px a pointer expects, so the
         * `Control` — which carries Kobalte's toggle `onClick` — grows an
         * invisible `::before` target around itself.
         *
         * The insets are bounded, not generous. Vertically 8px each side takes
         * the target to 30px, which still fits the SHORTER of the two row
         * heights — `h-8` (32px) at compact density, not just the comfortable
         * `h-9` (36px) — so two stacked rows' checkboxes can never both claim
         * the same pixel at either density. Horizontally 4px is exactly half of
         * the `gap-2` that separates this control from whatever sits beside it
         * — the priority glyph in a list row, the label in a filter popover —
         * so the two targets meet at the gap's midpoint and stop. An
         * overlapping cushion would steal clicks from its neighbour, which is
         * worse than the small target it was meant to fix.
         */
        "before:absolute before:content-[''] before:-inset-x-1 before:-inset-y-2",
        // Named properties, never `transition-all`.
        "transition-[background-color,border-color,transform] duration-150",
        "ease-[var(--lp-ease)] motion-reduce:transition-none",
        "active:scale-[0.97]",
        "peer-focus-visible:border-[var(--lp-accent)]",
        "data-[checked]:border-[var(--lp-accent)] data-[checked]:bg-[var(--lp-accent)]",
        "data-[indeterminate]:border-[var(--lp-accent)] data-[indeterminate]:bg-[var(--lp-accent)]",
      )}
    >
      <CheckboxPrimitive.Indicator class="flex items-center justify-center">
        <svg
          viewBox="0 0 14 14"
          class="size-3.5"
          fill="none"
          stroke="var(--lp-canvas)"
          stroke-width="1.6"
          stroke-linecap="butt"
          stroke-linejoin="miter"
          aria-hidden="true"
        >
          <path d="M3.4 7.2 L5.9 9.7 L10.6 4.5" />
        </svg>
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Control>
    <Show when={props.label}>
      {(label) => (
        <CheckboxPrimitive.Label
          class={cn(
            "text-item min-w-0 truncate text-[var(--lp-text)] select-none",
            props.labelHidden && "sr-only",
          )}
        >
          {label()}
        </CheckboxPrimitive.Label>
      )}
    </Show>
  </CheckboxPrimitive.Root>
);
