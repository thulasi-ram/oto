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
  putItem,
  type LabelSelector,
  type QueryParams,
  type RequestOptions,
} from "./client";
import type { components } from "./generated/schema";
import type {
  ActiveSnooze,
  Alert,
  AlertDetail,
  AlertEvent,
  AlertListQuery,
  AlertRollup,
  AlertRollupQuery,
  ApiToken,
  ApiTokenCreated,
  Channel,
  ChannelConnection,
  ChannelTest,
  ChannelTypeDescriptor,
  Cluster,
  Correlator,
  CreateChannelConnectionRequest,
  CreateChannelRequest,
  CreateClusterRequest,
  CreateCorrelatorRequest,
  CreatePolicyRequest,
  CreateSourceRequest,
  CreateTokenRequest,
  CreateNotificationTemplateRequest,
  DeliveryDrill,
  Delivery,
  Enricher,
  Enrichment,
  FailedBatch,
  FailedBatchListQuery,
  LabelNameRow,
  LabelValueRow,
  LoginRequest,
  ListEnvelope,
  Me,
  Notification,
  NotificationListQuery,
  Case,
  CaseDetail,
  CaseListItem,
  CaseListQuery,
  Incident,
  IncidentDetail,
  IncidentListQuery,
  Investigation,
  InvestigationClassSet,
  InvestigationDetail,
  Investigator,
  OrgSettingsView,
  PayloadMappingCatalogEntry,
  Policy,
  PolicyPreview,
  PolicyPreviewRequest,
  PreviewNotificationTemplateRequest,
  Rejection,
  RejectionListQuery,
  ReplaceInvestigationClassesRequest,
  ResolvedConversation,
  ResolveConversationRequest,
  RuleHistory,
  RuleSnapshot,
  RuleSnapshotQuery,
  Silence,
  SnoozeHistoryEntry,
  SnoozeRequest,
  Source,
  SourceCreated,
  SourceHealth,
  SourceTest,
  StatsOverview,
  TestConnectionMappingRequest,
  TimelineQuery,
  UpdateChannelConnectionRequest,
  UpdateChannelRequest,
  UpdateCorrelatorRequest,
  UpdateOrgSettingsRequest,
  UpdatePolicyRequest,
  UpdateSourceRequest,
  UpdateNotificationTemplateRequest,
  Uuid,
  VersionInfo,
  NotificationTemplate,
  TemplatePreview,
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
 * A bucket is **not** a case: it has no row of its own, nothing to acknowledge
 * and no timeline, and it lives for the duration of one query.
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

export function listAlertCases(
  id: Uuid,
  query: { limit?: number; cursor?: string },
  c: Ctx = {},
): Promise<ListEnvelope<Case>> {
  return getList<Case>(`${V1}/alerts/${id}/cases`, {
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

/**
 * Every enricher result for one alert.
 *
 * `listAlertEnrichments` is one of the **cursorless** collections: the contract
 * declares `EnrichmentListResponse` as `{ data, meta }` with no `page`, because
 * an alert's enrichment set is bounded by the number of enrichers that ran, not
 * by how much history it accumulated. Reading it with `getList` asked for a
 * keyset envelope the contract never promised and turned every enrichment panel
 * into "Something went wrong" — the contract is right, the read was wrong.
 */
export function listAlertEnrichments(id: Uuid, c: Ctx = {}): Promise<readonly Enrichment[]> {
  return getUnpagedList<Enrichment>(`${V1}/alerts/${id}/enrichments`, ctx(c));
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
/* Cases                                                                      */
/* -------------------------------------------------------------------------- */

/**
 * The org-wide case list — what is firing that somebody needs to acknowledge.
 *
 * A Case is ONE contiguous firing episode of ONE alert. This is the only list
 * that can be filtered by acknowledgement: `alerts` carries no ack column,
 * because a receipt belongs to the firing it was given for and the identity
 * outlives that firing.
 *
 * ⭐ SEND `open: true` FOR THE LIVE QUEUE. `state: ["firing","suppressed"]`
 * selects the same rows on paper — the schema's own CHECK proves it — but only
 * `open` reaches the partial indexes, because Postgres matches a partial index
 * against a query's predicates and never against the table's constraints.
 *
 * Every row carries its `alert`, batch-loaded server-side, so the list renders
 * without a request per row.
 */
export function listCases(
  query: CaseListQuery,
  c: Ctx = {},
): Promise<ListEnvelope<CaseListItem>> {
  return getList<CaseListItem>(`${V1}/cases`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/**
 * One firing episode, expanded with the identity it belongs to, the rule as it
 * was at fire time, its enrichment results and its delivery roll-up.
 */
export function getCase(id: Uuid, c: Ctx = {}): Promise<CaseDetail> {
  return getItem<CaseDetail>(`${V1}/cases/${id}`, ctx(c));
}

/**
 * The timeline of ONE episode, rather than of the identity that owns it.
 *
 * This is the difference between "what happened during this firing" and "what
 * has this alert ever done" — the alert's own timeline
 * (`getAlertTimeline`) spans every episode it has had, which is the wrong
 * window when the question is about the one on screen.
 */
export function getCaseTimeline(
  id: Uuid,
  query: TimelineQuery,
  c: Ctx = {},
): Promise<ListEnvelope<AlertEvent>> {
  return getList<AlertEvent>(`${V1}/cases/${id}/events`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/* -------------------------------------------------------------------------- */
/* Case actions                                                               */
/* -------------------------------------------------------------------------- */

/**
 * Acknowledge one firing episode. **`id` is a CASE id, not an alert id.**
 *
 * A receipt is a fact about one contiguous firing: it is stored on the case and
 * is cleared automatically when the next episode of the same alert opens. The
 * alert-addressed spelling this replaces had to resolve "whatever is open right
 * now" server-side, which made the subject of the receipt a race.
 *
 * SCOPE-BOUNDARY: this is a **receipt on a signal** — "a human has seen this".
 * It is not ownership, not an assignment, and an acked case is still firing.
 */
export function ackCase(id: Uuid, note: string | undefined, key: string): Promise<Case> {
  const body = note !== undefined && note !== "" ? { note } : {};
  return postItem<Case>(`${V1}/cases/${id}/ack`, body, { idempotencyKey: key });
}

/** Withdraw a receipt from one episode. Recorded with `reason: manual`. */
export function unackCase(id: Uuid, note: string | undefined, key: string): Promise<Case> {
  const body = note !== undefined && note !== "" ? { note } : {};
  return postItem<Case>(`${V1}/cases/${id}/unack`, body, { idempotencyKey: key });
}

/* -------------------------------------------------------------------------- */
/* Incidents                                                                  */
/* -------------------------------------------------------------------------- */

/**
 * Every Incident in the org, newest first by `number`.
 *
 * Each row carries its DERIVED `state` and the size of its current membership,
 * so the list renders without a request per row. `state` is read off the member
 * Cases by the server on every request (ADR 0052 §3) — it is never a field the
 * UI can send, and nothing below sends it.
 */
export function listIncidents(
  query: IncidentListQuery = {},
  c: Ctx = {},
): Promise<ListEnvelope<Incident>> {
  return getList<Incident>(`${V1}/incidents`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/**
 * The Incident one Case is in NOW, or `null` when it is in none.
 *
 * ⭐ NONE OR ONE, BY CONSTRUCTION. A Case is in at most one Incident (the
 * server's partial unique index), so `?case_id=` answers a list of at most one
 * row and there is nothing to page. It is the read a screen makes BEFORE a draw
 * or an add, so it can say "this Case is in #3 and will be moved" instead of
 * letting the `409 case_in_incident` be the first the operator hears of it
 * (git-bug f89c9cc).
 */
export async function getCaseIncident(caseId: Uuid, c: Ctx = {}): Promise<Incident | null> {
  const page = await listIncidents({ case_id: caseId }, c);
  return page.data[0] ?? null;
}

/**
 * One Incident by the NUMBER a human quotes — never by its id, because the
 * number is what is read out on a call and typed into a URL — with every spell
 * of every Case that has been in it, removed ones included as tombstones.
 */
export function getIncident(number: number, c: Ctx = {}): Promise<IncidentDetail> {
  return getItem<IncidentDetail>(`${V1}/incidents/${number}`, ctx(c));
}

/**
 * Draw an Incident over one or more Cases. The caller is recorded as the human
 * who drew it — actor metadata, never an assignment.
 *
 * ⛔ A CASE ALREADY IN ANOTHER INCIDENT REFUSES THE WHOLE DRAW with `409
 * case_in_incident`, and the problem's `detail` names that Incident and the move
 * that would take the Case out of it. Surface the `detail` verbatim: it is the
 * only place the pointer lives. The key is still minted per gesture, but a retry
 * is safe by construction — the first attempt's memberships refuse the second.
 */
export function createIncident(
  caseIds: readonly Uuid[],
  key: string,
): Promise<IncidentDetail> {
  return postItem<IncidentDetail>(
    `${V1}/incidents`,
    { case_ids: [...caseIds] },
    { idempotencyKey: key },
  );
}

/** Add one Case to an Incident. `409 case_in_incident` points at the move. */
export function addIncidentCase(
  number: number,
  caseId: Uuid,
  key: string,
): Promise<IncidentDetail> {
  return postItem<IncidentDetail>(
    `${V1}/incidents/${number}/cases`,
    { case_id: caseId },
    { idempotencyKey: key },
  );
}

/**
 * Take one Case out of an Incident. The membership is TOMBSTONED, not deleted:
 * the answer still lists the Case, as removed, by whom and when.
 *
 * It is a POST with no body rather than a DELETE because nothing is deleted —
 * the verb is in the path, as `/ack` is.
 */
export function removeIncidentCase(
  number: number,
  caseId: Uuid,
  key: string,
): Promise<IncidentDetail> {
  return postItem<IncidentDetail>(
    `${V1}/incidents/${number}/cases/${caseId}/remove`,
    undefined,
    { idempotencyKey: key },
  );
}

/**
 * Move one Case from Incident `number` to Incident `toNumber`, in one
 * transaction on the server.
 *
 * ⭐ THE ANSWER IS THE DESTINATION, NOT THE INCIDENT IN THE PATH. The path names
 * where the Case is leaving — so a stale screen gets a `404` rather than a move
 * from somewhere the Case no longer is — and the body names where it goes, which
 * is the Incident the operator now wants to be looking at.
 */
export function moveIncidentCase(
  number: number,
  caseId: Uuid,
  toNumber: number,
  key: string,
): Promise<IncidentDetail> {
  return postItem<IncidentDetail>(
    `${V1}/incidents/${number}/cases/${caseId}/move`,
    { to_number: toNumber },
    { idempotencyKey: key },
  );
}

/* -------------------------------------------------------------------------- */
/* Investigations (ADR 0053)                                                  */
/* -------------------------------------------------------------------------- */

/**
 * Every Investigator in the org, each with the version a new Investigation would
 * pin. The server answers the whole set (it is a settings list, never paged), and
 * it includes the switched-off ones: whether one may be asked is the caller's to
 * read off `enabled`, not this function's to hide.
 */
export function listInvestigators(c: Ctx = {}): Promise<ListEnvelope<Investigator>> {
  return getList<Investigator>(`${V1}/investigators`, ctx(c));
}

/**
 * A Case's Investigations, latest requested first, WITHOUT transcripts. The
 * first row is the one a Case shows (ADR 0053 §4); the rest are its history.
 */
export function listCaseInvestigations(
  caseId: Uuid,
  query: { readonly limit?: number } = {},
  c: Ctx = {},
): Promise<ListEnvelope<Investigation>> {
  return getList<Investigation>(`${V1}/cases/${caseId}/investigations`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/**
 * Ask one Investigator to investigate one Case. Answers `202` with the run AS
 * RECORDED — `queued`, or `skipped` with reason `disabled` when a kill switch is
 * off, which is a request taken and refused in the open rather than dropped.
 *
 * ⛔ NO IDEMPOTENCY KEY, BECAUSE THE CONTRACT TAKES NONE. Two presses are two
 * Investigations, each recorded, each with its own tokens; the screen guards the
 * double press by disabling the control while the first is in flight.
 */
export function requestCaseInvestigation(
  caseId: Uuid,
  investigatorId: Uuid,
): Promise<InvestigationDetail> {
  return postItem<InvestigationDetail>(`${V1}/cases/${caseId}/investigations`, {
    investigator_id: investigatorId,
  });
}

/**
 * An Incident's Investigations, latest requested first, WITHOUT transcripts — the
 * runs that looked at the story as a whole (ADR 0053 §4, git-bug 74ea849). Addressed
 * by the number a human quotes, like the Incident itself.
 */
export function listIncidentInvestigations(
  incidentNumber: number,
  query: { readonly limit?: number } = {},
  c: Ctx = {},
): Promise<ListEnvelope<Investigation>> {
  return getList<Investigation>(`${V1}/incidents/${incidentNumber}/investigations`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/**
 * Ask one Investigator to investigate one Incident as a whole. The same `202` and the
 * same absent idempotency key as `requestCaseInvestigation`, for the same reasons.
 */
export function requestIncidentInvestigation(
  incidentNumber: number,
  investigatorId: Uuid,
): Promise<InvestigationDetail> {
  return postItem<InvestigationDetail>(`${V1}/incidents/${incidentNumber}/investigations`, {
    investigator_id: investigatorId,
  });
}

/** One run with its whole transcript and its Finding. Frozen once it has ended. */
export function getInvestigation(id: Uuid, c: Ctx = {}): Promise<InvestigationDetail> {
  return getItem<InvestigationDetail>(`${V1}/investigations/${id}`, ctx(c));
}

/**
 * The org's Classification set (ADR 0053 §5), in the operator's order. Empty until an
 * operator writes one — oto ships no classes.
 */
export function getInvestigationClasses(c: Ctx = {}): Promise<InvestigationClassSet> {
  return getItem<InvestigationClassSet>(`${V1}/investigation-classes`, ctx(c));
}

/**
 * Replace the whole set. An empty list is legal and stops Findings being
 * classified. ⛔ No Finding is rewritten: each keeps the class it was given.
 */
export function replaceInvestigationClasses(
  body: ReplaceInvestigationClassesRequest,
): Promise<InvestigationClassSet> {
  return putItem<InvestigationClassSet>(`${V1}/investigation-classes`, body);
}

/* -------------------------------------------------------------------------- */
/* Alert actions                                                              */
/* -------------------------------------------------------------------------- */

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
 * The account of a bulk wake: one entry per requested id, in request order.
 *
 * ⛔ ALIASED HERE RATHER THAN IN `types.ts` because it is read at exactly one call
 * site — the function below — and an ergonomic alias exists for shapes the UI
 * passes around, not for a return value it destructures immediately.
 */
type UnsnoozeAlertsAccount = components["schemas"]["UnsnoozeAlertsDTO"];

/**
 * Wake several alerts at once, so the **Quiet** tab can resume a selection in one
 * gesture (§B.8).
 *
 * ⛔⛔ IT NAMES THE ALERTS AND THERE IS NO FILTER FORM. A filter is evaluated on the
 * server against rows the caller never saw: one press would resume thousands of
 * alerts whose extent the person pressing it cannot see, and every one of them
 * starts notifying channels nobody agreed to wake. Send the ids that were actually
 * on screen — at most 100, which is one page of the list.
 *
 * ⭐ THE ANSWER IS AN ACCOUNT AND PARTIAL SUCCESS IS A `200`. An alert that was
 * already awake is `outcome: "skipped"` with `reason: "not_snoozed"`, and an id this
 * org does not own is `skipped` / `"alert_not_found"` — neither is an error, so do
 * not treat a non-empty `skipped` as a failed request. Read `results` to say what
 * happened per row; a bare count cannot explain "3 of 5".
 *
 * There is deliberately no bulk SNOOZE counterpart: only the undo is offered.
 */
export function unsnoozeAlerts(
  alertIds: readonly Uuid[],
  note: string | undefined,
  key: string,
): Promise<UnsnoozeAlertsAccount> {
  const body =
    note !== undefined && note !== "" ? { alert_ids: alertIds, note } : { alert_ids: alertIds };
  return postItem<UnsnoozeAlertsAccount>(`${V1}/alerts/unsnooze`, body, { idempotencyKey: key });
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

/**
 * Register an upstream and receive its ingest token.
 *
 * ⛔ THE ANSWER IS NOT A `Source`, AND SAYING SO COST THE UI THE WEBHOOK URL.
 * This used to be `Promise<Source>` — a `SourceDTO`, the object every other
 * function in this block returns — while the handler answers `SourceCreatedDTO`:
 * a *wrapper* carrying `{source, ingest_token, token_prefix, webhook_url}`. The
 * only call site papered over the difference with `as unknown as SourceCreated`,
 * which is precisely the cast that suspends the generated types, so nothing
 * checked that the dialog was reading fields the response has. It was not:
 * `webhook_url` — the one URL nobody has to assemble, because the server builds
 * it from its own `OTO_HTTP_BASE_URL` — went unrendered for as long as the type
 * was wrong.
 */
export function createSource(body: CreateSourceRequest, key: string): Promise<SourceCreated> {
  return postItem<SourceCreated>(`${V1}/sources`, body, { idempotencyKey: key });
}

/**
 * Mint a new ingest token for a source and revoke the old one.
 *
 * ⚠️ THE OLD TOKEN STOPS WORKING IMMEDIATELY, and Alertmanager treats the `401`
 * it then receives as PERMANENT — so every alert it sends between this call and
 * the receiver being reconfigured is lost. Nothing delays the revocation to be
 * kind, which is why the screen that calls this has to say so before it does.
 *
 * The key is minted per gesture like every other one, and a REPLAY is refused
 * rather than replayed: the contract answers a reused key with `409
 * idempotency_key_reuse` and rotates nothing, because a blind retry would revoke
 * the secret the caller may still be holding from the first attempt.
 *
 * Answers the same `SourceCreatedDTO` as `createSource`, so the one-time dialog
 * is the same dialog.
 */
export function rotateSourceIngestToken(id: Uuid, key: string): Promise<SourceCreated> {
  return postItem<SourceCreated>(`${V1}/sources/${id}/rotate-token`, {}, { idempotencyKey: key });
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
 * Everything oto refused from one source, newest first (SPEC AC-6, AC-38).
 *
 * This is the read half of "oto never silently drops". Each row carries the
 * reason, the specifics, and the label set that was refused — already redacted,
 * because that is how it is stored.
 *
 * Keyset, and the cursor is bound to the filter set: a cursor minted under a
 * different `reason` selection is answered `400 cursor_filter_mismatch`, so a
 * caller that changes the filter must drop the cursor rather than reuse it.
 */
export function listSourceRejections(
  id: Uuid,
  query: RejectionListQuery = {},
  c: Ctx = {},
): Promise<ListEnvelope<Rejection>> {
  return getList<Rejection>(`${V1}/sources/${id}/rejections`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

/**
 * The batches from one source that stopped, newest first.
 *
 * The other half of the same question, and not the same fact: a rejection is an
 * alert oto looked at and refused, whereas a failed batch is a payload oto never
 * finished reading — its alerts are on disk and were never seen by anything.
 */
export function listSourceFailedBatches(
  id: Uuid,
  query: FailedBatchListQuery = {},
  c: Ctx = {},
): Promise<ListEnvelope<FailedBatch>> {
  return getList<FailedBatch>(`${V1}/sources/${id}/failed-batches`, {
    ...ctx(c),
    query: query as QueryParams,
  });
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

/**
 * Every connection — the org-wide setup a Settings admin owns. A channel
 * created from the notification policy screen picks one of these; it never
 * carries a credential of its own.
 */
export function listChannelConnections(c: Ctx = {}): Promise<ListEnvelope<ChannelConnection>> {
  return getList<ChannelConnection>(`${V1}/channel-connections`, ctx(c));
}

export function createChannelConnection(
  body: CreateChannelConnectionRequest,
  key: string,
): Promise<ChannelConnection> {
  return postItem<ChannelConnection>(`${V1}/channel-connections`, body, { idempotencyKey: key });
}

export function updateChannelConnection(
  id: Uuid,
  body: UpdateChannelConnectionRequest,
): Promise<ChannelConnection> {
  return patchItem<ChannelConnection>(`${V1}/channel-connections/${id}`, body);
}

export function deleteChannelConnection(id: Uuid): Promise<void> {
  return del(`${V1}/channel-connections/${id}`);
}

/**
 * Send one fact through a webhook connection's payload mapping, by way of one of
 * its channels. ⚠️ It may open a real incident in the tool the mapping points at.
 */
export function testChannelConnectionMapping(
  id: Uuid,
  body: TestConnectionMappingRequest,
  key: string,
): Promise<ChannelTest> {
  return postItem<ChannelTest>(`${V1}/channel-connections/${id}/mapping/test`, body, {
    idempotencyKey: key,
  });
}

/**
 * The payload-mapping catalog embedded in this oto (ADR 0055 §2). There is no
 * import call: importing is copying an entry's `mapping` into a connection's own
 * through the ordinary connection update.
 */
export function listPayloadMappingCatalog(
  c: Ctx = {},
): Promise<readonly PayloadMappingCatalogEntry[]> {
  return getUnpagedList<PayloadMappingCatalogEntry>(`${V1}/payload-mapping-catalog`, ctx(c));
}

/**
 * Ask a Slack connection for the other half of one channel: a name resolves
 * to its id, or the reverse. Backed by `conversations.list`/`.info` against
 * the connection's own bot token — settings-time metadata lookup, not oto
 * reading Slack back to reconstruct its own delivery state.
 */
export function resolveSlackConversation(
  connectionID: Uuid,
  body: ResolveConversationRequest,
): Promise<ResolvedConversation> {
  return postItem<ResolvedConversation>(`${V1}/channel-connections/${connectionID}/slack/resolve`, body);
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

/* -------------------------------------------------------------------------- */
/* Correlators                                                                */
/* -------------------------------------------------------------------------- */

/**
 * Every live Correlator, IN THE ORDER THE SERVER WALKS THEM — `priority`
 * ascending, then age, then id. The settings list renders this order as-is; it
 * must never re-sort, or "why did that one draw it?" stops being answerable from
 * the screen. Never paged: `page.has_more` is always false.
 */
export function listCorrelators(c: Ctx = {}): Promise<ListEnvelope<Correlator>> {
  return getList<Correlator>(`${V1}/correlators`, ctx(c));
}

/** Write a Correlator. A duplicate live name is a `409`. */
export function createCorrelator(body: CreateCorrelatorRequest): Promise<Correlator> {
  return postItem<Correlator>(`${V1}/correlators`, body);
}

/**
 * Change a Correlator. REORDERING IS A `priority` CHANGE, as for a policy. A
 * `null` count half clears it, and both halves must be cleared together.
 */
export function updateCorrelator(id: Uuid, body: UpdateCorrelatorRequest): Promise<Correlator> {
  return patchItem<Correlator>(`${V1}/correlators/${id}`, body);
}

/** Retire a Correlator. The Incidents it drew keep naming it. */
export function deleteCorrelator(id: Uuid): Promise<void> {
  return del(`${V1}/correlators/${id}`);
}

/**
 * The dry run: "given this alert, who is told, where, and rendered how."
 * Runs the real matcher and the real renderer, and sends nothing.
 */
export function previewPolicy(body: PolicyPreviewRequest, c: Ctx = {}): Promise<PolicyPreview> {
  return postItem<PolicyPreview>(`${V1}/notification-policies/preview`, body, ctx(c));
}

/* -------------------------------------------------------------------------- */
/* Notification templates — one whole message, in the operator's own words    */
/* -------------------------------------------------------------------------- */

/** Every NotificationTemplate in the org, newest first. */
export function listNotificationTemplates(
  c: Ctx = {},
): Promise<ListEnvelope<NotificationTemplate>> {
  return getList<NotificationTemplate>(`${V1}/notification-templates`, ctx(c));
}

/**
 * ⛔ NO `Idempotency-Key`, DELIBERATELY, and the handler says why: a template is
 * settings, so a retried create leaves a duplicate row an operator can see and
 * delete rather than a second message a human has already read.
 */
export function createNotificationTemplate(
  body: CreateNotificationTemplateRequest,
): Promise<NotificationTemplate> {
  return postItem<NotificationTemplate>(`${V1}/notification-templates`, body);
}

export function getNotificationTemplate(id: Uuid, c: Ctx = {}): Promise<NotificationTemplate> {
  return getItem<NotificationTemplate>(`${V1}/notification-templates/${id}`, ctx(c));
}

export function updateNotificationTemplate(
  id: Uuid,
  body: UpdateNotificationTemplateRequest,
): Promise<NotificationTemplate> {
  return patchItem<NotificationTemplate>(`${V1}/notification-templates/${id}`, body);
}

export function deleteNotificationTemplate(id: Uuid): Promise<void> {
  return del(`${V1}/notification-templates/${id}`);
}

/**
 * The authoring loop: what this template would SAY, on every fixture of the
 * shipped corpus, in every Dialect oto can spell.
 *
 * ⛔ A TEMPLATE THAT FAILS VALIDATION IS STILL A `200` CARRYING `problems`. The
 * refusal and the output belong in the same round trip, so a caller must never
 * treat a non-empty `problems` as an error that replaces the renderings — and
 * must read each problem's `kind`, because `warning` does not refuse anything.
 */
export function previewNotificationTemplate(
  body: PreviewNotificationTemplateRequest,
  c: Ctx = {},
): Promise<TemplatePreview> {
  return postItem<TemplatePreview>(`${V1}/notification-templates/preview`, body, ctx(c));
}

/* -------------------------------------------------------------------------- */
/* Deliveries, silences, enrichers, stats, identity                           */
/* -------------------------------------------------------------------------- */

/**
 * Every notification intent oto has formed, newest first — the activity log.
 *
 * ⛔ SUPPRESSED INTENTS ARE IN THIS LIST, AND THAT IS WHY IT IS WORTH READING.
 * oto records the decision *not* to send with the reason it was taken, so
 * "nobody was told about this" is answerable from the product with `no_policy`
 * or `throttled` beside it rather than being indistinguishable from "nothing
 * happened" (§B.6). `listAlertNotifications` above answers the same question
 * for one alert; this one answers it for the org.
 */
export function listNotifications(
  query: NotificationListQuery,
  c: Ctx = {},
): Promise<ListEnvelope<Notification>> {
  return getList<Notification>(`${V1}/notifications`, {
    ...ctx(c),
    query: query as QueryParams,
  });
}

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

/**
 * The dashboard roll-up for one window.
 *
 * The three parameters are the three the contract declares and the server reads
 * — `since`, `until`, `cluster` (`GET /stats/overview` binds exactly that set and
 * 400s on anything else). This used to take `{ window?: string }`, which no
 * version of the endpoint has ever accepted; passing it would have been rejected
 * rather than narrowed. `cluster` goes on the wire comma-separated, as everywhere
 * else in this file.
 *
 * `since`/`until` bound the alert, group and delivery counts only. The source and
 * channel counts are current state and ignore the window entirely, so calling
 * this with no arguments still costs the full aggregate — it does not become a
 * cheap read by asking for nothing.
 */
export function getStatsOverview(
  query: { since?: string; until?: string; cluster?: string } = {},
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

/* -------------------------------------------------------------------------- */
/* Personal access tokens                                                     */
/* -------------------------------------------------------------------------- */

/*
 * ⛔ ALL THREE ARE SESSION-ONLY, AND THAT IS WHY THEY LIVE IN THE UI AT ALL.
 * The contract declares `security: [sessionCookie]` on each of them, and
 * `internal/identity/api/tokens.go` states the reason: a token cannot enumerate
 * or mint its own siblings. The `oto_session` cookie is `HttpOnly`, so the
 * browser is the only client that can ever hold one — which makes this file the
 * only place these operations can be called from, not merely a convenient one.
 * Before these existed the sole path to a second PAT was `fetch()` typed into a
 * devtools console, because `oto bootstrap` refuses to run twice.
 */

/**
 * The caller's OWN tokens, newest first — never the org's.
 *
 * The narrowing is the service's (`ListTokens` filters to the principal's user),
 * and it is a privacy rule rather than an authorisation one: v1 has no RBAC, and
 * one operator's laptop token still has no business in another's settings screen.
 * The screen says so, because a list that silently showed only some of what its
 * title claims would be worse than one that is honest about its scope.
 */
export function listApiTokens(c: Ctx = {}): Promise<ListEnvelope<ApiToken>> {
  return getList<ApiToken>(`${V1}/api-tokens`, ctx(c));
}

/**
 * Mint one. **The 201 body is the only response in this API that ever carries
 * the secret**, and only its sha256 is stored — a lost token is replaced, never
 * recovered.
 *
 * ⭐ A REUSED KEY IS A `409`, NOT A REPLAY, and unusually the strictness is the
 * safe behaviour rather than the inconvenient one. Replaying would mean the
 * server had kept the secret in the clear under a string the client chose;
 * minting again would hand out a live credential whose secret went to a response
 * that may never have arrived. So the key is minted per submit, as everywhere
 * else, and a retry is a new gesture with a new key.
 */
export function createApiToken(body: CreateTokenRequest, key: string): Promise<ApiTokenCreated> {
  return postItem<ApiTokenCreated>(`${V1}/api-tokens`, body, { idempotencyKey: key });
}

/**
 * Revoke one. Idempotent server-side — revoking twice succeeds and does not move
 * the revocation timestamp.
 *
 * ⚠️ IT IS NOT INSTANT. The credential cache means a revoked token keeps working
 * for up to sixty seconds (contract), so the screen must not tell an operator
 * responding to a leak that the door is already shut.
 */
export function revokeApiToken(id: Uuid): Promise<void> {
  return del(`${V1}/api-tokens/${id}`);
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
