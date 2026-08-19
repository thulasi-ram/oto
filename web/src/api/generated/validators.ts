/**
 * GENERATED FILE — DO NOT EDIT BY HAND.
 *
 * Produced by `npm run gen:validators` (web/scripts/gen-validators.mjs) from
 * `api/openapi/openapi.yaml`. This file is checked in, and CI runs
 * `npm run gen:validators:check` and fails on any diff. That is **gate G4 of
 * SPEC §L.8.1**, and it is why no hand-written valibot schema may describe an
 * API response.
 *
 * Editing this file by hand will be reverted by the next generate, and will fail
 * CI in the meantime.
 *
 * Response schemas are `v.looseObject` and request schemas are
 * `v.strictObject` (SPEC §L.8): an additive server change must never break a
 * deployed UI, and a typo in a form must never reach the server. A schema whose
 * name ends in `Request` is a request; everything else is a response.
 *
 * Forms stay hand-written, and each one must `v.pipe` into the matching
 * `*RequestSchema` below as its final gate.
 */
/* eslint-disable */

import * as v from "valibot";

export const UuidSchema = v.pipe(
  v.string(),
  v.uuid(),
);

export const TimestampSchema = v.pipe(
  v.string(),
  v.isoTimestamp(),
);

export const AlertKeySchema = v.pipe(
  v.string(),
  v.regex(/^ak_[0-9a-v]{26}$/),
);

export const SourceFingerprintSchema = v.pipe(
  v.string(),
  v.regex(/^[0-9a-f]{16}$/),
);

export const GroupKeySchema = v.pipe(
  v.string(),
  v.regex(/^gk_[0-9a-v]{26}$/),
);

export const RuleFingerprintSchema = v.pipe(
  v.string(),
  v.regex(/^[0-9a-f]{64}$/),
);

export const ClusterKeySchema = v.pipe(
  v.string(),
  v.regex(/^[a-z0-9][a-z0-9._-]{0,62}$/),
);

export const LabelNameSchema = v.pipe(
  v.string(),
  v.maxLength(1024),
  v.regex(/^[a-zA-Z_][a-zA-Z0-9_]*$/),
);

export const LabelMapSchema = v.pipe(
  v.record(v.string(), v.pipe(
    v.string(),
    v.maxLength(4096),
  )),
  v.check((value) => Object.keys(value).length <= 64, "at most 64 properties allowed"),
);

export const AnnotationMapSchema = v.pipe(
  v.record(v.string(), v.pipe(
    v.string(),
    v.maxLength(16384),
  )),
  v.check((value) => Object.keys(value).length <= 32, "at most 32 properties allowed"),
);

export const CursorSchema = v.pipe(
  v.string(),
  v.maxLength(512),
);

export const StateSchema = v.picklist(["firing", "suppressed", "resolved", "expired"]);

export const CaseStateSchema = v.picklist(["open", "closed"]);

export const AckStateSchema = v.picklist(["unacked", "acked"]);

export const SuppressionReasonSchema = v.nullable(v.picklist(["silence", "inhibition", "mute_time_interval", "active_time_interval"]));

export const ResolveReasonSchema = v.nullable(v.picklist(["upstream", "timeout"]));

export const GroupStateSchema = v.picklist(["open", "closed"]);

export const ActorKindSchema = v.picklist(["system", "ingest", "reconciler", "reaper", "enricher", "notifier", "user", "slack"]);

export const AlertEventTypeSchema = v.picklist(["alert.created", "alert.mutated", "case.opened", "case.reopened", "case.suppressed", "case.unsuppressed", "case.resolved", "case.expired", "case.acknowledged", "case.unacknowledged", "alert.snoozed", "alert.unsnoozed", "group.opened", "group.closed", "group.member_joined", "group.member_left", "rule.snapshot_captured", "rule.definition_changed", "rule.lookup_failed", "enrichment.completed", "enrichment.failed", "notification.created", "notification.suppressed", "delivery.sent", "delivery.updated", "delivery.failed", "delivery.skipped", "delivery.dead", "comment.added", "source.unreachable", "source.recovered", "source.clock_skew"]);

export const NotificationReasonSchema = v.picklist(["fired", "new_alerts", "some_resolved", "all_resolved", "repeat", "suppressed", "unsuppressed", "expired", "refired", "acked", "unacked", "snoozed", "unsnoozed", "enriched", "rule_changed", "comment", "digest"]);

export const NotificationStatusSchema = v.picklist(["pending", "dispatched", "partial", "delivered", "failed", "suppressed"]);

export const NotificationSuppressedReasonSchema = v.nullable(v.picklist(["channel_disabled", "no_policy", "snoozed", "throttled", "verbosity", "duplicate_render"]));

export const DeliveryModeSchema = v.picklist(["post_root", "update_root", "thread_reply", "broadcast_reply"]);

export const DeliveryStatusSchema = v.picklist(["pending", "sending", "sent", "failed", "dead", "skipped"]);

export const DeliveryErrorClassSchema = v.nullable(v.picklist(["retryable", "rate_limited", "permanent", "config_invalid", "auth_expired"]));

export const ChannelTypeSchema = v.picklist(["slack", "webhook"]);

export const RendererIdSchema = v.picklist(["default", "slack.default", "webhook.json"]);

export const VerbositySchema = v.picklist(["all", "status_changes", "firing_and_resolved", "firing_only"]);

export const ChannelHealthStatusSchema = v.picklist(["healthy", "degraded", "auth_failed", "config_invalid", "unknown"]);

export const SourceKindSchema = v.picklist(["alertmanager", "grafana"]);

export const SourceHealthStatusSchema = v.picklist(["healthy", "degraded", "unreachable", "unknown"]);

export const RejectionReasonSchema = v.picklist(["too_many_labels", "label_value_too_large", "label_name_too_large", "labelset_too_large", "too_many_annotations", "annotation_too_large", "annotation_unstorable", "missing_alertname", "invalid_label_name", "invalid_label_value", "timestamp_out_of_window", "too_many_alerts", "body_too_large", "undecodable", "unknown_source"]);

export const FailedBatchStatusSchema = v.picklist(["failed", "partial"]);

export const IngestBatchModeSchema = v.picklist(["push", "reconcile", "synthetic"]);

export const RuleOriginSchema = v.picklist(["prometheus_api", "generator_url", "unavailable"]);

export const MatchConfidenceSchema = v.picklist(["exact", "probable", "ambiguous", "none"]);

export const EnrichmentStatusSchema = v.picklist(["ok", "partial", "skipped", "failed", "timeout"]);

export const EnrichmentPhaseSchema = v.picklist([1, 2]);

export const EnrichmentSubjectKindSchema = v.picklist(["alert", "case", "group"]);

export const SilenceStateSchema = v.picklist(["active", "pending", "expired"]);

export const MatcherOpSchema = v.picklist(["=", "!=", "=~", "!~"]);

export const SlackTsSchema = v.pipe(
  v.string(),
  v.regex(/^[0-9]{10}\.[0-9]{6}$/),
);

export const MetaSchema = v.looseObject({
  "request_id": v.pipe(
    v.string(),
    v.maxLength(64),
  ),
  "elapsed_ms": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  )),
});

export const PageInfoSchema = v.looseObject({
  "next_cursor": v.exactOptional(v.nullable(CursorSchema)),
  "has_more": v.boolean(),
  "limit": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
    v.maxValue(200),
  ),
});

export const ViolationSchema = v.looseObject({
  "field": v.pipe(
    v.string(),
    v.maxLength(512),
  ),
  "code": v.pipe(
    v.string(),
    v.maxLength(64),
  ),
  "message": v.pipe(
    v.string(),
    v.maxLength(500),
  ),
});

export const ProblemSchema = v.looseObject({
  "type": v.pipe(
    v.string(),
    v.url(),
  ),
  "title": v.pipe(
    v.string(),
    v.maxLength(200),
  ),
  "status": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(400),
    v.maxValue(599),
  ),
  "detail": v.exactOptional(v.pipe(
    v.string(),
    v.maxLength(2000),
  )),
  "instance": v.exactOptional(v.string()),
  "code": v.pipe(
    v.string(),
    v.maxLength(64),
  ),
  "request_id": v.pipe(
    v.string(),
    v.maxLength(64),
  ),
  "violations": v.exactOptional(v.pipe(
    v.array(ViolationSchema),
    v.maxLength(200),
  )),
  "retry_after_seconds": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  )),
});

export const SnoozeDTOSchema = v.looseObject({
  "id": UuidSchema,
  "snoozed_at": TimestampSchema,
  "snoozed_until": TimestampSchema,
  "snoozed_by_label": v.pipe(
    v.string(),
    v.maxLength(120),
  ),
  "note": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
  "ended_at": v.exactOptional(v.nullable(TimestampSchema)),
});

