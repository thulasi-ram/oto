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
  type LabelSelector,
  type QueryParams,
  type RequestOptions,
} from "./client";
import type {
  Alert,
  AlertDetail,
  AlertEvent,
  AlertListQuery,
  Channel,
  ChannelTest,
  ChannelTypeDescriptor,
  Cluster,
  CreateChannelRequest,
  CreateClusterRequest,
  CreatePolicyRequest,
  CreateSourceRequest,
  Delivery,
  Enricher,
  Enrichment,
  Group,
  GroupDetail,
  GroupListQuery,
  LabelNameRow,
  LabelValueRow,
  ListEnvelope,
  Me,
  Notification,
  Occurrence,
  Policy,
  PolicyPreview,
  PolicyPreviewRequest,
  RuleHistory,
  Silence,
  Source,
  SourceHealth,
  SourceTest,
  StatsOverview,
  TimelineQuery,
  UpdateChannelRequest,
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
 * `label` is passed separately because it is `style: deepObject` and carries a
 * `!` negation marker OpenAPI cannot express — the generated type sees only
 * `Record<string, string>`.
 */
export function listAlerts(
  query: AlertListQuery,
  label: LabelSelector,
  c: Ctx = {},
): Promise<ListEnvelope<Alert>> {
  return getList<Alert>(`${V1}/alerts`, {
    ...ctx(c),
    query: query as QueryParams,
    label,
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

export function getVersion(c: Ctx = {}): Promise<VersionInfo> {
  return getItem<VersionInfo>(`${V1}/version`, ctx(c));
}
