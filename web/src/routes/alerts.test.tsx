/**
 * The alert list's one promise about page position, and the only way to observe
 * it: **a cursor is never put on the wire with filters it was not minted under.**
 *
 * §E.3 binds a keyset cursor to the filter set that produced it and answers a
 * mismatched one with `400 cursor_filter_mismatch`, so a filter change on a
 * paged list is exactly where that guarantee gets broken. It used to be broken
 * here: the cursor was reset from a `createEffect`, and solid-query builds and
 * sends the request from a `createComputed` — Solid's *pure* phase — which runs
 * first. The correction then landed before the browser painted, so nothing was
 * ever visible on screen and nothing but the request log could catch it.
 *
 * Hence the shape of every case below. They assert on what was *sent*, not on
 * what was rendered: page once so a real cursor exists, change the thing that
 * invalidates it, then read the requests made afterwards. A screen assertion
 * would have passed against the old code.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import AlertsRoute from "./alerts";
import { SessionProvider } from "~/api/session";
import type { AlertRollup } from "~/api/types";
import { count as fmtCount } from "~/lib/format";
import { alert } from "~/test/fixtures";
import {
  item,
  list,
  renderScreen,
  stubFetch,
  unpaged,
  until,
  type FetchStub,
  type RecordedCall,
} from "~/test/harness";

/** What page one hands out. Named for what a request carrying it later is. */
const STALE_CURSOR = "cursor-minted-under-the-previous-filters";
const STALE_BUCKET_CURSOR = "bucket-cursor-minted-under-the-previous-axis";

function bucket(patch: Partial<AlertRollup> = {}): AlertRollup {
  return {
    key: "HighErrorRate",
    group_by: "alertname",
    state: "firing",
    total_count: 4,
    firing_count: 4,
    suppressed_count: 0,
    resolved_count: 0,
    expired_count: 0,
    acked_count: 0,
    unacked_count: 4,
    flapping_count: 0,
    snoozed_count: 0,
    severity_counts: { critical: 4 },
    first_seen_at: "2026-08-09T09:00:00.000Z",
    last_seen_at: "2026-08-09T09:00:00.000Z",
    ...patch,
  };
}

/**
 * Render the route over a keyset that always has one more page: whatever is
 * asked for, the answer offers a cursor, so "load more" is always available and
 * a request carrying a cursor is always distinguishable from page one.
 */
/**
 * A minimal principal, just enough for `AlertsRoute`'s `useSession()` read
 * (the search-capability note in `FilterBar`) to resolve to something rather
 * than throw outside a provider. Not the shell's concern here — that is
 * `App.test.tsx`'s `ME` — so this stays as small as the route actually reads.
 */
const ME = item({
  principal_kind: "user",
  user: { id: "11111111-1111-4111-8111-111111111111", email: "operator@example.test" },
  org: { id: "22222222-2222-4222-8222-222222222222", slug: "acme", name: "Acme", settings: {} },
  search: { partial_match_enabled: false },
});

function mount(path: string): FetchStub {
  const http = stubFetch({
    "GET /api/v1/me": ME,
    "GET /api/v1/clusters": list([]),
    "GET /api/v1/labels": unpaged([]),
    "GET /api/v1/alerts": (call: RecordedCall) =>
      call.search.get("cursor") === null
        ? { json: list([alert()], { has_more: true, next_cursor: STALE_CURSOR }) }
        : {
            json: list([alert({ id: "3f2a1b0c-4d5e-4f60-8a9b-0c1d2e3f4a5b" })], {
              has_more: true,
              next_cursor: "cursor-page-three",
            }),
          },
    "GET /api/v1/alerts/rollups": (call: RecordedCall) =>
      call.search.get("cursor") === null
        ? { json: list([bucket()], { has_more: true, next_cursor: STALE_BUCKET_CURSOR }) }
        : {
            json: list([bucket({ key: "KubePodCrashLooping" })], {
              has_more: true,
              next_cursor: "bucket-cursor-page-three",
            }),
          },
  });
  renderScreen(() => (
    <SessionProvider>
      <AlertsRoute />
    </SessionProvider>
  ), { path });
  return http;
}

const listCalls = (http: FetchStub): readonly RecordedCall[] =>
  http.calls.filter((c) => c.path === "/api/v1/alerts");
const rollupCalls = (http: FetchStub): readonly RecordedCall[] =>
  http.calls.filter((c) => c.path === "/api/v1/alerts/rollups");

/**
 * The flat list's load-more control names the page size it will add, so the name
 * is read back off the wire rather than written down here: the `limit` the first
 * request carried *is* the page size, and a change to it moves the button's name
 * and this expectation together instead of leaving a hard-coded 100 to rot.
 */