export const CaseDTOSchema = v.looseObject({
  "id": UuidSchema,
  "alert_id": UuidSchema,
  "group_id": v.exactOptional(v.nullable(UuidSchema)),
  "seq": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ),
  "state": CaseStateSchema,
  "suppression_reason": v.exactOptional(SuppressionReasonSchema),
  "suppressed_by": v.exactOptional(v.looseObject({
    "silenced_by": v.exactOptional(v.pipe(
      v.array(v.pipe(
        v.string(),
        v.maxLength(128),
      )),
      v.maxLength(64),
    )),
    "inhibited_by": v.exactOptional(v.pipe(
      v.array(v.pipe(
        v.string(),
        v.maxLength(128),
      )),
      v.maxLength(64),
    )),
    "muted_by": v.exactOptional(v.pipe(
      v.array(v.pipe(
        v.string(),
        v.maxLength(128),
      )),
      v.maxLength(64),
    )),
  })),
  "ack_state": AckStateSchema,
  "acked_by_label": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(120),
  ))),
  "acked_at": v.exactOptional(v.nullable(TimestampSchema)),
  "ack_note": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
  "started_at": TimestampSchema,
  "ended_at": v.exactOptional(v.nullable(TimestampSchema)),
  "last_observed_at": TimestampSchema,
  "source_starts_at": TimestampSchema,
  "source_ends_at": v.exactOptional(v.nullable(TimestampSchema)),
  "duration_seconds": v.exactOptional(v.nullable(v.pipe(
    v.number(),
    v.minValue(0),
  ))),
  "resolve_reason": v.exactOptional(ResolveReasonSchema),
  "rule_snapshot_id": v.exactOptional(v.nullable(UuidSchema)),
  "value": v.exactOptional(v.nullable(v.number())),
  "observed_skew_ms": v.pipe(
    v.number(),
    v.integer(),
  ),
});

export const EnrichmentDTOSchema = v.looseObject({
  "id": UuidSchema,
  "subject_kind": EnrichmentSubjectKindSchema,
  "subject_id": UuidSchema,
  "enricher": v.pipe(
    v.string(),
    v.maxLength(64),
    v.regex(/^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$/),
  ),
  "enricher_version": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ),
  "phase": EnrichmentPhaseSchema,
  "status": EnrichmentStatusSchema,
  "payload": v.record(v.string(), v.unknown()),
  "warnings": v.exactOptional(v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(500),
    )),
    v.maxLength(32),
  )),
  "error": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
  "duration_ms": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "from_cache": v.boolean(),
  "computed_at": TimestampSchema,
  "expires_at": v.exactOptional(v.nullable(TimestampSchema)),
});

export const RuleSnapshotRefDTOSchema = v.looseObject({
  "id": UuidSchema,
});

export const AlertDTOSchema = v.looseObject({
  "id": UuidSchema,
  "alert_key": AlertKeySchema,
  "source_fingerprint": SourceFingerprintSchema,
  "alertname": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(1024),
  ),
  "severity": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "namespace": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "service": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "cluster_key": ClusterKeySchema,
  "labels": LabelMapSchema,
  "annotations": AnnotationMapSchema,
  "generator_url": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.url(),
    v.maxLength(8192),
  ))),
  "state": StateSchema,
  "first_seen_at": TimestampSchema,
  "last_seen_at": TimestampSchema,
  "last_state_change_at": TimestampSchema,
  "total_cases": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "flap_score": v.pipe(
    v.number(),
    v.minValue(0),
  ),
  "is_flapping": v.boolean(),
  "synthetic": v.boolean(),
  "snooze": v.exactOptional(v.nullable(SnoozeDTOSchema)),
  "current_case": v.exactOptional(v.nullable(CaseDTOSchema)),
  "enrichments": v.exactOptional(v.nullable(v.pipe(
    v.array(EnrichmentDTOSchema),
    v.maxLength(32),
  ))),
  "rule": v.exactOptional(v.nullable(RuleSnapshotRefDTOSchema)),
});

export const EnrichmentSummaryDTOSchema = v.looseObject({
  "enricher": v.pipe(
    v.string(),
    v.maxLength(64),
  ),
  "enricher_version": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  )),
  "status": EnrichmentStatusSchema,
  "headline": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(500),
  ))),
  "computed_at": TimestampSchema,
});

export const SourceRefDTOSchema = v.looseObject({
  "id": UuidSchema,
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "kind": SourceKindSchema,
  "cluster_key": v.exactOptional(ClusterKeySchema),
  "base_url": v.exactOptional(v.pipe(
    v.string(),
    v.url(),
    v.maxLength(2048),
  )),
  "health_status": v.exactOptional(SourceHealthStatusSchema),
});

export const GroupRefDTOSchema = v.looseObject({
  "id": UuidSchema,
  "group_key": GroupKeySchema,
  "generation": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ),
  "title": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(500),
  ),
  "state": GroupStateSchema,
});

export const DeliverySummaryDTOSchema = v.looseObject({
  "total": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "sent": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "failed": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "dead": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "skipped": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "pending": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "last_error_class": v.exactOptional(DeliveryErrorClassSchema),
  "last_sent_at": v.exactOptional(v.nullable(TimestampSchema)),
});

export const AlertDetailDTOSchema = v.intersect([
  AlertDTOSchema,
  v.looseObject({
    "current_case": v.exactOptional(v.nullable(CaseDTOSchema)),
    "enrichment_summary": v.pipe(
      v.array(EnrichmentSummaryDTOSchema),
      v.maxLength(32),
    ),
    "rule": v.exactOptional(v.nullable(RuleSnapshotRefDTOSchema)),
    "source": v.exactOptional(v.nullable(SourceRefDTOSchema)),
    "group": v.exactOptional(v.nullable(GroupRefDTOSchema)),
    "delivery_summary": DeliverySummaryDTOSchema,
  }),
]);

export const AlertRefDTOSchema = v.looseObject({
  "id": UuidSchema,
  "alert_key": AlertKeySchema,
  "alertname": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(1024),
  ),
  "severity": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "namespace": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "cluster_key": ClusterKeySchema,
  "state": StateSchema,
});

export const RuleSnapshotDTOSchema = v.looseObject({
  "id": UuidSchema,
  "source_id": UuidSchema,
  "rule_fingerprint": RuleFingerprintSchema,
  "rule_file": v.pipe(
    v.string(),
    v.maxLength(4096),
  ),
  "rule_group": v.pipe(
    v.string(),
    v.maxLength(4096),
  ),
  "rule_name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(1024),
  ),
  "expr": v.pipe(
    v.string(),
    v.maxLength(65536),
  ),
  "for_seconds": v.pipe(
    v.number(),
    v.minValue(0),
  ),
  "keep_firing_for_seconds": v.pipe(
    v.number(),
    v.minValue(0),
  ),
  "rule_labels": LabelMapSchema,
  "rule_annotations": AnnotationMapSchema,
  "origin": RuleOriginSchema,
  "prometheus_url": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.url(),
    v.maxLength(2048),
  ))),
  "match_confidence": MatchConfidenceSchema,
  "candidate_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "captured_at": TimestampSchema,
});

export const CaseDetailDTOSchema = v.intersect([
  CaseDTOSchema,
  v.looseObject({
    "alert": v.exactOptional(AlertRefDTOSchema),
    "group": v.exactOptional(v.nullable(GroupRefDTOSchema)),
    "rule": v.exactOptional(v.nullable(RuleSnapshotDTOSchema)),
    "enrichments": v.pipe(
      v.array(EnrichmentDTOSchema),
      v.maxLength(32),
    ),
    "delivery_summary": DeliverySummaryDTOSchema,
  }),
]);

export const CaseListItemDTOSchema = v.intersect([
  CaseDTOSchema,
  v.looseObject({
    "alert": AlertRefDTOSchema,
  }),
]);

export const CasePolicyDTOSchema = v.looseObject({
  "id": UuidSchema,
  "namespace": v.pipe(
    v.string(),
    v.maxLength(1024),
  ),
  "alertname": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(1024),
  ),
  "retention_window_seconds": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
    v.maxValue(86400),
  ),
  "created_at": TimestampSchema,
  "updated_at": TimestampSchema,
});

export const AlertEventDTOSchema = v.looseObject({
  "id": UuidSchema,
  "alert_id": v.exactOptional(v.nullable(UuidSchema)),
  "case_id": v.exactOptional(v.nullable(UuidSchema)),
  "group_id": v.exactOptional(v.nullable(UuidSchema)),
  "type": AlertEventTypeSchema,
  "occurred_at": TimestampSchema,
  "recorded_at": TimestampSchema,
  "actor_kind": ActorKindSchema,
  "actor_id": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(128),
  ))),
  "actor_label": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(200),
  ))),
  "summary": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(500),
  ),
  "payload": v.exactOptional(v.record(v.string(), v.unknown())),
});

export const SnoozeEndReasonSchema = v.picklist(["manual", "expired", "superseded"]);

export const SnoozeHistoryDTOSchema = v.intersect([
  SnoozeDTOSchema,
  v.looseObject({
    "ended_reason": v.exactOptional(v.nullable(SnoozeEndReasonSchema)),
    "ended_by_label": v.exactOptional(v.nullable(v.pipe(
      v.string(),
      v.maxLength(120),
    ))),
    "active": v.boolean(),
  }),
]);

export const ActiveSnoozeDTOSchema = v.intersect([
  SnoozeDTOSchema,
  v.looseObject({
    "alert_id": UuidSchema,
    "alert_key": AlertKeySchema,
    "alert": v.exactOptional(v.nullable(AlertRefDTOSchema)),
    "remaining_seconds": v.pipe(
      v.number(),
      v.minValue(0),
    ),
  }),
]);

