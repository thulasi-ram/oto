/**
 * Alert-list filter state, and its lossless round trip through the URL.
 *
 * **Every view is linkable.** A filter that lives only in component state cannot
 * be pasted into a chat thread at 3am, and "what were you looking at?" is the
 * most expensive question in an incident. So the filter set is the URL, the URL
 * is the filter set, and the back button works because it is the same thing.
 *
 * Two deliberate exclusions:
 *
 *   - **The cursor is not in the URL.** A keyset cursor is minted under a
 *     specific filter set and the server rejects a mismatched one with
 *     `400 cursor_filter_mismatch` (§E.3). A shareable link to "page 4" would
 *     therefore be a link to an error most of the time. Page position is
 *     session state; the filters are the address.
 *   - **`label` is not folded into the query object.** It is `style: deepObject`
 *     and carries a `!` negation marker OpenAPI cannot express, so the client
 *     takes it as a separate argument and it is kept separate here too.
 */
import type { AlertListQuery, State } from "~/api/types";
import type { LabelSelector } from "~/api/client";
import {
  compileMatchers,
  formatMatchers,
  parseMatchers,
  selectorToMatchers,
  type LabelMatcher,
} from "~/lib/matchers";

/* -------------------------------------------------------------------------- */
/* The shape                                                                  */
/* -------------------------------------------------------------------------- */

/** The three groupings the UI offers. `none` is the flat table. */
export type GroupBy = "none" | "alertname" | "namespace" | "fingerprint";

export const GROUP_BY_VALUES: readonly GroupBy[] = [
  "none",
  "alertname",
  "namespace",
  "fingerprint",
];

export type SortKey = "-last_seen_at" | "-first_seen_at";

export const ALL_STATES: readonly State[] = ["firing", "suppressed", "resolved", "expired"];

export interface AlertFilters {
  readonly state: readonly State[];
  readonly severity: readonly string[];
  readonly cluster: readonly string[];
  readonly namespace: readonly string[];
  readonly alertname: readonly string[];
  readonly ack: "acked" | "unacked" | null;
  readonly flapping: boolean | null;
  readonly since: string | null;
  readonly q: string;
  readonly sort: SortKey;
  /** The raw matcher expression exactly as typed, so the input round-trips. */
  readonly matcherText: string;
  readonly groupBy: GroupBy;
}

export const DEFAULT_FILTERS: AlertFilters = {
  state: [],
  severity: [],
  cluster: [],
  namespace: [],
  alertname: [],
  ack: null,
  flapping: null,
  since: null,
  q: "",
  sort: "-last_seen_at",
  matcherText: "",
  groupBy: "none",
};

/** True when nothing is filtering — used to tell "no matches" from "nothing here". */
export function isUnfiltered(f: AlertFilters): boolean {
  return (
    f.state.length === 0 &&
    f.severity.length === 0 &&
    f.cluster.length === 0 &&
    f.namespace.length === 0 &&
    f.alertname.length === 0 &&
    f.ack === null &&
    f.flapping === null &&
    f.since === null &&
    f.q.trim() === "" &&
    f.matcherText.trim() === ""
  );
}

/** How many filters are active, for the "N filters" affordance. */
export function activeFilterCount(f: AlertFilters): number {
  let n = 0;
  if (f.state.length > 0) n += 1;
  if (f.severity.length > 0) n += 1;
  if (f.cluster.length > 0) n += 1;
  if (f.namespace.length > 0) n += 1;
  if (f.alertname.length > 0) n += 1;
  if (f.ack !== null) n += 1;
  if (f.flapping !== null) n += 1;
  if (f.since !== null) n += 1;
  if (f.q.trim() !== "") n += 1;
  if (f.matcherText.trim() !== "") n += 1;
  return n;
}

/* -------------------------------------------------------------------------- */
/* URL <-> filters                                                            */
/* -------------------------------------------------------------------------- */

function csv(raw: string | null): readonly string[] {
  if (raw === null || raw === "") return [];
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
}

function isState(s: string): s is State {
  return (ALL_STATES as readonly string[]).includes(s);
}

/**
 * Read filters out of a query string.
 *
 * Unrecognised values are dropped rather than passed through: the server
 * rejects an unknown `state` with a 422, and a URL someone hand-edited should
 * degrade to a working view rather than to an error page. Unknown *parameters*
 * are a different matter — those never reach the server at all, because the
 * request is built from this struct and nothing else.
 */
export function filtersFromSearch(search: string): AlertFilters {
  const p = new URLSearchParams(search);

  const groupRaw = p.get("group");
  const groupBy: GroupBy =
    groupRaw !== null && (GROUP_BY_VALUES as readonly string[]).includes(groupRaw)
      ? (groupRaw as GroupBy)
      : "none";

  const sortRaw = p.get("sort");
  const sort: SortKey = sortRaw === "-first_seen_at" ? "-first_seen_at" : "-last_seen_at";

  const ackRaw = p.get("ack");
  const ack = ackRaw === "acked" || ackRaw === "unacked" ? ackRaw : null;

  const flapRaw = p.get("flapping");
  const flapping = flapRaw === "true" ? true : flapRaw === "false" ? false : null;

  return {
    state: csv(p.get("state")).filter(isState),
    severity: csv(p.get("severity")),
    cluster: csv(p.get("cluster")),
    namespace: csv(p.get("namespace")),
    alertname: csv(p.get("alertname")),
    ack,
    flapping,
    since: p.get("since"),
    q: p.get("q") ?? "",
    sort,
    matcherText: p.get("m") ?? "",
    groupBy,
  };
}

