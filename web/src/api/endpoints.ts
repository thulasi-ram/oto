/**
 * One function per API operation the UI actually calls.
 *
 * Every signature is derived from `operations[...]` in the generated schema, so
 * a contract change that removes a parameter or renames a field breaks this
 * file at compile time rather than at 3am in a browser. Nothing here invents a
 * path or a parameter: if the UI needs something absent from the contract, the
 * answer is an amendment (§N), not a local addition.
 */
import {
  del,
  getItem,
  getList,
  getUnpagedList,
  patchItem,
  postItem,
  postVoid,
  type LabelSelector,
  type QueryParams,
  type RequestOptions,
} from "./client";
import type {
  ActiveSnooze,
  Alert,
  AlertDetail,
  AlertEvent,
  AlertListQuery,
  AlertRollup,
  AlertRollupQuery,
  Channel,
  ChannelTest,
  ChannelTypeDescriptor,
  Cluster,
  CreateChannelRequest,
  CreateClusterRequest,
  CreatePolicyRequest,
  CreateSourceRequest,
  DeliveryDrill,
  Delivery,
  Enricher,
  Enrichment,
  Group,
  GroupDetail,
  GroupListQuery,
  LabelNameRow,
  LabelValueRow,
  LoginRequest,
  ListEnvelope,
  Me,
  Notification,
  Occurrence,
  OrgSettingsView,
  Policy,
  PolicyPreview,
  PolicyPreviewRequest,
  RuleHistory,
  RuleSnapshot,
  RuleSnapshotQuery,
  Silence,
  SnoozeHistoryEntry,
  SnoozeRequest,
  Source,
  SourceHealth,
  SourceTest,
  StatsOverview,
  TimelineQuery,
  UpdateChannelRequest,
  UpdateOrgSettingsRequest,
  UpdatePolicyRequest,
  UpdateSourceRequest,
  Uuid,
  VersionInfo,
} from "./types";

const V1 = "/api/v1";

/** Signals shared by every read: solid-query hands one in, and we honour it. */
interface Ctx {
  readonly signal?: AbortSignal;
}

function ctx(c: Ctx): RequestOptions {
  return c.signal ? { signal: c.signal } : {};
}

/* -------------------------------------------------------------------------- */
/* Alerts                                                                     */
/* -------------------------------------------------------------------------- */

/**
 * The workhorse (§E.3).
 *
 * The UI's label selector travels in `matcher=` (Alertmanager syntax, ADR 0017)
 * because that is the only spelling that can carry `=~`/`!~`. `label[…]` is
 * still a contract parameter and is still passed separately when a caller needs
 * it — it is `style: deepObject` and carries a `!` negation marker OpenAPI
 * cannot express, so the generated type sees only `Record<string, string>`.
 * The two are AND-ed server-side.
 */
export function listAlerts(
  query: AlertListQuery,
  label: LabelSelector = {},
  c: Ctx = {},
): Promise<ListEnvelope<Alert>> {
  return getList<Alert>(`${V1}/alerts`, {
    ...ctx(c),
    query: query as QueryParams,
    label,
  });
}

/**
 * Server-side roll-up of the alert list (§E.3 `listAlertRollups`).
 *
 * This exists so the UI never counts over "whatever happened to load". Every
 * filter `listAlerts` takes is applied here identically and *before* the
 * aggregate, so the buckets always summarise exactly the list beside them.
 *
 * A bucket is **not** an AlertGroup: it has no row, no generation and no chat
 * thread, and it lives for the duration of one query.
 */
export function listAlertRollups(
  query: AlertRollupQuery,
  label: LabelSelector = {},
  c: Ctx = {},
): Promise<ListEnvelope<AlertRollup>> {
  // `label` is `style: deepObject` and cannot travel in the flat query bag, so
  // it is lifted out of the generated shape and rendered as `label[k]=v`.
  const { label: embedded, ...flat } = query;
  return getList<AlertRollup>(`${V1}/alerts/rollups`, {
    ...ctx(c),
    query: flat as QueryParams,
    label: { ...embedded, ...label },
  });
}

