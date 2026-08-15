/**
 * The keyset "load more" machine every paged feed in the app runs on.
 *
 * Pagination over a keyset cursor is **append-only**: "Load more" folds the
 * next page onto the stack rather than replacing it, because numbered pages
 * would be a lie — the list shifts underneath as rows arrive. The accumulated
 * stack is held in a signal rather than in the query cache: the cursor is part
 * of the query key, so every page is its own key there, and a cache-level
 * invalidation refetches only the key actually being read — which would leave
 * the rest of the stack silently stale. Held here, the stack resets
 * deliberately and visibly instead.
 *
 * The caller keeps its own `useQuery`: the feed hands out the `cursor` the
 * query key must carry and reads the resulting envelope back through the
 * `envelope` accessor. That loop is why the feed is created *before* the query
 * and closes over it — every accessor here is lazy, so nothing reads the query
 * until both exist.
 */
import { createComputed, createMemo, createSignal, type Accessor } from "solid-js";

/** The slice of the list envelope the machine reads. Structural, so any
 *  `ListEnvelope<T>` satisfies it without this module naming the contract. */
export interface KeysetEnvelope<T> {
  readonly data: readonly T[];
  readonly page: {
    readonly has_more: boolean;
    readonly next_cursor?: string | null;
  };
}

export interface KeysetFeedOptions<T> {
  /** The envelope the feed's query currently holds — `undefined` while cold. */
  readonly envelope: Accessor<KeysetEnvelope<T> | undefined>;
  /**
   * The query's `isPlaceholderData`, for queries using `keepPrevious`. A
   * placeholder envelope is the PREVIOUS key's page, still on screen while the
   * current one is in flight — so the cursor it carries was minted under the
   * query that key asked for. Paging off it is the fingerprint mismatch by a
   * longer route, and `loadMore` refuses it for the same reason.
   */
  readonly isPlaceholder?: Accessor<boolean>;
  /** Row identity for the dedupe fold — an id, or a roll-up bucket's key. */
  readonly keyOf: (row: T) => string;
  /**
   * The filter set the cursor is minted under, serialised — any change
   * discards the held position (see below). Omit only for a feed with no
   * filter axis at all, where nothing can invalidate a cursor.
   */
  readonly fingerprint?: Accessor<string>;
}

export interface KeysetFeed<T> {
  /** The cursor the query key must carry — `null` on page one. */
  readonly cursor: Accessor<string | null>;
  /** The visible rows: kept pages plus the page in flight, deduplicated. */
  readonly rows: Accessor<readonly T[]>;
  /** Straight off the envelope; `false` while the query is cold. */
  readonly hasMore: Accessor<boolean>;
  /** Pages folded in so far, for "N loaded across M pages" copy. */
  readonly pageCount: Accessor<number>;
  /** Freeze what is on screen, then advance the cursor. */
  readonly loadMore: () => void;
}

/**
 * Where one keyset stands, held as a single value stamped with the filters
 * that minted it.
 *
 * ⭐⭐ A CURSOR CANNOT OUTLIVE THE FILTER THAT MINTED IT. §E.3 answers a cursor
 * carried across a filter change with `400 cursor_filter_mismatch`, and
 * resetting it from an effect is too late to prevent that: solid-query reads
 * the query key in Solid's *pure* phase, so a request pairing the old cursor
 * with the new filters is built and sent before any effect gets to run. Nor is
 * a reset riding along in whatever `onChange` edits the filters enough on its
 * own: it covers one call site, and a filter set that lives in the URL is
 * edited from many — a filter bar, a label click, a drill-down, the back
 * button — only some of which pass through the component holding the feed.
 *
 * The stamp does the job from the other end: a held position whose fingerprint
 * is not the fingerprint on screen is a position in somebody else's keyset,
 * and `position` below hands back a first page instead of it. That is a
 * derivation, not a correction, so the doomed request is never constructed —
 * and the stamped position is then *dropped*, because a merely shadowed one
 * would come back the moment the fingerprint it was minted under is on screen
 * again.
 */
