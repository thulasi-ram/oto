/**
 * The alert list's filters, and the URL they are.
 *
 * Two behaviours carry the weight:
 *
 *   1. **The round trip is lossless.** A filter set that does not survive
 *      serialise → parse is a link that shows a different list to the person you
 *      pasted it to, and "what were you looking at?" is the most expensive
 *      question in an incident.
 *   2. **A matcher the server would refuse blocks the request.** Dropping it and
 *      querying anyway returns a page that looks filtered and is not — the exact
 *      failure ADR 0017 exists to prevent, and the one no user can detect.
 *
 * The enumerable filters are checked against the *contract's* enums rather than
 * against a list copied into the test, so a lifecycle state added server-side
 * fails here instead of being silently unofferable in the filter bar.
 */
import { describe, expect, it } from "vitest";

import {
  ALL_STATES,
  DEFAULT_FILTERS,
  GROUP_BY_VALUES,
  activeFilterCount,
  compileFilters,
  compileRollupFilters,
  filtersFromSearch,
  isExpiredOnly,
  isQuietTab,
  isUnfiltered,
  matcherTextFromSelector,
  quietBadgeFilters,
  searchFromFilters,
  toggleIn,
  withMatcher,
  withRollupBucket,
  withTab,
  withoutMatcher,
  type AlertFilters,
} from "./filters";
import { enumValues } from "~/test/contract";

const f = (patch: Partial<AlertFilters> = {}): AlertFilters => ({ ...DEFAULT_FILTERS, ...patch });

describe("the filter vocabulary is the contract's", () => {
  it("offers every lifecycle state the server serves, and nothing it does not", () => {
    expect([...ALL_STATES]).toEqual([...enumValues("State")]);
  });

  it("offers exactly the roll-up axes the server aggregates on, plus the flat table", () => {
    // `group_by` is the roll-up axis enum on `GET /api/v1/alerts/rollups`; `none`
    // is the UI's own "do not roll up", and it must be the only extra.
    expect([...GROUP_BY_VALUES]).toEqual(["none", ...enumValues("group_by")]);
  });
});

describe("filters <-> URL", () => {
  it("round-trips a fully-populated filter set without losing a field", () => {
    const original = f({
      state: ["firing", "suppressed"],
      severity: ["critical"],
      cluster: ["prod-eu"],
      namespace: ["payments"],
      alertname: ["HighErrorRate"],
      flapping: true,
      tab: "quiet",
      since: "2026-08-01T00:00:00.000Z",
      q: "error rate",
      sort: "-first_seen_at",
      matcherText: '{pod=~"api|web"}',
      groupBy: "namespace",
    });

    expect(filtersFromSearch(searchFromFilters(original))).toEqual(original);
  });

  it("writes nothing for a filter at its default, so a shared link stays readable", () => {
    expect(searchFromFilters(DEFAULT_FILTERS)).toBe("");
    // The main tab is the default and is not spelled out; the Quiet tab is, so a
    // link to it opens on it.
    expect(searchFromFilters(f({ tab: "active" }))).toBe("");
    expect(searchFromFilters(f({ tab: "quiet" }))).toBe("?tab=quiet");
  });

  it("degrades a hand-edited tab to the main list rather than to an error page", () => {
    expect(filtersFromSearch("?tab=loud").tab).toBe("active");
    expect(filtersFromSearch("?tab=quiet").tab).toBe("quiet");
    expect(filtersFromSearch("").tab).toBe("active");
  });

  it("drops a hand-edited state the server would 422 rather than showing an error page", () => {
    const parsed = filtersFromSearch("?state=firing,teapot,resolved");
    expect(parsed.state).toEqual(["firing", "resolved"]);
  });

  it("degrades an unknown grouping and an unknown sort to the defaults", () => {
    expect(filtersFromSearch("?group=colour&sort=alphabetical").groupBy).toBe("none");
    expect(filtersFromSearch("?group=colour&sort=alphabetical").sort).toBe("-last_seen_at");
  });

  it("keeps the matcher text exactly as typed, so a shared link reproduces it", () => {
    const typed = 'severity = "critical" ,pod=~"api|web"';
    expect(filtersFromSearch(searchFromFilters(f({ matcherText: typed }))).matcherText).toBe(
      typed.trim(),
    );
  });

  it("never puts the cursor in the URL — it is minted under one filter set", () => {
    const search = searchFromFilters(f({ state: ["firing"] }));
    expect(search).not.toContain("cursor");
  });
});

