/**
 * The keyset feed machine, exercised bare — no query, no screen.
 *
 * The §E.3 case is the one that pays for the file: the reset must be observable
 * **synchronously**, on the same read solid-query makes of the query key,
 * because a reset that lands a tick later has already paired a cursor with
 * filters it was not minted under. `routes/alerts.test.tsx` proves that end to
 * end on the wire; these cases pin the machine itself, so every feed built on
 * it inherits the property without each screen re-proving it. Hence the shape
 * below: signal writes and immediate reads, never an `await`.
 */
import { createRoot, createSignal } from "solid-js";
import { describe, expect, it } from "vitest";

import { createKeysetFeed, keepPrevious, type KeysetEnvelope } from "./keysetFeed";

interface Row {
  readonly id: string;
}

const page = (ids: readonly string[], next: string | null = null): KeysetEnvelope<Row> => ({
  data: ids.map((id) => ({ id })),
  page: { has_more: next !== null, next_cursor: next },
});

const ids = (rows: readonly Row[]): readonly string[] => rows.map((r) => r.id);

/** A feed over hand-cranked signals, standing in for the query it would read. */
function mount() {
  return createRoot((dispose) => {
    const [envelope, setEnvelope] = createSignal<KeysetEnvelope<Row> | undefined>(undefined);
    const [placeholder, setPlaceholder] = createSignal(false);
    const [fingerprint, setFingerprint] = createSignal("state=firing");
    const feed = createKeysetFeed<Row>({
      envelope,
      isPlaceholder: placeholder,
      keyOf: (r) => r.id,
      fingerprint,
    });
    return { feed, setEnvelope, setPlaceholder, setFingerprint, dispose };
  });
}

describe("the fold", () => {
  it("is the bare page before any paging, and nothing while the query is cold", () => {
    const w = mount();
    expect(w.feed.rows()).toEqual([]);
    expect(w.feed.hasMore()).toBe(false);
    expect(w.feed.cursor()).toBeNull();

    w.setEnvelope(page(["a1", "a2"], "c2"));
    expect(ids(w.feed.rows())).toEqual(["a1", "a2"]);
    expect(w.feed.hasMore()).toBe(true);
    // Reading is not paging: the cursor the envelope offers is not taken up
    // until `loadMore` asks for it.
    expect(w.feed.cursor()).toBeNull();
    w.dispose();
  });

  it("folds the next page onto the frozen one, deduplicated by key", () => {
    const w = mount();
    w.setEnvelope(page(["a1", "a2"], "c2"));
    w.feed.loadMore();

    // Page two overlaps page one — a live refetch legitimately does — and the
    // overlap must not render twice.
    w.setEnvelope(page(["a2", "a3"], null));
    expect(ids(w.feed.rows())).toEqual(["a1", "a2", "a3"]);
    expect(w.feed.hasMore()).toBe(false);
    expect(w.feed.pageCount()).toBe(2);
    w.dispose();
  });

  it("keeps the frozen rows on screen while the next page is in flight", () => {
    const w = mount();
    w.setEnvelope(page(["a1", "a2"], "c2"));
    w.feed.loadMore();

    // `keepPrevious` leaves the previous envelope in place, flagged as a
    // placeholder, until the cold key answers. The fold must not double it.
    w.setPlaceholder(true);
    expect(ids(w.feed.rows())).toEqual(["a1", "a2"]);
    w.dispose();
  });
});

describe("loadMore", () => {
  it("freezes what is on screen, then advances the cursor", () => {
    const w = mount();
    w.setEnvelope(page(["a1"], "c2"));
    w.feed.loadMore();
    expect(w.feed.cursor()).toBe("c2");
    expect(w.feed.pageCount()).toBe(2);
    w.dispose();
  });

  it("refuses without a next cursor", () => {
    const w = mount();
    w.setEnvelope(page(["a1"], null));
    w.feed.loadMore();
    expect(w.feed.cursor()).toBeNull();
    expect(w.feed.pageCount()).toBe(1);
    w.dispose();
  });

  it("refuses to page off a placeholder, whose cursor belongs to another key", () => {
    const w = mount();
    w.setEnvelope(page(["a1"], "c2"));
    w.setPlaceholder(true);
    w.feed.loadMore();
    expect(w.feed.cursor()).toBeNull();
    expect(w.feed.pageCount()).toBe(1);

    // Only the real page — the flag dropped — is a position worth advancing from.
    w.setPlaceholder(false);
    w.feed.loadMore();
    expect(w.feed.cursor()).toBe("c2");
    expect(w.feed.pageCount()).toBe(2);
    w.dispose();
  });
});

describe("the fingerprint reset (§E.3)", () => {
  it("discards the cursor in the same synchronous read as the filter change", () => {
    const w = mount();
    w.setEnvelope(page(["a1", "a2"], "c2"));
    w.feed.loadMore();
    expect(w.feed.cursor()).toBe("c2");

    // No `await` between the write and the reads: this is the read solid-query
    // makes of the query key in the pure phase, and it must already see page
    // one — a reset that needs a tick has already sent the doomed request.
    w.setFingerprint("state=resolved");
    expect(w.feed.cursor()).toBeNull();
    expect(w.feed.pageCount()).toBe(1);

    // The kept rows go with the cursor: they are the old keyset's pages, and
    // splicing them under the new filter's page one would show a list no
    // filter ever produced.
    w.setEnvelope(page(["b1"], null));
    expect(ids(w.feed.rows())).toEqual(["b1"]);
    w.dispose();
  });

  it("has discarded the position, not shadowed it: the old fingerprint returns to page one", () => {
    const w = mount();
    w.setEnvelope(page(["a1", "a2"], "c2"));
    w.feed.loadMore();

    // Filter away and back — the Back button's shape. A merely shadowed
    // position would resurrect the stale page-2 stack here.
    w.setFingerprint("state=resolved");
    w.setFingerprint("state=firing");
    expect(w.feed.cursor()).toBeNull();
    expect(w.feed.pageCount()).toBe(1);
    expect(ids(w.feed.rows())).toEqual(["a1", "a2"]);
    w.dispose();
  });
});

describe("keepPrevious", () => {
  it("hands back exactly what was on screen — the keep-alive is identity", () => {
    const previous = page(["a1"], "c2");
    expect(keepPrevious(previous)).toBe(previous);
    expect(keepPrevious(undefined)).toBeUndefined();
  });
});
