/**
 * Whether a cache entry can ever learn that its data changed.
 *
 * ⛔ THE FAILURE THIS FILE EXISTS FOR IS SILENT AND HAS NO DOM. A query keyed
 * outside every prefix anything invalidates renders perfectly: the right shape,
 * the right columns, the right empty state — and the wrong data, indefinitely.
 * `["policy-preview","recent-alerts"]` was a filtered alert list keyed by hand
 * outside `["alerts"]`, on the one screen whose purpose is dry-running a routing
 * policy against a *recent* alert, and every rendering assertion anybody could
 * have written about it passed.
 *
 * So the assertion here is never "the key has the right shape". A key's shape is
 * what was already agreed and already wrong. It is one of:
 *
 *   1. **A real frame reaches the real screen.** The dry-run list refetches
 *      because an `alert.upserted` arrived — pushed down a stubbed socket into
 *      the mounted section, not simulated by calling `invalidateQueries`.
 *   2. **One entry has one policy.** Two screens that read the same key are
 *      mounted on one client and must agree about `staleTime`, which is
 *      per-observer and therefore cannot be checked from either screen alone.
 *   3. **Every key in `qk` is reachable at all** — checked against the
 *      invalidations the tree actually contains, so a key added tomorrow with
 *      nothing to refresh it fails on the day it is written rather than during
 *      the incident it lengthens.
 *
 * (3) reads the source of `web/src` rather than importing it, for the same
 * reason `index.css.test.ts` does: what is under test is the *absence* of a
 * call, and absence is not observable from anything that ran.
 */
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { fireEvent, screen } from "@solidjs/testing-library";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Query, QueryClient } from "@tanstack/solid-query";

import { qk } from "./keys";
import { LiveProvider } from "./live";
import {
  CAPABILITY_STALE_MS,
  DEFAULT_STALE_MS,
  FRESHNESS,
  RECENT_ALERTS_QUIET_MS,
  REFERENCE_STALE_MS,
  channelTypesQuery,
  clustersQuery,
  labelNamesQuery,
  recentAlertsQuery,
  ruleSnapshotsQuery,
  type Freshness,
} from "./queries";
import { FilterBar } from "~/features/alerts/FilterBar";
import { DEFAULT_FILTERS } from "~/features/alerts/filters";
import { SourcesSection } from "~/features/settings/SourcesSection";
import { PoliciesSection } from "~/features/notifications/PoliciesSection";
import { alert, channel, cluster, policy, source, frame, sse } from "~/test/fixtures";
import {
  flush,
  list,
  renderScreen,
  stubFetch,
  unpaged,
  until,
  type FetchStub,
} from "~/test/harness";

/* -------------------------------------------------------------------------- */
/* 1. A frame, a socket, and the screen that has to hear it                   */
/* -------------------------------------------------------------------------- */

/**
 * The app's route table with a live socket wired in beside it.
 *
 * The stream cannot go through `stubFetch` — its answer is a body that stays
 * open — so this wraps the harness's `fetch` rather than replacing it: every
 * ordinary call still goes through the real client, the real envelope checks and
 * the real route table, and only `/api/v1/stream` is answered with a wire.
 */
function stubWorld(table: Readonly<Record<string, unknown>>): {
  readonly net: FetchStub;
  readonly push: (text: string) => Promise<void>;
} {
  const net = stubFetch(table);
  const inner = globalThis.fetch;
  const pushes: ((text: string) => void)[] = [];

  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (!String(input).includes("/api/v1/stream")) return inner(input, init);
      const encoder = new TextEncoder();
      let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
      const body = new ReadableStream<Uint8Array>({
        start(c) {
          controller = c;
        },
      });
      pushes.push((t) => controller?.enqueue(encoder.encode(t)));
      return Promise.resolve({ ok: true, status: 200, body });
    }),
  );

  return {
    net,
    push: async (text) => {
      await until(() => expect(pushes.length).toBeGreaterThanOrEqual(1));
      pushes[0]?.(text);
      await flush();
    },
  };
}

let unmount: (() => void) | null = null;
afterEach(() => {
  unmount?.();
  unmount = null;
});