describe("counting what is on", () => {
  it("calls the default view unfiltered, so `no matches` reads differently from `nothing here`", () => {
    expect(isUnfiltered(DEFAULT_FILTERS)).toBe(true);
    expect(activeFilterCount(DEFAULT_FILTERS)).toBe(0);
    // Grouping is a view, not a filter: it changes the shape, not the set.
    expect(isUnfiltered(f({ groupBy: "alertname" }))).toBe(true);
    expect(activeFilterCount(f({ groupBy: "alertname" }))).toBe(0);
  });

  it("does not count whitespace as a filter", () => {
    expect(activeFilterCount(f({ q: "   ", matcherText: "  " }))).toBe(0);
  });

  it("counts each axis once", () => {
    expect(activeFilterCount(f({ state: ["firing", "resolved"], flapping: true, q: "x" }))).toBe(3);
  });

  // §M.9 / ADR 0035. `expired` is the one state whose meaning is transience —
  // §M.1: it reads "oto stopped hearing about this", never "resolved" — and an
  // empty list under that filter used to borrow the sentence a typo'd cluster
  // name gets. This predicate is what tells the two apart, and it is deliberately
  // the narrow reading: `expired` plus anything else is a filtered search that
  // happens to include expired, and the honest sentence for an empty one of
  // those is the generic one.
  it("recognises a list looking at `expired` and at nothing else", () => {
    expect(isExpiredOnly(f({ state: ["expired"] }))).toBe(true);
    // Grouping is a view rather than a filter, exactly as `isUnfiltered` has it.
    expect(isExpiredOnly(f({ state: ["expired"], groupBy: "alertname" }))).toBe(true);

    expect(isExpiredOnly(DEFAULT_FILTERS)).toBe(false);
    expect(isExpiredOnly(f({ state: ["firing"] }))).toBe(false);
    expect(isExpiredOnly(f({ state: ["expired", "resolved"] }))).toBe(false);
    expect(isExpiredOnly(f({ state: ["expired"], namespace: ["prod"] }))).toBe(false);
    expect(isExpiredOnly(f({ state: ["expired"], q: "disk" }))).toBe(false);

    // ⭐ THE TAB IS NOT A FILTER, so it changes neither predicate. It is the
    // `<Switch>` on /alerts that asks `isQuietTab` FIRST — "nothing is quiet" is
    // a different sentence from "nothing matched", and folding the tab into
    // these two would have produced the wrong one of the two.
    expect(isExpiredOnly(f({ state: ["expired"], tab: "quiet" }))).toBe(true);
    expect(isUnfiltered(f({ tab: "quiet" }))).toBe(true);
  });

  // The two predicates read the same eleven fields, and the whole point of
  // splitting them was that neither could drift from the other. They must stay
  // mutually exclusive: a list is unfiltered, or it is looking at expired, never
  // both — the `<Switch>` on `/alerts` picks the first arm that matches.
  it("cannot call the same view both unfiltered and expired-only", () => {
    for (const view of [
      DEFAULT_FILTERS,
      f({ state: ["expired"] }),
      f({ state: ["expired"], q: "x" }),
      f({ q: "x" }),
    ]) {
      expect(isUnfiltered(view) && isExpiredOnly(view)).toBe(false);
    }
  });
});

