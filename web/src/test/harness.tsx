/**
 * Rendering a screen the way the app renders it, and a `fetch` you can reason
 * about.
 *
 * Two decisions worth stating:
 *
 *   1. **Screens are rendered with the real providers.** A query client and a
 *      router, both real, because the bugs this suite exists to catch —
 *      an optimistic row that survives a refusal, a cache key that never
 *      invalidates — live in the wiring and not in the leaves.
 *   2. **`fetch` is stubbed at the transport, never the endpoint module.** The
 *      real `~/api/client` runs: its envelope checks, its `Problem` decoding and
 *      its `Idempotency-Key` header are all under test, and a route table
 *      cannot drift from the paths `endpoints.ts` actually calls the way a
 *      `vi.mock` of that module silently can.
 */
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { MemoryRouter, Route, createMemoryHistory } from "@solidjs/router";
import { render } from "@solidjs/testing-library";
import type { JSX } from "solid-js";
import { expect, vi } from "vitest";

import type { Problem, Violation } from "~/api/types";

/* -------------------------------------------------------------------------- */
/* Rendering                                                                  */
/* -------------------------------------------------------------------------- */

export interface RenderOptions {
  /** Start the memory router here, for screens that read the location. */
  readonly path?: string;
}

export interface Rendered extends ReturnType<typeof render> {
  readonly client: QueryClient;
}

/**
 * Render a screen inside the providers it runs under in the app.
 *
 * Retries are off: a test that waits out three exponential backoffs to observe
 * an error is a test nobody will keep.
 */
export function renderScreen(ui: () => JSX.Element, options: RenderOptions = {}): Rendered {
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0, staleTime: 0 },
      mutations: { retry: false },
    },
  });

  // `path` has to reach the router through its own history, not through a prop:
  // a `MemoryRouter` that quietly started at `/` while the test asked for
  // `/alerts?state=firing` would make every location-reading assertion pass
  // against the wrong location.
  const history = createMemoryHistory();
  if (options.path !== undefined) history.set({ value: options.path });

  const result = render(() => (
    <QueryClientProvider client={client}>
      <MemoryRouter history={history} root={(p) => <>{p.children}</>}>
        <Route path="*" component={() => <>{ui()}</>} />
      </MemoryRouter>
    </QueryClientProvider>
  ));

  return { ...result, client };
}

/* -------------------------------------------------------------------------- */
/* A fetch you can reason about                                               */
/* -------------------------------------------------------------------------- */

export interface RecordedCall {
  readonly method: string;
  readonly url: string;
  readonly path: string;
  readonly search: URLSearchParams;
  readonly headers: Readonly<Record<string, string>>;
  readonly body: unknown;
}

export interface StubResponse {
  readonly status?: number;
  /** The JSON body. Omit for a bodyless 204. */
  readonly json?: unknown;
  /** Raw text, for the "the server sent something that is not JSON" cases. */
  readonly text?: string;
  readonly contentType?: string;
}

export type Route1 = (call: RecordedCall) => StubResponse | Promise<StubResponse>;

/**
 * What a route table entry may be.
 *
 * A plain envelope — `item(...)`, `list(...)` — is the body, because that is
 * what a route table is mostly made of and wrapping every one of them in
 * `{ json: … }` buys nothing. Anything whose keys are *only* the response knobs
 * below is read as the response spec instead.
 */
export type RouteValue = Route1 | StubResponse | unknown;

const STUB_KEYS: ReadonlySet<string> = new Set(["status", "json", "text", "contentType"]);

function asStubResponse(value: unknown): StubResponse {
  if (typeof value === "object" && value !== null && !Array.isArray(value)) {
    const keys = Object.keys(value);
    if (keys.length > 0 && keys.every((k) => STUB_KEYS.has(k))) return value as StubResponse;
  }
  return { json: value };
}

export interface FetchStub {
  /** Every call the app made, in order. */
  readonly calls: readonly RecordedCall[];
  /** Calls whose path ends with `suffix`, for the common assertion. */
  readonly to: (suffix: string) => readonly RecordedCall[];
  /** Register or replace the handler for `METHOD /path` (exact path match). */
  readonly on: (route: string, handler: RouteValue) => void;
}

/**
 * Install a `fetch` that answers a route table.
 *
 * An unrouted call throws with its own address in the message: a screen that
 * quietly asks for something the test did not anticipate is itself a finding,
 * and it should read as one rather than hang.
 */
