/**
 * Sticky section header for one issue group.
 *
 * Pins to the top of its own section (`sticky top-0`) rather than the top of the
 * scroller, so scrolling through a long group keeps that group's label on screen
 * and the next group's header pushes it out — Linear's behaviour.
 *
 * The leading glyph is chosen from the ACTIVE grouping, not from the row data:
 * grouping by status shows the status circle, by priority the priority bars, by
 * assignee the avatar. Grouping by project or none has no glyph — inventing one
 * would break the "hue is meaning" rule in SPEC.md §1.
 */
import { Match, Show, Switch, type JSX } from "solid-js";

import { cn } from "~/lib/cn";
import { GROUP_HEADER_H, GUTTER, ROW_GAP } from "./layout";
import { useIssueView, type IssueGroup } from "./store";
import { PRIORITY_ORDER, STATUS_ORDER } from "./types";
import { Avatar } from "./primitives/Avatar";
import { PriorityIcon } from "./primitives/PriorityIcon";
import { StatusIcon } from "./primitives/StatusIcon";

export function GroupHeader(props: {
  group: IssueGroup;
  collapsed: boolean;
  onToggle: () => void;
}): JSX.Element {
  const view = useIssueView();

  /**
   * `IssueGroup.key` is a plain string, so narrow it by lookup against the
   * canonical order rather than casting — an unknown key simply renders no
   * glyph instead of asserting its way into a wrong one.
   */
  const status = () => STATUS_ORDER.find((entry) => entry === props.group.key);
  const priority = () => PRIORITY_ORDER.find((entry) => entry === props.group.key);
  /** The group is the assignee, so every row in it carries the same one. */
  const assignee = () => props.group.issues[0]?.assignee ?? null;

  return (
    <button
      type="button"
      onClick={props.onToggle}
      aria-expanded={!props.collapsed}
      class={cn(
        GROUP_HEADER_H,
        GUTTER,
        ROW_GAP,
        "group sticky top-0 z-10 flex w-full items-center text-left",
        "bg-[var(--lp-surface)] outline-none",
        "border-b border-[var(--lp-border)]",
        // Matches the 2px focus rail every row reserves, so the glyph column
        // starts on the same x as the row glyph column.
        "border-l-2 border-l-transparent",
        "transition-colors duration-150 motion-reduce:transition-none",
        // Hover is the SPEC §5 row tint (0.04); keyboard focus is the one step
        // stronger 0.06 every other focusable control in the prototype uses.
        "hover:bg-white/[0.04] focus-visible:bg-white/[0.06]",
      )}
    >
      <svg
        viewBox="0 0 14 14"
        aria-hidden="true"
        class={cn(
          "h-3.5 w-3.5 shrink-0 text-[var(--lp-text-3)]",
          "transition-transform duration-150 motion-reduce:transition-none",
          // Press feedback belongs to the glyph, not the strip: scaling a
          // full-width sticky header would read as the whole list flinching.
          // `group-active` because :active lands on the button, not on this svg.
          "group-active:scale-[0.97]",
          // Collapsed points right (the universal "there is more inside"),
          // expanded points down at the rows it owns.
          props.collapsed && "-rotate-90",
        )}
        fill="none"
      >
        <path
          d="M3.5 5.5 L7 9 L10.5 5.5"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="square"
        />
      </svg>

      <Switch>
        <Match when={view.groupBy() === "status"}>
          <span class="flex w-3.5 shrink-0 items-center justify-center">
            <Show when={status()}>{(value) => <StatusIcon status={value()} />}</Show>
          </span>
        </Match>
        <Match when={view.groupBy() === "priority"}>
          <span class="flex w-3.5 shrink-0 items-center justify-center">
            <Show when={priority()}>{(value) => <PriorityIcon priority={value()} />}</Show>
          </span>
        </Match>
        <Match when={view.groupBy() === "assignee"}>
          <span class="flex w-3.5 shrink-0 items-center justify-center">
            <Avatar assignee={assignee()} class="h-3.5 w-3.5" />
          </span>
        </Match>
      </Switch>

      <span class="text-item font-medium min-w-0 truncate text-[var(--lp-text)]">
        {props.group.label}
      </span>

      {/* tabular-nums is explicit here: the count changes as filters change and
          as groups differ, and a jittering digit column is visible noise. */}
      <span class="text-micro font-medium uppercase tracking-[0.08em] shrink-0 tabular-nums text-[var(--lp-text-3)]">
        {props.group.issues.length}
      </span>
    </button>
  );
}