describe("filters -> the wire", () => {
  it("asks for the two things the row needs and nothing more", () => {
    const compiled = compileFilters(f(), 50, null);
    // `current_case` carries ack state and firing duration; `rule` carries
    // the snapshot id the Rule column resolves in one batch (ADR 0025). Both are
    // free on the same case read.
    expect(compiled.query.include).toEqual(["current_case", "rule"]);
    expect(compiled.query.limit).toBe(50);
    expect(compiled.query.sort).toBe("-last_seen_at");
    expect(compiled.ok).toBe(true);
  });

  it("sends all four matcher operators in the one parameter that can carry them", () => {
    const compiled = compileFilters(
      f({ matcherText: 'severity="critical", tier!="canary", pod=~"api|web", job!~"cron"' }),
      50,
      null,
    );
    expect(compiled.ok).toBe(true);
    expect(compiled.query.matcher).toBe(
      '{severity="critical", tier!="canary", pod=~"api|web", job!~"cron"}',
    );
  });

  it("refuses to send anything when a matcher would force a sequential scan", () => {
    const compiled = compileFilters(f({ matcherText: 'pod=~"api-.*"' }), 50, null);

    expect(compiled.ok).toBe(false);
    expect(compiled.rejected).toHaveLength(1);
    expect(compiled.rejected[0]?.reason).toMatch(/sequential scan/);
    // ⛔ The refused matcher must not be quietly stripped and the rest sent:
    // that is a page which looks filtered and is not.
    expect(compiled.query.matcher).toBeUndefined();
  });

  it("refuses on a parse error too, and says where", () => {
    const compiled = compileFilters(f({ matcherText: "9lives=cat" }), 50, null);
    expect(compiled.ok).toBe(false);
    expect(compiled.matcherErrors[0]?.message).toMatch(/not a valid label name/);
  });

  it("carries the cursor only when there is one", () => {
    expect(compileFilters(f(), 50, null).query.cursor).toBeUndefined();
    expect(compileFilters(f(), 50, "c-1").query.cursor).toBe("c-1");
  });

  it("gives the roll-up the identical filter set, so the buckets summarise the list beside them", () => {
    const filters = f({ state: ["firing"], severity: ["critical"], q: "err" });
    const listQuery = compileFilters(filters, 50, null).query as Record<string, unknown>;
    const rollup = compileRollupFilters(filters, "alertname", 50, null).query as Record<
      string,
      unknown
    >;

    for (const key of ["state", "severity", "q"]) {
      expect(rollup[key]).toEqual(listQuery[key]);
    }
    expect(rollup["group_by"]).toBe("alertname");
    // A bucket has no sub-resources to embed and its own keyset order.
    expect(rollup["include"]).toBeUndefined();
    expect(rollup["sort"]).toBeUndefined();
  });
});

describe("drilling from a bucket", () => {
  it("narrows to the bucket and keeps every other filter", () => {
    const base = f({ state: ["firing"], groupBy: "alertname" });
    const drilled = withRollupBucket(base, "alertname", "HighErrorRate");
    expect(drilled?.alertname).toEqual(["HighErrorRate"]);
    expect(drilled?.state).toEqual(["firing"]);
    expect(drilled?.groupBy).toBe("none");
  });

  it("says no rather than dropping to a filter that means something else", () => {
    // The alert list has no `source_fingerprint` parameter, so this drill-down is
    // not expressible against this contract. Silently filtering by something
    // adjacent would show a different set under the bucket's own name.
    expect(withRollupBucket(f(), "fingerprint", "9a8b7c6d")).toBeNull();
  });

  it("has an answer for every axis the roll-up serves", () => {
    for (const axis of enumValues("group_by")) {
      const result = withRollupBucket(f(), axis as "alertname", "k");
      expect(result === null || result.groupBy === "none").toBe(true);
    }
  });
});

