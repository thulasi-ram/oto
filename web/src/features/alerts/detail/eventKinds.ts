/**
 * What each timeline event *is*, in one place.
 *
 * Two rules govern the colour column and both come from §M:
 *
 *   - A `--oto-state-*` token is permitted on a **timeline marker** (M.7's lint
 *     rule names exactly three exceptions: state badges, row status bars and
 *     timeline markers). It is not permitted anywhere else on the row.
 *   - Even here, a state hue is spent only on events that genuinely **are** a
 *     lifecycle state change. A failed delivery is serious, but it is not the
 *     state of the alert, so it signals with weight and words rather than by
 *     borrowing red. Scarcity is the whole mechanism (§M.2).
 *
 * Every label is checked against SCOPE-BOUNDARY: acknowledging is a receipt on
 * a signal, never ownership; nothing here says "triage", "escalation-as-a-human-
 * process", "MTTA" or "assigned".
 */
import type { AlertEventType } from "~/api/types";

export type EventCategory =
  | "lifecycle"
  | "ack"
  | "comment"
  | "rule"
  | "enrichment"
  | "notification"
  | "group"
  | "source";

export type MarkerShape = "dot" | "ring" | "square" | "diamond" | "bar" | "quote";

export interface EventKind {
  readonly label: string;
  readonly category: EventCategory;
  /** A Tailwind text colour class. Tier B only where the event IS a state. */
  readonly tone: string;
  readonly shape: MarkerShape;
  /** Shown under the summary when there is something worth explaining. */
  readonly note?: string;
}

const LIFECYCLE_FIRING = "text-firing-solid";
const LIFECYCLE_RESOLVED = "text-resolved-solid";
const LIFECYCLE_EXPIRED = "text-expired-solid";
const LIFECYCLE_SUPPRESSED = "text-suppressed-solid";
const LIFECYCLE_ACKED = "text-acked-solid";
/** Tier A. Everything that is not a lifecycle state lives here. */
const NEUTRAL = "text-ink-subtle";
const NEUTRAL_STRONG = "text-ink";

export const EVENT_KINDS: Record<AlertEventType, EventKind> = {
  "alert.created": {
    label: "Alert first seen",
    category: "lifecycle",
    tone: NEUTRAL_STRONG,
    shape: "ring",
    note: "oto had never seen this alert identity before.",
  },
  "alert.mutated": {
    label: "Labels or annotations changed",
    category: "lifecycle",
    tone: NEUTRAL,
    shape: "dot",
  },
  "alert.flapping_started": {
    label: "Damped as flapping",
    category: "lifecycle",
    tone: NEUTRAL_STRONG,
    shape: "bar",
    note: "Notifications become update-only with a periodic digest. Nothing is dropped.",
  },
  "alert.flapping_ended": {
    label: "No longer flapping",
    category: "lifecycle",
    tone: NEUTRAL,
    shape: "bar",
  },

  "occurrence.opened": {
    label: "Started firing",
    category: "lifecycle",
    tone: LIFECYCLE_FIRING,
    shape: "dot",
  },
  "occurrence.reopened": {
    label: "Fired again",
    category: "lifecycle",
    tone: LIFECYCLE_FIRING,
    shape: "ring",
    note: "A re-fire inside the grace window reopens the same episode rather than starting a new one.",
  },
  "occurrence.suppressed": {
    label: "Suppressed upstream",
    category: "lifecycle",
    tone: LIFECYCLE_SUPPRESSED,
    shape: "square",
    note: "Alertmanager stopped delivering this — a silence, an inhibition or a mute window.",
  },
  "occurrence.unsuppressed": {
    label: "No longer suppressed",
    category: "lifecycle",
    tone: LIFECYCLE_FIRING,
    shape: "square",
  },
  "occurrence.resolved": {
    label: "Resolved",
    category: "lifecycle",
    tone: LIFECYCLE_RESOLVED,
    shape: "dot",
    note: "The upstream said this ended.",
  },
  "occurrence.expired": {
    label: "Expired",
    category: "lifecycle",
    tone: LIFECYCLE_EXPIRED,
    shape: "diamond",
    note: "oto stopped hearing about this. That is not the same as it being fixed.",
  },
  "occurrence.acknowledged": {
    label: "Acknowledged",
    category: "ack",
    tone: LIFECYCLE_ACKED,
    shape: "ring",
    note: "A receipt on a signal — someone has seen it. It is still firing.",
  },
  "occurrence.unacknowledged": {
    label: "Acknowledgement withdrawn",
    category: "ack",
    tone: NEUTRAL,
    shape: "ring",
  },

  "group.opened": { label: "Group opened", category: "group", tone: NEUTRAL, shape: "square" },
  "group.closed": { label: "Group closed", category: "group", tone: NEUTRAL, shape: "square" },
  "group.member_joined": {
    label: "Joined a group",
    category: "group",
    tone: NEUTRAL,
    shape: "dot",
  },
  "group.member_left": { label: "Left a group", category: "group", tone: NEUTRAL, shape: "dot" },
  "group.storm_started": {
    label: "Group entered storm mode",
    category: "group",
    tone: NEUTRAL_STRONG,
    shape: "bar",
    note: "More alerts joined at once than the storm threshold. One message with a count is posted instead of one per alert.",
  },
  "group.storm_ended": {
    label: "Group left storm mode",
    category: "group",
    tone: NEUTRAL,
    shape: "bar",
  },

  "rule.snapshot_captured": {
    label: "Rule captured",
    category: "rule",
    tone: NEUTRAL,
    shape: "square",
    note: "The rule definition as it was at this fire time was recorded.",
  },
  "rule.definition_changed": {
    label: "Rule definition changed",
    category: "rule",
    tone: NEUTRAL_STRONG,
    shape: "diamond",
    note: "The rule is not the same as it was when this last fired. The diff is below.",
  },
  "rule.lookup_failed": {
    label: "Rule could not be read",
    category: "rule",
    tone: NEUTRAL,
    shape: "diamond",
    note: "oto could not reach the rule's origin, so this occurrence has no snapshot.",
  },

  "enrichment.completed": {
    label: "Enrichment finished",
    category: "enrichment",
    tone: NEUTRAL,
    shape: "dot",
  },
  "enrichment.failed": {
    label: "Enrichment failed",
    category: "enrichment",
    tone: NEUTRAL,
    shape: "dot",
    note: "The alert is unaffected — enrichment is additive and never blocks a notification.",
  },

  "notification.created": {
    label: "Notification intended",
    category: "notification",
    tone: NEUTRAL,
    shape: "square",
  },
  "notification.suppressed": {
    label: "Notification suppressed",
    category: "notification",
    tone: NEUTRAL_STRONG,
    shape: "square",
    note: "Recorded with a reason rather than silently dropped, so the audit trail stays complete.",
  },
  "delivery.sent": { label: "Delivered", category: "notification", tone: NEUTRAL, shape: "dot" },
  "delivery.updated": {
    label: "Message updated",
    category: "notification",
    tone: NEUTRAL,
    shape: "dot",
  },
  "delivery.failed": {
    label: "Delivery failed",
    category: "notification",
    tone: NEUTRAL_STRONG,
    shape: "diamond",
    note: "oto will retry.",
  },
  "delivery.skipped": {
    label: "Delivery skipped",
    category: "notification",
    tone: NEUTRAL,
    shape: "dot",
  },
  "delivery.dead": {
    label: "Delivery gave up",
    category: "notification",
    tone: NEUTRAL_STRONG,
    shape: "diamond",
    note: "Nobody was told through this channel. oto's silence must never be mistaken for no alert.",
  },

  "comment.added": {
    label: "Comment",
    category: "comment",
    tone: NEUTRAL_STRONG,
    shape: "quote",
  },

  "source.unreachable": {
    label: "Source unreachable",
    category: "source",
    tone: NEUTRAL_STRONG,
    shape: "bar",
  },
  "source.recovered": {
    label: "Source recovered",
    category: "source",
    tone: NEUTRAL,
    shape: "bar",
  },
  "source.clock_skew": {
    label: "Clock skew observed",
    category: "source",
    tone: NEUTRAL,
    shape: "bar",
    note: "The upstream's clock disagrees with oto's. oto displays the upstream's time and orders by its own.",
  },
};

