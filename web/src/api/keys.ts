/**
 * Query keys, in one place.
 *
 * They are a factory rather than string literals because the live stream
 * invalidates by prefix: an `alert.upserted` frame invalidates `["alerts"]`,
 * which covers every filtered list without the stream needing to know what
 * filters are on screen.
 *
 * ⛔ THE PREFIX IS THE WHOLE MECHANISM, SO A KEY OUTSIDE IT IS NOT A KEY —
 * IT IS A LEAK. A cache entry no invalidation reaches has no way of learning
 * that its data changed, and the failure is silent: the list simply keeps
 * showing what it fetched. `["policy-preview","recent-alerts"]` was a filtered
 * alert list written as a literal outside `["alerts"]`, so the frame that
 * changed it — the one frame the screen existed to react to — went past it.
 *
 * Two rules follow, and `api/queries.ts` is where both are enforced rather than
 * remembered:
 *
 *   1. **A key's group in this object is its first segment.** Everything under
 *      `settings` starts `["settings", …]`, so a key is reachable by the prefix
 *      it reads like. `settings.drill` used to be `["drills", id]` — filed with
 *      the settings keys, invalidated by nothing that invalidates settings.
 *   2. **Every key names one source of freshness** — a stream frame, the
 *      mutation that is the only thing which can change it, a declared poll, or
 *      immutability. `api/queries.ts` holds that declaration for every path
 *      below, and `api/queries.test.tsx` checks each one against what the app
 *      actually invalidates. A key added here with no answer fails there.
 */
import type {
  AlertListQuery,
  AlertRollupQuery,
  FailedBatchListQuery,
  GroupListQuery,
  NotificationListQuery,
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
    /**
     * The short list of recent alerts the policy dry run is offered to run
     * against — a segment of its own, not `list({ limit: 20, … })`.
     *
     * Under `["alerts"]`, so every frame that changes an alert reaches it. Not
     * SHARING an entry with the alerts screen, because this one carries a
     * freshness policy no other alert list wants: it is deliberately quiet for a
     * minute at a time (`recentAlertsQuery`). `staleTime` is per-observer over a
     * shared entry and solid-query treats an entry as static if ANY of its
     * observers says so, so a filtered list that happened to compile to the same
     * query object would inherit the picker's quiet minute and stop refreshing
     * mid-storm — a screen changing another screen's freshness from a distance.
     */
    recent: () => ["alerts", "recent"] as const,
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
  },
  notifications: {
    all: () => ["notifications"] as const,
    /**
     * One page of the org-wide notification activity log.
     *
     * A root of its own rather than a segment under `["alerts"]`, because the
     * feed is not an alert list read another way: a row here is an *intent oto
     * formed*, including every one it suppressed, and the filter axes are the
     * intent's own (which status, which reason, which suppression). Filing it
     * under alerts would mean every `alert.upserted` in a storm invalidated the
     * whole history feed as a side effect of a row moving.
     *
     * The query is in the key because the cursor is minted against the filter
     * set server-side (§E.3) — two selections are two keysets, never one entry.
     */
    list: (query: NotificationListQuery) => ["notifications", "list", query] as const,
  },
  settings: {
    clusters: () => ["settings", "clusters"] as const,
    sources: () => ["settings", "sources"] as const,
    channelTypes: () => ["settings", "channel-types"] as const,
    channels: () => ["settings", "channels"] as const,
    policies: () => ["settings", "policies"] as const,
    /** The org's tuning, its origins and its bounds — one query, one screen. */
    org: () => ["settings", "org"] as const,
    /**
     * One page of a source's rejection feed. The query is part of the key
     * because the cursor is bound to the filter set server-side — two reason
     * selections are two different keysets, never one cache entry.
     */
    rejections: (sourceID: string, query: RejectionListQuery) =>
      ["settings", "sources", sourceID, "rejections", query] as const,
    failedBatches: (sourceID: string, query: FailedBatchListQuery) =>
      ["settings", "sources", sourceID, "failed-batches", query] as const,
    /**
     * The signed-in operator's own personal access tokens.
     *
     * Under `["settings"]` because that is the screen it belongs to, and not
     * under any per-user segment because there is no other user's list to
     * collide with: the server narrows this to the caller, and a sign-out
     * `clear()`s the whole cache (`api/session.tsx`) rather than trusting a key
     * to keep two operators' data apart.
     */
    apiTokens: () => ["settings", "api-tokens"] as const,
    /**
     * One drill, polled while it is still running.
     *
     * Under `["settings","drills"]` and not a root of its own: a drill is a
     * settings-screen object, and a key whose path here disagreed with its
     * prefix would be reachable by an invalidation nobody would think to write.
     */
    drill: (id: string) => ["settings", "drills", id] as const,
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
} as const;
