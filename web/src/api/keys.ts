/**
 * Query keys, in one place.
 *
 * They are a factory rather than string literals because the live stream
 * invalidates by prefix: an `alert.upserted` frame invalidates `["alerts"]`,
 * which covers every filtered list without the stream needing to know what
 * filters are on screen.
 */
import type {
  AlertListQuery,
  AlertRollupQuery,
  FailedBatchListQuery,
  GroupListQuery,
  RejectionListQuery,
  RuleSnapshotQuery,
  TimelineQuery,
} from "./types";

export const qk = {
  alerts: {
    all: () => ["alerts"] as const,
    list: (query: AlertListQuery) => ["alerts", "list", query] as const,
    /**
     * Roll-ups sit under the same `["alerts"]` prefix on purpose: an
     * `alert.upserted` frame changes the counts as surely as it changes a row,
     * and a bucket that keeps a stale count is the exact failure the
     * server-side aggregate exists to prevent.
     */
    rollups: (query: AlertRollupQuery) => ["alerts", "rollups", query] as const,
    detail: (id: string) => ["alerts", "detail", id] as const,
    events: (id: string, query: TimelineQuery) => ["alerts", "events", id, query] as const,
    occurrences: (id: string) => ["alerts", "occurrences", id] as const,
    enrichments: (id: string) => ["alerts", "enrichments", id] as const,
    rule: (id: string) => ["alerts", "rule", id] as const,
    snoozes: (id: string) => ["alerts", "snoozes", id] as const,
    /**
     * Every quiet period in force across the org — the standing banner of §B.8.6.
     *
     * It sits under the `["alerts"]` prefix on purpose: taking a snooze, ending
     * one early and one expiring all change the alert, so the `alert.upserted`
     * frame that invalidates the row invalidates this list with it. A banner that
     * went on naming a hold which had already ended would be worse than no
     * banner, because it would be the one thing on screen nobody could act on.
     */
    activeSnoozes: () => ["alerts", "active-snoozes"] as const,
    notifications: (id: string) => ["alerts", "notifications", id] as const,
  },
  rules: {
    snapshots: (query: RuleSnapshotQuery) => ["rules", "snapshots", query] as const,
    /**
     * The rule text behind a page of alert rows, keyed by the SORTED DISTINCT
     * ids so two pages that happen to share their rules — which content
     * addressing makes the common case — share one cache entry and one request.
     *
     * It sits under `["rules"]` and not `["alerts"]` on purpose: a snapshot is
     * immutable, so an `alert.upserted` frame changes which snapshots the list
     * needs but never what any of them says.
     */
    batch: (ids: readonly string[]) => ["rules", "batch", [...ids].sort().join(",")] as const,
  },
  groups: {
    all: () => ["groups"] as const,
    list: (query: GroupListQuery) => ["groups", "list", query] as const,
    detail: (id: string) => ["groups", "detail", id] as const,
    alerts: (id: string) => ["groups", "alerts", id] as const,
    timeline: (id: string, query: TimelineQuery) => ["groups", "timeline", id, query] as const,
  },
  labels: {
    names: () => ["labels", "names"] as const,
    values: (name: string, q: string) => ["labels", "values", name, q] as const,
  },
  settings: {
    clusters: () => ["settings", "clusters"] as const,
    sources: () => ["settings", "sources"] as const,
    sourceHealth: (id: string) => ["settings", "sources", id, "health"] as const,
    channelTypes: () => ["settings", "channel-types"] as const,
    channels: () => ["settings", "channels"] as const,
    policies: () => ["settings", "policies"] as const,
    /** The org's tuning, its origins and its bounds — one query, one screen. */
    org: () => ["settings", "org"] as const,
    /** A source's recent delivery drills. */
    drills: (sourceID: string) => ["settings", "sources", sourceID, "drills"] as const,
    /**
     * One page of a source's rejection feed. The query is part of the key
     * because the cursor is bound to the filter set server-side — two reason
     * selections are two different keysets, never one cache entry.
     */
    rejections: (sourceID: string, query: RejectionListQuery) =>
      ["settings", "sources", sourceID, "rejections", query] as const,
    failedBatches: (sourceID: string, query: FailedBatchListQuery) =>
      ["settings", "sources", sourceID, "failed-batches", query] as const,
    /** One drill, polled while it is still running. */
    drill: (id: string) => ["drills", id] as const,
  },
  deliveries: {
    all: () => ["deliveries"] as const,
  },
  stats: {
    /**
     * The org-wide dashboard roll-up for one window.
     *
     * The window is part of the key because it is part of the question: two
     * windows are two different aggregates and must never share a cache entry.
     * The default — no window at all — is the shell's, and it is deliberately
     * NOT under any entity prefix: a `source.health` frame changes the roll-up,
     * but the roll-up is twenty-six columns over five tables and refetching it
     * on every reconciler heartbeat would cost more than the sixty-second
     * safety net it already rides.
     */
    overview: (query: Readonly<Record<string, string>> = {}) =>
      ["stats", "overview", query] as const,
  },
  meta: {
    me: () => ["meta", "me"] as const,
    version: () => ["meta", "version"] as const,
  },
} as const;