interface Held<T> {
  /** What the position was minted under — `""` for a feed with no filters. */
  readonly fingerprint: string;
  readonly cursor: string | null;
  /** Pages already folded in. The page in flight is added by `rows`. */
  readonly kept: readonly T[];
  readonly pageCount: number;
}

const fresh = <T,>(fingerprint: string): Held<T> => ({
  fingerprint,
  cursor: null,
  kept: [],
  pageCount: 1,
});

export function createKeysetFeed<T>(options: KeysetFeedOptions<T>): KeysetFeed<T> {
  const fingerprint = (): string => options.fingerprint?.() ?? "";

  const [held, setHeld] = createSignal<Held<T>>(fresh<T>(fingerprint()));

  /**
   * The position in force, which is the held one only while it still belongs
   * to what is on screen. @see Held — being a *derivation* rather than a reset
   * is the whole point: it has already happened by the time solid-query reads
   * the query key.
   */
  const position = createMemo<Held<T>>(() => {
    const h = held();
    const fp = fingerprint();
    return h.fingerprint === fp ? h : fresh<T>(fp);
  });

  /**
   * ⭐⭐ AND A POSITION THAT LOST ITS KEYSET IS DISCARDED, NOT MERELY SHADOWED.
   *
   * The memo above is what stops the doomed request being built. On its own it
   * only *hides* the foreign position, and hiding is not enough when the old
   * filter set can return — filter B and then Back makes the old fingerprint
   * the fingerprint on screen again, and a hidden page-3 stack would come back
   * with it. What returns is a minutes-old snapshot: a live invalidation
   * refetches only the key actually being read, so the kept rows are frozen as
   * they were — and once their key has passed `gcTime` the fold below would
   * splice them onto whatever page one is on screen.
   *
   * So the derived position is written back over the held one, which leaves
   * the signal holding only ever the keyset on screen. `createComputed` is
   * Solid's pure-phase computation — the one place a write like this belongs,
   * and for the same reason the reset could not be an effect. It writes the
   * very object the memo derived, so when the two already agree the write is a
   * no-op by reference equality and nothing downstream sees an update.
   */
  createComputed(() => setHeld(position()));

  /**
   * The visible rows are a pure function of the kept pages plus the page in
   * flight, deduplicated by `keyOf` — a live refetch of page one legitimately
   * overlaps what is already held, and appending it twice would show ghosts.
   *
   * A plain derivation, not a memo, and it cannot be one: a memo computes
   * eagerly at creation, and the feed is created before the query it reads —
   * so that first run would land inside the temporal dead zone of the caller's
   * `const query`. Worse than the throw, the run would track nothing of the
   * envelope, so the memo would never learn page one had arrived. Read lazily,
   * both sides of the loop exist by the time anything asks.
   */
  const rows = (): readonly T[] => {
    const h = position();
    const page = options.envelope()?.data ?? [];
    if (h.cursor === null) return page;
    const seen = new Set(h.kept.map(options.keyOf));
    return [...h.kept, ...page.filter((row) => !seen.has(options.keyOf(row)))];
  };

  const loadMore = (): void => {
    const next = options.envelope()?.page.next_cursor;
    if (typeof next !== "string" || next === "" || options.isPlaceholder?.() === true) return;
    // Freeze what is on screen before asking for the next page, so the fold is
    // additive rather than a race between two in-flight responses.
    const frozen = rows();
    const h = position();
    setHeld({ ...h, cursor: next, kept: frozen, pageCount: h.pageCount + 1 });
  };

  return {
    cursor: () => position().cursor,
    rows,
    hasMore: () => options.envelope()?.page.has_more ?? false,
    pageCount: () => position().pageCount,
    loadMore,
  };
}

/**
 * The `placeholderData` keep-alive a paged feed's query wants.
 *
 * The cursor is part of the key, so every page is a *cold* key: without a
 * placeholder, `data` is `undefined` for the whole in-flight page — which
 * blanks the rows out from under a filter change, and unmounts the "load more"
 * button under the click that pressed it, dropping focus to `<body>`. Pair it
 * with `isPlaceholder` on the feed, which is what stops `loadMore` paging off
 * the previous key's page.
 */
export const keepPrevious = <T,>(previous: T | undefined): T | undefined => previous;
