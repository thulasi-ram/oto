/**
 * The reducer: what a frame does to the cache.
 *
 * The policy under test is deliberately conservative — a frame is a **change
 * notice, not a resource**, so oto invalidates a prefix and lets solid-query
 * refetch rather than patching a cached row from a partial payload. These tests
 * pin the two halves of that:
 *
 *   1. Every kind the contract publishes lands on the right prefix — and on a
 *      prefix some query actually owns. A `delivery.updated` that only
 *      invalidated `["deliveries"]` would leave the alert's own delivery summary
 *      stale on screen, which is the class of drift nobody notices until an
 *      incident; `api/queries.test.tsx` is where the other half of that claim
 *      lives, that no key is left with nothing to refresh it.
 *   2. A kind this build does not know, and a frame about an entity nothing on
 *      screen holds, are both **non-events** rather than errors — and neither
 *      tears the provider down.
 */
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { render } from "@solidjs/testing-library";
import { afterEach, describe, expect, it, vi } from "vitest";

import { LiveProvider, describeConnection } from "./live";
import type { ConnectionState, StreamDetail } from "./stream";
import { enumValues } from "~/test/contract";
import { frame, sse } from "~/test/fixtures";
import { until } from "~/test/harness";

/* -------------------------------------------------------------------------- */

interface Wire {
  readonly push: (text: string) => void;
}

function socket(): { readonly wire: () => Promise<Wire> } {
  const wires: Wire[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(() => {
      const encoder = new TextEncoder();
      let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
      const body = new ReadableStream<Uint8Array>({
        start(c) {
          controller = c;
        },
      });
      wires.push({ push: (t) => controller?.enqueue(encoder.encode(t)) });
      return Promise.resolve({ ok: true, status: 200, body });
    }),
  );
  return {
    wire: async () => {
      await until(() => expect(wires.length).toBeGreaterThanOrEqual(1));
      return wires[0]!;
    },
  };
}

interface Mounted {
  readonly push: (text: string) => Promise<void>;
  readonly keys: () => readonly (readonly unknown[])[];
  readonly invalidatedEverything: () => boolean;
  readonly unmount: () => void;
}

async function mountLive(): Promise<Mounted> {
  const net = socket();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  const keys: (readonly unknown[])[] = [];
  let everything = false;
  vi.spyOn(client, "invalidateQueries").mockImplementation((filters?: unknown) => {
    const key = (filters as { queryKey?: readonly unknown[] } | undefined)?.queryKey;
    if (key === undefined) everything = true;
    else keys.push(key);
    return Promise.resolve();
  });

  const result = render(() => (
    <QueryClientProvider client={client}>
      <LiveProvider>
        <p>mounted</p>
      </LiveProvider>
    </QueryClientProvider>
  ));

  const wire = await net.wire();
  return {
    push: async (text) => {
      wire.push(text);
      await new Promise((r) => setTimeout(r, 10));
    },
    keys: () => keys,
    invalidatedEverything: () => everything,
    unmount: () => result.unmount(),
  };
}

let mounted: Mounted | null = null;
afterEach(() => {
  mounted?.unmount();
  mounted = null;
});

/* -------------------------------------------------------------------------- */

