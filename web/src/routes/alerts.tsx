/**
 * `/alerts` — the primary screen.
 *
 * Pagination is keyset and **append-only**: "Load more" adds a page rather than
 * replacing one. Numbered pages would be a lie over a keyset cursor (the list
 * shifts under you as alerts fire), and an operator wants "show me more of this"
 * far more often than "take me to page 4".
 *
 * The accumulated pages are held in a signal rather than in the query cache,
 * because the cache is keyed by filters and a live `alert.upserted` frame
 * invalidates the whole `["alerts"]` prefix. That is the correct behaviour for
 * page one and the wrong behaviour for a stack of eight, so the stack resets
 * deliberately and visibly instead of silently re-fetching everything.
 *
 * **Grouping is server-side.** With a `group_by` axis selected this screen reads
 * `GET /api/v1/alerts/rollups`, which applies the identical filter set and
 * aggregates over the whole result. Rolling up the loaded pages client-side —
 * which this screen used to do — under-reported every count the moment the
 * result exceeded one page.
 */
import {
  For,
  Match,
  Show,
  Switch,
  createComputed,
  createMemo,
  createSignal,
  onCleanup,
  onMount,
} from "solid-js";
import { useNavigate, useSearchParams } from "@solidjs/router";
import { useQuery } from "@tanstack/solid-query";

import { batchGetRuleSnapshots, listAlertRollups, listAlerts } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { Alert, AlertRollup, ListEnvelope, RollupAxis, RuleSnapshot } from "~/api/types";
import { Button } from "~/components/ui/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "~/components/ui/states";
import { count as fmtCount } from "~/lib/format";
import { AlertTable } from "~/features/alerts/AlertTable";
import { FilterBar } from "~/features/alerts/FilterBar";
import { GroupedAlerts } from "~/features/alerts/GroupedAlerts";
import {
  DEFAULT_FILTERS,
  compileFilters,
  compileRollupFilters,
  filtersFromSearch,
  isUnfiltered,
  searchFromFilters,
  withMatcher,
  withRollupBucket,
  type AlertFilters,
} from "~/features/alerts/filters";

const PAGE_SIZE = 100;
const BUCKET_PAGE_SIZE = 100;

/**
 * Where one keyset stands, held as a single value stamped with the filters that
 * minted it.
 *
 * ⭐⭐ A CURSOR CANNOT OUTLIVE THE FILTER THAT MINTED IT. §E.3 answers a cursor
 * carried across a filter change with `400 cursor_filter_mismatch`, and
 * resetting it from an effect is too late to prevent that: solid-query reads the
 * query key in Solid's *pure* phase, so a request pairing the old cursor with
 * the new filters is built and sent before any effect gets to run. The same
 * hazard is guarded the same way in `RejectionsPanel`, where the reasons are
 * local state and the reset rides along in the `onChange` that changes them.
 *
 * Here the filter set is the URL — edited by the filter bar, by a label click,
 * by a drill-down from a bucket and by the back button, and only some of those
 * pass through this component — so there is no single call site to reset from.
 * The stamp does that job from the other end: a held position whose fingerprint
 * is not the fingerprint on screen is a position in somebody else's keyset, and
 * the memos below hand back a first page instead of it. That is a derivation,
 * not a correction, so the doomed request is never constructed — and the stamped
 * position is then *dropped*, because a shadowed one comes back the moment the
 * fingerprint it was minted under is on screen again.
 */
interface KeysetPage<T> {
  /** The filters (and, for buckets, the axis) this position was minted under. */
  readonly fingerprint: string;
  readonly cursor: string | null;
  /** Pages already folded in. The page in flight is added by the memos below. */
  readonly kept: readonly T[];
  readonly pageCount: number;
}

const freshPage = <T,>(fingerprint: string): KeysetPage<T> => ({
  fingerprint,
  cursor: null,
  kept: [],
  pageCount: 1,
});

