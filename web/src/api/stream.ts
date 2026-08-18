/**
 * The live feed (§E.4, ADR 0010).
 *
 * The contract's promise is durable resume: reconnect with `Last-Event-ID: N`
 * and the server replays `ui_events` with `seq > N` from the last 24 hours
 * before attaching you live. A laptop that slept through an incident therefore
 * wakes into a *correct* UI rather than a plausible one.
 *
 * **Why this is not `EventSource`.** `EventSource` resends `Last-Event-ID` only
 * across reconnects it manages itself; constructing a fresh one — which is what
 * any app-controlled backoff, any tab reload, and any wake-from-sleep recovery
 * necessarily does — silently drops the resume point and starts from now. That
 * would make the headline guarantee false in exactly the case it exists for. So
 * the stream is read over `fetch` with a real header, and the resume point is
 * persisted for the tab.
 *
 * Two honesty rules shape the rest:
 *
 * 1. **Connection state is reported, never assumed.** A UI that renders stale
 *    rows while claiming to be live is the failure oto exists to prevent, so
 *    `state` distinguishes live / connecting / reconnecting / offline and the
 *    chrome shows it.
 * 2. **A `resync` frame is obeyed.** The server is saying "your incremental
 *    state is not trustworthy". The only correct response is to refetch.
 */
import { buildQueryString } from "./client";
import type { StreamFrame, UiEventKind } from "./types";

export type ConnectionState =
  /** Never connected yet in this page's life. */
  | "idle"
  /** First attempt in flight. */
  | "connecting"
  /** Attached and receiving. */
  | "live"
  /** Dropped, backing off, will retry. */
  | "reconnecting"
  /** The browser says there is no network. Rows are stale and we say so. */
  | "offline";

export type StreamResource =
  | "alerts"
  | "groups"
  | "cases"
  | "events"
  | "deliveries"
  | "sources";

export interface StreamInterest {
  /** Omit for everything. */
  readonly resources?: readonly StreamResource[];
  readonly alertId?: string;
  readonly groupId?: string;
}

export type FrameHandler = (frame: StreamFrame) => void;
export type StateHandler = (state: ConnectionState, detail: StreamDetail) => void;

export interface StreamDetail {
  /** Highest `seq` seen — the resume point. `null` before the first frame ever. */
  readonly lastSeq: number | null;
  /** Epoch ms at which the next reconnect fires. `null` when not waiting. */
  readonly retryAt: number | null;
  /** Consecutive failed attempts. Resets on a successful frame. */
  readonly attempt: number;
  /** Set when the server said our incremental state is untrustworthy. */
  readonly resyncReason: "buffer_overflow" | "replay_window_exceeded" | null;
  /** When we last received anything, including a heartbeat. */
  readonly lastMessageAt: number | null;
}

/** Backoff: 1s, 2s, 4s, 8s, 15s, 30s, then hold. Jittered ±20 %. */
const BACKOFF_MS = [1000, 2000, 4000, 8000, 15_000, 30_000] as const;

function backoffFor(attempt: number): number {
  const base = BACKOFF_MS[Math.min(attempt, BACKOFF_MS.length - 1)] ?? 30_000;
  return Math.round(base + base * 0.2 * (Math.random() * 2 - 1));
}

/**
 * The server heartbeats every 15 s (§E.4). Three missed beats means the socket
 * is wedged open by an intermediary — a state `fetch` will happily hold
 * forever, and the one a sleeping laptop wakes into.
 */
const STALL_TIMEOUT_MS = 50_000;

const RESUME_KEY = "oto.stream.seq";

function isFrame(x: unknown): x is StreamFrame {
  if (typeof x !== "object" || x === null) return false;
  const o = x as Record<string, unknown>;
  return typeof o["seq"] === "number" && typeof o["kind"] === "string";
}

const KNOWN_KINDS: ReadonlySet<string> = new Set<UiEventKind>([
  "alert.upserted",
  "case.upserted",
  "group.upserted",
  "event.appended",
  "delivery.updated",
  "source.health",
  "resync",
]);

/** Is this a frame kind we understand? Unknown kinds are forward-compatible. */
export function isKnownKind(kind: string): kind is UiEventKind {
  return KNOWN_KINDS.has(kind);
}

/* -------------------------------------------------------------------------- */
/* SSE wire parsing                                                           */
/* -------------------------------------------------------------------------- */

interface WireEvent {
  readonly id: string | null;
  readonly event: string | null;
  readonly data: string;
}

/**
 * An incremental `text/event-stream` parser.
 *
 * Kept deliberately small and dependency-free: it handles the three fields oto
 * uses (`id`, `event`, `data`), ignores comment lines (the `: ping` heartbeat)
 * and tolerates `\r\n`. Anything else on the wire is ignored rather than
 * treated as an error, because a forward-compatible field must not kill a
 * stream that is otherwise healthy.
 */
