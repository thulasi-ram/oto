/**
 * Ergonomic aliases over the generated contract types.
 *
 * `schema.d.ts` is produced by `openapi-typescript` from `api/openapi/openapi.yaml`
 * and is CHECKED IN (SPEC §L.8.1 gate G3): `npm run generate` regenerates it and
 * CI asserts no diff. Nothing in this file may add a field the contract does not
 * have — if the UI needs something that is not here, the answer is an amendment
 * to the contract, not a local type.
 */
import type { components, operations } from "./generated/schema";

type S = components["schemas"];

/* ---- envelopes ---------------------------------------------------------- */

export type Meta = S["Meta"];
export type PageInfo = S["PageInfo"];
export type Problem = S["Problem"];
export type Violation = S["Violation"];

export interface ListEnvelope<T> {
  data: T[];
  page: PageInfo;
  meta: Meta;
}
export interface ItemEnvelope<T> {
  data: T;
  meta: Meta;
}

/* ---- scalars and enums -------------------------------------------------- */

export type Uuid = S["Uuid"];
export type Timestamp = S["Timestamp"];
export type Cursor = S["Cursor"];
export type LabelMap = S["LabelMap"];
export type AnnotationMap = S["AnnotationMap"];

export type State = S["State"];
export type AckState = S["AckState"];
export type GroupState = S["GroupState"];
export type SuppressionReason = S["SuppressionReason"];
export type ResolveReason = S["ResolveReason"];
export type ActorKind = S["ActorKind"];
export type AlertEventType = S["AlertEventType"];
export type NotificationReason = S["NotificationReason"];
export type NotificationStatus = S["NotificationStatus"];
export type NotificationSuppressedReason = S["NotificationSuppressedReason"];
export type DeliveryMode = S["DeliveryMode"];
export type DeliveryStatus = S["DeliveryStatus"];
export type DeliveryErrorClass = S["DeliveryErrorClass"];
export type ChannelType = S["ChannelType"];
export type RendererId = S["RendererId"];
export type Verbosity = S["Verbosity"];
/** Who the ONE unacked reminder addresses. `list` is the only form Slack documents as notifying from a thread. */
export type ReminderMention = S["ReminderMention"];
/** The severity class at or above which a reminder attaches a mention at all. */
export type ReminderMentionSeverity = S["ReminderMentionSeverity"];
export type ChannelHealthStatus = S["ChannelHealthStatus"];
export type SourceKind = S["SourceKind"];
export type SourceHealthStatus = S["SourceHealthStatus"];
export type RuleOrigin = S["RuleOrigin"];
export type MatchConfidence = S["MatchConfidence"];
export type EnrichmentStatus = S["EnrichmentStatus"];
export type SilenceState = S["SilenceState"];
export type MatcherOp = S["MatcherOp"];

/* ---- resources ---------------------------------------------------------- */

export type Alert = S["AlertDTO"];
export type AlertDetail = S["AlertDetailDTO"];
export type AlertRef = S["AlertRefDTO"];
/** One server-side roll-up bucket. **Not** an AlertGroup — it has no row and no thread. */
export type AlertRollup = S["AlertRollupDTO"];
export type Snooze = S["SnoozeDTO"];
/**
 * One row of the org-wide active-snooze list (§B.8.6).
 *
 * `alert` is nullable and the row is listed anyway — a hold whose subject cannot
 * be read is still a hold somebody has to know about, and dropping it would hide
 * exactly what the endpoint exists to surface.
 */
export type ActiveSnooze = S["ActiveSnoozeDTO"];
export type SnoozeHistoryEntry = S["SnoozeHistoryDTO"];
export type SnoozeEndReason = S["SnoozeEndReason"];
export type Occurrence = S["OccurrenceDTO"];
export type OccurrenceDetail = S["OccurrenceDetailDTO"];
export type AlertEvent = S["AlertEventDTO"];
export type Enrichment = S["EnrichmentDTO"];
export type EnrichmentSummary = S["EnrichmentSummaryDTO"];
export type DeliverySummary = S["DeliverySummaryDTO"];
export type Group = S["GroupDTO"];
export type GroupDetail = S["GroupDetailDTO"];
export type GroupRef = S["GroupRefDTO"];
export type RuleSnapshot = S["RuleSnapshotDTO"];
export type RuleChange = S["RuleChangeDTO"];
/**
 * What oto established about **how** an expression changed — a closed union
 * discriminated on `verdict`, so `numbers` is only reachable after narrowing to
 * `numbers_moved`. That is deliberate: `structural` and `uncharacterised` carry
 * no numeric claim and there must be no way to render one from them.
 */
