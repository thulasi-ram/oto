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
  untrack,
} from "solid-js";
import { useNavigate, useSearchParams } from "@solidjs/router";
import { useQuery } from "@tanstack/solid-query";

import { batchGetRuleSnapshots, listAlertRollups, listAlerts } from "~/api/endpoints";
import { qk } from "~/api/keys";
import { useSession } from "~/api/session";
import type { Alert, AlertRollup, RollupAxis, RuleSnapshot } from "~/api/types";
import { Button } from "~/components/ui/Button";
import { EmptyState, ErrorState, PageEmptyState, TableSkeleton } from "~/components/ui/states";
import { count as fmtCount } from "~/lib/format";
import { createKeysetFeed, keepPrevious, type KeysetFeed } from "~/lib/keysetFeed";
import { AlertTable } from "~/features/alerts/AlertTable";
import { AlertTabs, summariseQuiet, type QuietSummary } from "~/features/alerts/AlertTabs";
import { FilterBar } from "~/features/alerts/FilterBar";
import { GroupedAlerts } from "~/features/alerts/GroupedAlerts";
import {
  DEFAULT_FILTERS,
  compileFilters,
  compileRollupFilters,
  filtersFromSearch,
  isExpiredOnly,
  isQuietTab,
  isUnfiltered,
  quietBadgeFilters,
  searchFromFilters,
  withMatcher,
  withRollupBucket,
  withTab,
  type AlertFilters,
  type AlertTab,
} from "~/features/alerts/filters";

const PAGE_SIZE = 100;
const BUCKET_PAGE_SIZE = 100;

/**
 * How many quiet alerts the **Quiet** badge will count before it stops counting
 * and says `200+`.
 *
 * ⭐ IT IS BOUNDED BECAUSE THE BADGE IS NOT THE LIST. The count exists so an
 * operator can see at a glance that something live is being held back; it is not
 * a report, and paging an org's entire snooze history to render a number in a tab
 * would spend a request budget on a digit. Where the cap bites, the badge reads
 * `200+` — the same promise the org-wide snooze banner makes: it may understate
 * how many holds are in force, it may never claim a total it did not see.
 *
 * The 30-day maximum is what makes this bound safe in practice: a snooze cannot
 * accumulate forever, so the live set is small by construction.
 */
const QUIET_BADGE_LIMIT = 200;

/** One shared empty set, so "nothing is held" is stable by reference. */
const NOTHING_HELD: ReadonlySet<string> = new Set<string>();