class SseParser {
  #buffer = "";
  #id: string | null = null;
  #event: string | null = null;
  #data: string[] = [];

  push(chunk: string): readonly WireEvent[] {
    this.#buffer += chunk;
    const out: WireEvent[] = [];

    let idx: number;
    while ((idx = this.#buffer.indexOf("\n")) >= 0) {
      let line = this.#buffer.slice(0, idx);
      this.#buffer = this.#buffer.slice(idx + 1);
      if (line.endsWith("\r")) line = line.slice(0, -1);

      if (line === "") {
        if (this.#data.length > 0 || this.#event !== null) {
          out.push({ id: this.#id, event: this.#event, data: this.#data.join("\n") });
        }
        this.#event = null;
        this.#data = [];
        continue;
      }
      if (line.startsWith(":")) continue; // heartbeat / keep-alive comment

      const colon = line.indexOf(":");
      const field = colon < 0 ? line : line.slice(0, colon);
      let value = colon < 0 ? "" : line.slice(colon + 1);
      if (value.startsWith(" ")) value = value.slice(1);

      if (field === "id") this.#id = value;
      else if (field === "event") this.#event = value;
      else if (field === "data") this.#data.push(value);
      // `retry:` is ignored on purpose — we own the backoff schedule.
    }
    return out;
  }
}

/* -------------------------------------------------------------------------- */
/* The stream                                                                 */
/* -------------------------------------------------------------------------- */

/**
 * One SSE connection, owned by the app shell and shared by every screen.
 *
 * Screens subscribe to frames and translate them into cache invalidations;
 * nothing here knows about solid-query, which keeps the transport independently
 * testable and keeps cache policy in one place.
 */
export class AlertStream {
  #state: ConnectionState = "idle";
  #lastSeq: number | null = null;
  #attempt = 0;
  #retryAt: number | null = null;
  #resyncReason: StreamDetail["resyncReason"] = null;
  #lastMessageAt: number | null = null;

  #abort: AbortController | null = null;
  #retryTimer: ReturnType<typeof setTimeout> | null = null;
  #stallTimer: ReturnType<typeof setInterval> | null = null;
  #closed = false;
  #generation = 0;

  readonly #frameHandlers = new Set<FrameHandler>();
  readonly #stateHandlers = new Set<StateHandler>();
  readonly #interest: StreamInterest;

  constructor(interest: StreamInterest = {}) {
    this.#interest = interest;
    this.#lastSeq = readResumePoint();
  }

  get state(): ConnectionState {
    return this.#state;
  }

  get detail(): StreamDetail {
    return {
      lastSeq: this.#lastSeq,
      retryAt: this.#retryAt,
      attempt: this.#attempt,
      resyncReason: this.#resyncReason,
      lastMessageAt: this.#lastMessageAt,
    };
  }

  onFrame(handler: FrameHandler): () => void {
    this.#frameHandlers.add(handler);
    return () => {
      this.#frameHandlers.delete(handler);
    };
  }

  onState(handler: StateHandler): () => void {
    this.#stateHandlers.add(handler);
    handler(this.#state, this.detail);
    return () => {
      this.#stateHandlers.delete(handler);
    };
  }

  /** Acknowledge a `resync`: the consumer refetched and is trustworthy again. */
  clearResync(): void {
    if (this.#resyncReason === null) return;
    this.#resyncReason = null;
    this.#emitState();
  }

  start(): void {
    if (this.#closed) return;
    globalThis.addEventListener("online", this.#onOnline);
    globalThis.addEventListener("offline", this.#onOffline);
    document.addEventListener("visibilitychange", this.#onVisibilityChange);
    this.#stallTimer = setInterval(this.#checkStall, 10_000);
    void this.#connect();
  }

  close(): void {
    this.#closed = true;
    this.#clearRetryTimer();
    if (this.#stallTimer !== null) {
      clearInterval(this.#stallTimer);
      this.#stallTimer = null;
    }
    this.#abort?.abort();
    this.#abort = null;
    globalThis.removeEventListener("online", this.#onOnline);
    globalThis.removeEventListener("offline", this.#onOffline);
    document.removeEventListener("visibilitychange", this.#onVisibilityChange);
    this.#setState("idle");
  }

  /** Drop everything and reconnect immediately — the "Reconnect" affordance. */
  reconnectNow(): void {
    if (this.#closed) return;
    this.#clearRetryTimer();
    this.#abort?.abort();
    this.#abort = null;
    this.#attempt = 0;
    void this.#connect();
  }

  #onOnline = (): void => {
    if (this.#state === "offline" || this.#state === "reconnecting") this.reconnectNow();
  };

  #onOffline = (): void => {
    this.#clearRetryTimer();
    this.#abort?.abort();
    this.#abort = null;
    this.#setState("offline");
  };

  #onVisibilityChange = (): void => {
    // Waking from sleep: the socket can look open and be dead. If we are not
    // demonstrably live, reconnect and let the server's replay fill the gap.
    if (document.visibilityState === "visible" && this.#state !== "live") this.reconnectNow();
  };

  #checkStall = (): void => {
    if (this.#state !== "live" || this.#lastMessageAt === null) return;
    if (Date.now() - this.#lastMessageAt > STALL_TIMEOUT_MS) {
      // Missed heartbeats. Treat as a drop, not as health.
      this.#abort?.abort();
      this.#abort = null;
    }
  };

  async #connect(): Promise<void> {
    if (this.#closed) return;
    if (globalThis.navigator?.onLine === false) {
      this.#setState("offline");
      return;
    }

    const generation = ++this.#generation;
    const controller = new AbortController();
    this.#abort = controller;
    this.#setState(this.#lastSeq === null ? "connecting" : "reconnecting");

    const url =
      "/api/v1/stream" +
      buildQueryString({
        query: {
          resources: this.#interest.resources ? [...this.#interest.resources] : undefined,
          alert_id: this.#interest.alertId,
          group_id: this.#interest.groupId,
        },
      });

    const headers: Record<string, string> = { Accept: "text/event-stream" };
    // This is the whole point of the fetch transport.
    if (this.#lastSeq !== null) headers["Last-Event-ID"] = String(this.#lastSeq);

    try {
      const res = await fetch(url, {
        headers,
        credentials: "include",
        signal: controller.signal,
        cache: "no-store",
      });

      if (!res.ok || res.body === null) {
        this.#scheduleRetry(generation);
        return;
      }

      this.#attempt = 0;
      this.#retryAt = null;
      this.#lastMessageAt = Date.now();
      this.#setState("live");

      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      const parser = new SseParser();

      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        if (this.#generation !== generation) return;
        this.#lastMessageAt = Date.now();
        for (const wire of parser.push(value ?? "")) this.#handleWire(wire);
      }
      // A clean end-of-stream is still a disconnection.
      this.#scheduleRetry(generation);
    } catch (cause) {
      if (controller.signal.aborted && this.#generation !== generation) return;
      if (this.#closed) return;
      void cause;
      this.#scheduleRetry(generation);
    }
  }

  #handleWire(wire: WireEvent): void {
    if (wire.data === "") return;

    let parsed: unknown;
    try {
      parsed = JSON.parse(wire.data) as unknown;
    } catch {
      return; // one unreadable frame is not a reason to tear down a healthy stream
    }
    if (!isFrame(parsed)) return;

    // `seq` is strictly monotonic but NOT contiguous (§E.4) — the sequence is
    // allocated across orgs from one counter, so a gap is normal and must never
    // be read as loss.
    if (this.#lastSeq === null || parsed.seq > this.#lastSeq) {
      this.#lastSeq = parsed.seq;
      writeResumePoint(parsed.seq);
    }

    if (parsed.kind === "resync") {
      const data = parsed.data as { reason?: StreamDetail["resyncReason"] };
      this.#resyncReason = data.reason ?? "buffer_overflow";
    }

    if (this.#state !== "live") this.#setState("live");
    else this.#emitState();

    for (const handler of this.#frameHandlers) handler(parsed);
  }

  #scheduleRetry(generation: number): void {
    if (this.#closed || this.#generation !== generation) return;
    const wait = backoffFor(this.#attempt);
    this.#attempt += 1;
    this.#retryAt = Date.now() + wait;
    this.#setState("reconnecting");
    this.#clearRetryTimer();
    this.#retryTimer = setTimeout(() => {
      this.#retryTimer = null;
      void this.#connect();
    }, wait);
  }

  #clearRetryTimer(): void {
    if (this.#retryTimer !== null) {
      clearTimeout(this.#retryTimer);
      this.#retryTimer = null;
    }
  }

  #setState(next: ConnectionState): void {
    this.#state = next;
    this.#emitState();
  }

  #emitState(): void {
    const detail = this.detail;
    for (const handler of this.#stateHandlers) handler(this.#state, detail);
  }
}

/**
 * The resume point survives a reload but not a new tab, which matches the
 * server's 24-hour replay window closely enough to be useful and is discarded
 * cheaply when it is not.
 */
function readResumePoint(): number | null {
  try {
    const raw = sessionStorage.getItem(RESUME_KEY);
    if (raw === null) return null;
    const n = Number.parseInt(raw, 10);
    return Number.isSafeInteger(n) && n > 0 ? n : null;
  } catch {
    return null;
  }
}

function writeResumePoint(seq: number): void {
  try {
    sessionStorage.setItem(RESUME_KEY, String(seq));
  } catch {
    /* private mode — resume degrades to "from now", and the UI still refetches */
  }
}