describe("the frame reaches the screen", () => {
  it("reaches the policy dry-run list when an alert changes, without paying per frame", async () => {
    const world = stubWorld({
      "GET /api/v1/notification-policies": list([policy()]),
      "GET /api/v1/channels": list([channel()]),
      "GET /api/v1/alerts": list([alert()]),
      "GET /api/v1/labels": unpaged([]),
    });

    const rendered = renderScreen(() => (
      <LiveProvider>
        <PoliciesSection />
      </LiveProvider>
    ));
    unmount = () => rendered.unmount();

    // The dry run lives in the editor, so the list it offers is only on screen
    // once the editor is.
    await until(() => expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));

    // Wait for the ANSWER, not merely the request: an entry with no data yet is
    // never quiet, and a frame arriving mid-flight would be measuring the empty
    // case rather than the policy.
    const picker = (): Query | undefined =>
      rendered.client.getQueryCache().find({ queryKey: qk.alerts.recent(), exact: true });
    await until(() => expect(picker()?.state.dataUpdatedAt ?? 0).toBeGreaterThan(0));
    const asked = world.net.to("/alerts").length;

    // The one frame this screen exists to react to. Nothing here names a key:
    // the assertion is that the wire reaches the list through the real reducer.
    await world.push(sse(frame(1, "alert.upserted", { id: "an-alert" })));

    // Reaching it means being MARKED by it. That is the half the original defect
    // failed — a key outside `["alerts"]` is never marked by anything — and it is
    // the half that makes the entry refetch the next time the screen asks.
    await until(() =>
      expect(
        picker()?.state.isInvalidated,
        'the dry-run list ignored `alert.upserted` — it is keyed outside `["alerts"]`',
      ).toBe(true),
    );

    // And the other half: being marked is not being refetched. The picker is
    // quiet for a minute after each answer (`RECENT_ALERTS_QUIET_MS`), so a
    // storm costs one request a minute rather than one per frame — a list of
    // twenty alerts to choose between does not need to be right to the second.
    expect(
      world.net.to("/alerts").length,
      "the dry-run list refetched on the frame; an alert storm would refetch it on every frame",
    ).toBe(asked);
  });
});

/* -------------------------------------------------------------------------- */
/* 2. One entry, one policy                                                   */
/* -------------------------------------------------------------------------- */

interface ObserverLike {
  readonly options: { readonly staleTime?: unknown };
}

/**
 * The observers watching one cache entry, with their *resolved* options.
 *
 * This is the only place the divergence is visible. `staleTime` is per-observer
 * over a shared entry, so a screen that omits it does not inherit the other
 * screen's number — it inherits the client default, and both screens read as
 * correct in isolation.
 */
function observersOf(client: QueryClient, key: readonly unknown[]): readonly ObserverLike[] {
  const query = client.getQueryCache().find({ queryKey: key, exact: true });
  expect(query, `nothing mounted \`${key.join("/")}\``).toBeTruthy();
  return (query as unknown as { readonly observers: readonly ObserverLike[] }).observers;
}

describe("one cache entry, one freshness policy", () => {
  it("gives every reader of the cluster list the same staleTime", async () => {
    const net = stubFetch({
      "GET /api/v1/clusters": list([cluster()]),
      "GET /api/v1/sources": list([source()]),
      "GET /api/v1/labels": unpaged([{ name: "namespace", alert_count: 3 }]),
    });

    // The two screens that read clusters, on one client — which is how the app
    // holds them, and the only arrangement in which the disagreement exists.
    const rendered = renderScreen(() => (
      <>
        <FilterBar filters={DEFAULT_FILTERS} onChange={() => {}} onReset={() => {}} />
        <SourcesSection />
      </>
    ));
    unmount = () => rendered.unmount();

    await until(() => expect(net.to("/clusters").length).toBe(1));

    const observers = observersOf(rendered.client, qk.settings.clusters());
    expect(observers.length, "only one screen mounted; the test proves nothing").toBeGreaterThan(1);

    const declared = new Set(observers.map((o) => o.options.staleTime));
    expect(
      [...declared],
      "the cluster list has more than one freshness policy — the filter bar and the settings screen disagree about how long it may be trusted",
    ).toEqual([REFERENCE_STALE_MS]);
  });

  it("asks once for a list two screens read", async () => {
    const net = stubFetch({
      "GET /api/v1/clusters": list([cluster()]),
      "GET /api/v1/sources": list([source()]),
      "GET /api/v1/labels": unpaged([]),
    });

    const rendered = renderScreen(() => (
      <>
        <FilterBar filters={DEFAULT_FILTERS} onChange={() => {}} onReset={() => {}} />
        <SourcesSection />
      </>
    ));
    unmount = () => rendered.unmount();

    await until(() => expect(net.to("/clusters").length).toBe(1));
    expect(net.to("/clusters")).toHaveLength(1);
  });
});

/* -------------------------------------------------------------------------- */
/* 3. Every key is reachable by something                                     */
/* -------------------------------------------------------------------------- */

