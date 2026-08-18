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
 *
 * It is deliberately the *same* row language as `AlertTable`: the same 5 px
 * gutter (transparent focus rail, then the state bar), the same severity and
 * state tracks, the same one-elastic-column discipline and the same trailing
 * "last seen" edge, all resolved from that file's `TRACK`. Switching between
 * list and grouped mode should feel like re-reading the same table, not like
 * changing screens.
 *
 * ⭐ AND THE SAME PADDING MODEL, WHICH IS WHAT MADE THAT CLAIM TRUE.
 *
 * The paragraph above used to be an aspiration the layout quietly contradicted:
 * this grid put a `gap-md` *between* tracks while the table pads *inside* each
 * cell, so every shared track started 12 px further right over here than over
 * there and the two views did not in fact line up. The gap is gone and the
 * inset moved into a per-cell `px-md` — the table's own model, one padding
 * rule, two views. The focus rail is likewise driven from the same focus signal
 * the list uses rather than from `:focus-visible`, so a mouse click paints it
 * in both views or in neither.
 */
import { For, Show, createSignal, type Component } from "solid-js";

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
import { SECTION_LABEL } from "~/components/ui/surfaces";
import { cn } from "~/lib/cn";
import { count as fmtCount } from "~/lib/format";
import { TRACK } from "./AlertTable";

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

/**
 * The bucket row's grid, so a chip that only sometimes renders — unacked,
 * flapping, snoozed — can never shift "Last seen" sideways depending on which
 * of its neighbours happened to show up on *this* bucket. Every track but the
 * bucket name is a fixed width, sized for its worst realistic
 * content: the div-soup equivalent of `AlertTable`'s `<colgroup>`, one grid per
 * row rather than shared table columns, so this string is the single source
 * that both row renderings (linked and unlinked) and the pinned header must
 * share to stay aligned with each other.
 *
 * ⭐ THE TRACKS THAT HAVE A COUNTERPART IN THE LIST COME FROM `TRACK`. Severity,
 * state and "last seen" are the same widths as the alert table's, and the bucket
 * name is the one elastic, truncating column exactly as the alert name is over
 * there — so scanning down either view puts your eye in the same three places.
 * The middle tracks are the bucket's own facts and are sized for those; they do
 * not pretend to be the list's cluster/duration/episode columns.
 *
 * ⛔ AND THEY ARE BUDGETED AGAINST THE SAME 976 px THE LIST IS (see `TRACK`).
 * This view renders inside `routes/alerts.tsx`, beside the same filter rail, so
 * it gets exactly the width the list gets and no more — the design preview
 * shows it full-bleed, which flatters it, and is not the number to budget from.
 *
 * A grid fails differently from the table it mirrors: fixed `grid-template`
 * tracks are never ignored the way `<col width>` was under `table-layout: auto`,
 * so this half was never *inert* — it was simply overspent. The tracks summed to
 * 701 px including the gutter, leaving the bucket name — the one thing the view
 * exists to show — 275 px at 1280 px. Re-measured against what each cell
 * actually renders (severity's widest label is the 82 px "no severity"; "Total"
 * never exceeds 47 px; four state counts want 131 px and three signal chips want
 * 210 px, and those two have always clipped by design) the tracks now total
 * 693 px, leaving the name **283 px**.
 *
 * The clipping is deliberately unequal, and that is the decision: `By state` and
 * `Signals` lose their last chip before the bucket name loses a character,
 * because a count you can read three of is still a count and a name cut in half
 * is not a name. The `minmax` floor is what turns any further squeeze into an
 * honest scrollbar rather than a silently crushed name; 16 rem is the width
 * below which a bucket key stops being identifiable.
 */
const BUCKET_TRACKS = `${TRACK.severity} minmax(16rem,1fr) ${TRACK.state} 5rem 7rem 8rem ${TRACK.seen}`;

/**
 * The row's shared class half; the tracks above arrive via `style`.
 *
 * No `gap` and no padding of its own: the inset is `BUCKET_CELL`'s `px-md`, on
 * each cell, which is exactly how `AlertTable` pads a `<td>`. A gap here would
 * put every shared track 12 px out of line with the list's.
 */
const BUCKET_ROW_GRID = "grid min-w-0 flex-1 items-center";

