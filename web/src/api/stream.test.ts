/**
 * The live feed, and the four ways it is normally got wrong.
 *
 * The contract's headline promise is durable resume: reconnect with
 * `Last-Event-ID: N` and the server replays everything after `N`. That promise
 * is only as good as the client's bookkeeping, so these tests are mostly about
 * the *unhappy* wire:
 *
 *   - **Reconnect** must carry the highest `seq` seen, and must carry a resume
 *     point that survived a reload (`sessionStorage`) on the very first connect.
 *   - **Out of order** must never rewind the resume point. `seq` is strictly
 *     monotonic per the contract, but a client that trusts the last frame it saw
 *     rather than the highest would ask the server to replay a window it has
 *     already consumed — and a duplicated timeline is indistinguishable from a
 *     duplicated event.
 *   - **Duplicates** must still be delivered. Deduplicating here would be a
 *     cache-invalidation policy hidden in a transport, and the consumer's job
 *     (invalidate, refetch) is idempotent anyway.
 *   - **An unknown kind** must be delivered and survive. Forward compatibility is
 *     a promise the server relies on when it ships a new event type.
 *
 * ⛔ AND NOTHING IS EVER SILENTLY DROPPED. Every assertion below counts frames
 * reaching the handler, because "the reducer swallowed it" and "the server never
 * sent it" look identical on screen and only one of them is a bug oto can fix.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import { AlertStream, isKnownKind, type ConnectionState, type StreamDetail } from "./stream";
import type { StreamFrame } from "./types";
import { enumValues } from "~/test/contract";
import { frame, sse } from "~/test/fixtures";
import { until } from "~/test/harness";

/* -------------------------------------------------------------------------- */
/* A wire you can drive by hand                                               */
/* -------------------------------------------------------------------------- */

interface Wire {
  /** Bytes onto the socket, exactly as written. */
  readonly push: (text: string) => void;
  /** A clean end of stream — which is still a disconnection. */
  readonly end: () => void;
}

interface Socket {
  /** Every request the stream made, newest last. */
  readonly requests: { url: string; headers: Record<string, string> }[];
  /** Resolves once the nth connection has been established. */
  readonly wire: (n?: number) => Promise<Wire>;
}

/**
 * Stand in for the SSE endpoint.
 *
 * `AlertStream` only ever touches `res.ok` and `res.body`, and `res.body` must be
 * a real web `ReadableStream` because the transport pipes it through a
 * `TextDecoderStream`. Everything else about `Response` is irrelevant here and
 * faking it would only obscure what is under test.
 */
function socket(options: { readonly failFirst?: number } = {}): Socket {
  const requests: { url: string; headers: Record<string, string> }[] = [];
  const wires: Wire[] = [];
  let failuresLeft = options.failFirst ?? 0;

  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      requests.push({
        url: typeof input === "string" ? input : String(input),
        headers: { ...((init?.headers ?? {}) as Record<string, string>) },
      });

      if (failuresLeft > 0) {
        failuresLeft -= 1;
        return Promise.resolve({ ok: false, status: 503, body: null });
      }

      const encoder = new TextEncoder();
      let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
      const body = new ReadableStream<Uint8Array>({
        start(c) {
          controller = c;
        },
      });

      wires.push({
        push: (text) => controller?.enqueue(encoder.encode(text)),
        end: () => {
          try {
            controller?.close();
          } catch {
            /* already closed */
          }
        },
      });

      return Promise.resolve({ ok: true, status: 200, body });
    }),
  );

  return {
    requests,
    wire: async (n = 1) => {
      await until(() => expect(wires.length).toBeGreaterThanOrEqual(n));
      return wires[n - 1]!;
    },
  };
}

/** Start a stream and collect everything it emits. */
function listen(stream: AlertStream): {
  readonly frames: StreamFrame[];
  readonly states: ConnectionState[];
  readonly detail: () => StreamDetail;
} {
  const frames: StreamFrame[] = [];
  const states: ConnectionState[] = [];
  stream.onFrame((f) => frames.push(f));
  stream.onState((s) => states.push(s));
  stream.start();
  return { frames, states, detail: () => stream.detail };
}

