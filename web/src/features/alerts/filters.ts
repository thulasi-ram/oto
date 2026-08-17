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
 *   - **The matcher expression is stored as typed.** It round-trips through the
 *     URL verbatim so a shared link reproduces the exact text, and is only
 *     canonicalised on its way to the `matcher=` parameter.
 */
import { enumValuesOf } from "~/api/bounds";
import { AlertRollupDTOSchema, StateSchema } from "~/api/generated/validators";
import type { AlertListQuery, AlertRollupQuery, RollupAxis, State } from "~/api/types";
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

/**
 * The groupings the UI offers. `none` is the flat table; the other three are
 * exactly the axes `GET /api/v1/alerts/rollups` aggregates on server-side.
 */
export type GroupBy = "none" | RollupAxis;

/**
 * `none` is this UI's own idea — the flat table is not an axis the server
 * aggregates on — so it is the one member written here. The rest are the
 * contract's, read off the only runtime value the generator emits for them: the
 * axis `AlertRollupDTO` echoes back. The `readonly GroupBy[]` annotation is the
 * cross-check, because `GroupBy` comes from the *request* type — if the echoed
 * set and the accepted set ever parted, this line would stop compiling.
 */
export const GROUP_BY_VALUES: readonly GroupBy[] = [
  "none",
  ...enumValuesOf(AlertRollupDTOSchema, "group_by"),
];

/**
 * The two sort orders the contract accepts, taken from the contract.
 *
 * §E.3 permits only these because keyset pagination needs a total order backed by
 * an index; anything else is a `422`. The list is not re-typed here — it is the
 * `sort` query parameter of `listAlerts`, so adding a third order to the contract
 * is a compile error at every site that switches on it rather than a silent
 * divergence between what the UI offers and what the server accepts.
 */
export type SortKey = NonNullable<AlertListQuery["sort"]>;

/** Every lifecycle state the contract publishes, in the contract's own order. */
export const ALL_STATES: readonly State[] = StateSchema.options;

export interface AlertFilters {
  readonly state: readonly State[];
  readonly severity: readonly string[];
  readonly cluster: readonly string[];
  readonly namespace: readonly string[];
  readonly alertname: readonly string[];
  readonly ack: "acked" | "unacked" | null;
  readonly flapping: boolean | null;
  /**
   * `null` includes both, and that is the default **on purpose**. A snooze is a
   * fact about oto's notification behaviour, never about the signal: hiding
   * snoozed alerts from the default list is how an incident is lost.
   */
  readonly snoozed: boolean | null;
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
  snoozed: null,
  since: null,
  q: "",
  sort: "-last_seen_at",
  matcherText: "",
  groupBy: "none",
};

/** True when nothing is filtering — used to tell "no matches" from "nothing here". */
export function isUnfiltered(f: AlertFilters): boolean {
  return f.state.length === 0 && nothingElseFilters(f);
}

/**
 * True when the list is looking at `expired` and at nothing else.
 *
 * ⭐ IT EXISTS TO GIVE ONE EMPTY STATE ITS OWN SENTENCE. `expired` is the one
 * state whose meaning is transience — §M.1: it reads *"oto stopped hearing about
 * this"*, never *"resolved"* — and an empty list under that filter used to say
 * "No alerts match these filters", which is true of a typo'd cluster name too.
 * The two facts are different and the screen now says which one it is (and
 * carries the §M.9 motif that says it a second way).
 *
 * ⛔ EXACTLY ONE STATE, AND NOTHING ELSE ACTIVE. `expired` plus a namespace is a
 * filtered search that happens to include expired, and the honest sentence for
 * an empty one of those is the generic one.
 */
export function isExpiredOnly(f: AlertFilters): boolean {
  return f.state.length === 1 && f.state[0] === "expired" && nothingElseFilters(f);
}