export const UnsnoozeOutcomeDTOSchema = v.looseObject({
  "alert_id": UuidSchema,
  "outcome": v.picklist(["woken", "skipped"]),
  "reason": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
});

export const UnsnoozeAlertsDTOSchema = v.looseObject({
  "requested": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
    v.maxValue(100),
  ),
  "woken": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "skipped": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "results": v.pipe(
    v.array(UnsnoozeOutcomeDTOSchema),
    v.maxLength(100),
  ),
});

export const SnoozeRequestSchema = v.strictObject({
  "until": v.exactOptional(v.nullable(TimestampSchema)),
  "duration_seconds": v.exactOptional(v.nullable(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(300),
    v.maxValue(2592000),
  ))),
  "note": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
});

export const UnsnoozeRequestSchema = v.strictObject({
  "note": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
});

export const UnsnoozeAlertsRequestSchema = v.strictObject({
  "alert_ids": v.pipe(
    v.array(UuidSchema),
    v.minLength(1),
    v.maxLength(100),
    v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
  ),
  "note": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
});

export const AlertRollupDTOSchema = v.looseObject({
  "key": v.pipe(
    v.string(),
    v.maxLength(4096),
  ),
  "group_by": v.picklist(["alertname", "namespace", "fingerprint"]),
  "state": StateSchema,
  "total_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "firing_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "suppressed_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "resolved_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "expired_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "flapping_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "severity_counts": v.pipe(
    v.record(v.string(), v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    )),
    v.check((value) => Object.keys(value).length <= 64, "at most 64 properties allowed"),
  ),
  "first_seen_at": TimestampSchema,
  "last_seen_at": TimestampSchema,
});

export const GroupDTOSchema = v.looseObject({
  "id": UuidSchema,
  "group_key": GroupKeySchema,
  "generation": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ),
  "source_id": UuidSchema,
  "cluster_key": ClusterKeySchema,
  "source_group_key": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "receiver": v.pipe(
    v.string(),
    v.maxLength(4096),
  ),
  "group_labels": LabelMapSchema,
  "title": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(500),
  ),
  "state": GroupStateSchema,
  "severity": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "state_version": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ),
  "firing_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "suppressed_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "resolved_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "expired_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "total_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "acked_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "snoozed_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "snoozed_until": v.exactOptional(v.nullable(TimestampSchema)),
  "last_notification_reason": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "first_seen_at": TimestampSchema,
  "last_activity_at": TimestampSchema,
  "closed_at": v.exactOptional(v.nullable(TimestampSchema)),
});

export const GroupDetailDTOSchema = v.intersect([
  GroupDTOSchema,
  v.looseObject({
    "source": v.exactOptional(v.nullable(SourceRefDTOSchema)),
    "severity_counts": v.pipe(
      v.record(v.string(), v.pipe(
        v.number(),
        v.integer(),
        v.minValue(0),
      )),
      v.check((value) => Object.keys(value).length <= 32, "at most 32 properties allowed"),
    ),
    "top_alerts": v.pipe(
      v.array(AlertRefDTOSchema),
      v.maxLength(20),
    ),
    "delivery_summary": DeliverySummaryDTOSchema,
  }),
]);

export const RuleExprNumberChangeDTOSchema = v.looseObject({
  "index": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "previous_value": v.number(),
  "new_value": v.number(),
});

export const RuleExprNumbersMovedDTOSchema = v.looseObject({
  "verdict": v.picklist(["numbers_moved"]),
  "numbers": v.exactOptional(v.pipe(
    v.array(RuleExprNumberChangeDTOSchema),
    v.maxLength(64),
  )),
});

export const RuleExprStructuralDTOSchema = v.looseObject({
  "verdict": v.picklist(["structural"]),
});

export const RuleExprUncharacterisedDTOSchema = v.looseObject({
  "verdict": v.picklist(["uncharacterised"]),
});

export const RuleExprDiffDTOSchema = v.union([
  RuleExprNumbersMovedDTOSchema,
  RuleExprStructuralDTOSchema,
  RuleExprUncharacterisedDTOSchema,
]);

export const RuleChangeDTOSchema = v.looseObject({
  "previous_snapshot_id": UuidSchema,
  "previous_fingerprint": RuleFingerprintSchema,
  "previous_captured_at": TimestampSchema,
  "expr_changed": v.boolean(),
  "previous_expr": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(65536),
  ))),
  "new_expr": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(65536),
  ))),
  "expr_diff": v.exactOptional(v.nullable(RuleExprDiffDTOSchema)),
  "for_changed": v.boolean(),
  "previous_for_seconds": v.exactOptional(v.nullable(v.pipe(
    v.number(),
    v.minValue(0),
  ))),
  "new_for_seconds": v.exactOptional(v.nullable(v.pipe(
    v.number(),
    v.minValue(0),
  ))),
  "label_diff": v.exactOptional(v.pipe(
    v.record(v.string(), v.pipe(
      v.array(v.pipe(
        v.string(),
        v.maxLength(4096),
      )),
      v.minLength(2),
      v.maxLength(2),
    )),
    v.check((value) => Object.keys(value).length <= 64, "at most 64 properties allowed"),
  )),
  "annotation_diff": v.exactOptional(v.pipe(
    v.record(v.string(), v.pipe(
      v.array(v.pipe(
        v.string(),
        v.maxLength(16384),
      )),
      v.minLength(2),
      v.maxLength(2),
    )),
    v.check((value) => Object.keys(value).length <= 32, "at most 32 properties allowed"),
  )),
});

export const RuleHistoryDTOSchema = v.looseObject({
  "rule_key": v.looseObject({
    "source_id": UuidSchema,
    "rule_file": v.exactOptional(v.pipe(
      v.string(),
      v.maxLength(4096),
    )),
    "rule_group": v.exactOptional(v.pipe(
      v.string(),
      v.maxLength(4096),
    )),
    "rule_name": v.pipe(
      v.string(),
      v.minLength(1),
      v.maxLength(1024),
    ),
  }),
  "current": v.nullable(RuleSnapshotDTOSchema),
  "change": v.exactOptional(v.nullable(RuleChangeDTOSchema)),
  "versions": v.pipe(
    v.array(RuleSnapshotDTOSchema),
    v.maxLength(200),
  ),
});

export const ClusterDTOSchema = v.looseObject({
  "id": UuidSchema,
  "cluster_key": ClusterKeySchema,
  "display_name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "source_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "created_at": TimestampSchema,
  "updated_at": TimestampSchema,
});

export const TimingProvenanceSchema = v.picklist(["observed", "default_applies", "unknown"]);

export const RouteTimingDTOSchema = v.looseObject({
  "provenance": TimingProvenanceSchema,
  "value_ms": v.nullable(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  )),
});

export const ReceiverBasisSchema = v.picklist(["sole_webhook", "ambiguous", "no_webhook", "unknown"]);

export const RouteStepDTOSchema = v.looseObject({
  "matchers": v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(512),
    )),
    v.maxLength(64),
  ),
  "deprecated": v.boolean(),
  "continue": v.boolean(),
});

export const InheritedTimingDTOSchema = v.looseObject({
  "provenance": TimingProvenanceSchema,
  "value_ms": v.nullable(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  )),
  "from_depth": v.nullable(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  )),
});

export const ReceiverRouteDTOSchema = v.looseObject({
  "receiver": v.pipe(
    v.string(),
    v.maxLength(256),
  ),
  "path": v.pipe(
    v.array(RouteStepDTOSchema),
    v.minLength(1),
    v.maxLength(16),
  ),
  "group_wait": InheritedTimingDTOSchema,
  "group_interval": InheritedTimingDTOSchema,
  "repeat_interval": InheritedTimingDTOSchema,
  "group_by": v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(256),
    )),
    v.maxLength(64),
  ),
  "group_by_all": v.boolean(),
  "reaches_oto": v.boolean(),
  "unreachable": v.boolean(),
});

export const RouteTimingsDTOSchema = v.looseObject({
  "group_wait": RouteTimingDTOSchema,
  "group_interval": RouteTimingDTOSchema,
  "repeat_interval": RouteTimingDTOSchema,
  "route": v.picklist(["top_level", "oto_receiver"]),
  "child_routes": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "child_routes_with_timings": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "receiver": v.nullable(v.pipe(
    v.string(),
    v.maxLength(256),
  )),
  "receiver_basis": ReceiverBasisSchema,
  "webhook_receivers": v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(256),
    )),
    v.maxLength(256),
  ),
  "routes": v.pipe(
    v.array(ReceiverRouteDTOSchema),
    v.maxLength(64),
  ),
  "routes_agree": v.boolean(),
  "routes_dropped": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "defaults_from_version": v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  )),
  "defaults_verified": v.boolean(),
  "observed_at": v.nullable(TimestampSchema),
});