export default function AlertsRoute() {
  const navigate = useNavigate();
  const session = useSession();
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

  // A discarded roll-up position is not hypothetical here: a shadowed one
  // surviving a Back would splice two axes of buckets into one list, and a
  // drill-down would write a namespace into `alertname=`. The machine that
  // prevents it — and the §E.3 argument for why it must be a pure-phase
  // derivation — lives in `createKeysetFeed`.
  // The annotations cut the type-inference loop the closure creates: the feed
  // reads the query's envelope, and the query's key carries the feed's cursor.
  const listFeed: KeysetFeed<Alert> = createKeysetFeed({
    envelope: () => query.data,
    isPlaceholder: () => query.isPlaceholderData,
    keyOf: (a) => a.id,
    fingerprint: filterFingerprint,
  });

  const bucketFeed: KeysetFeed<AlertRollup> = createKeysetFeed({
    envelope: () => rollups.data,
    isPlaceholder: () => rollups.isPlaceholderData,
    keyOf: (b) => b.key,
    fingerprint: rollupFingerprint,
  });

  const compiled = createMemo(() => compileFilters(filters(), PAGE_SIZE, listFeed.cursor()));

  const query = useQuery(() => ({
    queryKey: qk.alerts.list(compiled().query),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlerts(compiled().query, {}, { signal }),
    // A matcher the server refuses at parse time must never be sent — a request
    // with the filter quietly dropped returns an unfiltered page that looks
    // filtered. The flat list is also not fetched while a roll-up is on screen.
    enabled: compiled().ok && axis() === null,
    placeholderData: keepPrevious,
  }));

  const rollupCompiled = createMemo(() =>
    compileRollupFilters(filters(), axis() ?? "alertname", BUCKET_PAGE_SIZE, bucketFeed.cursor()),
  );

  const rollups = useQuery(() => ({
    queryKey: qk.alerts.rollups(rollupCompiled().query),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlertRollups(rollupCompiled().query, {}, { signal }),
    enabled: rollupCompiled().ok && axis() !== null,
    placeholderData: keepPrevious,
  }));

  const alerts = listFeed.rows;
  const buckets = bucketFeed.rows;

  /* ---- the Quiet tab's badge ---------------------------------------------- */

  /**
   * ⭐⭐ THE BADGE IS ITS OWN BOUNDED REQUEST, AND IT IS NOT THE LIST QUERY.
   *
   * Splitting snoozed alerts onto their own tab is only safe if the tab says how
   * many are behind it and how alive they are (`AlertTabs` argues this at
   * length). Neither of the two queries above can answer that:
   *
   *   - the flat list is on ONE tab at a time, so on the main tab it has never
   *     seen a quiet alert, and on the Quiet tab it holds however many pages the
   *     operator has pressed "Load more" for — a running total, not a total;
   *   - the roll-up answers in buckets and would need summing across its own
   *     pagination to produce one number, which is the client-side-aggregate
   *     mistake this screen was rewritten to stop making.
   *
   * So it is one small request of its own, capped, carrying the operator's whole
   * filter set (`quietBadgeFilters`) so the number promises exactly what the tab
   * will contain. It rides the same `["alerts"]` cache prefix, so a snooze — or
   * its expiry arriving over SSE — refreshes the badge with everything else.
   */
  const quietCompiled = createMemo(() =>
    compileFilters(quietBadgeFilters(filters()), QUIET_BADGE_LIMIT, null),
  );

  const quietCount = useQuery(() => ({
    queryKey: qk.alerts.list(quietCompiled().query),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listAlerts(quietCompiled().query, {}, { signal }),
    enabled: quietCompiled().ok,
    placeholderData: keepPrevious,
  }));

  /** `null` until the count is known: the tab renders bare rather than "(0)". */
  const quiet = createMemo<QuietSummary | null>(() => {
    const envelope = quietCount.data;
    if (envelope === undefined) return null;
    return summariseQuiet(envelope.data, envelope.page.has_more === true);
  });

  const setTab = (tab: AlertTab): void => {
    setWentQuiet(null);
    setFilters(withTab(filters(), tab));
  };

  /**
   * The alert the operator has just snoozed, if any — so the screen can say
   * where its row went.
   *
   * ⛔ A DISAPPEARING ROW IS NOT A RESULT. Snoozing from the main tab removes the
   * row from the list it was snoozed on, which is correct and is also exactly
   * what a failed request looks like from the operator's chair. The sentence
   * below is what makes the two distinguishable, and it carries the way back.
   */
  const [wentQuiet, setWentQuiet] = createSignal<string | null>(null);

  /* ---- inserts do not move the list under the operator (§0.5) ------------ */

  /**
   * ⭐⭐ A ROW THE OPERATOR IS READING NEVER GETS PUSHED DOWN BY A NEW ONE.
   *
   * `alert.upserted` invalidates the whole `["alerts"]` prefix, page one
   * refetches, and the server re-sorts — so a fresh alert lands at the TOP of
   * the array this screen renders. Spliced in silently, every row below it moves
   * down by `--oto-row-h` while somebody is reading one of them, mid-incident.
   * The keyset feed is the right place to get the rows from and the wrong place
   * to fix this: it is a pagination machine, and what is held back here is a
   * *presentation* decision that must not touch the cursor.
   *
   * So the feed stays authoritative and this filters it. `held` names the ids
   * that arrived above what was already on screen; everything else — content
   * changes to rows already visible, rows that left the filter, rows appended
   * below — flows straight through, because withholding those would be a
   * different lie (a list frozen at a state that is no longer true).
   *
   * Deferral only happens while the operator is actually *in* the list
   * (`engaged` below). Idle at the top, alerts appear as they always did; that
   * is the state this screen spends most of its life in, and a "1 new" chip
   * over a list nobody is reading is friction bought for nothing.
   */
  const [held, setHeld] = createSignal<ReadonlySet<string>>(NOTHING_HELD);

  /**
   * Whether the operator has a position in the list worth protecting: a
   * scroller inside the content column is off its top, or focus is somewhere
   * inside it (the table region, a row link, the load-more button).
   *
   * Observed from the column, not from `AlertTable`: `scroll` does not bubble
   * but it does traverse the capture phase, so one capture listener on the
   * column sees whichever scroller the table owns without this route knowing
   * anything about its markup.
   */
  const [scrolled, setScrolled] = createSignal(false);
  const [reading, setReading] = createSignal(false);
  const engaged = (): boolean => scrolled() || reading();

  let column: HTMLDivElement | undefined;
  /** The filter band. Inside the column, but never part of "the list". */
  let chrome: HTMLDivElement | undefined;

  onMount(() => {
    const el = column;
    if (el === undefined) return;
    const onScroll = (e: Event): void => {
      if (e.target instanceof HTMLElement) setScrolled(e.target.scrollTop > 0);
    };
    /**
     * Focus inside the toolbar is somebody *changing* the list, not somebody
     * reading it — and the two want opposite behaviour from the deferral. A
     * filter change is the operator's own act (`asked` below already lets those
     * through); typing in the search box while alerts arrive should not freeze
     * the rows underneath, because nothing is being read yet.
     */
    const inChrome = (node: EventTarget | Node | null): boolean =>
      node instanceof Node && chrome !== undefined && chrome.contains(node);
    const onFocusIn = (e: FocusEvent): void => {
      if (!inChrome(e.target)) setReading(true);
    };
    const onFocusOut = (e: FocusEvent): void => {
      const next = e.relatedTarget;
      setReading(next instanceof Node && el.contains(next) && !inChrome(next));
    };
    el.addEventListener("scroll", onScroll, true);
    el.addEventListener("focusin", onFocusIn);
    el.addEventListener("focusout", onFocusOut);
    onCleanup(() => {
      el.removeEventListener("scroll", onScroll, true);
      el.removeEventListener("focusin", onFocusIn);
      el.removeEventListener("focusout", onFocusOut);
    });
  });

  /**
   * The ids on screen, in order — a plain variable rather than a signal, on
   * purpose. The computation below both reads it and writes the held set, and a
   * reactive read of either would make it its own dependency and spin.
   */
  let onScreen: readonly string[] = [];
  let heldFingerprint = untrack(filterFingerprint);
  let heldPageCount = untrack(listFeed.pageCount);

  /**
   * Pure-phase, for the same reason `createKeysetFeed` is: what the table
   * renders must already be decided by the time it reads its rows, so a held
   * insert is never painted and then retracted.
   */
  createComputed(() => {
    const incoming = alerts();
    const fingerprint = filterFingerprint();
    const pageCount = listFeed.pageCount();
    const ids = incoming.map((a) => a.id);

    // A filter change and a "load more" are the operator's OWN actions — they
    // asked for a different list, so there is nothing to protect them from.
    const asked = fingerprint !== heldFingerprint || pageCount !== heldPageCount;
    heldFingerprint = fingerprint;
    heldPageCount = pageCount;

    if (asked || !untrack(engaged)) {
      onScreen = ids;
      setHeld(NOTHING_HELD);
      return;
    }

    // The anchor is the first row already on screen. Anything new *before* it
    // would push that row down; anything at or after it lands below the top of
    // what is being read and is spliced in as before.
    const visible = new Set(onScreen);
    const carried = untrack(held);
    const anchor = ids.findIndex((id) => visible.has(id));
    const next = new Set<string>();
    for (const [i, id] of ids.entries()) {
      if (carried.has(id)) next.add(id);
      else if (!visible.has(id) && anchor >= 0 && i < anchor) next.add(id);
    }

    onScreen = ids.filter((id) => !next.has(id));
    setHeld(next.size === 0 ? NOTHING_HELD : next);
  });

  const rows = createMemo<readonly Alert[]>(() => {
    const back = held();
    const all = alerts();
    return back.size === 0 ? all : all.filter((a) => !back.has(a.id));
  });

  const pending = (): number => held().size;

  /** Let the held rows in, wherever they sort to. The operator's own act. */
  const showHeld = (): void => {
    onScreen = untrack(alerts).map((a) => a.id);
    setHeld(NOTHING_HELD);
  };

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
    placeholderData: keepPrevious,
  }));

  const rulesById = createMemo<ReadonlyMap<string, RuleSnapshot>>(() => {
    const byId = new Map<string, RuleSnapshot>();
    for (const snapshot of rules.data ?? []) byId.set(snapshot.id, snapshot);
    return byId;
  });

  const hasMore = (): boolean => (axis() === null ? listFeed.hasMore() : bucketFeed.hasMore());

  const failed = (): boolean => (axis() === null ? query.isError : rollups.isError);
  const failure = (): unknown => (axis() === null ? query.error : rollups.error);
  const retry = (): void => {
    void (axis() === null ? query.refetch() : rollups.refetch());
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

      // `/` and `f` both land on `#alert-q` now that the search box and the
      // matcher box are one merged control — `f` used to target a second
      // `#alert-matchers` field that no longer exists.
      if (e.key === "/" || e.key === "f") {
        e.preventDefault();
        document.getElementById("alert-q")?.focus();
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
    // The count of what is ON SCREEN, not of what has been loaded — held rows
    // are counted by the chip beside it, and the two sum to the loaded total.
    const n = rows().length;
    if (query.isPending && n === 0) return "Loading…";
    return `${fmtCount(n)}${hasMore() ? "+" : ""} alert${n === 1 ? "" : "s"}`;
  };

  return (
    // ⛔ THE `min-h-0` / `overflow-hidden` CHAIN IS LOAD-BEARING. `AlertTable`
    // virtualises against its own scroller, which only has a bounded height
    // because every ancestor from `AppShell`'s `h-screen` down to here refuses
    // to grow with its content. This element is the whole screen now — there is
    // no side-by-side row above it any more — so it is the link in that chain,
    // and it must keep refusing to grow with its content.
    //
    // ⭐ NO HORIZONTAL PADDING OF ITS OWN — the shell's gutter is the only
    // horizontal gutter on the screen. Inner alignment is `px-md`, the same step
    // the table's cells use, so the strip below lines up with the first column.
    <div
      ref={(el) => {
        column = el;
      }}
      class="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden"
    >
      {/* The filters are a band above the table, not a rail beside it (ADR
          0033). They are instruments belonging to this list, so they live in
          this column, spanning exactly what they narrow.

          ⛔ THE `ref` IS NOT DECORATION — it is what keeps the held-alerts
          deferral below honest. That machine asks "does the operator have a
          position in this list worth protecting?", and answers it partly from
          focus. While the panel lived in the shell's rail, focus in a filter
          control was trivially not focus *in the list*; now it is focus inside
          this very column, and without the guard in `onFocusIn` below, tabbing
          into the Severity menu would start withholding incoming alerts. The
          chrome is marked here and excluded there. */}
      <div
        ref={(el) => {
          chrome = el;
        }}
        class="shrink-0"
      >
        <FilterBar
          filters={filters()}
          onChange={setFilters}
          onReset={() => setFilters(DEFAULT_FILTERS)}
          totalCountLabel={
            axis() === null ? `${fmtCount(rows().length)}${hasMore() ? "+" : ""}` : undefined
          }
          partialMatchEnabled={session.me()?.search?.partial_match_enabled}
        />

        {/* ⛔ THE TABS SIT BELOW THE FILTERS, NOT BESIDE THEM, AND THE ORDER IS
            THE ARGUMENT. The filters narrow both tabs identically — the Quiet
            badge is counted under the same filter set — so they scope the tabs
            rather than the other way round. Putting the tabs above would read as
            two independent screens that happen to share a toolbar. */}
        <div class="flex items-center border-b border-line px-md py-1.5">
          <AlertTabs tab={filters().tab} onChange={setTab} quiet={quiet()} />
        </div>
      </div>

      {/* ⛔ A STRIP, NOT A SECOND CHROME BAR. This was `h-14`, holding one 12 px
          string, which read as two stacked bars disagreeing about which one was
          the app's. It is `h-9` and belongs to the content column alone — the
          rail runs full height beside it — so it reads as the table's own
          status line.
          The polite region is the strip itself and is mounted unconditionally:
          both the count and the arrival of the held-alerts chip are mutations
          *inside* a region that already existed, which is the only kind a
          screen reader reliably speaks. */}
      <header
        class="flex h-9 shrink-0 items-center gap-md border-b border-line px-md"
        aria-live="polite"
      >
        <span class="text-body tabular-nums text-ink-muted">{status()}</span>

        {/* §0.5. Quiet by construction: Tier A only, no state hue and no
            accent, so it cannot out-shout a severity mark two rows below it
            (§0.6). It is a real button, so Enter and Space apply the held
            rows for free. */}
        <Show when={pending() > 0}>
          <Button
            variant="secondary"
            size="sm"
            onClick={showHeld}
            title="New alerts arrived while you were reading. They are held back so the list does not move under you."
            aria-label={`Show ${fmtCount(pending())} new alert${pending() === 1 ? "" : "s"}`}
          >
            {fmtCount(pending())} new
          </Button>
        </Show>

        {/* ⭐ WHERE THE ROW WENT. It is spoken inside the strip's existing polite
            region rather than as a toast, for the reason `AppShell` records: a
            message about what just happened to the screen must not be able to
            expire before it is read. It clears on the next tab change, and the
            button is the way back rather than a second copy of the tab. */}
        <Show when={wentQuiet()}>
          {(name) => (
            <span class="flex items-center gap-2 text-body text-ink-muted">
              <span>
                oto is now quiet about <span class="text-ink">{name()}</span>. It is still firing —
                its row moved to Quiet.
              </span>
              <Button variant="secondary" size="sm" onClick={() => setTab("quiet")}>
                Show me
              </Button>
            </span>
          )}
        </Show>
      </header>

      <Switch>
        {/* A blocked query is not an error and not an empty list — it is a
            filter oto refuses to approximate. It gets its own state.

            ⛔ AND IT DELIBERATELY GETS NO §M.9 MOTIF, THOUGH IT IS A FULL-PAGE
            EMPTY STATE. Kumo means stillness — nothing is wrong, and that is the
            point. Here something IS wrong and there is a fix two lines below;
            putting a serene cloud behind a refusal would be the ink saying the
            opposite of the sentence. */}
        <Match when={blocked()}>
          <EmptyState
            title="This filter cannot be served exactly, so nothing was requested."
            body="oto refuses to run a filter that would fall back to a sequential scan. Fix the matcher in the panel and the list will return — see the reasons under the input."
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
              <PageEmptyState
                motif="kumo"
                title={
                  isQuietTab(filters())
                    ? "Nothing is quiet, so there is nothing to group."
                    : "No alerts match these filters, so there is nothing to group."
                }
                body="The buckets are computed from the same filtered set as the list, on the same tab. An empty roll-up means an empty list, not a grouping problem."
                action={
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() =>
                      isQuietTab(filters()) ? setTab("active") : setFilters(DEFAULT_FILTERS)
                    }
                  >
                    {isQuietTab(filters()) ? "Back to alerts" : "Clear filters"}
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
                onLoadMore={bucketFeed.loadMore}
                onDrillDown={drillDown()}
              />
            </Match>
          </Switch>
        </Match>

        <Match when={query.isPending && rows().length === 0}>
          <div class="flex-1 overflow-hidden">
            <TableSkeleton rows={14} cols={6} />
          </div>
        </Match>

        {/* Three empty lists, three different facts about the world, and until
            §M.9 the middle one did not exist. `expired` is the one state whose
            meaning is transience — §M.1: it reads "oto stopped hearing about
            this", never "resolved" — and an empty list under that filter used to
            borrow the sentence a typo'd cluster name gets. The order matters:
            `isExpiredOnly` is strictly narrower than the fallback, so it has to
            be asked first. */}
        <Match when={rows().length === 0}>
          <Switch>
            {/* ⭐ FOUR EMPTY LISTS NOW, AND THE NEW ONE IS THE GOOD VERSION OF
                THIS SCREEN. "Nothing is quiet" is not "nothing matched" and is
                certainly not "nothing has fired" — it means oto is telling you
                about everything it knows, which is the state this tab exists to
                let somebody confirm. It is asked FIRST because it is true
                regardless of what else is filtering. */}
            <Match when={isQuietTab(filters())}>
              <PageEmptyState
                motif="kumo"
                title="oto is not holding anything back."
                body="Nothing matching this view is snoozed, so every alert oto knows about is on the main tab. A snooze always expires — at most thirty days — so this tab empties itself."
                action={
                  <Button variant="secondary" size="sm" onClick={() => setTab("active")}>
                    Back to alerts
                  </Button>
                }
              />
            </Match>
            <Match when={isUnfiltered(filters())}>
              <PageEmptyState
                motif="kumo"
                title="Nothing has fired yet."
                body="oto is listening. When your Alertmanager sends its first webhook, the alert and everything that happens to it afterwards will appear here."
              />
            </Match>
            <Match when={isExpiredOnly(filters())}>
              <PageEmptyState
                motif="sakura"
                title="Nothing has gone quiet."
                body="`expired` means oto stopped hearing about an alert — never that it resolved. No alert currently carries that state, which is the good version of this screen."
                action={
                  <Button variant="secondary" size="sm" onClick={() => setFilters(DEFAULT_FILTERS)}>
                    Clear filters
                  </Button>
                }
              />
            </Match>
            <Match when={true}>
              <PageEmptyState
                motif="kumo"
                title="No alerts match these filters."
                body="The filters are doing something — that is not the same as there being nothing here. Clear them to see everything oto has."
                action={
                  <Button variant="secondary" size="sm" onClick={() => setFilters(DEFAULT_FILTERS)}>
                    Clear filters
                  </Button>
                }
              />
            </Match>
          </Switch>
        </Match>

        <Match when={true}>
          <AlertTable
            alerts={rows()}
            quiet={isQuietTab(filters())}
            rules={rulesById()}
            rulesPending={rules.isFetching}
            onFilterLabel={onFilterLabel}
            onSnoozed={(a) => setWentQuiet(a.alertname)}
            footer={
              <Footer
                hasMore={hasMore()}
                loading={query.isFetching}
                loaded={alerts().length}
                pageCount={listFeed.pageCount()}
                pageSize={PAGE_SIZE}
                onLoadMore={listFeed.loadMore}
              />
            }
          />
        </Match>
      </Switch>

      {/* Rejected matchers are explained at the bottom too, where the empty
          state points, so the reason is never off-screen from its effect. */}
      <Show when={rejected().length > 0}>
        <ul class="shrink-0 border-t border-line bg-raised px-md py-sm text-meta leading-snug text-ink">
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
          <span class="text-meta text-ink-subtle">
            That is all {fmtCount(props.loaded)} of them.
          </span>
        }
      >
        <Button variant="secondary" size="sm" busy={props.loading} onClick={props.onLoadMore}>
          Load {fmtCount(props.pageSize)} more
        </Button>
        <span class="text-meta text-ink-subtle">
          {fmtCount(props.loaded)} loaded across {props.pageCount} page
          {props.pageCount === 1 ? "" : "s"}
        </span>
      </Show>
    </div>
  );
}