let live: AlertStream | null = null;

function open(): AlertStream {
  live = new AlertStream({ resources: ["alerts"] });
  return live;
}

afterEach(() => {
  live?.close();
  live = null;
});

/* -------------------------------------------------------------------------- */

describe("the frame vocabulary", () => {
  it("understands exactly the kinds the contract publishes, and no more", () => {
    // Derived, never re-typed: a `UiEventKind` added server-side must show up
    // here as a failure, because a kind this build does not know is a change
    // notice this build will not act on.
    for (const kind of enumValues("UiEventKind")) {
      expect(isKnownKind(kind), `contract kind \`${kind}\` is unknown to the client`).toBe(true);
    }
    expect(isKnownKind("alert.frobnicated")).toBe(false);
  });
});

describe("resume", () => {
  it("asks for nothing on a first-ever connect", async () => {
    const net = socket();
    const stream = open();
    listen(stream);
    await net.wire();

    expect(net.requests[0]?.headers["Last-Event-ID"]).toBeUndefined();
    expect(net.requests[0]?.url).toContain("resources=alerts");
  });

  it("resumes from the highest seq it has seen, not the last one it saw", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);

    const w = await net.wire();
    // Deliberately out of order. `seq` is monotonic on the wire, but a client
    // that trusts "the last frame" would rewind its resume point to 3 and ask
    // the server to replay 4 and 5 all over again.
    w.push(sse(frame(5, "alert.upserted"), frame(3, "alert.upserted")));
    await until(() => expect(tap.frames.length).toBe(2));

    // Neither was dropped — that is the whole point.
    expect(tap.frames.map((f) => (f as unknown as { seq: number }).seq)).toEqual([5, 3]);
    expect(tap.detail().lastSeq).toBe(5);

    stream.reconnectNow();
    await net.wire(2);
    expect(net.requests[1]?.headers["Last-Event-ID"]).toBe("5");
  });

  it("delivers a duplicate rather than swallowing it", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);

    const w = await net.wire();
    w.push(sse(frame(9, "event.appended"), frame(9, "event.appended")));
    await until(() => expect(tap.frames.length).toBe(2));

    // A transport that deduplicated would be making a cache policy decision
    // behind the consumer's back. Invalidation is idempotent; silence is not.
    expect(tap.detail().lastSeq).toBe(9);
  });

  it("picks a reload's resume point out of sessionStorage on the first connect", async () => {
    sessionStorage.setItem("oto.stream.seq", "412");
    const net = socket();
    const stream = open();
    const tap = listen(stream);
    await net.wire();

    expect(net.requests[0]?.headers["Last-Event-ID"]).toBe("412");
    // And it says "reconnecting", not "connecting": this page already had state,
    // so what is on screen may be out of date until the replay lands.
    expect(tap.states).toContain("reconnecting");
  });

  it("ignores a corrupt resume point instead of sending nonsense upstream", async () => {
    sessionStorage.setItem("oto.stream.seq", "not-a-number");
    const net = socket();
    listen(open());
    await net.wire();
    expect(net.requests[0]?.headers["Last-Event-ID"]).toBeUndefined();
  });

  it("persists the resume point so a reload does not start from now", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);
    (await net.wire()).push(sse(frame(77, "group.upserted")));
    await until(() => expect(tap.frames.length).toBe(1));
    expect(sessionStorage.getItem("oto.stream.seq")).toBe("77");
  });
});