export const SourceHealthDTOSchema = v.looseObject({
  "source_id": UuidSchema,
  "status": SourceHealthStatusSchema,
  "last_push_at": v.exactOptional(v.nullable(TimestampSchema)),
  "last_reconcile_at": v.exactOptional(v.nullable(TimestampSchema)),
  "last_reconcile_status": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "last_error": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
  "consecutive_failures": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "am_version": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "send_resolved": v.exactOptional(v.nullable(v.boolean())),
  "clock_skew_ms": v.pipe(
    v.number(),
    v.integer(),
  ),
  "divergence_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "route_timings": RouteTimingsDTOSchema,
  "warnings": v.pipe(
    v.array(v.looseObject({
      "code": v.pipe(
        v.string(),
        v.maxLength(64),
      ),
      "message": v.pipe(
        v.string(),
        v.maxLength(500),
      ),
      "subject": v.exactOptional(v.pipe(
        v.string(),
        v.maxLength(256),
      )),
      "since": v.exactOptional(TimestampSchema),
    })),
    v.maxLength(32),
  ),
  "updated_at": TimestampSchema,
});

export const SourceDTOSchema = v.looseObject({
  "id": UuidSchema,
  "cluster_id": UuidSchema,
  "cluster_key": v.exactOptional(ClusterKeySchema),
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "kind": SourceKindSchema,
  "base_url": v.pipe(
    v.string(),
    v.url(),
    v.maxLength(2048),
    v.regex(/^https?:\/\/[^\s]+[^\/]$/),
  ),
  "prometheus_url": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.url(),
    v.maxLength(2048),
    v.regex(/^https?:\/\/[^\s]+[^\/]$/),
  ))),
  "tls_skip_verify": v.boolean(),
  "inject_labels": LabelMapSchema,
  "ignore_labels": v.pipe(
    v.array(LabelNameSchema),
    v.maxLength(64),
    v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
  ),
  "redact_labels": v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(256),
    )),
    v.maxLength(64),
  ),
  "redact_annotations": v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(256),
    )),
    v.maxLength(64),
  ),
  "push_enabled": v.boolean(),
  "reconcile_interval_seconds": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(10),
    v.maxValue(3600),
  ),
  "ingest_path": v.string(),
  "health": v.exactOptional(v.nullable(SourceHealthDTOSchema)),
  "created_at": TimestampSchema,
  "updated_at": TimestampSchema,
});

export const SourceCreatedDTOSchema = v.looseObject({
  "source": SourceDTOSchema,
  "ingest_token": v.pipe(
    v.string(),
    v.minLength(20),
    v.maxLength(200),
    v.regex(/^oto_ingest_[A-Za-z0-9]+$/),
  ),
  "token_prefix": v.exactOptional(v.pipe(
    v.string(),
    v.regex(/^oto_(pat|ingest)_[A-Za-z0-9]{4}$/),
  )),
  "webhook_url": v.exactOptional(v.pipe(
    v.string(),
    v.url(),
  )),
});

export const SourceTestDTOSchema = v.looseObject({
  "ok": v.boolean(),
  "am_version": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "cluster_status": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "cluster_peers": v.exactOptional(v.nullable(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ))),
  "server_time": v.exactOptional(v.nullable(TimestampSchema)),
  "clock_skew_ms": v.exactOptional(v.nullable(v.pipe(
    v.number(),
    v.integer(),
  ))),
  "send_resolved": v.exactOptional(v.pipe(
    v.record(v.string(), v.boolean()),
    v.check((value) => Object.keys(value).length <= 200, "at most 200 properties allowed"),
  )),
  "prometheus_ok": v.exactOptional(v.nullable(v.boolean())),
  "error": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
  "checked_at": TimestampSchema,
});

export const ReconcileResultDTOSchema = v.looseObject({
  "source_id": UuidSchema,
  "ok": v.boolean(),
  "started_at": TimestampSchema,
  "finished_at": TimestampSchema,
  "observed": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "suppressed_observed": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "recovered": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "missing_upstream": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "divergence_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "error": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
});

export const RejectionDTOSchema = v.looseObject({
  "id": UuidSchema,
  "source_id": UuidSchema,
  "batch_id": v.exactOptional(v.nullable(UuidSchema)),
  "received_at": TimestampSchema,
  "reason": RejectionReasonSchema,
  "detail": v.pipe(
    v.string(),
    v.maxLength(2000),
  ),
  "labels": v.record(v.string(), v.string()),
});

export const FailedBatchDTOSchema = v.looseObject({
  "id": UuidSchema,
  "source_id": UuidSchema,
  "mode": IngestBatchModeSchema,
  "received_at": TimestampSchema,
  "status": FailedBatchStatusSchema,
  "processed_at": v.exactOptional(v.nullable(TimestampSchema)),
  "error": v.pipe(
    v.string(),
    v.maxLength(2000),
  ),
  "alert_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "truncated_alerts": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
});

export const ChannelTypeDTOSchema = v.looseObject({
  "type": ChannelTypeSchema,
  "display_name": v.pipe(
    v.string(),
    v.maxLength(120),
  ),
  "config_schema": v.record(v.string(), v.unknown()),
  "credential_kinds": v.pipe(
    v.array(v.picklist(["slack_bot_token", "slack_app_token", "slack_signing_secret", "basic", "bearer", "none"])),
    v.maxLength(6),
  ),
  "capabilities": v.pipe(
    v.array(v.picklist(["threading", "amend", "rich_layout", "interactive", "broadcast", "dedupe_key"])),
    v.maxLength(6),
    v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
  ),
  "renderers": v.pipe(
    v.array(RendererIdSchema),
    v.maxLength(3),
  ),
  "rate_limit_class": v.exactOptional(v.picklist(["slack", "none"])),
});

export const ChannelDTOSchema = v.looseObject({
  "id": UuidSchema,
  "type": ChannelTypeSchema,
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "config": v.record(v.string(), v.unknown()),
  "credential_kind": v.exactOptional(v.nullable(v.picklist(["slack_bot_token", "slack_app_token", "slack_signing_secret", "basic", "bearer", "none"]))),
  "credential_rotated_at": v.exactOptional(v.nullable(TimestampSchema)),
  "renderer": RendererIdSchema,
  "verbosity": VerbositySchema,
  "thread_updates": v.boolean(),
  "show_field_emoji": v.boolean(),
  "enabled": v.boolean(),
  "health_status": ChannelHealthStatusSchema,
  "health_error": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
  "health_checked_at": v.exactOptional(v.nullable(TimestampSchema)),
  "created_at": TimestampSchema,
  "updated_at": TimestampSchema,
});

export const ChannelTestDTOSchema = v.looseObject({
  "ok": v.boolean(),
  "provider_conversation_id": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "provider_message_id": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "permalink": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.url(),
    v.maxLength(3000),
  ))),
  "error": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
  "error_class": v.exactOptional(DeliveryErrorClassSchema),
  "checked_at": TimestampSchema,
});

export const MatcherDTOSchema = v.looseObject({
  "name": LabelNameSchema,
  "op": MatcherOpSchema,
  "value": v.pipe(
    v.string(),
    v.maxLength(4096),
  ),
});

export const ThrottleDTOSchema = v.looseObject({
  "max": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
    v.maxValue(1000),
  ),
  "window_seconds": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(60),
    v.maxValue(86400),
  ),
});

export const PolicyDTOSchema = v.looseObject({
  "id": UuidSchema,
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "priority": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
    v.maxValue(10000),
  ),
  "enabled": v.boolean(),
  "matchers": v.pipe(
    v.array(MatcherDTOSchema),
    v.maxLength(32),
  ),
  "reasons": v.pipe(
    v.array(NotificationReasonSchema),
    v.minLength(1),
    v.maxLength(17),
    v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
  ),
  "channel_ids": v.pipe(
    v.array(UuidSchema),
    v.minLength(1),
    v.maxLength(16),
    v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
  ),
  "throttle": v.exactOptional(v.nullable(ThrottleDTOSchema)),
  "digest_window_seconds": v.exactOptional(v.nullable(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(300),
    v.maxValue(86400),
  ))),
  "digest_floor": v.exactOptional(v.nullable(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
    v.maxValue(10000),
  ))),
  "created_at": TimestampSchema,
  "updated_at": TimestampSchema,
});

export const NotificationDTOSchema = v.looseObject({
  "id": UuidSchema,
  "subject_kind": v.picklist(["alert", "case", "alert_group", "digest"]),
  "subject_id": UuidSchema,
  "group_id": v.nullable(UuidSchema),
  "alert_id": v.exactOptional(v.nullable(UuidSchema)),
  "case_id": v.exactOptional(v.nullable(UuidSchema)),
  "reason": NotificationReasonSchema,
  "policy_id": v.exactOptional(v.nullable(UuidSchema)),
  "state_version": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ),
  "status": NotificationStatusSchema,
  "suppressed_reason": v.exactOptional(NotificationSuppressedReasonSchema),
  "delivery_summary": v.exactOptional(DeliverySummaryDTOSchema),
  "created_at": TimestampSchema,
  "updated_at": v.exactOptional(v.nullable(TimestampSchema)),
});

