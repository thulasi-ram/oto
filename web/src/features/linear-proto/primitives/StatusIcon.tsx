/**
 * Status = CIRCLE, encoded by monotonic fill (SPEC.md §2).
 *
 * Status is ordered, so the visual variable is ordered too: the fraction of the
 * disc that is inked rises strictly across the workflow —
 *   backlog (dotted ring) → todo (solid ring) → in progress (half disc)
 *   → done (full disc) → canceled (ring + slash, the terminal exit).
 *
 * Every state occupies the same 1..13 ink box (ring r=5.25 + 1.5 stroke = outer
 * r 6; the Done disc is drawn at r=6 to match), so the column never jitters.
 * No rounded caps or joins, no gradients — Bauhaus geometry only.
 */
import type { Component } from "solid-js";
import { Match, Switch } from "solid-js";

import { cn } from "~/lib/cn";
import type { Status } from "~/features/linear-proto/types";

const CX = 7;
const CY = 7;
/** Stroke centreline. With a 1.5 stroke the ink runs 4.5..6 from centre. */
const R = 5.25;
const STROKE = 1.5;

/**
 * Circumference is 2π·5.25 ≈ 32.99, so a 1.65/1.65 dash lays down exactly ten
 * evenly spaced ticks with no visible seam where the path closes.
 */
const BACKLOG_DASH = "1.65 1.65";

/** Right half of an r=3 inner disc: the "half filled" reading of in progress. */
const HALF_DISC = "M7 4 A3 3 0 0 1 7 10 Z";

/** Inset check, drawn in the canvas colour so it reads as cut out of the disc. */
const CHECK = "M4.4 7.15 L6.2 8.95 L9.7 4.8";

/** 45° slash, chord-length limited to the ring's inner edge. */
const SLASH = "M3.29 10.71 L10.71 3.29";

function colorVar(status: Status): string {
  switch (status) {
    case "backlog":
      return "var(--lp-neutral)";
    case "todo":
      return "var(--lp-neutral-2)";
    case "in_progress":
      return "var(--lp-amber)";
    case "done":
      return "var(--lp-accent)";
    case "canceled":
      return "var(--lp-neutral)";
  }
}

export const StatusIcon: Component<{ status: Status; class?: string }> = (props) => {
  const color = () => colorVar(props.status);

  return (
    <svg
      viewBox="0 0 14 14"
      class={cn("w-3.5 h-3.5 shrink-0", props.class)}
      fill="none"
      stroke-linecap="butt"
      stroke-linejoin="miter"
      aria-hidden="true"
    >
      <Switch>
        <Match when={props.status === "backlog"}>
          <circle
            cx={CX}
            cy={CY}
            r={R}
            stroke={color()}
            stroke-width={STROKE}
            stroke-dasharray={BACKLOG_DASH}
          />
        </Match>

        <Match when={props.status === "todo"}>
          <circle cx={CX} cy={CY} r={R} stroke={color()} stroke-width={STROKE} />
        </Match>

        <Match when={props.status === "in_progress"}>
          <circle cx={CX} cy={CY} r={R} stroke={color()} stroke-width={STROKE} />
          <path d={HALF_DISC} fill={color()} />
        </Match>

        <Match when={props.status === "done"}>
          <circle cx={CX} cy={CY} r={R + STROKE / 2} fill={color()} />
          <path d={CHECK} stroke="var(--lp-canvas)" stroke-width={STROKE} />
        </Match>

        <Match when={props.status === "canceled"}>
          <circle cx={CX} cy={CY} r={R} stroke={color()} stroke-width={STROKE} />
          <path d={SLASH} stroke={color()} stroke-width={STROKE} />
        </Match>
      </Switch>
    </svg>
  );
};
