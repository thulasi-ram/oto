/**
 * The grouped view: **server-side** roll-up buckets.
 *
 * Every count here is computed over the whole filtered result set by
 * `GET /api/v1/alerts/rollups`, not over the rows that happened to load. The UI
 * used to roll up client-side over loaded pages, which under-reported the moment
 * the result exceeded one page — a number that looks authoritative and is not.
 *
 * Two properties of the endpoint shape this component:
 *
 *   - **Buckets are keyset-ordered by their own key**, so they are rendered in
 *     the order the server returned them. Re-sorting a page by liveliness would
 *     put page two's firing bucket below page one's resolved one, which is worse
 *     than alphabetical.
 *   - **A bucket has no members attached.** It is a view over one query, not a
 *     container, so opening one means drilling into the alert list with this
 *     bucket added to the filter set — every other filter carried through.
 */
import { For, Show, type Component } from "solid-js";

import { StateSchema } from "~/api/generated/validators";
import type { AlertRollup, RollupAxis, State } from "~/api/types";
import { RelativeTime } from "~/components/Time";
import {
  SeverityMark,
  STATE_BAR,
  STATE_LABEL,
  StateChip,
  normaliseSeverity,
  type KnownSeverity,
} from "~/components/StateChip";
import { Button } from "~/components/ui/Button";
import { cn } from "~/lib/cn";
import { count as fmtCount } from "~/lib/format";

const NOUN: Record<RollupAxis, string> = {
  alertname: "alert name",
  namespace: "namespace",
  fingerprint: "source fingerprint",
};

const SEVERITY_RANK: Record<KnownSeverity, number> = { critical: 0, warning: 1, info: 2 };

/**
 * The order a bucket's counts read in: still-live first, then ended.
 *
 * The *set* is the contract's, so a lifecycle state added server-side is counted
 * rather than silently dropped from every bucket. Only the order is local — and
 * `Record<State, …>` makes it exhaustive, so a new state is a compile error here
 * asking where it belongs instead of an unranked value sorting arbitrarily.
 */
const COUNT_ORDER: Record<State, number> = { firing: 0, suppressed: 1, expired: 2, resolved: 3 };
const COUNTED_STATES: readonly State[] = [...StateSchema.options].sort(
  (a, b) => COUNT_ORDER[a] - COUNT_ORDER[b],
);

/**
 * The most urgent severity present in a bucket.
 *
 * `severity_counts` is raw and deliberately unranked — operators choose their
 * own vocabulary (`sev1`, `P1`, `page`) — so precedence is applied here, where
 * the local convention is known, and an unrecognised value simply does not rank.
 */
function topSeverity(bucket: AlertRollup): string | null {
  let best: { raw: string; rank: number } | null = null;
  for (const raw of Object.keys(bucket.severity_counts)) {
    const known = normaliseSeverity(raw);
    if (known === null) continue;
    const rank = SEVERITY_RANK[known];
    if (best === null || rank < best.rank) best = { raw, rank };
  }
  return best?.raw ?? null;
}

export interface GroupedAlertsProps {
  readonly buckets: readonly AlertRollup[];
  readonly by: RollupAxis;
  readonly hasMore: boolean;
  readonly loading: boolean;
  readonly onLoadMore: () => void;
  /**
   * Drill into one bucket. `null` when the axis has no matching list filter, in
   * which case the bucket is rendered without a link rather than with one that
   * would silently mean something else.
   */
  readonly onDrillDown: ((key: string) => void) | null;
}

export const GroupedAlerts: Component<GroupedAlertsProps> = (props) => (
  <div class="min-h-0 flex-1 overflow-auto">
    <p class="border-b border-line bg-raised px-3 py-1.5 text-meta text-ink-muted">
      {fmtCount(props.buckets.length)}
      {props.hasMore ? "+" : ""} bucket{props.buckets.length === 1 ? "" : "s"} by{" "}
      {NOUN[props.by]}. Counted server-side over every alert matching these filters, not over the
      rows on screen. Ordered by bucket key, which is what makes paging over them exact.
    </p>

    <Show when={props.onDrillDown === null}>
      <p class="border-b border-line bg-surface px-3 py-1.5 text-meta leading-snug text-ink-muted">
        These buckets cannot be opened: the alert list has no source-fingerprint filter, so oto
        cannot narrow to one fingerprint without pretending a different filter means the same
        thing. The counts below are still exact.
      </p>
    </Show>

    <ul>
      <For each={props.buckets}>
        {(bucket) => (
          <BucketRow bucket={bucket} by={props.by} onDrillDown={props.onDrillDown} />
        )}
      </For>
    </ul>

    <Show when={props.hasMore}>
      <div class="border-t border-line px-3 py-2 text-center">
        <Button size="sm" busy={props.loading} onClick={props.onLoadMore}>
          Load more buckets
        </Button>
      </div>
    </Show>
  </div>
);

/**
 * The bucket row's grid, so a chip that only sometimes renders — unacked,
 * flapping, snoozed — can never shift "Last seen" sideways depending on which
 * of its neighbours happened to show up on *this* bucket. Every track but the
 * name column is a fixed width, sized generously for its worst realistic
 * content, the same discipline `AlertTable`'s `COLUMNS` applies with real
 * `<col>` widths — this is the div-soup equivalent, one grid per row rather
 * than shared table columns, so the widths below are the single source both
 * row renderings (linked and unlinked) must share to stay aligned with
 * each other.
 */
