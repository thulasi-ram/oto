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
  CaseListQuery,
  FailedBatchListQuery,
  IncidentListQuery,
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
    cases: (id: string) => ["alerts", "cases", id] as const,
    enrichments: (id: string) => ["alerts", "enrichments", id] as const,
    rule: (id: string) => ["alerts", "rule", id] as const,
    snoozes: (id: string) => ["alerts", "snoozes", id] as const,
    /**
     * The dead deliveries of one notification, for the retry affordance on the
     * delivery panel.
     *
     * Under `["alerts"]` so `live.tsx`'s `delivery.updated` invalidation already
     * reaches it: a retry that succeeds must make the row it was offered on go
     * away, and a key outside the prefix nothing invalidates is a button that
     * lies about what it did.
     */
    deadDeliveries: (notificationId: string) =>
      ["alerts", "dead-deliveries", notificationId] as const,
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
  /**
   * The firing episodes — a root of its own, and not a segment under
   * `["alerts"]`.
   *
   * ⛔ THE SEPARATION IS THE POINT. `alerts.cases(id)` is one alert's own
   * history and belongs under that alert; this is the ORG-WIDE list of open
   * episodes, filtered by acknowledgement, and it is what the primary screen
   * reads. Filing it under `["alerts"]` would have been reachable — every alert
   * frame invalidates that prefix — and would also have meant an alert list
   * refetch dragging the queue with it and vice versa. It gets its own prefix
   * and `api/live.tsx` invalidates it from the frames that can actually move an
   * episode: `case.upserted` and `event.appended`.
   */
  cases: {
    all: () => ["cases"] as const,
    list: (query: CaseListQuery) => ["cases", "list", query] as const,
    detail: (id: string) => ["cases", "detail", id] as const,
    timeline: (id: string, query: TimelineQuery) => ["cases", "timeline", id, query] as const,
    /**
     * One Case's Investigations, latest first (ADR 0053), and one run with its
     * transcript. Under `["cases"]` rather than a root of their own because a
     * Case is their only subject today, and because the frames that move a Case
     * (`case.upserted`, `event.appended`) are the ones that already reach this
     * prefix. A run that is still `queued` or `running` changes with no frame at
     * all, so the panel that reads these polls while one is in progress — a
     * screen's own safety net, stated there (`InvestigationPanel`).
     */
    investigations: (caseId: string) => ["cases", "investigations", caseId] as const,
    investigation: (id: string) => ["cases", "investigation", id] as const,
    /**
     * One run's Suggestions (ADR 0053 §2, git-bug 8327c00), keyed by the run's id like
     * its detail. Under `["cases"]` with it; applying one invalidates it, and the
     * Incident and policy roots the edit touched.
     */
    suggestions: (investigationId: string) => ["cases", "suggestions", investigationId] as const,
    /**
     * One run's Remedies (ADR 0054, git-bug 4148256), keyed by the run's id like its
     * Suggestions. Another approver, the expiry sweep and the executor all move one with
     * no frame, so the panel that reads it polls while any is open.
     */
    remedies: (investigationId: string) => ["cases", "remedies", investigationId] as const,
  },
  /**
   * Incidents (ADR 0052) — a root of their own, beside `["cases"]` and not under
   * it.
   *
   * ⛔ THE STREAM REACHES THEM EVEN THOUGH NO FRAME IS ABOUT AN INCIDENT. An
   * Incident's `state` is read off its member Cases, so a Case closing is what
   * turns an Incident quiet — and that arrives as `case.upserted`, not as any
   * Incident frame. `api/live.tsx` therefore invalidates this prefix from the
   * frames that move a Case; the membership writes themselves are local and
   * invalidate it in their own handlers.
   *
   * `detail` is keyed by the NUMBER as a string, because that is what the route
   * parameter is and what a human quotes; the id never reaches the URL.
   */
  incidents: {
    all: () => ["incidents"] as const,
    list: (query: IncidentListQuery) => ["incidents", "list", query] as const,
    detail: (number: string) => ["incidents", "detail", number] as const,
    /**
     * The Incident one Case is in now (`?case_id=`), keyed by the CASE id. Under
     * the `["incidents"]` root, so every membership write — and every frame
     * `api/live.tsx` already sends that root — refreshes it with the rest.
     */
    holding: (caseId: string) => ["incidents", "holding", caseId] as const,
    /**
     * One Incident's Investigations, latest first (ADR 0053 §4, git-bug 74ea849),
     * keyed by its NUMBER as `detail` is. Under `["incidents"]`, so the frames that
     * move the Incident's Cases refresh it with the rest; a run in progress is polled
     * by `InvestigationPanel`, as a Case's is. One run's detail stays
     * `qk.cases.investigation` — a run is addressed by its own id, whatever it is about.
     */
    investigations: (number: string) => ["incidents", "investigations", number] as const,
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
    channelConnections: () => ["settings", "channel-connections"] as const,
    mappingCatalog: () => ["settings", "payload-mapping-catalog"] as const,
    policies: () => ["settings", "policies"] as const,
    /**
     * One policy's digest Investigations (review D4). Under `policies()` on purpose:
     * an edit to the policies invalidates the prefix, and with it this list.
     */
    policyInvestigations: (policyId: string) =>
      ["settings", "policies", policyId, "investigations"] as const,
    /**
     * The org's Correlators (ADR 0052 §2), in evaluation order. Only this
     * screen writes them, so a mutation invalidates; no frame is about one.
     */
    correlators: () => ["settings", "correlators"] as const,
    /**
     * The org's Investigators (ADR 0053), read by the Case screen to offer
     * "Investigate". No screen writes one yet — they are configured through the
     * API — so no mutation reaches this, and its staleness is bounded instead.
     */
    investigators: () => ["settings", "investigators"] as const,
    /**
     * The org's Classification set (ADR 0053 §5). Only the Classification section
     * writes it, and its save writes the answer back with `setQueryData`.
     */
    investigationClasses: () => ["settings", "investigation-classes"] as const,
    /**
     * The org's Remedy risk rules and risk model (ADR 0054 §3). Only the Remedy risk
     * section writes them, and its save writes the answer back with `setQueryData`.
     */
    remedyRiskRules: () => ["settings", "remedy-risk-rules"] as const,
    /**
     * The org's model endpoints, read by the Remedy risk section to offer a risk model.
     * No screen writes one yet — they are configured through the API.
     */
    modelProviders: () => ["settings", "model-providers"] as const,
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
  /**
   * The customer's own words — a root of its own, and it has to be.
   *
   * ⛔ THE TWO ENTRIES UNDER IT MUST NOT REACH EACH OTHER, WHICH IS WHY THERE IS
   * NO `all()` HERE. `list` is settings and is invalidated by the writes on the
   * screen that edits it; `preview` is a pure question about three strings and is
   * exempt from invalidation entirely (`FRESHNESS`). An `all()` — or invalidating
   * `["templates"]` instead of `["templates","list"]` — would drag every cached
   * preview into the refetch of a save, which is a POST per keystroke the author
   * ever typed.
   *
   * It is not under `["notifications"]` for the same reason: the activity log's
   * frames invalidate that whole prefix (`api/live.tsx`), and a preview is not a
   * thing a stream frame can change.
   */
  templates: {
    /** Every NotificationTemplate in the org, newest first. */
    list: () => ["templates", "list"] as const,
    /**
     * One candidate template's rendering against the whole fixture corpus.
     *
     * Every argument is in the key because every one is the question: the answer
     * is a pure function of the format and the two bodies, so two drafts are two settled
     * answers rather than one entry that keeps being overwritten. That is what
     * lets an author undo a keystroke and get the previous rendering back without
     * another round trip.
     */
    preview: (format: string, source: string, replySource: string) =>
      ["templates", "preview", format, source, replySource] as const,
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
