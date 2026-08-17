/**
 * Priority = RECTANGLE bars, encoded by count; TRIANGLE for urgent (SPEC.md §2).
 *
 * Priority sits directly beside status in the row, so it must be readable as a
 * *different primitive family* at 14px — bars, not circles. Ordered data gets an
 * ordered visual variable: the number of filled bars.
 *
 * Every state draws all three bars. Unfilled ones render at 30% opacity rather
 * than being omitted, so the glyph's ink bounding box is identical in every row
 * and the column cannot appear to jitter as you scan down it.
 */
import type { Component, JSX } from "solid-js";
import { For, Show } from "solid-js";

import { cn } from "~/lib/cn";
import type { Priority } from "~/features/linear-proto/types";

/**
 * Three bars on a shared baseline at y=13, ascending 5 / 8.5 / 12 tall.
 * Width 3, gap 1.5 — the run spans x=1..13, matching the status ring's own
 * 1..13 extent so the two columns optically agree.
 */
const BARS: readonly { x: number; y: number; h: number }[] = [
  { x: 1, y: 8, h: 5 },
  { x: 5.5, y: 4.5, h: 8.5 },
  { x: 10, y: 1, h: 12 },
];

const BAR_W = 3;

/**
 * Urgent is the one glyph that leaves the bar family: an apex-up triangle
 * spanning the same 1..13 box, with an exclamation (bar + dot) cut straight out
 * of it via `fill-rule="evenodd"`. The cut is genuine negative space, not an
 * overpainted canvas-coloured shape, so it survives on any row background.
 */
const URGENT_PATH =
  "M7 1 L13 13 L1 13 Z " + // triangle, apex up
  "M6.25 5 h1.5 v4.5 h-1.5 Z " + // exclamation bar
  "M6.25 10.4 h1.5 v1.5 h-1.5 Z"; // exclamation dot

function filledCount(priority: Priority): number {
  switch (priority) {
    case "urgent":
      return 3;
    case "high":
      return 3;
    case "medium":
      return 2;
    case "low":
      return 1;
    case "none":
      return 0;
  }
}

function colorVar(priority: Priority): string {
  switch (priority) {
    case "urgent":
      return "var(--lp-red)";
    case "high":
      return "var(--lp-amber)";
    case "medium":
    case "low":
      return "var(--lp-neutral)";
    case "none":
      return "var(--lp-text-3)";
  }
}

export const PriorityIcon: Component<{ priority: Priority; class?: string }> = (props) => {
  const color = () => colorVar(props.priority);

  /** "None" is uniformly faint at 50%; elsewhere unfilled bars sit at 30%. */
  const barOpacity = (index: number): number => {
    if (props.priority === "none") return 0.5;
    return index < filledCount(props.priority) ? 1 : 0.3;
  };

  return (
    <svg
      viewBox="0 0 14 14"
      class={cn("w-3.5 h-3.5 shrink-0", props.class)}
      fill="none"
      stroke-linecap="butt"
      stroke-linejoin="miter"
      aria-hidden="true"
    >
      <Show
        when={props.priority === "urgent"}
        fallback={
          <For each={BARS}>
            {(bar, index): JSX.Element => (
              <rect
                x={bar.x}
                y={bar.y}
                width={BAR_W}
                height={bar.h}
                fill={color()}
                opacity={barOpacity(index())}
              />
            )}
          </For>
        }
      >
        <path d={URGENT_PATH} fill={color()} fill-rule="evenodd" />
      </Show>
    </svg>
  );
};