async function loadMoreName(http: FetchStub): Promise<RegExp> {
  await until(() => expect(listCalls(http).length).toBeGreaterThan(0));
  const limit = Number(listCalls(http)[0]!.search.get("limit"));
  return new RegExp(`^Load ${fmtCount(limit)} more$`);
}

/**
 * Page once, and answer with the number of requests made by the time the cursor
 * it minted was actually on the wire — the mark every later request is judged
 * against.
 */
async function pageOnce(
  http: FetchStub,
  button: RegExp,
  calls: (http: FetchStub) => readonly RecordedCall[],
  cursor: string,
): Promise<number> {
  await until(() => expect(screen.getByRole("button", { name: button })).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: button }));
  await until(() => {
    expect(calls(http).map((c) => c.search.get("cursor"))).toContain(cursor);
  });
  return calls(http).length;
}

/**
 * Open one of the filter toolbar's menus (ADR 0033 — the controls moved out of
 * the shell's rail and four axes now live behind a trigger). Anchored on the
 * axis name because a trigger's accessible name grows to include its value.
 */
async function openMenu(axis: string): Promise<void> {
  fireEvent.click(screen.getByRole("button", { name: new RegExp(`^${axis}`) }));
  await until(() => expect(screen.getByRole("dialog")).toBeTruthy());
}

/** Toggle the first lifecycle state — the smallest possible filter change. */
async function changeAFilter(): Promise<void> {
  await openMenu("Status");
  const states = await screen.findByRole("group", { name: "Lifecycle state" });
  fireEvent.click(within(states).getAllByRole("button")[0]!);
}

describe("changing the filters while a page is loaded", () => {
  it("issues no request carrying the cursor the previous filters minted", async () => {
    const http = mount("/alerts");
    const mark = await pageOnce(http, await loadMoreName(http), listCalls, STALE_CURSOR);

    await changeAFilter();

    // The new filter set is asked for, so the reset did not cost the query.
    await until(() => {
      expect(listCalls(http).slice(mark).some((c) => c.search.get("state") !== null)).toBe(true);
    });

    // And nothing sent since the change carries a cursor at all: not the doomed
    // pairing the server is obliged to refuse, and not a corrected request
    // issued after it either. The first request under the new filters is page
    // one, because the position was discarded before the key was read.
    for (const call of listCalls(http).slice(mark)) {
      expect(call.search.get("cursor")).toBeNull();
    }
  });
});

describe("changing the grouping while a bucket page is loaded", () => {
  it("issues no roll-up request carrying the cursor the previous axis minted", async () => {
    const http = mount("/alerts?group=alertname");
    const mark = await pageOnce(http, /^Load more buckets$/, rollupCalls, STALE_BUCKET_CURSOR);

    // A bucket cursor is a bucket key, so regrouping invalidates it twice over:
    // the keys it orders do not exist under the new axis.
    await openMenu("Group");
    fireEvent.click(screen.getByRole("tab", { name: "By namespace" }));

    await until(() => {
      expect(
        rollupCalls(http).slice(mark).some((c) => c.search.get("group_by") === "namespace"),
      ).toBe(true);
    });
    for (const call of rollupCalls(http).slice(mark)) {
      expect(call.search.get("cursor")).toBeNull();
    }
  });

  it("issues no roll-up request carrying the cursor the previous filters minted", async () => {
    // The roll-up cursor is bound to the filter set as well as to the axis, and
    // the filters are the half that changes without the grouping control.
    const http = mount("/alerts?group=alertname");
    const mark = await pageOnce(http, /^Load more buckets$/, rollupCalls, STALE_BUCKET_CURSOR);

    await changeAFilter();

    await until(() => {
      expect(rollupCalls(http).slice(mark).some((c) => c.search.get("state") !== null)).toBe(true);
    });
    for (const call of rollupCalls(http).slice(mark)) {
      expect(call.search.get("cursor")).toBeNull();
    }
  });
});

describe("paging itself", () => {
  it("folds the next page onto the one on screen rather than replacing it", async () => {
    // The guard above is only worth having if paging still works — a position
    // discarded too eagerly reads exactly like a "load more" that does nothing.
    const http = mount("/alerts");
    await pageOnce(http, await loadMoreName(http), listCalls, STALE_CURSOR);

    // Page two carries a different id from page one, so "2 loaded" can only be
    // the two of them together: a replace would say 1, and a double-append would
    // say 3. The page count comes from the same held position the fold does.
    await until(() => {
      expect(document.body.textContent).toMatch(/2 loaded across 2 pages/);
    });
  });
});
