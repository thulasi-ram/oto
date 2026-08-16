/**
 * The shell every filter row shares: a flex-wrap strip that holds a
 * `ToggleGroup`, a `Select` or two, maybe an `Input`, and a trailing control
 * pushed right with the caller's own `ml-auto`. Only the shell is shared here
 * — which controls live inside, and in what order, stays a decision each
 * screen makes for itself; this component has no opinion on them.
 *
 * `standalone` (default) is for a row that *is* the whole filter bar, carrying
 * its own border and background — `/groups`, and the alert Timeline's
 * event-kind row. Pass `standalone={false}` for a row that is one of several
 * sharing an ancestor which already supplies the border and background:
 * `FilterBar` stacks three such rows under one shell, so each row contributes
 * only its own bottom padding and the top inset comes from the shell (its
 * first row) or the row above (its own bottom padding).
 */
import { type Component, type JSX } from "solid-js";

import { cn } from "~/lib/cn";

export type FilterRowTone = "surface" | "raised";
export type FilterRowGap = "default" | "tight";

export interface FilterRowProps {
  readonly class?: string;
  readonly standalone?: boolean;
  /** Background when `standalone`. Irrelevant otherwise — a non-standalone
   *  row inherits whatever background its shell already painted. */
  readonly tone?: FilterRowTone;
  readonly gap?: FilterRowGap;
  readonly children: JSX.Element;
}

const TONE: Record<FilterRowTone, string> = {
  surface: "bg-surface",
  raised: "bg-raised",
};

const GAP: Record<FilterRowGap, string> = {
  default: "gap-x-3 gap-y-2",
  tight: "gap-2",
};

export const FilterRow: Component<FilterRowProps> = (props) => (
  <div
    class={cn(
      "flex flex-wrap items-center px-3",
      GAP[props.gap ?? "default"],
      props.standalone === false
        ? "pb-2"
        : cn("border-b border-line py-2", TONE[props.tone ?? "surface"]),
      props.class,
    )}
  >
    {props.children}
  </div>
);