export type RuleExprDiff = S["RuleExprDiffDTO"];
/** One numeric literal that moved. Only ever reached through `numbers_moved`. */
export type RuleExprNumberChange = S["RuleExprNumberChangeDTO"];
export type RuleHistory = S["RuleHistoryDTO"];
export type Cluster = S["ClusterDTO"];
export type Source = S["SourceDTO"];
export type SourceRef = S["SourceRefDTO"];
export type SourceCreated = S["SourceCreatedDTO"];
export type SourceHealth = S["SourceHealthDTO"];
/**
 * What governs one Alertmanager's batching, and **how oto knows** — the three
 * timings in force, each with its provenance, plus the whole resolved route tree
 * they came out of. Read from the source's published configuration; never typed
 * in.
 */
export type RouteTimings = S["RouteTimingsDTO"];
/** One route timing plus where its number came from. */
export type RouteTiming = S["RouteTimingDTO"];
/**
 * One route that DELIVERS to a receiver, resolved: its inherited receiver, its
 * inherited timings, its matcher path, and whether it reaches oto.
 */
export type ReceiverRoute = S["ReceiverRouteDTO"];
/** One route on the path from the top-level route down to a delivering one. */
export type RouteStep = S["RouteStepDTO"];
/** One per-route timing, with the depth on the path that stated it. */
export type InheritedTiming = S["InheritedTimingDTO"];
/**
 * How oto decided which receiver is its own. It is an INFERENCE, never a
 * reading: Alertmanager redacts `webhook_config.url` as `<secret>`, so the URL
 * that carries oto's own source id never reaches oto.
 */
export type ReceiverBasis = S["ReceiverBasis"];
/**
 * `observed` (the source's config states it), `default_applies` (the config is
 * silent, so Alertmanager's documented default governs) or `unknown` (oto could
 * not read the config at all). **`default_applies` must never render as
 * `observed`**: the arithmetic is the same, the operator's next move is not.
 */
export type TimingProvenance = S["TimingProvenance"];
export type SourceTest = S["SourceTestDTO"];
/**
 * One delivery drill: a synthetic alert oto pushed through the REAL pipeline,
 * with a per-stage verdict. `failed_stage` is the field the screen exists for —
 * not "it did not work" but "the policy matched nothing".
 */
export type DeliveryDrill = S["DeliveryDrillDTO"];
export type DrillStage = S["DrillStageDTO"];
export type DrillStageName = S["DrillStageName"];
export type DrillStageStatus = S["DrillStageStatus"];
export type DrillStatus = S["DrillStatus"];
export type DrillDestination = S["DrillDestinationDTO"];
export type ReconcileResult = S["ReconcileResultDTO"];
export type ChannelTypeDescriptor = S["ChannelTypeDTO"];
export type Channel = S["ChannelDTO"];
export type ChannelTest = S["ChannelTestDTO"];
export type Matcher = S["MatcherDTO"];
export type Throttle = S["ThrottleDTO"];
export type Policy = S["PolicyDTO"];
export type PolicyPreview = S["PolicyPreviewDTO"];
export type Notification = S["NotificationDTO"];
export type NotificationDetail = S["NotificationDetailDTO"];
export type Delivery = S["DeliveryDTO"];
export type DeliveryDetail = S["DeliveryDetailDTO"];
export type Silence = S["SilenceDTO"];
export type SilenceDetail = S["SilenceDetailDTO"];
export type LabelNameRow = S["LabelNameDTO"];
export type LabelValueRow = S["LabelValueDTO"];
export type Enricher = S["EnricherDTO"];
export type StatsOverview = S["StatsOverviewDTO"];
export type AlertQuality = S["AlertQualityDTO"];
export type Me = S["MeDTO"];
export type Org = S["OrgDTO"];
export type OrgSettings = S["OrgSettingsDTO"];
/**
 * The effective tuning, plus where each value came from and what the server will
 * accept. The three together are the feature: an effective value with no origin
 * cannot be acted on, because "600 because we chose it" and "600 because that is
 * what oto ships" behave identically today and diverge the moment oto's default
 * moves.
 */