/** Every filter except `state` at its default. */
function nothingElseFilters(f: AlertFilters): boolean {
  return (
    f.severity.length === 0 &&
    f.cluster.length === 0 &&
    f.namespace.length === 0 &&
    f.alertname.length === 0 &&
    f.ack === null &&
    f.flapping === null &&
    f.snoozed === null &&
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
  if (f.snoozed !== null) n += 1;
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

  const snoozeRaw = p.get("snoozed");
  const snoozed = snoozeRaw === "true" ? true : snoozeRaw === "false" ? false : null;

  return {
    state: csv(p.get("state")).filter(isState),
    severity: csv(p.get("severity")),
    cluster: csv(p.get("cluster")),
    namespace: csv(p.get("namespace")),
    alertname: csv(p.get("alertname")),
    ack,
    flapping,
    snoozed,
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
  if (f.snoozed !== null) p.set("snoozed", String(f.snoozed));
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

interface CompiledBase {
  /** Parse failures in the matcher text. Non-empty means: do not send. */
  readonly matcherErrors: readonly { readonly at: number; readonly message: string }[];
  /** Matchers the server refuses at parse time. Also blocking. */
  readonly rejected: readonly { readonly matcher: LabelMatcher; readonly reason: string }[];
  /** True when the request is safe to send. */
  readonly ok: boolean;
}

export interface CompiledQuery extends CompiledBase {
  readonly query: AlertListQuery;
}

export interface CompiledRollupQuery extends CompiledBase {
  readonly query: AlertRollupQuery;
}

/**
 * The filters every read shares, as a plain bag.
 *
 * `listAlerts` and `listAlertRollups` accept an identical filter set by design —
 * that identity is what lets the buckets promise they summarise exactly the list
 * beside them — so it is built once and both callers narrow it.
 */
function sharedFilters(f: AlertFilters): {
  readonly query: Record<string, unknown>;
  readonly compiled: ReturnType<typeof compileMatchers>;
  readonly parsed: ReturnType<typeof parseMatchers>;
} {
  const parsed = parseMatchers(f.matcherText);
  const compiled = compileMatchers(parsed.matchers);

  const query: Record<string, unknown> = {};
  if (f.state.length > 0) query["state"] = [...f.state];
  if (f.severity.length > 0) query["severity"] = [...f.severity];
  if (f.cluster.length > 0) query["cluster"] = [...f.cluster];
  if (f.namespace.length > 0) query["namespace"] = [...f.namespace];
  if (f.alertname.length > 0) query["alertname"] = [...f.alertname];
  if (f.ack !== null) query["ack"] = f.ack;
  if (f.flapping !== null) query["flapping"] = f.flapping;
  if (f.snoozed !== null) query["snoozed"] = f.snoozed;
  if (f.since !== null && f.since !== "") query["since"] = f.since;
  if (f.q.trim() !== "") query["q"] = f.q.trim();
  // The whole label selector, in Alertmanager syntax, exactly as the contract's
  // `matcher=` parameter takes it. All four operators travel; only a regex the
  // server would refuse at parse time is held back (see `compileMatchers`).
  if (compiled.matcher !== null) query["matcher"] = compiled.matcher;

  return { query, compiled, parsed };
}

/**
 * Turn filters into the exact arguments `listAlerts` takes.
 *
 * The important behaviour is what happens when a matcher cannot be served:
 * **nothing is sent**. Silently dropping it would return a page that looks
 * filtered and is not, which is precisely the failure ADR 0017 exists to
 * prevent. The caller renders the reason instead.
 */
export function compileFilters(f: AlertFilters, limit: number, cursor: string | null): CompiledQuery {
  const { query, compiled, parsed } = sharedFilters(f);
  query["limit"] = limit;
  query["sort"] = f.sort;
  // `current_occurrence` is what makes the row show ack state, firing
  // duration and the suppression reason without an N+1.
  //
  // `rule` costs NOTHING on top of it — the server resolves both from the same
  // occurrence read — and it is what carries the snapshot id the row needs to
  // show what the rule said. The id alone is not the rule text; the list resolves
  // a page of them in one further call via `batchGetRuleSnapshots` (ADR 0025).
  query["include"] = ["current_occurrence", "rule"];
  if (cursor !== null) query["cursor"] = cursor;

  return {
    query: query as AlertListQuery,
    matcherErrors: parsed.errors,
    rejected: compiled.rejected,
    ok: parsed.errors.length === 0 && compiled.rejected.length === 0,
  };
}

/**
 * The same filters, aimed at the server-side roll-up.
 *
 * `sort` and `include` are deliberately absent: buckets are keyset-ordered by
 * their own key, and a bucket has no sub-resources to embed.
 */
export function compileRollupFilters(
  f: AlertFilters,
  by: RollupAxis,
  limit: number,
  cursor: string | null,
): CompiledRollupQuery {
  const { query, compiled, parsed } = sharedFilters(f);
  query["group_by"] = by;
  query["limit"] = limit;
  if (cursor !== null) query["cursor"] = cursor;

  return {
    query: query as AlertRollupQuery,
    matcherErrors: parsed.errors,
    rejected: compiled.rejected,
    ok: parsed.errors.length === 0 && compiled.rejected.length === 0,
  };
}

/**
 * Narrow the filter set to one roll-up bucket, for drilling from a bucket into
 * its alerts with **every other filter still applied**.
 *
 * `fingerprint` returns `null`: the alert list has no `source_fingerprint`
 * parameter, so that drill-down is not expressible against this contract and
 * the UI says so rather than dropping to a filter that means something else.
 */
export function withRollupBucket(
  f: AlertFilters,
  by: RollupAxis,
  key: string,
): AlertFilters | null {
  switch (by) {
    case "alertname":
      return { ...f, alertname: [key], groupBy: "none" };
    case "namespace":
      return { ...f, namespace: [key], groupBy: "none" };
    case "fingerprint":
      return null;
    default:
      return null;
  }
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