describe("what arrives on the wire", () => {
  it("delivers a kind this build has never heard of", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);

    (await net.wire()).push(sse(frame(1, "alert.frobnicated", { id: "a1" })));
    await until(() => expect(tap.frames.length).toBe(1));

    expect((tap.frames[0] as unknown as { kind: string }).kind).toBe("alert.frobnicated");
    // Still advances the resume point: an unrecognised event is one the server
    // will not replay again, and pretending otherwise would resume into a gap.
    expect(tap.detail().lastSeq).toBe(1);
  });

  it("survives one unreadable frame and keeps delivering the next", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);

    const w = await net.wire();
    w.push("id: 2\nevent: alert.upserted\ndata: {not json\n\n");
    w.push(sse(frame(3, "alert.upserted")));
    await until(() => expect(tap.frames.length).toBe(1));

    expect((tap.frames[0] as unknown as { seq: number }).seq).toBe(3);
    expect(stream.state).toBe("live");
  });

  it("ignores a frame with no seq rather than treating it as seq 0", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);

    const w = await net.wire();
    w.push('id: 4\nevent: alert.upserted\ndata: {"kind":"alert.upserted"}\n\n');
    w.push(sse(frame(4, "alert.upserted")));
    await until(() => expect(tap.frames.length).toBe(1));
    expect(tap.detail().lastSeq).toBe(4);
  });

  it("treats the heartbeat comment as liveness, not as an event", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);

    const w = await net.wire();
    await until(() => expect(stream.state).toBe("live"));
    w.push(": ping\n\n");
    await new Promise((r) => setTimeout(r, 10));

    expect(tap.frames).toHaveLength(0);
    expect(stream.state).toBe("live");
    expect(tap.detail().lastMessageAt).not.toBeNull();
  });

  it("reassembles a frame split across two chunks", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);

    const w = await net.wire();
    const text = sse(frame(11, "delivery.updated"));
    w.push(text.slice(0, 20));
    await new Promise((r) => setTimeout(r, 5));
    expect(tap.frames).toHaveLength(0);
    w.push(text.slice(20));
    await until(() => expect(tap.frames.length).toBe(1));
  });

  it("obeys a resync, and lets the consumer say it has caught up", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);

    (await net.wire()).push(
      sse(frame(20, "resync", { reason: "replay_window_exceeded" })),
    );
    await until(() => expect(tap.detail().resyncReason).toBe("replay_window_exceeded"));

    stream.clearResync();
    expect(tap.detail().resyncReason).toBeNull();
  });

  it("defaults a reasonless resync to buffer_overflow rather than to nothing", async () => {
    const net = socket();
    const stream = open();
    const tap = listen(stream);
    (await net.wire()).push(sse(frame(21, "resync", {})));
    await until(() => expect(tap.detail().resyncReason).toBe("buffer_overflow"));
  });
});

describe("losing the connection", () => {
  it("schedules a backed-off retry when the server refuses, and says so", async () => {
    vi.useFakeTimers();
    try {
      const net = socket({ failFirst: 1 });
      const stream = open();
      const tap = listen(stream);

      await vi.waitFor(() => expect(stream.state).toBe("reconnecting"));
      expect(tap.detail().retryAt).not.toBeNull();
      expect(tap.detail().attempt).toBe(1);

      // The first backoff step is 1s ± 20 %.
      await vi.advanceTimersByTimeAsync(1500);
      await vi.waitFor(() => expect(net.requests.length).toBe(2));
    } finally {
      vi.useRealTimers();
    }
  });

  it("treats a clean end of stream as a disconnection, not as health", async () => {
    const net = socket();
    const stream = open();
    listen(stream);

    const w = await net.wire();
    await until(() => expect(stream.state).toBe("live"));
    w.end();
    await until(() => expect(stream.state).toBe("reconnecting"));
  });

  it("stops claiming to be live the moment the browser says it is offline", async () => {
    socket();
    const stream = open();
    listen(stream);
    await until(() => expect(stream.state).toBe("live"));

    globalThis.dispatchEvent(new Event("offline"));
    expect(stream.state).toBe("offline");
  });

  it("closes down completely, leaving no timer and no listener behind", async () => {
    const net = socket();
    const stream = open();
    listen(stream);
    await net.wire();

    stream.close();
    expect(stream.state).toBe("idle");

    // A closed stream must not resurrect itself from a browser event.
    globalThis.dispatchEvent(new Event("online"));
    await new Promise((r) => setTimeout(r, 20));
    expect(net.requests).toHaveLength(1);
  });
});