export type OrgSettingsView = S["OrgSettingsViewDTO"];
/** `org` (this org wrote it) or `default` (oto's shipped value is in force). */
export type SettingOrigin = S["SettingOrigin"];
/** One knob's server-enforced range, **with the argument for it**. */
export type SettingBound = S["SettingBoundDTO"];
export type User = S["UserDTO"];
export type ApiToken = S["ApiTokenDTO"];
export type ApiTokenCreated = S["ApiTokenCreatedDTO"];
export type VersionInfo = S["VersionDTO"];

/* ---- requests ----------------------------------------------------------- */

export type AckRequest = S["AckRequest"];
export type UnackRequest = S["UnackRequest"];
export type CommentRequest = S["CommentRequest"];
/** Exactly one of `until` and `duration_seconds`. Both, or neither, is a 422. */
export type SnoozeRequest = S["SnoozeRequest"];
export type UnsnoozeRequest = S["UnsnoozeRequest"];
export type CreateClusterRequest = S["CreateClusterRequest"];
export type UpdateClusterRequest = S["UpdateClusterRequest"];
export type CreateSourceRequest = S["CreateSourceRequest"];
export type UpdateSourceRequest = S["UpdateSourceRequest"];
export type CreateChannelRequest = S["CreateChannelRequest"];
export type UpdateChannelRequest = S["UpdateChannelRequest"];
export type CreatePolicyRequest = S["CreatePolicyRequest"];
export type UpdatePolicyRequest = S["UpdatePolicyRequest"];
export type PolicyPreviewRequest = S["PolicyPreviewRequest"];
/** A partial write: an omitted key is left alone, `reset` returns one to the default. */
export type UpdateOrgSettingsRequest = S["UpdateOrgSettingsRequest"];
export type LoginRequest = S["LoginRequest"];

/* ---- streaming ---------------------------------------------------------- */

export type StreamFrame = S["StreamFrame"];
export type UiEventKind = S["UiEventKind"];
export type AlertUpsertedData = S["AlertUpsertedData"];
export type OccurrenceUpsertedData = S["OccurrenceUpsertedData"];
export type GroupUpsertedData = S["GroupUpsertedData"];
export type EventAppendedData = S["EventAppendedData"];
export type DeliveryUpdatedData = S["DeliveryUpdatedData"];
export type SourceHealthData = S["SourceHealthData"];
export type ResyncData = S["ResyncData"];

/* ---- query shapes taken straight off the operations --------------------- */

export type AlertListQuery = NonNullable<operations["listAlerts"]["parameters"]["query"]>;
/** Every filter `listAlerts` takes, plus the required `group_by` axis. */
export type AlertRollupQuery = NonNullable<operations["listAlertRollups"]["parameters"]["query"]>;
export type RollupAxis = AlertRollupQuery["group_by"];
export type RuleSnapshotQuery = NonNullable<
  operations["listRuleSnapshots"]["parameters"]["query"]
>;
export type GroupListQuery = NonNullable<operations["listAlertGroups"]["parameters"]["query"]>;
export type TimelineQuery = NonNullable<operations["listAlertEvents"]["parameters"]["query"]>;
export type NotificationListQuery = NonNullable<
  operations["listNotifications"]["parameters"]["query"]
>;
export type DeliveryListQuery = NonNullable<operations["listDeliveries"]["parameters"]["query"]>;
export type SilenceListQuery = NonNullable<operations["listSilences"]["parameters"]["query"]>;
export type LabelValueQuery = NonNullable<operations["listLabelValues"]["parameters"]["query"]>;
export type StreamQuery = NonNullable<operations["streamEvents"]["parameters"]["query"]>;

export type { components, operations };