export function stubFetch(table: Readonly<Record<string, RouteValue>> = {}): FetchStub {
  const routes = new Map<string, RouteValue>(Object.entries(table));
  const calls: RecordedCall[] = [];

  const impl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const raw = typeof input === "string" ? input : String(input);
    const url = new URL(raw, "http://oto.test");
    const method = (init?.method ?? "GET").toUpperCase();

    const headers: Record<string, string> = {};
    const h = init?.headers;
    if (h !== undefined) {
      for (const [k, v] of Object.entries(h as Record<string, string>)) headers[k] = v;
    }

    const call: RecordedCall = {
      method,
      url: raw,
      path: url.pathname,
      search: url.searchParams,
      headers,
      body:
        typeof init?.body === "string" ? (JSON.parse(init.body) as unknown) : (init?.body ?? null),
    };
    calls.push(call);

    const handler = routes.get(`${method} ${url.pathname}`) ?? routes.get(url.pathname);
    if (handler === undefined) {
      throw new Error(`oto test: no stub for ${method} ${url.pathname}`);
    }

    const spec =
      typeof handler === "function"
        ? await (handler as Route1)(call)
        : asStubResponse(handler);
    const status = spec.status ?? 200;
    if (status === 204) return new Response(null, { status: 204 });

    const body = spec.text ?? JSON.stringify(spec.json ?? null);
    return new Response(body, {
      status,
      headers: { "Content-Type": spec.contentType ?? "application/json" },
    });
  };

  vi.stubGlobal("fetch", vi.fn(impl));

  return {
    calls,
    to: (suffix) => calls.filter((c) => c.path.endsWith(suffix)),
    on: (route, handler) => routes.set(route, handler),
  };
}

/** `{ data, meta }`, the item envelope every read endpoint uses. */
export function item<T>(data: T): { data: T; meta: { request_id: string } } {
  return { data, meta: { request_id: "01JD8Z2K7M3TQ9" } };
}

/** `{ data: [...], page, meta }`, the keyset list envelope. */
export function list<T>(
  data: readonly T[],
  page: Partial<{ has_more: boolean; limit: number; next_cursor: string | null }> = {},
): unknown {
  return {
    data: [...data],
    page: { has_more: false, limit: 50, next_cursor: null, ...page },
    meta: { request_id: "01JD8Z2K7M3TQ9" },
  };
}

/** `{ data: [...], meta }` — the bounded collections that carry no cursor. */
export function unpaged<T>(data: readonly T[]): unknown {
  return { data: [...data], meta: { request_id: "01JD8Z2K7M3TQ9" } };
}

/** An RFC 9457 problem document, exactly as the contract describes one. */
export function problem(
  status: number,
  code: string,
  extra: Partial<Problem> = {},
): { status: number; json: Problem } {
  const body = {
    type: `https://oto.dev/errors/${code}`,
    title: code === "validation_failed" ? "Validation failed" : code,
    status,
    code,
    request_id: "01JD8Z2K7M3TQ9",
    ...extra,
  } as unknown as Problem;
  return { status, json: body };
}

/** A `422 validation_failed` carrying field-level violations. */
export function validationFailed(
  ...violations: readonly Violation[]
): { status: number; json: Problem } {
  return problem(422, "validation_failed", {
    detail: `${violations.length} field failed validation.`,
    violations: [...violations],
  } as Partial<Problem>);
}

/* -------------------------------------------------------------------------- */
/* Waiting                                                                    */
/* -------------------------------------------------------------------------- */

/** Let queued microtasks and one macrotask run — the shape solid-query settles in. */
export async function flush(times = 3): Promise<void> {
  for (let i = 0; i < times; i += 1) {
    await Promise.resolve();
    await new Promise<void>((r) => setTimeout(r, 0));
  }
}

/** Poll an assertion until it holds, or fail with its last error. */
export async function until(assertion: () => void, timeoutMs = 2000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let last: unknown;
  for (;;) {
    try {
      assertion();
      return;
    } catch (err) {
      last = err;
      if (Date.now() > deadline) throw last;
      await new Promise<void>((r) => setTimeout(r, 10));
    }
  }
}

/** Assert the subtree renders no literal `undefined` — the contract-drift smell. */
export function expectNoUndefined(root: HTMLElement): void {
  expect(root.textContent ?? "").not.toMatch(/\bundefined\b/);
}