describe("click-to-filter", () => {
  it("adds a label matcher and is idempotent", () => {
    const once = withMatcher(f(), "pod", "api-7");
    expect(once.matcherText).toBe('{pod="api-7"}');
    expect(withMatcher(once, "pod", "api-7")).toBe(once);
  });

  it("removes by position, leaving the rest canonical", () => {
    const two = withMatcher(withMatcher(f(), "pod", "api-7"), "tier", "prod");
    expect(withoutMatcher(two, 0).matcherText).toBe('{tier="prod"}');
    expect(withoutMatcher(two, 1).matcherText).toBe('{pod="api-7"}');
  });

  it("hydrates the input from a deep link's label selector, negation included", () => {
    expect(matcherTextFromSelector({ team: "core", "!tier": "canary" })).toBe(
      '{team="core", tier!="canary"}',
    );
  });

  it("toggles a value in and out of an OR-ed array", () => {
    expect(toggleIn(["firing"], "resolved")).toEqual(["firing", "resolved"]);
    expect(toggleIn(["firing", "resolved"], "firing")).toEqual(["resolved"]);
  });
});

/**
 * The Quiet tab.
 *
 * ⛔ THESE ARE THE ASSERTIONS THAT MAKE HIDING SNOOZED ALERTS SAFE. Splitting
 * them off the default list reverses `filters.ts`'s own former rule — *"hiding
 * snoozed alerts from the default list is how an incident is lost"* — and the
 * reversal only holds while the tab is unmissable and its badge is honest. Each
 * test below is one clause of that argument.
 */
describe("the Quiet tab", () => {
  it("always tells the server which tab it is on, and never asks for both", () => {
    // ⛔ An omitted `snoozed` means "both tabs at once" — a list with no honest
    // heading, and on the main tab exactly the behaviour this replaced.
    const main = compileFilters(f(), 50, null).query as Record<string, unknown>;
    const quiet = compileFilters(f({ tab: "quiet" }), 50, null).query as Record<string, unknown>;

    expect(main["snoozed"]).toBe(false);
    expect(quiet["snoozed"]).toBe(true);
  });

  it("sends the tab on the roll-up too, so the buckets summarise the tab beside them", () => {
    const rollup = compileRollupFilters(f({ tab: "quiet" }), "namespace", 50, null).query as Record<
      string,
      unknown
    >;
    expect(rollup["snoozed"]).toBe(true);
    // The same three axes as the main tab — the Quiet tab is the whole list
    // machine pointed at a different set, not a reduced view of it.
    expect(rollup["group_by"]).toBe("namespace");
  });

  it("keeps every filter when moving between tabs", () => {
    const base = f({ state: ["firing"], namespace: ["payments"], q: "disk" });
    const quiet = withTab(base, "quiet");

    expect(isQuietTab(quiet)).toBe(true);
    expect(quiet.state).toEqual(["firing"]);
    expect(quiet.namespace).toEqual(["payments"]);
    expect(quiet.q).toBe("disk");
    expect(withTab(quiet, "active")).toEqual(base);
  });

  it("returns the same object when the tab is not changing, so nothing re-renders", () => {
    const base = f({ tab: "quiet" });
    expect(withTab(base, "quiet")).toBe(base);
  });

  it("counts the badge under the operator's own filters, never under none", () => {
    // ⛔ A count of every quiet alert in the org, beside a list narrowed to one
    // namespace, sends somebody to a tab that then shows them nothing. The badge
    // promises exactly what the tab will contain.
    const base = f({ namespace: ["payments"], severity: ["critical"], groupBy: "alertname" });
    const badge = quietBadgeFilters(base);

    expect(badge.tab).toBe("quiet");
    expect(badge.namespace).toEqual(["payments"]);
    expect(badge.severity).toEqual(["critical"]);
    // The badge counts alerts; the roll-up would answer in buckets.
    expect(badge.groupBy).toBe("none");

    const query = compileFilters(badge, 200, null).query as Record<string, unknown>;
    expect(query["snoozed"]).toBe(true);
    expect(query["namespace"]).toEqual(["payments"]);
  });

  it("does not count the tab as an active filter", () => {
    // "You are on the Quiet tab" is not a filter somebody forgot to clear, and a
    // "1 filter" badge over a tab that is plainly labelled would be noise.
    expect(activeFilterCount(f({ tab: "quiet" }))).toBe(0);
    expect(activeFilterCount(f({ tab: "quiet", state: ["firing"] }))).toBe(1);
  });
});
