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
 */
import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createSignal,
  onCleanup,
  onMount,
} from "solid-js";
import { useNavigate, useSearchParams } from "@solidjs/router";
import { useQuery } from "@tanstack/solid-query";

import { listAlerts } from "~/api/endpoints";
import { qk } from "~/api/keys";
import type { Alert, ListEnvelope } from "~/api/types";
import { Button } from "~/components/ui/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "~/components/ui/states";
import { count as fmtCount } from "~/lib/format";
import { AlertTable } from "~/features/alerts/AlertTable";
import { FilterBar } from "~/features/alerts/FilterBar";
import { GroupedAlerts } from "~/features/alerts/GroupedAlerts";
import { groupAlerts } from "~/features/alerts/grouping";
import {
  DEFAULT_FILTERS,
  compileFilters,
  filtersFromSearch,
  isUnfiltered,
  searchFromFilters,
  withMatcher,
  type AlertFilters,
} from "~/features/alerts/filters";

const PAGE_SIZE = 100;

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

  /* ---- keyset pagination ------------------------------------------------- */

  const [cursor, setCursor] = createSignal<string | null>(null);
  /** Pages already folded in. The page in flight is added by the memo below. */
  const [kept, setKept] = createSignal<readonly Alert[]>([]);
  const [pageCount, setPageCount] = createSignal(1);

  // Any filter change invalidates every cursor minted under the old filters
  // (§E.3 answers a stale one with `400 cursor_filter_mismatch`), so the stack
  // resets. Serialising the filters is the cheapest correct change detector.
  const filterFingerprint = createMemo(() => searchFromFilters({ ...filters(), groupBy: "none" }));
  createEffect((previous: string | undefined) => {
    const current = filterFingerprint();
    if (previous !== undefined && previous !== current) {
      setCursor(null);
      setKept([]);
      setPageCount(1);
    }
    return current;
  });

  const compiled = createMemo(() => compileFilters(filters(), PAGE_SIZE, cursor()));

  const query = useQuery(() => ({
    queryKey: qk.alerts.list(compiled().query, compiled().label),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlerts(compiled().query, compiled().label, { signal }),
    // A matcher the contract cannot express must never be sent — a request with
    // the filter quietly dropped returns an unfiltered page that looks filtered.
    enabled: compiled().ok,
    // Keep the previous page on screen while the next one is in flight, so a
    // filter change never blanks the table out from under a cursor.
    placeholderData: (prev: ListEnvelope<Alert> | undefined) => prev,
  }));

  /**
   * The visible list is a pure function of the kept pages plus the page in
   * flight, deduplicated by id — a live refetch of page one legitimately
   * overlaps what we already hold, and appending it twice would show ghosts.
   */
  const alerts = createMemo<readonly Alert[]>(() => {
    const page = query.data?.data ?? [];
    if (cursor() === null) return page;
    const seen = new Set(kept().map((a) => a.id));
    return [...kept(), ...page.filter((a) => !seen.has(a.id))];
  });

  const hasMore = (): boolean => query.data?.page.has_more ?? false;

  const loadMore = (): void => {
    const next = query.data?.page.next_cursor;
    if (typeof next !== "string" || next === "") return;
    // Freeze what is on screen before asking for the next page, so the fold is
    // additive rather than a race between two in-flight responses.
    setKept(alerts());
    setPageCount((n) => n + 1);
    setCursor(next);
  };

  const onFilterLabel = (name: string, value: string): void => {
    setFilters(withMatcher(filters(), name, value));
  };

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

  const grouped = createMemo(() => {
    const by = filters().groupBy;
    return by === "none" ? null : groupAlerts(alerts(), by);
  });

  const status = (): string => {
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
        <Match when={!compiled().ok}>
          <EmptyState
            title="This filter cannot be served exactly, so nothing was requested."
            body="oto refuses to run a filter it can only approximate. Fix the matcher above and the list will return — see the reasons under the input."
          />
        </Match>

        <Match when={query.isError}>
          <ErrorState error={query.error} onRetry={() => void query.refetch()} />
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

        <Match when={grouped()}>
          {(groups) => (
            <GroupedAlerts
              groups={groups()}
              by={filters().groupBy as "alertname" | "namespace" | "fingerprint"}
              loadedCount={alerts().length}
              hasMore={hasMore()}
              onFilterLabel={onFilterLabel}
            />
          )}
        </Match>

        <Match when={true}>
          <AlertTable
            alerts={alerts()}
            onFilterLabel={onFilterLabel}
            footer={
              <Footer
                hasMore={hasMore()}
                loading={query.isFetching}
                loaded={alerts().length}
                pageCount={pageCount()}
                onLoadMore={loadMore}
              />
            }
          />
        </Match>
      </Switch>

      {/* The grouped view scrolls its own container, so its footer lives here. */}
      <Show when={grouped() !== null && hasMore()}>
        <Footer
          hasMore={hasMore()}
          loading={query.isFetching}
          loaded={alerts().length}
          pageCount={pageCount()}
          onLoadMore={loadMore}
        />
      </Show>

      {/* Rejected matchers are explained at the bottom too, where the empty
          state points, so the reason is never off-screen from its effect. */}
      <Show when={compiled().rejected.length > 0}>
        <ul class="border-t border-line bg-raised px-3 py-2 text-[11px] leading-snug text-ink">
          <For each={compiled().rejected}>
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
          Load {fmtCount(100)} more
        </Button>
        <span class="text-[11px] text-ink-subtle">
          {fmtCount(props.loaded)} loaded across {props.pageCount} page
          {props.pageCount === 1 ? "" : "s"}
        </span>
      </Show>
    </div>
  );
}