describe("frames become invalidations", () => {
  const cases: readonly { kind: string; expects: readonly (readonly string[])[] }[] = [
    // `["notifications"]` rides along with the three frames that can mint or
    // move an intent (ADR 0034). A SUPPRESSED intent leaves no delivery behind,
    // so `delivery.updated` alone would never announce the rows the activity log
    // exists for; the log absorbs the rate at the cache entry instead
    // (`notificationActivityQuery`), not by being told about less.
    { kind: "alert.upserted", expects: [["alerts"], ["notifications"]] },
    // ⛔ `["cases"]` FIRST, AND ITS ABSENCE WOULD BE INVISIBLE. `/cases` is the
    // primary list and is keyed under its own prefix — a case opening, being
    // acknowledged or ending is exactly what moves a row in and out of it, and an
    // `["alerts"]` invalidation does not reach it.
    { kind: "case.upserted", expects: [["cases"], ["alerts"]] },
    // ⛔ NOTHING, AND THE EMPTY EXPECTATION IS THE ASSERTION. The AlertGroup is
    // gone (git-bug 7570090) and no query is keyed under `["groups"]` any more,
    // so the one honest thing this frame can do is nothing. It still has a case
    // of its own in the reducer, because the kind is still in the contract enum
    // and the coverage test below is what would otherwise stop noticing it.
    { kind: "group.upserted", expects: [] },
    { kind: "event.appended", expects: [["cases"], ["alerts"], ["notifications"]] },
    // `["alerts"]` because a delivery is read as part of an alert
    // (`qk.alerts.notifications`); `["notifications"]` because a delivery moving
    // is what changes an intent's status. The `["deliveries"]` prefix this used
    // to invalidate as well owned no query at all.
    { kind: "delivery.updated", expects: [["alerts"], ["notifications"]] },
    { kind: "source.health", expects: [["settings", "sources"]] },
  ];

  for (const c of cases) {
    it(`${c.kind} invalidates ${c.expects.map((k) => k.join("/")).join(" and ")}`, async () => {
      mounted = await mountLive();
      await mounted.push(sse(frame(1, c.kind, { id: "does-not-matter" })));
      expect(mounted.keys()).toEqual(c.expects);
    });
  }

  it("covers every kind the contract publishes", async () => {
    // Derived from the contract so a new `UiEventKind` cannot be added
    // server-side and quietly land in the reducer's `default:` branch, where it
    // would update nothing and look exactly like a working UI.
    const handled = new Set([...cases.map((c) => c.kind), "resync"]);
    for (const kind of enumValues("UiEventKind")) {
      expect(handled.has(kind), `\`${kind}\` has no case in the live reducer's tests`).toBe(true);
    }
  });

  it("refetches everything on a resync, because incremental state is untrustworthy", async () => {
    mounted = await mountLive();
    await mounted.push(sse(frame(2, "resync", { reason: "buffer_overflow" })));
    expect(mounted.invalidatedEverything()).toBe(true);
  });

  it("does nothing at all for a kind this build does not know", async () => {
    mounted = await mountLive();
    await mounted.push(sse(frame(3, "alert.frobnicated", { id: "a1" })));
    expect(mounted.keys()).toEqual([]);
    expect(mounted.invalidatedEverything()).toBe(false);
  });

  it("survives a frame about an entity nothing on screen holds", async () => {
    // Prefix invalidation is exactly why this is a non-event: there is no row to
    // patch and no row to fail to find. The assertion is that it still fires and
    // still does not throw.
    mounted = await mountLive();
    await mounted.push(sse(frame(4, "alert.upserted", { id: "an-alert-nobody-fetched" })));
    expect(mounted.keys()).toEqual([["alerts"], ["notifications"]]);
  });

  it("keeps working after an unreadable frame and an unknown kind in the same batch", async () => {
    mounted = await mountLive();
    await mounted.push("id: 5\nevent: alert.upserted\ndata: {broken\n\n");
    await mounted.push(sse(frame(6, "alert.frobnicated"), frame(7, "case.upserted")));
    expect(mounted.keys()).toEqual([["cases"], ["alerts"]]);
  });
});

describe("describeConnection", () => {
  const detail = (patch: Partial<StreamDetail> = {}): StreamDetail => ({
    lastSeq: null,
    retryAt: null,
    attempt: 0,
    resyncReason: null,
    lastMessageAt: null,
    ...patch,
  });

  it("has a sentence for every state, and never renders undefined", () => {
    const states: readonly ConnectionState[] = [
      "idle",
      "connecting",
      "live",
      "reconnecting",
      "offline",
    ];
    for (const s of states) {
      const text = describeConnection(s, detail());
      expect(text, `no copy for connection state \`${s}\``).toBeTruthy();
      expect(text).not.toMatch(/undefined/);
    }
  });

  it("only says `live` when frames are actually arriving", () => {
    expect(describeConnection("live", detail())).toMatch(/^Live/);
    // Every other state names what is wrong AND warns the view may be stale.
    expect(describeConnection("offline", detail())).toMatch(/snapshot, not a live view/);
    expect(describeConnection("reconnecting", detail({ retryAt: Date.now() + 4200 }))).toMatch(
      /retrying in 5s\. What you see may be out of date\./,
    );
  });

  it("never counts down past zero", () => {
    expect(describeConnection("reconnecting", detail({ retryAt: Date.now() - 60_000 }))).toContain(
      "retrying in 0s",
    );
  });
});