/** One cell of that grid — the div-soup equivalent of the table's `CELL`. */
const BUCKET_CELL = "min-w-0 px-md";

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
    <p class="border-b border-line bg-surface px-md py-sm text-meta leading-snug text-ink-muted">
      {fmtCount(props.buckets.length)}
      {props.hasMore ? "+" : ""} bucket{props.buckets.length === 1 ? "" : "s"} by{" "}
      {NOUN[props.by]}. Counted server-side over every alert matching these filters, not over the
      rows on screen. Ordered by bucket key, which is what makes paging over them exact.
    </p>

    <Show when={props.onDrillDown === null}>
      <p class="border-b border-line bg-surface px-md py-sm text-meta leading-snug text-ink-muted">
        These buckets cannot be opened: the alert list has no source-fingerprint filter, so oto
        cannot narrow to one fingerprint without pretending a different filter means the same
        thing. The counts below are still exact.
      </p>
    </Show>

    {/*
      ⭐ THE SECTION HEADER PINS, exactly as the list's `<thead>` does.

      This is the grouped view's group header, and it is one header rather than
      one per sub-section for a reason the endpoint decides: a bucket carries no
      members, so there is nothing beneath a bucket to pin a header over. The
      only honest section here is "the buckets on this axis", and it stays on
      screen while you scroll them. Sectioning the list further would mean
      inventing an order the server did not return — the one thing that would
      make its keyset paging wrong.
    */}
    <section aria-label={`Alert buckets by ${NOUN[props.by]}`}>
      <div class="sticky top-0 z-10 flex items-stretch border-b border-line bg-raised">
        <span class="shrink-0" style={{ width: TRACK.gutter }} aria-hidden="true" />
        <div
          class={cn(BUCKET_ROW_GRID, "py-sm", SECTION_LABEL, "text-ink-muted")}
          style={{ "grid-template-columns": BUCKET_TRACKS }}
        >
          <span class={cn(BUCKET_CELL, "truncate")}>Severity</span>
          <span class={cn(BUCKET_CELL, "truncate")}>
            {props.by === "alertname" ? "Alert name" : NOUN[props.by]}
          </span>
          <span class={cn(BUCKET_CELL, "truncate")}>State</span>
          <span class={cn(BUCKET_CELL, "truncate")}>Total</span>
          <span class={cn(BUCKET_CELL, "truncate")}>By state</span>
          <span class={cn(BUCKET_CELL, "truncate")}>Signals</span>
          <span class={cn(BUCKET_CELL, "truncate text-right")}>Last seen</span>
        </div>
      </div>

      <ul>
        <For each={props.buckets}>
          {(bucket) => (
            <BucketRow bucket={bucket} by={props.by} onDrillDown={props.onDrillDown} />
          )}
        </For>
      </ul>
    </section>

    <Show when={props.hasMore}>
      <div class="border-t border-line px-md py-md text-center">
        <Button size="sm" busy={props.loading} onClick={props.onLoadMore}>
          Load more buckets
        </Button>
      </div>
    </Show>
  </div>
);

/**
 * The gutter, byte-for-byte the alert row's: a 2 px rail that is always present
 * and only ever changes colour, then the 3 px state bar.
 *
 * The rail is transparent at rest and accent when the row holds focus, so focus
 * is legible without a ring that would move anything — and a bucket that cannot
 * be opened still reserves the same 2 px, so linked and unlinked rows line up
 * with each other and with the list.
 *
 * ⛔ THE RAIL IS DRIVEN FROM FOCUS, NOT FROM `:focus-visible`. It used to be
 * `group-focus-visible:`, which the browser withholds after a mouse click —
 * while `AlertTable` paints its rail from a signal set on `focusin` and so
 * paints it for a click too. One view lit up under the pointer and the other
 * did not, on the same gesture, for the same meaning. The list's behaviour is
 * the one that survives: focus is focus, however it was acquired.
 */
const BucketGutter: Component<{ readonly state: State; readonly focused: boolean }> = (props) => (
  <span class="flex shrink-0 self-stretch" style={{ width: TRACK.gutter }} aria-hidden="true">
    <span class={cn("w-0.5 shrink-0", props.focused ? "bg-accent" : "bg-transparent")} />
    <span class={cn("w-[3px] shrink-0", STATE_BAR[props.state])} />
  </span>
);

