/**
 * The single HTTP entry point for the UI.
 *
 * Everything here is typed off `./generated/schema.d.ts`, which is generated
 * from `api/openapi/openapi.yaml` by `npm run generate` and CHECKED IN. CI runs
 * `npm run generate:check` and fails on any diff — that is gate G3 of SPEC
 * §L.8.1, and it is why no hand-written response type may exist in this app.
 *
 * Paths are relative: the Vite dev server proxies them to the Go process
 * (see vite.config.ts) and in production the same origin serves both.
 */
import type { Problem, Violation, ListEnvelope, ItemEnvelope, PageInfo } from "./types";

/* -------------------------------------------------------------------------- */
/* Errors                                                                     */
/* -------------------------------------------------------------------------- */

/**
 * An RFC 9457 `application/problem+json` response.
 *
 * `violations` is the field-level detail the forms need. Per the contract it is
 * populated **only** for `code === "validation_failed"`, so a caller that finds
 * it empty on a 422 has a server bug, not a form bug.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly problem: Problem | null;
  readonly requestId: string | null;
  readonly retryAfterSeconds: number | null;

  constructor(status: number, problem: Problem | null, fallback: string) {
    super(problem ? (problem.detail ?? problem.title) : fallback);
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
    this.code = problem?.code ?? httpCodeFor(status);
    this.requestId = problem?.request_id ?? null;
    this.retryAfterSeconds = problem?.retry_after_seconds ?? null;
  }

  /** Field-level failures, JSON-pointer paths exactly as the server sent them. */
  get violations(): readonly Violation[] {
    return this.problem?.violations ?? [];
  }

  /** True when retrying the identical request could plausibly succeed. */
  get retryable(): boolean {
    return this.status === 429 || this.status === 502 || this.status === 503 || this.status === 504;
  }

  /** A short, honest, human sentence. Never leaks a payload — the server promises that. */
  get headline(): string {
    if (this.status === 0) return "Cannot reach oto.";
    // Without a problem body `message` is already `"<status> <statusText>"`, so
    // prefixing the status again would read "500 500 Internal Server Error".
    return this.problem?.title ?? this.message;
  }
}

/**
 * The response did not match the shape the contract promises. In dev this is
 * thrown loudly because it is a contract bug; the caller decides what to do.
 */
export class ApiContractError extends Error {
  constructor(
    readonly path: string,
    readonly reason: string,
  ) {
    super(`oto: ${path} returned a body the contract does not describe — ${reason}`);
    this.name = "ApiContractError";
  }
}

function httpCodeFor(status: number): string {
  switch (status) {
    case 0:
      return "network_error";
    case 400:
      return "malformed_request";
    case 401:
      return "unauthenticated";
    case 403:
      return "forbidden";
    case 404:
      return "not_found";
    case 409:
      return "conflict";
    case 412:
      return "precondition_failed";
    case 422:
      return "validation_failed";
    case 429:
      return "rate_limited";
    case 503:
      return "unavailable";
    default:
      return status >= 500 ? "internal_error" : "error";
  }
}

/**
 * Map `violations[]` onto form control names.
 *
 * SPEC §L.8.2: `field` is a `/`-separated JSON-name path, so `matchers/0/name`
 * becomes `matchers.0.name`. A violation whose field matches no control is the
 * caller's problem to surface — never swallowed.
 */
export function violationsByField(err: unknown): ReadonlyMap<string, string> {
  const out = new Map<string, string>();
  if (!(err instanceof ApiError)) return out;
  for (const v of err.violations) {
    const key = v.field.replaceAll("/", ".");
    if (!out.has(key)) out.set(key, v.message);
  }
  return out;
}

/** Violations that did not land on any known control, for the form-level slot. */
export function orphanViolations(err: unknown, knownFields: readonly string[]): readonly string[] {
  if (!(err instanceof ApiError)) return [];
  const known = new Set(knownFields);
  return err.violations
    .filter((v) => !known.has(v.field.replaceAll("/", ".")))
    .map((v) => (v.field ? `${v.field}: ${v.message}` : v.message));
}

/* -------------------------------------------------------------------------- */
/* Query string building                                                      */
/* -------------------------------------------------------------------------- */

/**
 * A query value the contract can actually express.
 *
 * Arrays are `style: form, explode: false` — comma-joined in one parameter.
 * The `label` selector is `style: deepObject` and is handled separately, since
 * OpenAPI cannot express its `!` negation marker anyway (§E.3).
 */
export type QueryValue =
  | string
  | number
  | boolean
  | readonly string[]
  | readonly number[]
  | null
  | undefined;

export type QueryParams = Readonly<Record<string, QueryValue>>;

/** `{team: "core", "!tier": "canary"}` → `label[team]=core&label[!tier]=canary`. */
export type LabelSelector = Readonly<Record<string, string>>;

export interface QueryOptions {
  readonly query?: QueryParams;
  /** Rendered as `label[<name>]=<value>`, deepObject style. */
  readonly label?: LabelSelector;
}

export function buildQueryString(opts: QueryOptions): string {
  const sp = new URLSearchParams();

  for (const [key, value] of Object.entries(opts.query ?? {})) {
    if (value === undefined || value === null || value === "") continue;
    if (Array.isArray(value)) {
      if (value.length === 0) continue;
      sp.set(key, value.join(","));
    } else {
      sp.set(key, String(value));
    }
  }

  for (const [name, value] of Object.entries(opts.label ?? {})) {
    if (value === "") continue;
    sp.set(`label[${name}]`, value);
  }

  const s = sp.toString();
  return s === "" ? "" : `?${s}`;
}