export function getAlert(id: Uuid, c: Ctx = {}): Promise<AlertDetail> {
  return getItem<AlertDetail>(`${V1}/alerts/${id}`, ctx(c));
}

export function listAlertOccurrences(
  id: Uuid,
  query: { limit?: number; cursor?: string },
  c: Ctx = {},
): Promise<ListEnvelope<Occurrence>> {
  return getList<Occurrence>(`${V1}/alerts/${id}/occurrences`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/** The timeline. Ordering is `(recorded_at, id)`; `order` picks the direction. */
export function listAlertEvents(
  id: Uuid,
  query: TimelineQuery,
  c: Ctx = {},
): Promise<ListEnvelope<AlertEvent>> {
  return getList<AlertEvent>(`${V1}/alerts/${id}/events`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

export function listAlertEnrichments(id: Uuid, c: Ctx = {}): Promise<ListEnvelope<Enrichment>> {
  return getList<Enrichment>(`${V1}/alerts/${id}/enrichments`, ctx(c));
}

/** The rule as it was at fire time, the drift diff, and every captured version. */
export function getAlertRuleHistory(
  id: Uuid,
  query: { limit?: number } = {},
  c: Ctx = {},
): Promise<RuleHistory> {
  return getItem<RuleHistory>(`${V1}/alerts/${id}/rule`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/**
 * Every captured version of one rule, keyset-paginated over `(captured_at, id)`.
 *
 * `getAlertRuleHistory` embeds at most 200 versions with no cursor, so this is
 * the only way to reach the rest of a heavily edited rule's history. The cursor
 * here is real: following it reaches every capture.
 */
export function listRuleSnapshots(
  query: RuleSnapshotQuery,
  c: Ctx = {},
): Promise<ListEnvelope<RuleSnapshot>> {
  return getList<RuleSnapshot>(`${V1}/rule-snapshots`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/**
 * Resolve a page's worth of snapshot ids in **one** call (ADR 0025).
 *
 * `listAlerts(..., include: ["rule"])` gives each row a `{ id }` reference and
 * nothing more, because `alerts/api` may not name the rules module's types. This
 * is the other half of that join: the alert list renders `expr` in two requests
 * instead of one per row.
 *
 * Two behaviours the caller must not be surprised by, both of them deliberate:
 *
 *   - **The result can be shorter than the request.** Ids that resolve to
 *     nothing are absent rather than an error, so one stale id cannot blank the
 *     whole column. Join by `id`; never by position.
 *   - **Duplicates are fine.** Snapshots are content-addressed, so a page under
 *     one unchanged rule is the same id over and over. It is still worth
 *     deduplicating here — it is what makes the batch small enough to fit in one
 *     call — but it is not required for correctness.
 *
 * Ids beyond the contract's cap are chunked. The chunking is arithmetic, not a
 * fallback: the cap is a URL-length bound, and a truncated request line fails in
 * a way no error message ever reaches the user.
 */
const MAX_SNAPSHOT_IDS_PER_CALL = 100;

export async function batchGetRuleSnapshots(
  ids: readonly Uuid[],
  c: Ctx = {},
): Promise<readonly RuleSnapshot[]> {
  const distinct = [...new Set(ids)].filter((id) => id !== "");
  if (distinct.length === 0) return [];

  const chunks: Uuid[][] = [];
  for (let i = 0; i < distinct.length; i += MAX_SNAPSHOT_IDS_PER_CALL) {
    chunks.push(distinct.slice(i, i + MAX_SNAPSHOT_IDS_PER_CALL));
  }

  const pages = await Promise.all(
    chunks.map((chunk) =>
      getUnpagedList<RuleSnapshot>(`${V1}/rule-snapshots/batch`, {
        ...ctx(c),
        query: { id: chunk } as QueryParams,
      }),
    ),
  );
  return pages.flat();
}

export function listAlertNotifications(
  id: Uuid,
  query: { limit?: number; cursor?: string } = {},
  c: Ctx = {},
): Promise<ListEnvelope<Notification>> {
  return getList<Notification>(`${V1}/alerts/${id}/notifications`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/* -------------------------------------------------------------------------- */
/* Alert actions                                                              */
/* -------------------------------------------------------------------------- */

/**
 * Acknowledge the current occurrence.
 *
 * SCOPE-BOUNDARY: this is a **receipt on a signal** — "a human has seen this".
 * It is not ownership, not an assignment, and an acked alert is still firing.
 */
export function ackAlert(id: Uuid, note: string | undefined, key: string): Promise<Occurrence> {
  const body = note !== undefined && note !== "" ? { note } : {};
  return postItem<Occurrence>(`${V1}/alerts/${id}/ack`, body, { idempotencyKey: key });
}

/** Withdraw a receipt. Recorded with `reason: manual`. */
export function unackAlert(id: Uuid, note: string | undefined, key: string): Promise<Occurrence> {
  const body = note !== undefined && note !== "" ? { note } : {};
  return postItem<Occurrence>(`${V1}/alerts/${id}/unack`, body, { idempotencyKey: key });
}

/** Append an immutable `comment.added` event to the timeline. */
export function commentOnAlert(id: Uuid, body: string, key: string): Promise<AlertEvent> {
  return postItem<AlertEvent>(`${V1}/alerts/${id}/comments`, { body }, { idempotencyKey: key });
}

/**
 * oto's quiet button (§B.8).
 *
 * Snooze suppresses **oto's own notifications** for this alert until a fixed
 * time. It writes nothing into Alertmanager, creates no silence, and changes
 * nothing about the signal: the response still carries the state the snooze did
 * not change, and every surface must keep rendering it that way.
 *
 * The body must carry exactly one of `until` and `duration_seconds`. Neither is
 * a 422, because there is no indefinite snooze and therefore no default window.
 */
export function snoozeAlert(id: Uuid, body: SnoozeRequest, key: string): Promise<AlertDetail> {
  return postItem<AlertDetail>(`${V1}/alerts/${id}/snooze`, body, { idempotencyKey: key });
}

/** End an active snooze early. A not-snoozed alert is a `412`, never a `409`. */
export function unsnoozeAlert(
  id: Uuid,
  note: string | undefined,
  key: string,
): Promise<AlertDetail> {
  const body = note !== undefined && note !== "" ? { note } : {};
  return postItem<AlertDetail>(`${V1}/alerts/${id}/unsnooze`, body, { idempotencyKey: key });
}

/**
 * Every snooze this alert has ever had, newest first.
 *
 * Membership of a snooze is **history, not a boolean**: an ended row survives
 * with who asked, until when, and how it finished. A quiet period nobody can
 * review afterwards is a quiet period nobody is accountable for.
 */
export function listAlertSnoozes(
  id: Uuid,
  query: { limit?: number } = {},
  c: Ctx = {},
): Promise<readonly SnoozeHistoryEntry[]> {
  return getUnpagedList<SnoozeHistoryEntry>(`${V1}/alerts/${id}/snoozes`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/**
 * Every quiet period currently in force across the org, soonest wake-up first.
 *
 * This is the counterweight that makes snoozing safe (§B.8.6). It is a top-level
 * collection rather than a per-alert one because the question it answers — "what
 * is oto not telling us right now?" — has no alert to ask it from: the operator
 * who needs it is looking at a list that seems calm.
 */
export function listActiveSnoozes(
  query: { limit?: number; cursor?: string } = {},
  c: Ctx = {},
): Promise<ListEnvelope<ActiveSnooze>> {
  return getList<ActiveSnooze>(`${V1}/snoozes`, { ...ctx(c), query: query as QueryParams });
}

/* -------------------------------------------------------------------------- */
/* Groups                                                                     */
/* -------------------------------------------------------------------------- */

export function listAlertGroups(
  query: GroupListQuery,
  c: Ctx = {},
): Promise<ListEnvelope<Group>> {
  return getList<Group>(`${V1}/alert-groups`, { ...ctx(c), query: query as QueryParams });
}

export function getAlertGroup(id: Uuid, c: Ctx = {}): Promise<GroupDetail> {
  return getItem<GroupDetail>(`${V1}/alert-groups/${id}`, ctx(c));
}

export function listAlertGroupAlerts(
  id: Uuid,
  query: { limit?: number; cursor?: string } = {},
  c: Ctx = {},
): Promise<ListEnvelope<Alert>> {
  return getList<Alert>(`${V1}/alert-groups/${id}/alerts`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

export function getAlertGroupTimeline(
  id: Uuid,
  query: TimelineQuery,
  c: Ctx = {},
): Promise<ListEnvelope<AlertEvent>> {
  return getList<AlertEvent>(`${V1}/alert-groups/${id}/timeline`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

export function ackAlertGroup(id: Uuid, note: string | undefined, key: string): Promise<GroupDetail> {
  const body = note !== undefined && note !== "" ? { note } : {};
  return postItem<GroupDetail>(`${V1}/alert-groups/${id}/ack`, body, { idempotencyKey: key });
}

export function commentOnAlertGroup(id: Uuid, body: string, key: string): Promise<AlertEvent> {
  return postItem<AlertEvent>(
    `${V1}/alert-groups/${id}/comments`,
    { body },
    { idempotencyKey: key },
  );
}

/**
 * Snooze every **currently-joined** member. A fan-out of the per-alert
 * primitive, not a new one.
 *
 * Alerts that join after the request are not snoozed: a group-level mute that
 * covered future members would silence alerts nobody has ever seen.
 */
export function snoozeAlertGroup(
  id: Uuid,
  body: SnoozeRequest,
  key: string,
): Promise<GroupDetail> {
  return postItem<GroupDetail>(`${V1}/alert-groups/${id}/snooze`, body, { idempotencyKey: key });
}

/** Wake every currently-joined member. Members already awake are skipped, not refused. */
export function unsnoozeAlertGroup(
  id: Uuid,
  note: string | undefined,
  key: string,
): Promise<GroupDetail> {
  const body = note !== undefined && note !== "" ? { note } : {};
  return postItem<GroupDetail>(`${V1}/alert-groups/${id}/unsnooze`, body, {
    idempotencyKey: key,
  });
}

/* -------------------------------------------------------------------------- */
/* Labels — the typeahead behind the matcher input                            */
/* -------------------------------------------------------------------------- */

export function listLabelNames(c: Ctx = {}): Promise<readonly LabelNameRow[]> {
  return getUnpagedList<LabelNameRow>(`${V1}/labels`, ctx(c));
}

export function listLabelValues(
  name: string,
  query: { q?: string; limit?: number } = {},
  c: Ctx = {},
): Promise<readonly LabelValueRow[]> {
  return getUnpagedList<LabelValueRow>(`${V1}/labels/${encodeURIComponent(name)}/values`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/* -------------------------------------------------------------------------- */
/* Settings — clusters, sources, channels, policies                           */
/* -------------------------------------------------------------------------- */

export function listClusters(c: Ctx = {}): Promise<ListEnvelope<Cluster>> {
  return getList<Cluster>(`${V1}/clusters`, ctx(c));
}

export function createCluster(body: CreateClusterRequest, key: string): Promise<Cluster> {
  return postItem<Cluster>(`${V1}/clusters`, body, { idempotencyKey: key });
}

export function updateCluster(id: Uuid, body: { display_name?: string }): Promise<Cluster> {
  return patchItem<Cluster>(`${V1}/clusters/${id}`, body);
}

export function listSources(c: Ctx = {}): Promise<ListEnvelope<Source>> {
  return getList<Source>(`${V1}/sources`, ctx(c));
}

export function getSource(id: Uuid, c: Ctx = {}): Promise<Source> {
  return getItem<Source>(`${V1}/sources/${id}`, ctx(c));
}

export function createSource(body: CreateSourceRequest, key: string): Promise<Source> {
  return postItem<Source>(`${V1}/sources`, body, { idempotencyKey: key });
}

export function updateSource(id: Uuid, body: UpdateSourceRequest): Promise<Source> {
  return patchItem<Source>(`${V1}/sources/${id}`, body);
}

export function deleteSource(id: Uuid): Promise<void> {
  return del(`${V1}/sources/${id}`);
}

/** Probe the upstream. Same client and same auth as the real reconciler. */
export function testSource(id: Uuid): Promise<SourceTest> {
  return postItem<SourceTest>(`${V1}/sources/${id}/test`, {});
}

/**
 * Push one synthetic alert through the REAL pipeline and get back its staged
 * result.
 *
 * Not `testSource` (which probes the upstream) and not `testChannel` (which
 * renders a card and hands it to the provider). This runs ingestion, alert
 * identity, grouping, the policy match, threading, the ordering gate and the
 * delivery record — the stages where every real failure lives.
 *
 * Answers 202 with everything still `pending`: poll `getDeliveryDrill`.
 */
export function startDeliveryDrill(sourceID: Uuid, severity?: string): Promise<DeliveryDrill> {
  return postItem<DeliveryDrill>(`${V1}/drills`, {
    source_id: sourceID,
    ...(severity ? { severity } : {}),
  });
}

/** Poll one drill. Settled drills return their frozen verdict unchanged. */
export function getDeliveryDrill(id: Uuid, c: Ctx = {}): Promise<DeliveryDrill> {
  return getItem<DeliveryDrill>(`${V1}/drills/${id}`, ctx(c));
}

/** A source's recent drills, newest first. Uncursored by design. */
export function listDeliveryDrills(sourceID: Uuid, c: Ctx = {}): Promise<readonly DeliveryDrill[]> {
  return getUnpagedList<DeliveryDrill>(
    `${V1}/drills?source_id=${encodeURIComponent(sourceID)}`,
    ctx(c),
  );
}

/**
 * Delete a drill's synthetic alert now, without waiting for the sweep.
 *
 * The receipt survives, which is why this returns the drill rather than void:
 * "deleted" means the fake alert is gone, not the record that a drill ran.
 */
export function disposeDeliveryDrill(id: Uuid): Promise<DeliveryDrill> {
  return getItem<DeliveryDrill>(`${V1}/drills/${id}`, { method: "DELETE" });
}

export function getSourceHealth(id: Uuid, c: Ctx = {}): Promise<SourceHealth> {
  return getItem<SourceHealth>(`${V1}/sources/${id}/health`, ctx(c));
}

/**
 * The provider registry, including each provider's JSON Schema.
 *
 * Those schema bytes are the single source of truth for the channel form: the
 * server validates against them and the UI renders from them, so adding a
 * provider needs no UI code at all.
 */
export function listChannelTypes(c: Ctx = {}): Promise<readonly ChannelTypeDescriptor[]> {
  return getUnpagedList<ChannelTypeDescriptor>(`${V1}/channel-types`, ctx(c));
}

export function listChannels(c: Ctx = {}): Promise<ListEnvelope<Channel>> {
  return getList<Channel>(`${V1}/channels`, ctx(c));
}

export function getChannel(id: Uuid, c: Ctx = {}): Promise<Channel> {
  return getItem<Channel>(`${V1}/channels/${id}`, ctx(c));
}

export function createChannel(body: CreateChannelRequest, key: string): Promise<Channel> {
  return postItem<Channel>(`${V1}/channels`, body, { idempotencyKey: key });
}

export function updateChannel(id: Uuid, body: UpdateChannelRequest): Promise<Channel> {
  return patchItem<Channel>(`${V1}/channels/${id}`, body);
}

export function deleteChannel(id: Uuid): Promise<void> {
  return del(`${V1}/channels/${id}`);
}

/** Send one synthetic card through the real renderer and the real validator. */
export function testChannel(id: Uuid): Promise<ChannelTest> {
  return postItem<ChannelTest>(`${V1}/channels/${id}/test`, {});
}

export function listPolicies(c: Ctx = {}): Promise<ListEnvelope<Policy>> {
  return getList<Policy>(`${V1}/notification-policies`, ctx(c));
}

export function createPolicy(body: CreatePolicyRequest, key: string): Promise<Policy> {
  return postItem<Policy>(`${V1}/notification-policies`, body, { idempotencyKey: key });
}

export function updatePolicy(id: Uuid, body: UpdatePolicyRequest): Promise<Policy> {
  return patchItem<Policy>(`${V1}/notification-policies/${id}`, body);
}

export function deletePolicy(id: Uuid): Promise<void> {
  return del(`${V1}/notification-policies/${id}`);
}

/**
 * The dry run: "given this alert, who is told, where, and rendered how."
 * Runs the real matcher and the real renderer, and sends nothing.
 */
export function previewPolicy(body: PolicyPreviewRequest, c: Ctx = {}): Promise<PolicyPreview> {
  return postItem<PolicyPreview>(`${V1}/notification-policies/preview`, body, ctx(c));
}

/* -------------------------------------------------------------------------- */
/* Deliveries, silences, enrichers, stats, identity                           */
/* -------------------------------------------------------------------------- */

export function listDeliveries(
  query: QueryParams,
  c: Ctx = {},
): Promise<ListEnvelope<Delivery>> {
  return getList<Delivery>(`${V1}/deliveries`, { ...ctx(c), query });
}

export function retryDelivery(id: Uuid, key: string): Promise<Delivery> {
  return postItem<Delivery>(`${V1}/deliveries/${id}/retry`, {}, { idempotencyKey: key });
}

export function listSilences(query: QueryParams, c: Ctx = {}): Promise<ListEnvelope<Silence>> {
  return getList<Silence>(`${V1}/silences`, { ...ctx(c), query });
}

export function listEnrichers(c: Ctx = {}): Promise<readonly Enricher[]> {
  return getUnpagedList<Enricher>(`${V1}/enrichers`, ctx(c));
}

export function getStatsOverview(
  query: { window?: string } = {},
  c: Ctx = {},
): Promise<StatsOverview> {
  return getItem<StatsOverview>(`${V1}/stats/overview`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

export function getCurrentPrincipal(c: Ctx = {}): Promise<Me> {
  return getItem<Me>(`${V1}/me`, ctx(c));
}

/**
 * Exchange a password for the `oto_session` cookie.
 *
 * ⛔ THE RESPONSE IS NOT THE CREDENTIAL. The cookie is, it is `HttpOnly`, and no
 * script in this app can read it or set it — which is the whole point, and also
 * why there is no "am I signed in?" the UI can answer locally. The body is the
 * same `MeResponse` `getCurrentPrincipal` returns, so a successful login seeds
 * the session with no second round trip.
 *
 * No idempotency key: the contract accepts one, but a retried login is not a
 * duplicated side effect the way a retried acknowledgement is, and minting a key
 * per keystroke-driven submit would key the rate limiter's own evidence.
 */
export function login(body: LoginRequest, c: Ctx = {}): Promise<Me> {
  return postItem<Me>(`${V1}/auth/login`, body, ctx(c));
}

/** Revoke the session SERVER-SIDE. 204, and the cookie is gone. */
export function logout(c: Ctx = {}): Promise<void> {
  return postVoid(`${V1}/auth/logout`, ctx(c));
}

/**
 * The org's tuning, each value with its origin and its bounds.
 *
 * All three parts are needed by the screen and none is derivable from another.
 * `settings` is what oto is using; `origins` says whether that came from this
 * org or from the shipped default — two answers that behave identically today
 * and diverge the moment oto's default moves; `bounds` is the same table the
 * server rejects with, which is what lets the form refuse a value before the
 * write instead of guessing after a 422.
 */
export function getOrgSettings(c: Ctx = {}): Promise<OrgSettingsView> {
  return getItem<OrgSettingsView>(`${V1}/org/settings`, ctx(c));
}

/**
 * A **partial** write. An omitted key is left alone; `reset` is the only way to
 * return one to oto's shipped default.
 *
 * Writing the default value back by hand is deliberately NOT the same operation
 * and this function never does it: it records an override that happens to equal
 * today's default, and that override would not follow the default if oto moved
 * it. The API distinguishes the two facts, so the UI must too.
 */
export function updateOrgSettings(body: UpdateOrgSettingsRequest): Promise<OrgSettingsView> {
  return patchItem<OrgSettingsView>(`${V1}/org/settings`, body);
}

export function getVersion(c: Ctx = {}): Promise<VersionInfo> {
  return getItem<VersionInfo>(`${V1}/version`, ctx(c));
}