/**
 * Serialise back to a query string, omitting everything at its default.
 *
 * Omitting defaults keeps the address bar readable, which matters because these
 * URLs get pasted into incident channels and read by humans.
 */
export function searchFromFilters(f: AlertFilters): string {
  const p = new URLSearchParams();
  if (f.state.length > 0) p.set("state", f.state.join(","));
  if (f.severity.length > 0) p.set("severity", f.severity.join(","));
  if (f.cluster.length > 0) p.set("cluster", f.cluster.join(","));
  if (f.namespace.length > 0) p.set("namespace", f.namespace.join(","));
  if (f.alertname.length > 0) p.set("alertname", f.alertname.join(","));
  if (f.ack !== null) p.set("ack", f.ack);
  if (f.flapping !== null) p.set("flapping", String(f.flapping));
  if (f.since !== null && f.since !== "") p.set("since", f.since);
  if (f.q.trim() !== "") p.set("q", f.q.trim());
  if (f.sort !== "-last_seen_at") p.set("sort", f.sort);
  if (f.matcherText.trim() !== "") p.set("m", f.matcherText.trim());
  if (f.groupBy !== "none") p.set("group", f.groupBy);
  const s = p.toString();
  return s === "" ? "" : `?${s}`;
}

/* -------------------------------------------------------------------------- */
/* Filters -> the wire                                                        */
/* -------------------------------------------------------------------------- */

export interface CompiledQuery {
  readonly query: AlertListQuery;
  readonly label: LabelSelector;
  /** Parse failures in the matcher text. Non-empty means: do not send. */
  readonly matcherErrors: readonly { readonly at: number; readonly message: string }[];
  /** Matchers that parsed but the contract cannot express. Also blocking. */
  readonly rejected: readonly { readonly matcher: LabelMatcher; readonly reason: string }[];
  /** True when the request is safe to send. */
  readonly ok: boolean;
}

/**
 * Turn filters into the exact arguments `listAlerts` takes.
 *
 * The important behaviour is what happens when the matcher text cannot be
 * served: **nothing is sent**. Silently dropping an unexpressible matcher would
 * return a page that looks filtered and is not, which is precisely the failure
 * ADR 0017 exists to prevent. The caller renders the reason instead.
 */
export function compileFilters(f: AlertFilters, limit: number, cursor: string | null): CompiledQuery {
  const parsed = parseMatchers(f.matcherText);
  const compiled = compileMatchers(parsed.matchers);

  const query: Record<string, unknown> = {
    limit,
    sort: f.sort,
    // `current_occurrence` is what makes the row show ack state, firing
    // duration and the suppression reason without an N+1.
    include: ["current_occurrence"],
  };
  if (f.state.length > 0) query["state"] = [...f.state];
  if (f.severity.length > 0) query["severity"] = [...f.severity];
  if (f.cluster.length > 0) query["cluster"] = [...f.cluster];
  if (f.namespace.length > 0) query["namespace"] = [...f.namespace];
  if (f.alertname.length > 0) query["alertname"] = [...f.alertname];
  if (f.ack !== null) query["ack"] = f.ack;
  if (f.flapping !== null) query["flapping"] = f.flapping;
  if (f.since !== null && f.since !== "") query["since"] = f.since;
  if (f.q.trim() !== "") query["q"] = f.q.trim();
  if (cursor !== null) query["cursor"] = cursor;

  return {
    query: query as AlertListQuery,
    label: compiled.selector,
    matcherErrors: parsed.errors,
    rejected: compiled.rejected,
    ok: parsed.errors.length === 0 && compiled.rejected.length === 0,
  };
}

/* -------------------------------------------------------------------------- */
/* Small edits, used by the chips and by click-to-filter                      */
/* -------------------------------------------------------------------------- */

/** Add `name="value"` to the matcher text, if it is not already there. */
export function withMatcher(f: AlertFilters, name: string, value: string): AlertFilters {
  const existing = parseMatchers(f.matcherText).matchers;
  if (existing.some((m) => m.name === name && m.op === "=" && m.value === value)) return f;
  return {
    ...f,
    matcherText: formatMatchers([...existing, { name, op: "=", value }]),
  };
}

export function withoutMatcher(f: AlertFilters, index: number): AlertFilters {
  const existing = parseMatchers(f.matcherText).matchers;
  return { ...f, matcherText: formatMatchers(existing.filter((_, i) => i !== index)) };
}

/** Hydrate the matcher input from a `label[…]` selector, e.g. from a deep link. */
export function matcherTextFromSelector(selector: LabelSelector): string {
  return formatMatchers(selectorToMatchers(selector));
}

/** Toggle one value inside a comma-OR-ed array filter. */
export function toggleIn<T extends string>(list: readonly T[], value: T): readonly T[] {
  return list.includes(value) ? list.filter((v) => v !== value) : [...list, value];
}