/* -------------------------------------------------------------------------- */
/* The request primitive                                                      */
/* -------------------------------------------------------------------------- */

export interface RequestOptions extends QueryOptions {
  readonly method?: "GET" | "POST" | "PATCH" | "PUT" | "DELETE";
  readonly body?: unknown;
  readonly signal?: AbortSignal;
  /** Makes a retried mutation safe. Sent as `Idempotency-Key`. */
  readonly idempotencyKey?: string;
}

async function rawRequest(path: string, opts: RequestOptions = {}): Promise<Response> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";
  if (opts.idempotencyKey !== undefined) headers["Idempotency-Key"] = opts.idempotencyKey;

  const init: RequestInit = {
    method: opts.method ?? "GET",
    headers,
    credentials: "include",
  };
  if (opts.body !== undefined) init.body = JSON.stringify(opts.body);
  if (opts.signal) init.signal = opts.signal;

  try {
    return await fetch(path + buildQueryString(opts), init);
  } catch (cause) {
    // A network failure is not an HTTP status, and pretending it is one lies to
    // the UI. Status 0 is the honest encoding, and the banner says so.
    if (cause instanceof DOMException && cause.name === "AbortError") throw cause;
    throw new ApiError(0, null, cause instanceof Error ? cause.message : "network error");
  }
}

function isRecord(x: unknown): x is Record<string, unknown> {
  return typeof x === "object" && x !== null && !Array.isArray(x);
}

function asProblem(body: unknown): Problem | null {
  if (!isRecord(body)) return null;
  if (typeof body["title"] !== "string" || typeof body["status"] !== "number") return null;
  return body as unknown as Problem;
}

async function decode(res: Response, path: string): Promise<unknown> {
  if (res.status === 204) return null;
  const text = await res.text();
  if (text === "") return null;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    throw new ApiContractError(path, "body was not JSON");
  }
}

/** Perform a request and return the decoded body, or throw an `ApiError`. */
async function request(path: string, opts: RequestOptions = {}): Promise<unknown> {
  const res = await rawRequest(path, opts);
  const body = await decode(res, path);

  if (!res.ok) {
    const problem = asProblem(body);
    throw new ApiError(res.status, problem, `${res.status} ${res.statusText}`);
  }
  return body;
}

/**
 * `{ data, meta }` — the single-resource envelope every read endpoint uses.
 *
 * The envelope is checked structurally; the resource inside is trusted to the
 * generated type. SPEC §L.8 forbids hand-written valibot schemas for responses
 * (they must come from gate G4, which does not exist yet — see the report), so
 * this deliberately validates the envelope and nothing deeper rather than
 * duplicating the contract by hand and creating a second source of truth.
 */
export async function getItem<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const body = await request(path, opts);
  if (!isRecord(body) || !("data" in body)) {
    throw new ApiContractError(path, "expected an object with a `data` key");
  }
  return (body as unknown as ItemEnvelope<T>).data;
}

/** The same, but keeps `meta` for the callers that show a request id. */
export async function getItemEnvelope<T>(
  path: string,
  opts: RequestOptions = {},
): Promise<ItemEnvelope<T>> {
  const body = await request(path, opts);
  if (!isRecord(body) || !("data" in body)) {
    throw new ApiContractError(path, "expected an object with a `data` key");
  }
  return body as unknown as ItemEnvelope<T>;
}

/** `{ data: [...], page, meta }` — the list envelope, keyset-paginated. */
export async function getList<T>(
  path: string,
  opts: RequestOptions = {},
): Promise<ListEnvelope<T>> {
  const body = await request(path, opts);
  if (!isRecord(body) || !Array.isArray(body["data"])) {
    throw new ApiContractError(path, "expected an object with a `data` array");
  }
  if (!isRecord(body["page"])) {
    throw new ApiContractError(path, "list responses must carry keyset `page` info");
  }
  return body as unknown as ListEnvelope<T>;
}

/**
 * `{ data: [...], meta }` — the handful of collections the contract serves
 * without a cursor because they are bounded by construction (channel types,
 * label names, enrichers).
 */
export async function getUnpagedList<T>(
  path: string,
  opts: RequestOptions = {},
): Promise<readonly T[]> {
  const body = await request(path, opts);
  if (!isRecord(body) || !Array.isArray(body["data"])) {
    throw new ApiContractError(path, "expected an object with a `data` array");
  }
  return body["data"] as T[];
}

/** A mutation whose response body is an item envelope. */
export function postItem<T>(path: string, body: unknown, opts: RequestOptions = {}): Promise<T> {
  return getItem<T>(path, { ...opts, method: "POST", body });
}

export function patchItem<T>(path: string, body: unknown, opts: RequestOptions = {}): Promise<T> {
  return getItem<T>(path, { ...opts, method: "PATCH", body });
}

/** A mutation with no response body worth reading (204, or an ignored envelope). */
export async function del(path: string, opts: RequestOptions = {}): Promise<void> {
  await request(path, { ...opts, method: "DELETE" });
}

/** An empty page, for optimistic and placeholder rendering. */
export function emptyPage<T>(): ListEnvelope<T> {
  const page: PageInfo = { has_more: false, limit: 0, next_cursor: null };
  return { data: [] as T[], page, meta: { request_id: "" } };
}
