/**
 * The grouped view: collapsible roll-ups with counts and a roll-up state.
 *
 * Each group states honestly what it is counting — see `grouping.ts` — because
 * the alternative is a number that looks authoritative and is not.
 *
 * `<details>`/`<summary>` does the disclosure. It is keyboard-operable,
 * announced correctly and searchable by the browser without a line of ARIA, and
 * every hand-rolled accordion is worse at all three.
 */
import { For, Show, type Component } from "solid-js";

import type { Alert, State } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import { SeverityMark, STATE_BAR, STATE_LABEL, StateChip } from "~/components/StateChip";
import { cx } from "~/components/ui/primitives";
import { count as fmtCount } from "~/lib/format";
import { AlertTable } from "./AlertTable";
import type { AlertGrouping } from "./grouping";
import type { GroupBy } from "./filters";

const NOUN: Record<Exclude<GroupBy, "none">, string> = {
  alertname: "alert name",
  namespace: "namespace",
  fingerprint: "source fingerprint",
};

export interface GroupedAlertsProps {
  readonly groups: readonly AlertGrouping[];
  readonly by: Exclude<GroupBy, "none">;
  /** How many rows the roll-up was computed over — stated, never implied. */
  readonly loadedCount: number;
  readonly hasMore: boolean;
  readonly onFilterLabel: (name: string, value: string) => void;
}

export const GroupedAlerts: Component<GroupedAlertsProps> = (props) => (
  <div class="min-h-0 flex-1 overflow-auto">
    {/* The honesty line. It is small, permanent and unmissable if you look for
        it, which is the right weight for "this number has a scope". */}
    <p class="border-b border-line bg-raised px-3 py-1.5 text-[11px] text-ink-muted">
      {props.groups.length} group{props.groups.length === 1 ? "" : "s"} by {NOUN[props.by]}, rolled
      up over the {fmtCount(props.loadedCount)} alert
      {props.loadedCount === 1 ? "" : "s"} loaded so far.
      {props.hasMore
        ? " More pages match these filters — load them to complete the counts."
        : " That is every alert matching these filters."}
    </p>

    <For each={props.groups}>
      {(group) => <GroupBlock group={group} onFilterLabel={props.onFilterLabel} />}
    </For>
  </div>
);

const GroupBlock: Component<{
  readonly group: AlertGrouping;
  readonly onFilterLabel: (name: string, value: string) => void;
}> = (props) => (
  <details class="border-b border-line" open={props.group.rollupState === "firing"}>
    <summary
      class={cx(
        "flex cursor-pointer list-none items-center gap-3 px-3 py-2",
        "hover:bg-raised [&::-webkit-details-marker]:hidden",
        props.group.rollupState === "firing" ? "bg-firing-fill/30" : "bg-surface",
      )}
    >
      <Chevron />

      {/* The roll-up's own status bar, the same 3 px language as a row. */}
      <span
        aria-hidden="true"
        class={cx("h-5 w-[3px] shrink-0 rounded-full", STATE_BAR[props.group.rollupState])}
      />

      <SeverityMark severity={props.group.topSeverity} />

      <span class="min-w-0 flex-1">
        <span class="block truncate font-medium text-ink" title={props.group.label}>
          {props.group.label}
        </span>
        <Show when={props.group.sublabel}>
          <span class="block truncate font-mono text-[11px] text-ink-subtle">
            {props.group.sublabel}
          </span>
        </Show>
      </span>

      <StateChip state={props.group.rollupState} size="sm" />

      {/* Per-state counts. `resolved` and `expired` stay separate — they are
          different facts and merging them would lose the interesting one. */}
      <span class="flex shrink-0 items-center gap-1.5 text-[11px] tabular-nums text-ink-muted">
        <For each={["firing", "suppressed", "expired", "resolved"] as readonly State[]}>
          {(state) => (
            <Show when={props.group.counts[state] > 0}>
              <span
                class="inline-flex items-center gap-1"
                title={`${props.group.counts[state]} ${STATE_LABEL[state].toLowerCase()}`}
              >
                <span class={cx("size-1.5 rounded-full", STATE_BAR[state])} aria-hidden="true" />
                {props.group.counts[state]}
                <span class="sr-only-focusable">{STATE_LABEL[state]}</span>
              </span>
            </Show>
          )}
        </For>
      </span>

      <Show when={props.group.unackedCount > 0}>
        <span
          class="shrink-0 rounded-[3px] border border-line-strong bg-raised px-1 text-[11px] leading-4 text-ink"
          title="Nobody has recorded seeing these yet."
        >
          {props.group.unackedCount} unseen
        </span>
      </Show>

      <span class="w-14 shrink-0 text-right text-[11px] text-ink-subtle">
        <RelativeTime value={props.group.lastSeenAt} label="Newest activity" />
      </span>
    </summary>

    <div class="flex max-h-[70vh] flex-col border-t border-line">
      <AlertTable alerts={props.group.alerts as readonly Alert[]} onFilterLabel={props.onFilterLabel} />
    </div>
  </details>
);

const Chevron: Component = () => (
  <svg
    viewBox="0 0 12 12"
    class="size-3 shrink-0 text-ink-subtle transition-transform duration-100 [details[open]_&]:rotate-90"
    aria-hidden="true"
  >
    <path
      d="m4.5 2.5 4 3.5-4 3.5"
      fill="none"
      stroke="currentColor"
      stroke-width="1.5"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  </svg>
);