const BucketRow: Component<{
  readonly bucket: AlertRollup;
  readonly by: RollupAxis;
  readonly onDrillDown: ((key: string) => void) | null;
}> = (props) => {
  const b = (): AlertRollup => props.bucket;

  /**
   * The row's own focus, mirroring the list's `focusId` cursor: set on
   * `focusin` so a click paints the accent rail exactly as a keyboard arrival
   * does. See `BucketGutter`.
   */
  const [focused, setFocused] = createSignal(false);

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
      {/* The bucket's severity reads exactly as a row's does, in the same track:
          the most urgent severity present, glyph and word together (U1). */}
      <div class={BUCKET_CELL}>
        <SeverityMark severity={topSeverity(b())} withLabel />
      </div>

      <span class={BUCKET_CELL}>
        <span class="block truncate font-medium text-ink" title={b().key}>
          {label()}
        </span>
        <Show when={b().key === "" && props.by === "namespace"}>
          <span class="block truncate text-meta text-ink-subtle">
            These alerts carry no promoted namespace label.
          </span>
        </Show>
      </span>

      <div class={BUCKET_CELL}>
        <StateChip state={b().state} size="sm" />
      </div>

      <span class={cn(BUCKET_CELL, "truncate text-meta tabular-nums text-ink-muted")}>
        {fmtCount(b().total_count)} total
      </span>

      {/* Per-state counts. `resolved` and `expired` stay separate — they are
          different facts and merging them would lose the interesting one. */}
      <span
        class={cn(
          BUCKET_CELL,
          "flex items-center gap-xs overflow-hidden text-meta tabular-nums text-ink-muted",
        )}
      >
        <For each={counts()}>
          {(c) => (
            <span
              class="inline-flex shrink-0 items-center gap-2xs"
              title={`${c.n} ${STATE_LABEL[c.state].toLowerCase()}`}
            >
              <span class={cn("size-1.5 rounded-full", STATE_BAR[c.state])} aria-hidden="true" />
              {c.n}
              <span class="sr-only-focusable">{STATE_LABEL[c.state]}</span>
            </span>
          )}
        </For>
      </span>

      {/* The chips below are each conditional on their own count, but together
          they still occupy exactly one grid cell: the reserved column is the
          same width whether none or all of them render, so it alone absorbs
          that variability and nothing after it has to.

          ⛔ THERE IS NO "N unseen" CHIP. It counted `unacked_count`, which the
          bucket no longer carries: every other counter here is a property of
          the Alert, and an ack is a receipt for one of its firings. A bucket
          that said "12 unseen" while some of those twelve had been acknowledged
          during a firing that ended months ago is worse than no number. */}
      <div class={cn(BUCKET_CELL, "flex items-center gap-xs overflow-hidden")}>
        <Show when={b().flapping_count > 0}>
          <span
            class="shrink-0 rounded-chip border border-line bg-surface px-2xs text-meta leading-4 text-ink-muted"
            title="Members oto has damped as flapping. A visible state, never a silent drop."
          >
            {b().flapping_count} flapping
          </span>
        </Show>

        {/* ⛔ THERE IS NO `snoozed` CHIP, AND ITS ABSENCE IS NOT AN OVERSIGHT. A
            roll-up shares the filter of the list it summarises, and the list is
            always on one tab or the other — so this count could only ever read 0
            (main tab) or the bucket's whole total (Quiet). It would be a
            restatement of which tab you are on, drawn once per bucket. The
            Quiet tab's own badge is where the number lives now. */}
      </div>

      <span class={cn(BUCKET_CELL, "truncate text-right text-meta text-ink-subtle")}>
        <RelativeTime value={b().last_seen_at} label="Newest activity" />
      </span>
    </>
  );

  const firing = (): boolean => b().state === "firing";

  return (
    <li
      class={cn(
        // Hairline only, as in the list: the tonal shift and the row's height do
        // the separating, not a heavy rule.
        "border-b border-line/50",
        // The same alpha the list uses, for the same reason (§0.6): at `/30` a
        // firing bucket read quieter than the filter rail's selection, and the
        // two views disagreed with each other about how hot "firing" looks.
        firing() ? "bg-firing-fill/70" : "bg-surface",
      )}
    >
      <Show
        when={props.onDrillDown}
        fallback={
          // `height`, not `min-height`: the row is `--oto-row-h` exactly, the
          // same contract the list's `<tr>` keeps, so the two views scan at one
          // rhythm and neither can grow a row out from under it.
          <div class="flex items-stretch" style={{ height: "var(--oto-row-h)" }}>
            <BucketGutter state={b().state} focused={false} />
            <div class={BUCKET_ROW_GRID} style={{ "grid-template-columns": BUCKET_TRACKS }}>
              <Body />
            </div>
          </div>
        }
      >
        {(drill) => (
          <button
            type="button"
            // `outline-none` because the rail IS the focus indicator and a ring
            // on a full-width row is the heavy border §3 rules out.
            class={cn(
              "flex w-full items-stretch text-left outline-none",
              firing() ? "hover:bg-firing-fill/85" : "hover:bg-raised",
            )}
            style={{ height: "var(--oto-row-h)" }}
            title={`Open the ${b().total_count} alert${b().total_count === 1 ? "" : "s"} in this bucket, keeping every other filter`}
            onFocusIn={() => setFocused(true)}
            onFocusOut={() => setFocused(false)}
            onClick={() => drill()(b().key)}
          >
            <BucketGutter state={b().state} focused={focused()} />
            <div class={BUCKET_ROW_GRID} style={{ "grid-template-columns": BUCKET_TRACKS }}>
              <Body />
            </div>
          </button>
        )}
      </Show>
    </li>
  );
};