/** A forward-compatible fallback: an unknown type renders its server summary. */
export const UNKNOWN_KIND: EventKind = {
  label: "Event",
  category: "lifecycle",
  tone: NEUTRAL,
  shape: "dot",
};

export function kindOf(type: string): EventKind {
  return EVENT_KINDS[type as AlertEventType] ?? UNKNOWN_KIND;
}

export const CATEGORY_LABEL: Record<EventCategory, string> = {
  lifecycle: "Lifecycle",
  ack: "Acknowledgement",
  comment: "Comments",
  rule: "Rule",
  enrichment: "Enrichment",
  notification: "Notifications",
  group: "Grouping",
  source: "Source",
};

export const ALL_CATEGORIES = Object.keys(CATEGORY_LABEL) as readonly EventCategory[];

/** The event types belonging to a set of categories, for the `type=` filter. */
export function typesForCategories(
  categories: readonly EventCategory[],
): readonly AlertEventType[] {
  const wanted = new Set(categories);
  return (Object.keys(EVENT_KINDS) as AlertEventType[]).filter((t) =>
    wanted.has(EVENT_KINDS[t].category),
  );
}

/* -------------------------------------------------------------------------- */
/* Actors                                                                     */
/* -------------------------------------------------------------------------- */

/**
 * Who did it.
 *
 * SCOPE-BOUNDARY: attribution is metadata on the event, never the subject of
 * one. So this answers "what wrote this row" and stops there — no ownership, no
 * assignment, and never a per-person aggregate.
 */
export const ACTOR_LABEL: Record<string, string> = {
  system: "oto",
  ingest: "webhook ingest",
  reconciler: "reconciler",
  reaper: "reaper",
  enricher: "enricher",
  notifier: "notifier",
  user: "a person",
  slack: "Slack",
};

export function describeActor(kind: string, label: string | null | undefined): string {
  if (label !== null && label !== undefined && label !== "") return label;
  return ACTOR_LABEL[kind] ?? kind;
}

/** True when a human did this, which is the only case worth naming prominently. */
export function isHuman(kind: string): boolean {
  return kind === "user" || kind === "slack";
}