export default function AlertsRoute() {
  const navigate = useNavigate();
  // Subscribing to the router's params is what makes back/forward work; the
  // filters are derived from the URL and never held in parallel.
  const [searchParams] = useSearchParams();

  const filters = createMemo<AlertFilters>(() => {
    const sp = new URLSearchParams();
    for (const [k, v] of Object.entries(searchParams)) {
      if (typeof v === "string") sp.set(k, v);
      else if (Array.isArray(v) && v[0] !== undefined) sp.set(k, v[0]);
    }
    return filtersFromSearch(sp.toString());
  });

  const setFilters = (next: AlertFilters): void => {
    navigate(`/alerts${searchFromFilters(next)}`, { replace: false, scroll: false });
  };

  const axis = (): RollupAxis | null => {
    const by = filters().groupBy;
    return by === "none" ? null : by;
  };

  /* ---- keyset pagination ------------------------------------------------- */

  // Any filter change invalidates every cursor minted under the old filters
  // (§E.3 answers a stale one with `400 cursor_filter_mismatch`). The roll-up
  // cursor is bound to the filters **and** to `group_by`, because regrouping
  // changes the bucket keys themselves — so its fingerprint carries the axis.
  const filterFingerprint = createMemo(() => searchFromFilters({ ...filters(), groupBy: "none" }));
  const rollupFingerprint = createMemo(() => `${axis() ?? "none"} ${filterFingerprint()}`);

  const [heldList, setHeldList] = createSignal(freshPage<Alert>(filterFingerprint()));
  const [heldBuckets, setHeldBuckets] = createSignal(freshPage<AlertRollup>(rollupFingerprint()));

  /**
   * The page position in force, which is the held one only while it still
   * belongs to what is on screen. @see KeysetPage — being a *derivation* rather
   * than a reset is the whole point: it has already happened by the time
   * solid-query reads the query key.
   */
  const listPage = createMemo<KeysetPage<Alert>>(() => {
    const held = heldList();
    const fingerprint = filterFingerprint();
    return held.fingerprint === fingerprint ? held : freshPage<Alert>(fingerprint);
  });

  const bucketPage = createMemo<KeysetPage<AlertRollup>>(() => {
    const held = heldBuckets();
    const fingerprint = rollupFingerprint();
    return held.fingerprint === fingerprint ? held : freshPage<AlertRollup>(fingerprint);
  });

  /**
   * ⭐⭐ AND A POSITION THAT LOST ITS KEYSET IS DISCARDED, NOT MERELY SHADOWED.
   *
   * The memos above are what stops the doomed request being built. On their own
   * they only *hide* the foreign position, and hiding is not enough, because the
   * filter set is the URL: filter B and then Back makes the old fingerprint the
   * fingerprint on screen again, and a hidden page-3 stack would come back with
   * it. What returns is a minutes-old snapshot — a live `alert.upserted` frame
   * invalidates the `["alerts"]` prefix, but only the key actually being read
   * refetches, so ack, state and severity on those rows are frozen — and once
   * that key has passed `gcTime` the fold below would splice them onto whatever
   * page one is on screen. On the roll-up path that is two axes of buckets in one
   * list, and a drill-down that writes a namespace into `alertname=`.
   *
   * So the derived position is written back over the held one, which leaves the
   * signal holding only ever the keyset on screen. `createComputed` is Solid's
   * pure-phase computation — the one place a write like this belongs, and for the
   * same reason the reset could not be an effect. It writes the very object the
   * memo derived, so when the two already agree the write is a no-op by reference
   * equality and nothing downstream sees an update.
   */
  createComputed(() => setHeldList(listPage()));
  createComputed(() => setHeldBuckets(bucketPage()));

  const compiled = createMemo(() => compileFilters(filters(), PAGE_SIZE, listPage().cursor));

  const query = useQuery(() => ({
    queryKey: qk.alerts.list(compiled().query),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlerts(compiled().query, {}, { signal }),
    // A matcher the server refuses at parse time must never be sent — a request
    // with the filter quietly dropped returns an unfiltered page that looks
    // filtered. The flat list is also not fetched while a roll-up is on screen.
    enabled: compiled().ok && axis() === null,
    // Keep the previous page on screen while the next one is in flight, so a
    // filter change never blanks the table out from under a cursor.
    placeholderData: (prev: ListEnvelope<Alert> | undefined) => prev,
  }));

  const rollupCompiled = createMemo(() =>
    compileRollupFilters(filters(), axis() ?? "alertname", BUCKET_PAGE_SIZE, bucketPage().cursor),
  );

  const rollups = useQuery(() => ({
    queryKey: qk.alerts.rollups(rollupCompiled().query),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlertRollups(rollupCompiled().query, {}, { signal }),
    enabled: rollupCompiled().ok && axis() !== null,
    placeholderData: (prev: ListEnvelope<AlertRollup> | undefined) => prev,
  }));

  /**
   * The visible list is a pure function of the kept pages plus the page in
   * flight, deduplicated by id — a live refetch of page one legitimately
   * overlaps what we already hold, and appending it twice would show ghosts.
   */
  const alerts = createMemo<readonly Alert[]>(() => {
    const held = listPage();
    const page = query.data?.data ?? [];
    if (held.cursor === null) return page;
    const seen = new Set(held.kept.map((a) => a.id));
    return [...held.kept, ...page.filter((a) => !seen.has(a.id))];
  });

  /** The same fold for buckets, deduplicated by bucket key. */
  const buckets = createMemo<readonly AlertRollup[]>(() => {
    const held = bucketPage();
    const page = rollups.data?.data ?? [];
    if (held.cursor === null) return page;
    const seen = new Set(held.kept.map((b) => b.key));
    return [...held.kept, ...page.filter((b) => !seen.has(b.key))];
  });

  /* ---- what the rule said (ADR 0025) ------------------------------------- */

  /**
   * The distinct snapshot ids on screen.
   *
   * `include=rule` puts one id on each row and nothing more, because
   * `alerts/api` may not name the rules module's types. Content addressing is
   * what makes this set small: a page of alerts firing under one unchanged rule
   * is **one** id, not one per row. Sorted so the query key is stable — the same
   * set in a different order is the same question.
   */
  const ruleIds = createMemo<readonly string[]>(() => {
    const ids = new Set<string>();
    for (const a of alerts()) {
      const id = a.rule?.id;
      if (typeof id === "string" && id !== "") ids.add(id);
    }
    return [...ids].sort();
  });

  /**
   * One call for the whole page, never one per row — the N+1 this endpoint
   * exists to remove.
   *
   * `staleTime: Infinity` is a statement about the data, not a tuning knob: a
   * rule snapshot is immutable and content-addressed, so once an id has been
   * resolved its answer can never change. Refetching it would be asking a
   * settled question again.
   */
  const rules = useQuery(() => ({
    queryKey: qk.rules.batch(ruleIds()),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      batchGetRuleSnapshots(ruleIds(), { signal }),
    enabled: ruleIds().length > 0,
    staleTime: Infinity,
    // Keep what is already resolved while a bigger batch is in flight, so
    // "Load more" never blanks the rule column of the rows already on screen.
    placeholderData: (prev: readonly RuleSnapshot[] | undefined) => prev,
  }));

  const rulesById = createMemo<ReadonlyMap<string, RuleSnapshot>>(() => {
    const byId = new Map<string, RuleSnapshot>();
    for (const snapshot of rules.data ?? []) byId.set(snapshot.id, snapshot);
    return byId;
  });

  const hasMore = (): boolean =>
    (axis() === null ? query.data?.page.has_more : rollups.data?.page.has_more) ?? false;

  const failed = (): boolean => (axis() === null ? query.isError : rollups.isError);
  const failure = (): unknown => (axis() === null ? query.error : rollups.error);
  const retry = (): void => {
    void (axis() === null ? query.refetch() : rollups.refetch());
  };

  // A placeholder envelope is the PREVIOUS key's page, still on screen while the
  // current one is in flight — so the cursor it carries was minted under the
  // filters that key asked for. Paging off it is the same mismatch by a longer
  // route, and the answer is the same: that page is not this keyset's page.
  const loadMore = (): void => {
    const next = query.data?.page.next_cursor;
    if (typeof next !== "string" || next === "" || query.isPlaceholderData) return;
    // Freeze what is on screen before asking for the next page, so the fold is
    // additive rather than a race between two in-flight responses.
    const frozen = alerts();
    const held = listPage();
    setHeldList({ ...held, cursor: next, kept: frozen, pageCount: held.pageCount + 1 });
  };

  const loadMoreBuckets = (): void => {
    const next = rollups.data?.page.next_cursor;
    if (typeof next !== "string" || next === "" || rollups.isPlaceholderData) return;
    const frozen = buckets();
    const held = bucketPage();
    setHeldBuckets({ ...held, cursor: next, kept: frozen, pageCount: held.pageCount + 1 });
  };

  const onFilterLabel = (name: string, value: string): void => {
    setFilters(withMatcher(filters(), name, value));
  };

  /**
   * Drilling from a bucket keeps the whole filter set and adds the bucket's own
   * key. `null` when the axis has no matching list filter — see
   * `withRollupBucket`, which refuses rather than substituting.
   */
  const drillDown = createMemo<((key: string) => void) | null>(() => {
    const by = axis();
    if (by === null) return null;
    if (withRollupBucket(filters(), by, "") === null) return null;
    return (key: string) => {
      const next = withRollupBucket(filters(), by, key);
      if (next !== null) setFilters(next);
    };
  });

  /* ---- keyboard ---------------------------------------------------------- */

  onMount(() => {
    const onKey = (e: KeyboardEvent): void => {
      const target = e.target as HTMLElement | null;
      const typing =
        target !== null &&
        (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable);
      if (typing || e.metaKey || e.ctrlKey || e.altKey) return;

      if (e.key === "/") {
        e.preventDefault();
        document.getElementById("alert-q")?.focus();
      }
      if (e.key === "f") {
        e.preventDefault();
        document.getElementById("alert-matchers")?.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    // `createEffect` cannot return a cleanup in Solid — its return value is the
    // next run's argument — so the teardown is an explicit `onCleanup`.
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  /* ---- render ------------------------------------------------------------ */

  const blocked = (): boolean => (axis() === null ? !compiled().ok : !rollupCompiled().ok);
  const rejected = () => (axis() === null ? compiled().rejected : rollupCompiled().rejected);

  const status = (): string => {
    // A disabled query reports `isPending`, which is not the same as "loading".
    // Saying "Loading…" over a request that was deliberately never sent would be
    // the small lie this whole screen is built to avoid.
    if (blocked()) return "Nothing requested";
    if (axis() !== null) {
      const n = buckets().length;
      if (rollups.isPending && n === 0) return "Loading…";
      return `${fmtCount(n)}${hasMore() ? "+" : ""} bucket${n === 1 ? "" : "s"}`;
    }
    const n = alerts().length;
    if (query.isPending && n === 0) return "Loading…";
    return `${fmtCount(n)}${hasMore() ? "+" : ""} alert${n === 1 ? "" : "s"}`;
  };

  return (
    <div class="flex min-h-0 flex-1 flex-col">
      <FilterBar
        filters={filters()}
        onChange={setFilters}
        onReset={() => setFilters(DEFAULT_FILTERS)}
        status={
          <span class="text-[12px] tabular-nums text-ink-muted" aria-live="polite">
            {status()}
          </span>
        }
      />

      <Switch>
        {/* A blocked query is not an error and not an empty list — it is a
            filter oto refuses to approximate. It gets its own state. */}
        <Match when={blocked()}>
          <EmptyState
            title="This filter cannot be served exactly, so nothing was requested."
            body="oto refuses to run a filter that would fall back to a sequential scan. Fix the matcher above and the list will return — see the reasons under the input."
          />
        </Match>

        <Match when={failed()}>
          <ErrorState error={failure()} onRetry={retry} />
        </Match>

        <Match when={axis() !== null}>
          <Switch>
            <Match when={rollups.isPending && buckets().length === 0}>
              <div class="flex-1 overflow-hidden">
                <TableSkeleton rows={10} cols={5} />
              </div>
            </Match>
            <Match when={buckets().length === 0}>
              <EmptyState
                title="No alerts match these filters, so there is nothing to group."
                body="The buckets are computed from the same filtered set as the list. An empty roll-up means an empty list, not a grouping problem."
                action={
                  <Button size="sm" onClick={() => setFilters(DEFAULT_FILTERS)}>
                    Clear filters
                  </Button>
                }
              />
            </Match>
            <Match when={true}>
              <GroupedAlerts
                buckets={buckets()}
                by={axis() as RollupAxis}
                hasMore={hasMore()}
                loading={rollups.isFetching}
                onLoadMore={loadMoreBuckets}
                onDrillDown={drillDown()}
              />
            </Match>
          </Switch>
        </Match>

        <Match when={query.isPending && alerts().length === 0}>
          <div class="flex-1 overflow-hidden">
            <TableSkeleton rows={14} cols={6} />
          </div>
        </Match>

        <Match when={alerts().length === 0}>
          <Show
            when={isUnfiltered(filters())}
            fallback={
              <EmptyState
                title="No alerts match these filters."
                body="The filters are doing something — that is not the same as there being nothing here. Clear them to see everything oto has."
                action={
                  <Button size="sm" onClick={() => setFilters(DEFAULT_FILTERS)}>
                    Clear filters
                  </Button>
                }
              />
            }
          >
            <EmptyState
              title="Nothing has fired yet."
              body="oto is listening. When your Alertmanager sends its first webhook, the alert and everything that happens to it afterwards will appear here."
            />
          </Show>
        </Match>

        <Match when={true}>
          <AlertTable
            alerts={alerts()}
            snoozedKnown={filters().snoozed}
            rules={rulesById()}
            rulesPending={rules.isFetching}
            onFilterLabel={onFilterLabel}
            footer={
              <Footer
                hasMore={hasMore()}
                loading={query.isFetching}
                loaded={alerts().length}
                pageCount={listPage().pageCount}
                pageSize={PAGE_SIZE}
                onLoadMore={loadMore}
              />
            }
          />
        </Match>
      </Switch>

      {/* Rejected matchers are explained at the bottom too, where the empty
          state points, so the reason is never off-screen from its effect. */}
      <Show when={rejected().length > 0}>
        <ul class="border-t border-line bg-raised px-3 py-2 text-[11px] leading-snug text-ink">
          <For each={rejected()}>
            {(r) => (
              <li>
                <code class="font-mono">
                  {r.matcher.name}
                  {r.matcher.op}
                  {JSON.stringify(r.matcher.value)}
                </code>{" "}
                — {r.reason}
              </li>
            )}
          </For>
        </ul>
      </Show>
    </div>
  );
}

function Footer(props: {
  readonly hasMore: boolean;
  readonly loading: boolean;
  readonly loaded: number;
  readonly pageCount: number;
  /** How many rows one press adds — the number the button names, not a second copy of it. */
  readonly pageSize: number;
  readonly onLoadMore: () => void;
}) {
  return (
    <div class="flex items-center justify-center gap-3 border-t border-line bg-surface px-3 py-2">
      <Show
        when={props.hasMore}
        fallback={
          <span class="text-[11px] text-ink-subtle">
            That is all {fmtCount(props.loaded)} of them.
          </span>
        }
      >
        <Button size="sm" busy={props.loading} onClick={props.onLoadMore}>
          Load {fmtCount(props.pageSize)} more
        </Button>
        <span class="text-[11px] text-ink-subtle">
          {fmtCount(props.loaded)} loaded across {props.pageCount} page
          {props.pageCount === 1 ? "" : "s"}
        </span>
      </Show>
    </div>
  );
}