export const DeliveryDTOSchema = v.looseObject({
  "id": UuidSchema,
  "notification_id": UuidSchema,
  "channel_id": UuidSchema,
  "channel_name": v.exactOptional(v.pipe(
    v.string(),
    v.maxLength(120),
  )),
  "channel_type": v.exactOptional(ChannelTypeSchema),
  "thread_id": v.exactOptional(v.nullable(UuidSchema)),
  "thread_seq": v.exactOptional(v.nullable(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ))),
  "mode": DeliveryModeSchema,
  "status": DeliveryStatusSchema,
  "attempts": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
    v.maxValue(32),
  ),
  "next_attempt_at": v.exactOptional(v.nullable(TimestampSchema)),
  "provider_message_id": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "provider_conversation_id": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "error": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
  "error_class": v.exactOptional(DeliveryErrorClassSchema),
  "ambiguous": v.boolean(),
  "rendered_fallback": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(3000),
  ))),
  "sent_at": v.exactOptional(v.nullable(TimestampSchema)),
  "created_at": TimestampSchema,
  "updated_at": TimestampSchema,
});

export const NotificationDetailDTOSchema = v.intersect([
  NotificationDTOSchema,
  v.looseObject({
    "delivery_summary": DeliverySummaryDTOSchema,
    "deliveries": v.pipe(
      v.array(DeliveryDTOSchema),
      v.maxLength(64),
    ),
    "group": v.exactOptional(v.nullable(GroupRefDTOSchema)),
    "alert": v.exactOptional(v.nullable(AlertRefDTOSchema)),
  }),
]);

export const DeliveryDetailDTOSchema = v.intersect([
  DeliveryDTOSchema,
  v.looseObject({
    "rendered": v.exactOptional(v.nullable(v.record(v.string(), v.unknown()))),
    "rendered_hash": v.exactOptional(v.nullable(v.pipe(
      v.string(),
      v.regex(/^[0-9a-f]{64}$/),
    ))),
    "provider_response": v.exactOptional(v.nullable(v.record(v.string(), v.unknown()))),
    "notification": v.exactOptional(v.nullable(NotificationDTOSchema)),
  }),
]);

export const PolicyPreviewDTOSchema = v.looseObject({
  "matched": v.boolean(),
  "results": v.pipe(
    v.array(v.looseObject({
      "policy_id": UuidSchema,
      "policy_name": v.pipe(
        v.string(),
        v.maxLength(120),
      ),
      "channel_id": UuidSchema,
      "channel_name": v.pipe(
        v.string(),
        v.maxLength(120),
      ),
      "channel_type": ChannelTypeSchema,
      "mode": DeliveryModeSchema,
      "would_send": v.boolean(),
      "suppressed_reason": v.exactOptional(NotificationSuppressedReasonSchema),
      "rendered_fallback": v.exactOptional(v.nullable(v.pipe(
        v.string(),
        v.maxLength(3000),
      ))),
      "rendered": v.exactOptional(v.nullable(v.record(v.string(), v.unknown()))),
    })),
    v.maxLength(64),
  ),
  "warnings": v.exactOptional(v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(500),
    )),
    v.maxLength(32),
  )),
});

export const SilenceMatcherDTOSchema = v.looseObject({
  "name": LabelNameSchema,
  "value": v.pipe(
    v.string(),
    v.maxLength(4096),
  ),
  "is_regex": v.boolean(),
  "is_equal": v.boolean(),
  "op": v.exactOptional(MatcherOpSchema),
});

export const SilenceDTOSchema = v.looseObject({
  "id": UuidSchema,
  "source_id": UuidSchema,
  "source_silence_id": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(128),
  ),
  "matchers": v.pipe(
    v.array(SilenceMatcherDTOSchema),
    v.minLength(1),
    v.maxLength(64),
  ),
  "starts_at": TimestampSchema,
  "ends_at": TimestampSchema,
  "created_by": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(256),
  ),
  "comment": v.pipe(
    v.string(),
    v.maxLength(4096),
  ),
  "annotations": v.exactOptional(AnnotationMapSchema),
  "state": SilenceStateSchema,
  "source_updated_at": v.exactOptional(v.nullable(TimestampSchema)),
  "mirrored_at": TimestampSchema,
  "alertmanager_url": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.url(),
    v.maxLength(2048),
  ))),
});

export const SilenceDetailDTOSchema = v.intersect([
  SilenceDTOSchema,
  v.looseObject({
    "matched_alerts": v.pipe(
      v.array(AlertRefDTOSchema),
      v.maxLength(200),
    ),
    "matched_count": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "source": v.exactOptional(v.nullable(SourceRefDTOSchema)),
  }),
]);

export const LabelNameDTOSchema = v.looseObject({
  "name": LabelNameSchema,
  "alert_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "promoted": v.boolean(),
});

export const LabelValueDTOSchema = v.looseObject({
  "value": v.pipe(
    v.string(),
    v.maxLength(4096),
  ),
  "alert_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
});

export const EnricherDTOSchema = v.looseObject({
  "name": v.pipe(
    v.string(),
    v.maxLength(64),
    v.regex(/^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$/),
  ),
  "version": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ),
  "phase": EnrichmentPhaseSchema,
  "timeout_ms": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  )),
  "enabled": v.boolean(),
  "health_status": v.exactOptional(v.picklist(["healthy", "degraded", "failing", "unknown"])),
});

export const StatsOverviewDTOSchema = v.looseObject({
  "alerts": v.looseObject({
    "firing": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "suppressed": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "resolved": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "expired": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "acked": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "unacked": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "flapping": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
  }),
  "groups": v.looseObject({
    "open": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "closed": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
  }),
  "deliveries": v.looseObject({
    "sent": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "failed": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "dead": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "skipped": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "pending": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "ambiguous": v.exactOptional(v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    )),
  }),
  "sources": v.looseObject({
    "healthy": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "degraded": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "unreachable": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "unknown": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "max_clock_skew_ms": v.exactOptional(v.pipe(
      v.number(),
      v.integer(),
    )),
    "total_divergence": v.exactOptional(v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    )),
  }),
  "channels": v.exactOptional(v.looseObject({
    "healthy": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "degraded": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "auth_failed": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
    "config_invalid": v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
    ),
  })),
  "window": v.exactOptional(v.looseObject({
    "since": TimestampSchema,
    "until": TimestampSchema,
  })),
  "generated_at": TimestampSchema,
});

export const AlertQualityDTOSchema = v.looseObject({
  "alertname": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(1024),
  ),
  "cluster_key": ClusterKeySchema,
  "cases": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "notifications": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "deliveries": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "acked_cases": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "ack_rate": v.pipe(
    v.number(),
    v.minValue(0),
    v.maxValue(1),
  ),
  "auto_resolved": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "expired": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "total_firing_seconds": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "flap_transitions": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "flap_score": v.pipe(
    v.number(),
    v.minValue(0),
  ),
});

export const OrgSettingsDTOSchema = v.looseObject({
  "refire_grace_s": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(600),
    v.maxValue(86400),
  ),
  "resolve_grace_s": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(60),
    v.maxValue(86400),
  ),
  "group_close_delay_s": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(60),
    v.maxValue(86400),
  ),
  "flap_threshold": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(3),
    v.maxValue(100),
  ),
  "flap_window_s": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(300),
    v.maxValue(86400),
  ),
  "flap_digest_interval_s": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(60),
    v.maxValue(86400),
  ),
  "raw_retention_days": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
    v.maxValue(365),
  ),
  "event_retention_months": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
    v.maxValue(120),
  ),
  "default_verbosity": VerbositySchema,
  "broadcast_on_resolved": v.boolean(),
});

export const OrgDTOSchema = v.looseObject({
  "id": UuidSchema,
  "slug": v.pipe(
    v.string(),
    v.regex(/^[a-z0-9][a-z0-9-]{1,62}$/),
  ),
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(200),
  ),
  "settings": OrgSettingsDTOSchema,
});

export const UserDTOSchema = v.looseObject({
  "id": UuidSchema,
  "email": v.pipe(
    v.string(),
    v.email(),
    v.maxLength(254),
  ),
  "display_name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "slack_user_id": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.regex(/^[UW][A-Z0-9]{2,}$/),
  ))),
});

export const SearchDTOSchema = v.looseObject({
  "partial_match_enabled": v.boolean(),
});

export const MeDTOSchema = v.looseObject({
  "principal_kind": v.picklist(["user", "pat"]),
  "user": v.exactOptional(v.nullable(UserDTOSchema)),
  "org": OrgDTOSchema,
  "token_id": v.exactOptional(v.nullable(UuidSchema)),
  "session_expires_at": v.exactOptional(v.nullable(TimestampSchema)),
  "search": SearchDTOSchema,
});

export const ApiTokenDTOSchema = v.looseObject({
  "id": UuidSchema,
  "kind": v.picklist(["pat", "ingest"]),
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "prefix": v.pipe(
    v.string(),
    v.regex(/^oto_(pat|ingest)_[A-Za-z0-9]{4}$/),
  ),
  "source_id": v.exactOptional(v.nullable(UuidSchema)),
  "last_used_at": v.exactOptional(v.nullable(TimestampSchema)),
  "expires_at": v.exactOptional(v.nullable(TimestampSchema)),
  "created_at": TimestampSchema,
  "revoked_at": v.exactOptional(v.nullable(TimestampSchema)),
});