const BUCKET_ROW_GRID =
  "grid grid-cols-[3px_0.75rem_minmax(0,1fr)_7rem_6rem_8rem_17rem_3.5rem] items-center gap-3";

const BucketRow: Component<{
  readonly bucket: AlertRollup;
  readonly by: RollupAxis;
  readonly onDrillDown: ((key: string) => void) | null;
}> = (props) => {
  const b = (): AlertRollup => props.bucket;

  const label = (): string => {
    if (b().key !== "") return b().key;
    return props.by === "namespace" ? "No namespace" : "(empty)";
  };

  const counts = (): readonly { readonly state: State; readonly n: number }[] =>
    COUNTED_STATES.map((state) => ({
      state,
      n:
        state === "firing"
          ? b().firing_count
          : state === "suppressed"
            ? b().suppressed_count
            : state === "expired"
              ? b().expired_count
              : b().resolved_count,
    })).filter((c) => c.n > 0);

  // A component rather than a stored element: `<Show>` may swap between the two
  // branches, and each branch must get its own nodes rather than sharing one set.
  const Body: Component = () => (
    <>
      {/* The roll-up's own status bar, the same 3 px language as a row. A bucket
          is as alive as its liveliest member, and `expired` outranks `resolved`
          because "we stopped hearing about this" is the open question. */}
      <span
        aria-hidden="true"
        class={cn("h-5 w-[3px] rounded-full", STATE_BAR[b().state])}
      />

      <SeverityMark severity={topSeverity(b())} />

      <span class="min-w-0">
        <span class="block truncate font-medium text-ink" title={b().key}>
          {label()}
        </span>
        <Show when={b().key === "" && props.by === "namespace"}>
          <span class="block truncate text-meta text-ink-subtle">
            These alerts carry no promoted namespace label.
          </span>
        </Show>
      </span>

      <StateChip state={b().state} size="sm" class="justify-self-start" />

      <span class="justify-self-start text-meta tabular-nums text-ink-muted">
        {fmtCount(b().total_count)} total
      </span>

      {/* Per-state counts. `resolved` and `expired` stay separate — they are
          different facts and merging them would lose the interesting one. */}
      <span class="flex items-center justify-self-start gap-1.5 text-meta tabular-nums text-ink-muted">
        <For each={counts()}>
          {(c) => (
            <span
              class="inline-flex items-center gap-1"
              title={`${c.n} ${STATE_LABEL[c.state].toLowerCase()}`}
            >
              <span class={cn("size-1.5 rounded-full", STATE_BAR[c.state])} aria-hidden="true" />
              {c.n}
              <span class="sr-only-focusable">{STATE_LABEL[c.state]}</span>
            </span>
          )}
        </For>
      </span>

      {/* The three chips below are each conditional on their own count, but
          together they still occupy exactly one grid cell: the reserved
          column is the same width whether none, one, or all three render, so
          it alone absorbs that variability and nothing after it has to. */}
      <div class="flex items-center justify-self-start gap-3">
        <Show when={b().unacked_count > 0}>
          <span
            class="shrink-0 rounded-chip border border-line-strong bg-raised px-1 text-meta leading-4 text-ink"
            title="Nobody has recorded seeing these yet."
          >
            {b().unacked_count} unseen
          </span>
        </Show>

        <Show when={b().flapping_count > 0}>
          <span
            class="shrink-0 rounded-chip border border-line bg-surface px-1 text-meta leading-4 text-ink-muted"
            title="Members oto has damped as flapping. A visible state, never a silent drop."
          >
            {b().flapping_count} flapping
          </span>
        </Show>

        {/* Snoozed members are counted, never dimmed: they are still firing and
            still whatever severity they were. */}
        <Show when={b().snoozed_count > 0}>
          <span
            class="shrink-0 rounded-chip border border-line bg-surface px-1 text-meta leading-4 text-ink-muted"
            title="Members whose notifications oto is holding. They are still firing and still counted as such."
          >
            {b().snoozed_count} snoozed
          </span>
        </Show>
      </div>

      <span class="text-right text-meta text-ink-subtle">
        <RelativeTime value={b().last_seen_at} label="Newest activity" />
      </span>
    </>
  );

  return (
    <li class={cn("border-b border-line", b().state === "firing" ? "bg-firing-fill/30" : "bg-surface")}>
      <Show
        when={props.onDrillDown}
        fallback={
          <div class={cn(BUCKET_ROW_GRID, "px-3 py-2")}>
            <Body />
          </div>
        }
      >
        {(drill) => (
          <button
            type="button"
            class={cn(BUCKET_ROW_GRID, "w-full px-3 py-2 text-left hover:bg-raised")}
            title={`Open the ${b().total_count} alert${b().total_count === 1 ? "" : "s"} in this bucket, keeping every other filter`}
            onClick={() => drill()(b().key)}
          >
            <Body />
          </button>
        )}
      </Show>
    </li>
  );
};
