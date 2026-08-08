/**
 * Ergonomic aliases over the generated contract types.
 *
 * `schema.d.ts` is produced by `openapi-typescript` from `docs/api/openapi.yaml`
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
export type Occurrence = S["OccurrenceDTO"];
export type OccurrenceDetail = S["OccurrenceDetailDTO"];
export type AlertEvent = S["AlertEventDTO"];
export type Enrichment = S["EnrichmentDTO"];
export type EnrichmentSummary = S["EnrichmentSummaryDTO"];
export type DeliverySummary = S["DeliverySummaryDTO"];
export type Group = S["GroupDTO"];
export type GroupDetail = S["GroupDetailDTO"];
export type GroupRef = S["GroupRefDTO"];
export type ChannelThread = S["ChannelThreadDTO"];
export type RuleSnapshot = S["RuleSnapshotDTO"];
export type RuleChange = S["RuleChangeDTO"];
export type RuleHistory = S["RuleHistoryDTO"];
export type Cluster = S["ClusterDTO"];
export type Source = S["SourceDTO"];
export type SourceRef = S["SourceRefDTO"];
export type SourceCreated = S["SourceCreatedDTO"];
export type SourceHealth = S["SourceHealthDTO"];
export type SourceTest = S["SourceTestDTO"];
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
export type User = S["UserDTO"];
export type ApiToken = S["ApiTokenDTO"];
export type ApiTokenCreated = S["ApiTokenCreatedDTO"];
export type VersionInfo = S["VersionDTO"];

/* ---- requests ----------------------------------------------------------- */

export type AckRequest = S["AckRequest"];
export type UnackRequest = S["UnackRequest"];
export type CommentRequest = S["CommentRequest"];
export type CreateClusterRequest = S["CreateClusterRequest"];
export type UpdateClusterRequest = S["UpdateClusterRequest"];
export type CreateSourceRequest = S["CreateSourceRequest"];
export type UpdateSourceRequest = S["UpdateSourceRequest"];
export type CreateChannelRequest = S["CreateChannelRequest"];
export type UpdateChannelRequest = S["UpdateChannelRequest"];
export type CreatePolicyRequest = S["CreatePolicyRequest"];
export type UpdatePolicyRequest = S["UpdatePolicyRequest"];
export type PolicyPreviewRequest = S["PolicyPreviewRequest"];
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