export const ApiTokenCreatedDTOSchema = v.looseObject({
  "token": ApiTokenDTOSchema,
  "secret": v.pipe(
    v.string(),
    v.minLength(20),
    v.maxLength(200),
    v.regex(/^oto_pat_[A-Za-z0-9]+$/),
  ),
});

export const AlertmanagerWebhookAlertSchema = v.looseObject({
  "status": v.picklist(["firing", "resolved"]),
  "labels": v.record(v.string(), v.string()),
  "annotations": v.exactOptional(v.record(v.string(), v.string())),
  "startsAt": v.pipe(
    v.string(),
    v.isoTimestamp(),
  ),
  "endsAt": v.exactOptional(v.string()),
  "generatorURL": v.exactOptional(v.string()),
  "fingerprint": v.exactOptional(v.string()),
});

export const AlertmanagerWebhookPayloadSchema = v.looseObject({
  "version": v.string(),
  "groupKey": v.exactOptional(v.string()),
  "truncatedAlerts": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  )),
  "status": v.picklist(["firing", "resolved"]),
  "receiver": v.exactOptional(v.string()),
  "notification_reason": v.exactOptional(v.string()),
  "groupLabels": v.exactOptional(v.record(v.string(), v.string())),
  "commonLabels": v.exactOptional(v.record(v.string(), v.string())),
  "commonAnnotations": v.exactOptional(v.record(v.string(), v.string())),
  "routeLabels": v.exactOptional(v.record(v.string(), v.string())),
  "externalURL": v.exactOptional(v.string()),
  "alerts": v.array(AlertmanagerWebhookAlertSchema),
});

export const IngestAcceptedDTOSchema = v.looseObject({
  "batch_id": UuidSchema,
  "alert_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
    v.maxValue(10000),
  ),
  "duplicate": v.boolean(),
  "truncated_alerts": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "rejected_alerts": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
});

export const UiEventKindSchema = v.picklist(["alert.upserted", "case.upserted", "group.upserted", "event.appended", "delivery.updated", "source.health", "resync"]);

export const UiEventResourceSchema = v.picklist(["alert", "case", "group", "alert_event", "delivery", "source"]);

export const AlertUpsertedDataSchema = v.looseObject({
  "state": StateSchema,
  "severity": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "alertname": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(1024),
  ),
  "namespace": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "cluster_key": v.exactOptional(ClusterKeySchema),
  "last_seen_at": TimestampSchema,
  "total_cases": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  )),
  "is_flapping": v.exactOptional(v.boolean()),
});

export const CaseUpsertedDataSchema = v.looseObject({
  "alert_id": UuidSchema,
  "group_id": v.exactOptional(v.nullable(UuidSchema)),
  "seq": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ),
  "state": StateSchema,
  "ack_state": AckStateSchema,
  "started_at": TimestampSchema,
  "ended_at": v.exactOptional(v.nullable(TimestampSchema)),
});

export const GroupUpsertedDataSchema = v.looseObject({
  "state": GroupStateSchema,
  "severity": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(4096),
  ))),
  "firing_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "total_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "acked_count": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  ),
  "last_activity_at": TimestampSchema,
});

export const EventAppendedDataSchema = v.looseObject({
  "alert_id": v.exactOptional(v.nullable(UuidSchema)),
  "case_id": v.exactOptional(v.nullable(UuidSchema)),
  "group_id": v.exactOptional(v.nullable(UuidSchema)),
  "type": AlertEventTypeSchema,
  "occurred_at": TimestampSchema,
  "recorded_at": TimestampSchema,
  "actor_kind": ActorKindSchema,
  "actor_label": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(200),
  ))),
  "summary": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(500),
  ),
});

export const DeliveryUpdatedDataSchema = v.looseObject({
  "notification_id": UuidSchema,
  "channel_id": UuidSchema,
  "mode": DeliveryModeSchema,
  "status": DeliveryStatusSchema,
  "error_class": v.exactOptional(DeliveryErrorClassSchema),
});

export const SourceHealthDataSchema = v.looseObject({
  "status": SourceHealthStatusSchema,
  "last_reconcile_at": v.exactOptional(v.nullable(TimestampSchema)),
  "divergence_count": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
  )),
  "warnings": v.exactOptional(v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(500),
    )),
    v.maxLength(32),
  )),
});

export const ResyncDataSchema = v.looseObject({
  "reason": v.picklist(["buffer_overflow", "replay_window_exceeded"]),
});

export const StreamFrameSchema = v.looseObject({
  "seq": v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
  ),
  "kind": UiEventKindSchema,
  "resource": v.exactOptional(v.nullable(UiEventResourceSchema)),
  "id": v.exactOptional(v.nullable(UuidSchema)),
  "org_id": UuidSchema,
  "at": TimestampSchema,
  "data": v.union([
    AlertUpsertedDataSchema,
    CaseUpsertedDataSchema,
    GroupUpsertedDataSchema,
    EventAppendedDataSchema,
    DeliveryUpdatedDataSchema,
    SourceHealthDataSchema,
    ResyncDataSchema,
  ]),
});

export const HealthDTOSchema = v.looseObject({
  "status": v.picklist(["ok"]),
});

export const ReadyDTOSchema = v.looseObject({
  "status": v.picklist(["ready", "not_ready"]),
  "database": v.picklist(["ok", "unreachable"]),
  "migrations": v.picklist(["applied", "pending", "unknown"]),
  "schema_version": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "detail": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(500),
  ))),
});

export const VersionDTOSchema = v.looseObject({
  "version": v.pipe(
    v.string(),
    v.maxLength(64),
  ),
  "commit": v.pipe(
    v.string(),
    v.maxLength(64),
  ),
  "built_at": v.exactOptional(TimestampSchema),
  "go_version": v.exactOptional(v.pipe(
    v.string(),
    v.maxLength(32),
  )),
  "schema_version": v.pipe(
    v.string(),
    v.maxLength(64),
  ),
});

export const DrillStatusSchema = v.picklist(["running", "passed", "failed", "timed_out"]);

export const DrillStageNameSchema = v.picklist(["accept", "process", "identity", "case", "group", "rule_snapshot", "policy", "thread", "ordering", "delivery"]);

export const DrillStageStatusSchema = v.picklist(["pending", "passed", "failed", "skipped"]);

export const DrillStageDTOSchema = v.looseObject({
  "name": DrillStageNameSchema,
  "status": DrillStageStatusSchema,
  "detail": v.pipe(
    v.string(),
    v.maxLength(2000),
  ),
  "facts": v.exactOptional(v.record(v.string(), v.pipe(
    v.string(),
    v.maxLength(1000),
  ))),
});

export const DrillDestinationDTOSchema = v.looseObject({
  "channel_id": UuidSchema,
  "channel_name": v.pipe(
    v.string(),
    v.maxLength(200),
  ),
  "status": DeliveryStatusSchema,
  "mode": DeliveryModeSchema,
  "thread_id": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "provider_message_id": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(64),
  ))),
  "broadcast": v.boolean(),
  "error": v.exactOptional(v.nullable(v.pipe(
    v.string(),
    v.maxLength(2000),
  ))),
  "error_class": v.exactOptional(DeliveryErrorClassSchema),
});

export const DeliveryDrillDTOSchema = v.looseObject({
  "id": UuidSchema,
  "source_id": UuidSchema,
  "severity": v.pipe(
    v.string(),
    v.maxLength(64),
  ),
  "status": DrillStatusSchema,
  "failed_stage": v.exactOptional(v.nullable(DrillStageNameSchema)),
  "stages": v.pipe(
    v.array(DrillStageDTOSchema),
    v.maxLength(32),
  ),
  "destinations": v.pipe(
    v.array(DrillDestinationDTOSchema),
    v.maxLength(64),
  ),
  "alert_id": v.exactOptional(v.nullable(UuidSchema)),
  "case_id": v.exactOptional(v.nullable(UuidSchema)),
  "group_id": v.exactOptional(v.nullable(UuidSchema)),
  "notification_id": v.exactOptional(v.nullable(UuidSchema)),
  "batch_id": v.exactOptional(v.nullable(UuidSchema)),
  "started_by_label": v.pipe(
    v.string(),
    v.maxLength(200),
  ),
  "started_at": TimestampSchema,
  "deadline_at": TimestampSchema,
  "finished_at": v.exactOptional(v.nullable(TimestampSchema)),
  "disposed_at": v.exactOptional(v.nullable(TimestampSchema)),
});

export const StartDrillRequestSchema = v.strictObject({
  "source_id": UuidSchema,
  "severity": v.exactOptional(v.pipe(
    v.string(),
    v.maxLength(64),
  )),
});

export const AckRequestSchema = v.strictObject({
  "note": v.exactOptional(v.pipe(
    v.string(),
    v.maxLength(2000),
  )),
});

export const UnackRequestSchema = v.strictObject({
  "note": v.exactOptional(v.pipe(
    v.string(),
    v.maxLength(2000),
  )),
});

export const CommentRequestSchema = v.strictObject({
  "body": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(10000),
  ),
});

export const CreateCasePolicyRequestSchema = v.strictObject({
  "namespace": v.exactOptional(v.pipe(
    v.string(),
    v.maxLength(1024),
  ), ""),
  "alertname": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(1024),
  ),
  "retention_window_seconds": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
    v.maxValue(86400),
  ), 0),
});