const SRC = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const LIVE = path.join(SRC, "api", "live.tsx");

/** Every `.ts`/`.tsx` under `src/` that ships — no tests, no generated code. */
function sourceFiles(dir: string = SRC): readonly string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name !== "generated" && entry.name !== "test") out.push(...sourceFiles(full));
    } else if (/\.tsx?$/.test(entry.name) && !/\.(test|spec)\.tsx?$/.test(entry.name)) {
      out.push(full);
    }
  }
  return out;
}

/**
 * Every `qk` path the tree invalidates or rewrites.
 *
 * `invalidateQueries()` with no key — the `resync` catch-all — is deliberately
 * not counted. It is the "your incremental state is not trustworthy" path, and
 * a key whose only hope is a resync is exactly the defect being looked for.
 */
function writtenPaths(files: readonly string[]): ReadonlySet<string> {
  const found = new Set<string>();
  const re =
    /(?:invalidateQueries\(\s*\{\s*queryKey:\s*|setQueryData(?:<[^>]*>)?\(\s*)qk\.(\w+)\.(\w+)\s*\(/g;
  for (const file of files) {
    for (const m of readFileSync(file, "utf8").matchAll(re)) found.add(`${m[1]}.${m[2]}`);
  }
  return found;
}

type Factory = (...args: readonly never[]) => readonly unknown[];

/**
 * A concrete key for a factory, with stand-ins for whatever it takes.
 *
 * The arguments never matter: what is being compared is prefixes, and every
 * factory puts its identifying segments before its parameters.
 */
const SAMPLE: readonly unknown[] = ["sample-id", { limit: 1 }];
function keyOf(fn: Factory): readonly unknown[] {
  return fn(...(SAMPLE.slice(0, fn.length) as unknown as readonly never[]));
}

interface Entry {
  readonly path: string;
  readonly group: string;
  readonly key: readonly unknown[];
}

function entries(): readonly Entry[] {
  const out: Entry[] = [];
  for (const [group, factories] of Object.entries(qk as Record<string, Record<string, Factory>>)) {
    for (const [name, fn] of Object.entries(factories)) {
      out.push({ path: `${group}.${name}`, group, key: keyOf(fn) });
    }
  }
  return out;
}

/** Whether invalidating `prefix` reaches `key` — solid-query's own rule. */
function reaches(prefix: readonly unknown[], key: readonly unknown[]): boolean {
  return prefix.length <= key.length && prefix.every((seg, i) => seg === key[i]);
}

describe("every key names one source of freshness", () => {
  const all = entries();
  const files = sourceFiles();
  const byLive = writtenPaths([LIVE]);
  const byMutation = writtenPaths(files.filter((f) => f !== LIVE));

  const prefixesOf = (paths: ReadonlySet<string>): readonly (readonly unknown[])[] =>
    all.filter((e) => paths.has(e.path)).map((e) => e.key);

  it("declares one for every key `qk` can produce, and none for a key it cannot", () => {
    // Both directions: a key added without an answer fails, and an answer left
    // behind by a deleted key fails too, so this table cannot rot into fiction.
    expect(Object.keys(FRESHNESS).sort()).toEqual(all.map((e) => e.path).sort());
  });

  it("puts every key under the prefix its name promises", () => {
    for (const entry of all) {
      expect(
        entry.key[0],
        `\`${entry.path}\` is filed under \`${entry.group}\` but keyed \`${String(entry.key[0])}\` — nothing that invalidates the group would reach it`,
      ).toBe(entry.group);
    }
  });

  it("proves the `live` claim against `api/live.tsx` itself", () => {
    const live = prefixesOf(byLive);
    for (const entry of all) {
      if (FRESHNESS[entry.path]?.by !== "live") continue;
      expect(
        live.some((p) => reaches(p, entry.key)),
        `\`${entry.path}\` claims the stream keeps it fresh, but no frame in \`api/live.tsx\` invalidates a prefix of \`${entry.key.join("/")}\``,
      ).toBe(true);
    }
  });

  it("proves the `mutation` claim against the writes the screens perform", () => {
    const written = prefixesOf(byMutation);
    for (const entry of all) {
      if (FRESHNESS[entry.path]?.by !== "mutation") continue;
      expect(
        written.some((p) => reaches(p, entry.key)),
        `\`${entry.path}\` claims a local write keeps it fresh, but nothing outside \`api/live.tsx\` invalidates or rewrites \`${entry.key.join("/")}\``,
      ).toBe(true);
    }
  });

  it("makes every unreachable key say so, in words", () => {
    // The inverse of the two tests above, and the one that would have caught the
    // dry-run list had it been in `qk` at all: a key nothing reaches is allowed,
    // but only as a stated decision with a stated reason.
    const reachable = [...prefixesOf(byLive), ...prefixesOf(byMutation)];
    for (const entry of all) {
      const freshness = FRESHNESS[entry.path] as Freshness | undefined;
      if (freshness === undefined) continue;
      if (freshness.by === "live" || freshness.by === "mutation") continue;
      expect(
        freshness.why.length,
        `\`${entry.path}\` is exempt from invalidation and does not say why`,
      ).toBeGreaterThan(20);
      // And the exemption must be real: something nothing reaches, rather than
      // an entry quietly opting out of a guarantee it already has.
      expect(
        reachable.some((p) => reaches(p, entry.key)),
        `\`${entry.path}\` is declared \`${freshness.by}\` but something does invalidate it — say so instead`,
      ).toBe(false);
    }
  });

  it("states the number a `bounded` key promises where the query is declared", () => {
    const declared: Readonly<Record<string, number | undefined>> = {
      "labels.names": labelNamesQuery().staleTime,
      "settings.channelTypes": channelTypesQuery().staleTime,
      // Read off the query the drift panel actually mounts. It used to be the
      // literal `DEFAULT_STALE_MS`, which compared the table to itself: the
      // panel could have declared any number, or none, and this still passed.
      "rules.snapshots": ruleSnapshotsQuery({ source_id: "sample-id", rule_name: "sample" })
        .staleTime,
    };
    for (const [path_, freshness] of Object.entries(FRESHNESS)) {
      if (freshness.by !== "bounded") continue;
      expect(
        declared[path_],
        `\`${path_}\` promises ${freshness.ms}ms of staleness that no query declares`,
      ).toBe(freshness.ms);
    }
    // The two named constants are the app's, not this test's.
    expect(labelNamesQuery().staleTime).toBe(REFERENCE_STALE_MS);
    expect(channelTypesQuery().staleTime).toBe(CAPABILITY_STALE_MS);

    // And the factory above is only the truth while the screen uses it: a panel
    // that goes back to declaring its own options makes the number in
    // `FRESHNESS` a claim about nothing again.
    expect(
      readFileSync(path.join(SRC, "features", "alerts", "detail", "RuleDrift.tsx"), "utf8"),
      "the drift panel declares its own snapshot query again, so the `bounded` number above is no longer the one it mounts",
    ).toContain("ruleSnapshotsQuery(");
  });

  it("keeps the dry-run list under the prefix the stream invalidates", () => {
    // The narrow statement of the defect, next to the end-to-end proof above:
    // whatever the recent-alerts list is keyed as, `alert.upserted` must reach it.
    expect(reaches(qk.alerts.all(), recentAlertsQuery().queryKey)).toBe(true);
    expect(clustersQuery().queryKey).toEqual(qk.settings.clusters());
  });

  it("lets the dry-run list go quiet for a minute, and no more than a minute", () => {
    const { staleTime } = recentAlertsQuery();
    // `"static"` is the only staleness solid-query's refetch pass honours, and
    // it is what turns a storm of invalidations into one request. A plain number
    // here would be inert: `invalidateQueries` refetches an active observer
    // whatever its `staleTime` says.
    expect(staleTime({ state: { dataUpdatedAt: Date.now() } })).toBe("static");
    // ...and it has to expire, or the picker is a list nothing can ever refresh
    // — the same silence as the original defect, arrived at from the other side.
    expect(
      staleTime({ state: { dataUpdatedAt: Date.now() - RECENT_ALERTS_QUIET_MS - 1 } }),
    ).toBe(DEFAULT_STALE_MS);
    expect(staleTime({ state: { dataUpdatedAt: 0 } })).toBe(DEFAULT_STALE_MS);
  });
});

describe("no screen writes a query key by hand", () => {
  it("keys every query through `qk`", () => {
    const offenders: string[] = [];
    for (const file of sourceFiles()) {
      for (const m of readFileSync(file, "utf8").matchAll(/queryKey:\s*([^\n,]+)/g)) {
        const value = (m[1] ?? "").trim();
        if (!value.startsWith("qk.")) offenders.push(`${path.relative(SRC, file)}: ${value}`);
      }
    }
    // A literal is how a key escapes the prefix tree in the first place, and it
    // is invisible at the call site: `["policy-preview","recent-alerts"]` reads
    // like a key and behaves like a leak.
    expect(offenders, "a query key that is not from `qk` is a key nothing can invalidate").toEqual(
      [],
    );
  });
});