export const UpdateCasePolicyRequestSchema = v.pipe(
  v.strictObject({
    "retention_window_seconds": v.exactOptional(v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
      v.maxValue(86400),
    )),
  }),
  v.check((value) => Object.keys(value).length >= 1, "at least 1 property required"),
);

export const CreateClusterRequestSchema = v.strictObject({
  "cluster_key": ClusterKeySchema,
  "display_name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
});

export const UpdateClusterRequestSchema = v.pipe(
  v.strictObject({
    "display_name": v.exactOptional(v.pipe(
      v.string(),
      v.minLength(1),
      v.maxLength(120),
    )),
  }),
  v.check((value) => Object.keys(value).length >= 1, "at least 1 property required"),
);

export const CredentialInputSchema = v.looseObject({
  "kind": v.picklist(["slack_bot_token", "slack_app_token", "slack_signing_secret", "basic", "bearer", "none"]),
  "values": v.exactOptional(v.pipe(
    v.record(v.string(), v.pipe(
      v.string(),
      v.maxLength(4096),
    )),
    v.check((value) => Object.keys(value).length <= 8, "at most 8 properties allowed"),
  )),
});

export const CreateSourceRequestSchema = v.strictObject({
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "cluster_id": UuidSchema,
  "kind": SourceKindSchema,
  "base_url": v.pipe(
    v.string(),
    v.url(),
    v.maxLength(2048),
    v.regex(/^https?:\/\/[^\s]+[^\/]$/),
  ),
  "prometheus_url": v.exactOptional(v.pipe(
    v.string(),
    v.url(),
    v.maxLength(2048),
    v.regex(/^https?:\/\/[^\s]+[^\/]$/),
  )),
  "tls_skip_verify": v.exactOptional(v.boolean(), false),
  "inject_labels": v.exactOptional(LabelMapSchema),
  "ignore_labels": v.exactOptional(v.pipe(
    v.array(LabelNameSchema),
    v.maxLength(64),
    v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
  ), () => (["prometheus_replica","__replica__","monitor","replica","pod_template_hash"])),
  "redact_labels": v.exactOptional(v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(256),
    )),
    v.maxLength(64),
  )),
  "redact_annotations": v.exactOptional(v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(256),
    )),
    v.maxLength(64),
  )),
  "push_enabled": v.exactOptional(v.boolean(), true),
  "reconcile_interval_seconds": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(10),
    v.maxValue(3600),
  ), 30),
  "credential": v.exactOptional(CredentialInputSchema),
});

export const UpdateSourceRequestSchema = v.pipe(
  v.strictObject({
    "name": v.exactOptional(v.pipe(
      v.string(),
      v.minLength(1),
      v.maxLength(120),
    )),
    "cluster_id": v.exactOptional(UuidSchema),
    "base_url": v.exactOptional(v.pipe(
      v.string(),
      v.url(),
      v.maxLength(2048),
      v.regex(/^https?:\/\/[^\s]+[^\/]$/),
    )),
    "prometheus_url": v.exactOptional(v.nullable(v.pipe(
      v.string(),
      v.url(),
      v.maxLength(2048),
      v.regex(/^https?:\/\/[^\s]+[^\/]$/),
    ))),
    "tls_skip_verify": v.exactOptional(v.boolean()),
    "inject_labels": v.exactOptional(LabelMapSchema),
    "ignore_labels": v.exactOptional(v.pipe(
      v.array(LabelNameSchema),
      v.maxLength(64),
      v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
    )),
    "redact_labels": v.exactOptional(v.pipe(
      v.array(v.pipe(
        v.string(),
        v.maxLength(256),
      )),
      v.maxLength(64),
    )),
    "redact_annotations": v.exactOptional(v.pipe(
      v.array(v.pipe(
        v.string(),
        v.maxLength(256),
      )),
      v.maxLength(64),
    )),
    "push_enabled": v.exactOptional(v.boolean()),
    "reconcile_interval_seconds": v.exactOptional(v.pipe(
      v.number(),
      v.integer(),
      v.minValue(10),
      v.maxValue(3600),
    )),
    "credential": v.exactOptional(CredentialInputSchema),
  }),
  v.check((value) => Object.keys(value).length >= 1, "at least 1 property required"),
);

export const CreateChannelRequestSchema = v.strictObject({
  "type": ChannelTypeSchema,
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "config": v.record(v.string(), v.unknown()),
  "credential": v.exactOptional(CredentialInputSchema),
  "renderer": v.exactOptional(RendererIdSchema),
  "verbosity": v.exactOptional(VerbositySchema),
  "thread_updates": v.exactOptional(v.boolean(), true),
  "show_field_emoji": v.exactOptional(v.boolean(), true),
  "enabled": v.exactOptional(v.boolean(), true),
});

export const UpdateChannelRequestSchema = v.pipe(
  v.strictObject({
    "name": v.exactOptional(v.pipe(
      v.string(),
      v.minLength(1),
      v.maxLength(120),
    )),
    "config": v.exactOptional(v.record(v.string(), v.unknown())),
    "credential": v.exactOptional(CredentialInputSchema),
    "renderer": v.exactOptional(RendererIdSchema),
    "verbosity": v.exactOptional(VerbositySchema),
    "thread_updates": v.exactOptional(v.boolean()),
    "show_field_emoji": v.exactOptional(v.boolean()),
    "enabled": v.exactOptional(v.boolean()),
  }),
  v.check((value) => Object.keys(value).length >= 1, "at least 1 property required"),
);

export const CreatePolicyRequestSchema = v.strictObject({
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "priority": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(0),
    v.maxValue(10000),
  ), 100),
  "enabled": v.exactOptional(v.boolean(), true),
  "matchers": v.exactOptional(v.pipe(
    v.array(MatcherDTOSchema),
    v.maxLength(32),
  )),
  "reasons": v.pipe(
    v.array(NotificationReasonSchema),
    v.minLength(1),
    v.maxLength(17),
    v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
  ),
  "channel_ids": v.pipe(
    v.array(UuidSchema),
    v.minLength(1),
    v.maxLength(16),
    v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
  ),
  "throttle": v.exactOptional(ThrottleDTOSchema),
  "digest_window_seconds": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(300),
    v.maxValue(86400),
  )),
  "digest_floor": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
    v.maxValue(10000),
  )),
});

export const UpdatePolicyRequestSchema = v.pipe(
  v.strictObject({
    "name": v.exactOptional(v.pipe(
      v.string(),
      v.minLength(1),
      v.maxLength(120),
    )),
    "priority": v.exactOptional(v.pipe(
      v.number(),
      v.integer(),
      v.minValue(0),
      v.maxValue(10000),
    )),
    "enabled": v.exactOptional(v.boolean()),
    "matchers": v.exactOptional(v.pipe(
      v.array(MatcherDTOSchema),
      v.maxLength(32),
    )),
    "reasons": v.exactOptional(v.pipe(
      v.array(NotificationReasonSchema),
      v.minLength(1),
      v.maxLength(17),
      v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
    )),
    "channel_ids": v.exactOptional(v.pipe(
      v.array(UuidSchema),
      v.minLength(1),
      v.maxLength(16),
      v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
    )),
    "throttle": v.exactOptional(v.nullable(ThrottleDTOSchema)),
    "digest_window_seconds": v.exactOptional(v.nullable(v.pipe(
      v.number(),
      v.integer(),
      v.minValue(300),
      v.maxValue(86400),
    ))),
    "digest_floor": v.exactOptional(v.nullable(v.pipe(
      v.number(),
      v.integer(),
      v.minValue(1),
      v.maxValue(10000),
    ))),
  }),
  v.check((value) => Object.keys(value).length >= 1, "at least 1 property required"),
);

export const PolicyPreviewRequestSchema = v.strictObject({
  "alert_id": v.exactOptional(UuidSchema),
  "case_id": v.exactOptional(UuidSchema),
  "group_id": v.exactOptional(UuidSchema),
  "reason": v.exactOptional(NotificationReasonSchema),
  "policy": v.exactOptional(CreatePolicyRequestSchema),
});

export const LoginRequestSchema = v.strictObject({
  "email": v.pipe(
    v.string(),
    v.email(),
    v.maxLength(254),
  ),
  "password": v.pipe(
    v.string(),
    v.minLength(8),
    v.maxLength(1024),
  ),
});

export const CreateTokenRequestSchema = v.strictObject({
  "name": v.pipe(
    v.string(),
    v.minLength(1),
    v.maxLength(120),
  ),
  "expires_at": v.exactOptional(TimestampSchema),
});

export const DeliveryDrillResponseSchema = v.looseObject({
  "data": DeliveryDrillDTOSchema,
  "meta": MetaSchema,
});

export const DeliveryDrillListResponseSchema = v.looseObject({
  "data": v.array(DeliveryDrillDTOSchema),
  "meta": MetaSchema,
});

export const AlertListResponseSchema = v.looseObject({
  "data": v.array(AlertDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const AlertResponseSchema = v.looseObject({
  "data": AlertDetailDTOSchema,
  "meta": MetaSchema,
});

export const AlertRollupListResponseSchema = v.looseObject({
  "data": v.array(AlertRollupDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const UnsnoozeAlertsResponseSchema = v.looseObject({
  "data": UnsnoozeAlertsDTOSchema,
  "meta": MetaSchema,
});

export const SnoozeHistoryResponseSchema = v.looseObject({
  "data": v.pipe(
    v.array(SnoozeHistoryDTOSchema),
    v.maxLength(200),
  ),
  "meta": MetaSchema,
});

export const ActiveSnoozeListResponseSchema = v.looseObject({
  "data": v.array(ActiveSnoozeDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const CaseListResponseSchema = v.looseObject({
  "data": v.array(CaseDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const CaseListItemListResponseSchema = v.looseObject({
  "data": v.array(CaseListItemDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const CaseResponseSchema = v.looseObject({
  "data": CaseDTOSchema,
  "meta": MetaSchema,
});

export const CaseDetailResponseSchema = v.looseObject({
  "data": CaseDetailDTOSchema,
  "meta": MetaSchema,
});

export const AlertEventListResponseSchema = v.looseObject({
  "data": v.array(AlertEventDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const AlertEventResponseSchema = v.looseObject({
  "data": AlertEventDTOSchema,
  "meta": MetaSchema,
});

export const EnrichmentListResponseSchema = v.looseObject({
  "data": v.array(EnrichmentDTOSchema),
  "meta": MetaSchema,
});

export const RuleHistoryResponseSchema = v.looseObject({
  "data": RuleHistoryDTOSchema,
  "meta": MetaSchema,
});

export const RuleSnapshotResponseSchema = v.looseObject({
  "data": RuleSnapshotDTOSchema,
  "meta": MetaSchema,
});

export const RuleSnapshotBatchResponseSchema = v.looseObject({
  "data": v.pipe(
    v.array(RuleSnapshotDTOSchema),
    v.maxLength(100),
  ),
  "meta": MetaSchema,
});

export const RuleSnapshotListResponseSchema = v.looseObject({
  "data": v.array(RuleSnapshotDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const GroupListResponseSchema = v.looseObject({
  "data": v.array(GroupDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const GroupDetailResponseSchema = v.looseObject({
  "data": GroupDetailDTOSchema,
  "meta": MetaSchema,
});

export const SourceListResponseSchema = v.looseObject({
  "data": v.array(SourceDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const SourceResponseSchema = v.looseObject({
  "data": SourceDTOSchema,
  "meta": MetaSchema,
});

export const SourceCreatedResponseSchema = v.looseObject({
  "data": SourceCreatedDTOSchema,
  "meta": MetaSchema,
});

export const SourceTestResponseSchema = v.looseObject({
  "data": SourceTestDTOSchema,
  "meta": MetaSchema,
});

export const SourceHealthResponseSchema = v.looseObject({
  "data": SourceHealthDTOSchema,
  "meta": MetaSchema,
});

export const ReconcileResultResponseSchema = v.looseObject({
  "data": ReconcileResultDTOSchema,
  "meta": MetaSchema,
});

export const RejectionListResponseSchema = v.looseObject({
  "data": v.array(RejectionDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const FailedBatchListResponseSchema = v.looseObject({
  "data": v.array(FailedBatchDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const CasePolicyListResponseSchema = v.looseObject({
  "data": v.array(CasePolicyDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const CasePolicyResponseSchema = v.looseObject({
  "data": CasePolicyDTOSchema,
  "meta": MetaSchema,
});

export const ClusterListResponseSchema = v.looseObject({
  "data": v.array(ClusterDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const ClusterResponseSchema = v.looseObject({
  "data": ClusterDTOSchema,
  "meta": MetaSchema,
});

export const ChannelTypeListResponseSchema = v.looseObject({
  "data": v.array(ChannelTypeDTOSchema),
  "meta": MetaSchema,
});

export const ChannelListResponseSchema = v.looseObject({
  "data": v.array(ChannelDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const ChannelResponseSchema = v.looseObject({
  "data": ChannelDTOSchema,
  "meta": MetaSchema,
});

export const ChannelTestResponseSchema = v.looseObject({
  "data": ChannelTestDTOSchema,
  "meta": MetaSchema,
});

export const PolicyListResponseSchema = v.looseObject({
  "data": v.array(PolicyDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const PolicyResponseSchema = v.looseObject({
  "data": PolicyDTOSchema,
  "meta": MetaSchema,
});

export const PolicyPreviewResponseSchema = v.looseObject({
  "data": PolicyPreviewDTOSchema,
  "meta": MetaSchema,
});

export const NotificationListResponseSchema = v.looseObject({
  "data": v.array(NotificationDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const NotificationDetailResponseSchema = v.looseObject({
  "data": NotificationDetailDTOSchema,
  "meta": MetaSchema,
});

export const DeliveryListResponseSchema = v.looseObject({
  "data": v.array(DeliveryDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const DeliveryResponseSchema = v.looseObject({
  "data": DeliveryDTOSchema,
  "meta": MetaSchema,
});

export const DeliveryDetailResponseSchema = v.looseObject({
  "data": DeliveryDetailDTOSchema,
  "meta": MetaSchema,
});

export const SilenceListResponseSchema = v.looseObject({
  "data": v.array(SilenceDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const SilenceDetailResponseSchema = v.looseObject({
  "data": SilenceDetailDTOSchema,
  "meta": MetaSchema,
});

export const LabelNameListResponseSchema = v.looseObject({
  "data": v.array(LabelNameDTOSchema),
  "meta": MetaSchema,
});

export const LabelValueListResponseSchema = v.looseObject({
  "data": v.array(LabelValueDTOSchema),
  "meta": MetaSchema,
});

export const EnricherListResponseSchema = v.looseObject({
  "data": v.array(EnricherDTOSchema),
  "meta": MetaSchema,
});

export const StatsOverviewResponseSchema = v.looseObject({
  "data": StatsOverviewDTOSchema,
  "meta": MetaSchema,
});

export const AlertQualityListResponseSchema = v.looseObject({
  "data": v.array(AlertQualityDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const MeResponseSchema = v.looseObject({
  "data": MeDTOSchema,
  "meta": MetaSchema,
});

export const SettingBoundDTOSchema = v.looseObject({
  "min": v.pipe(
    v.number(),
    v.integer(),
  ),
  "max": v.pipe(
    v.number(),
    v.integer(),
  ),
  "why": v.string(),
});

export const SettingOriginSchema = v.picklist(["default", "org", "config"]);

export const OrgSettingsPatchDTOSchema = v.looseObject({
  "refire_grace_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
  )),
  "resolve_grace_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
  )),
  "group_close_delay_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
  )),
  "flap_threshold": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
  )),
  "flap_window_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
  )),
  "flap_digest_interval_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
  )),
  "raw_retention_days": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
  )),
  "event_retention_months": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
  )),
  "default_verbosity": v.exactOptional(VerbositySchema),
  "broadcast_on_resolved": v.exactOptional(v.boolean()),
});

export const OrgSettingsViewDTOSchema = v.looseObject({
  "settings": OrgSettingsDTOSchema,
  "origins": v.record(v.string(), SettingOriginSchema),
  "config_keys": v.record(v.string(), v.pipe(
    v.string(),
    v.maxLength(200),
  )),
  "shadowed": OrgSettingsPatchDTOSchema,
  "bounds": v.record(v.string(), SettingBoundDTOSchema),
});

export const OrgSettingsViewResponseSchema = v.looseObject({
  "data": OrgSettingsViewDTOSchema,
  "meta": MetaSchema,
});

export const UpdateOrgSettingsRequestSchema = v.strictObject({
  "refire_grace_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(600),
    v.maxValue(86400),
  )),
  "resolve_grace_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(60),
    v.maxValue(86400),
  )),
  "group_close_delay_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(60),
    v.maxValue(86400),
  )),
  "flap_threshold": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(3),
    v.maxValue(100),
  )),
  "flap_window_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(300),
    v.maxValue(86400),
  )),
  "flap_digest_interval_s": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(60),
    v.maxValue(86400),
  )),
  "raw_retention_days": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
    v.maxValue(365),
  )),
  "event_retention_months": v.exactOptional(v.pipe(
    v.number(),
    v.integer(),
    v.minValue(1),
    v.maxValue(120),
  )),
  "default_verbosity": v.exactOptional(VerbositySchema),
  "broadcast_on_resolved": v.exactOptional(v.boolean()),
  "reset": v.exactOptional(v.pipe(
    v.array(v.pipe(
      v.string(),
      v.maxLength(64),
    )),
    v.maxLength(32),
    v.check((items) => new Set(items).size === items.length, "must not contain duplicates"),
  )),
});

export const ApiTokenListResponseSchema = v.looseObject({
  "data": v.array(ApiTokenDTOSchema),
  "page": PageInfoSchema,
  "meta": MetaSchema,
});

export const ApiTokenCreatedResponseSchema = v.looseObject({
  "data": ApiTokenCreatedDTOSchema,
  "meta": MetaSchema,
});

export const IngestAcceptedResponseSchema = v.looseObject({
  "data": IngestAcceptedDTOSchema,
  "meta": MetaSchema,
});

export const VersionResponseSchema = v.looseObject({
  "data": VersionDTOSchema,
  "meta": MetaSchema,
});
