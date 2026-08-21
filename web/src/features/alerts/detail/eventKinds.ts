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
import type { ActorKind, AlertEventType } from "~/api/types";

export type EventCategory =
  | "lifecycle"
  | "ack"
  | "snooze"
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
  // ⛔ `alert.flapping_started` AND `alert.flapping_ended` WERE HERE AND ARE GONE
  // FROM THE ENUM (migration `00060`). The flap detector went blind under the case
  // retention window W (ADR 0041 Amendment 1) rather than merely idle: an episode
  // damped by W appends none of the `case.*` events `flap_score` counts, so the
  // crossing fell below the threshold exactly when an alert was flapping hardest.
  // They were briefly kept here so an old timeline could still render one; the
  // CHECK now refuses the spellings and the database was reset, so there is no
  // such row to render.
  "case.opened": {
    label: "Started firing",
    category: "lifecycle",
    tone: LIFECYCLE_FIRING,
    shape: "dot",
  },
  "case.reopened": {
    label: "Fired again",
    category: "lifecycle",
    tone: LIFECYCLE_FIRING,
    shape: "ring",
    note: "A re-fire inside the grace window reopens the same case rather than starting a new one.",
  },
  "case.suppressed": {
    label: "Suppressed upstream",
    category: "lifecycle",
    tone: LIFECYCLE_SUPPRESSED,
    shape: "square",
    note: "Alertmanager stopped delivering this — a silence, an inhibition or a mute window.",
  },
  "case.unsuppressed": {
    label: "No longer suppressed",
    category: "lifecycle",
    tone: LIFECYCLE_FIRING,
    shape: "square",
  },
  "case.resolved": {
    label: "Resolved",
    category: "lifecycle",
    tone: LIFECYCLE_RESOLVED,
    shape: "dot",
    note: "The upstream said this ended.",
  },
  "case.expired": {
    label: "Expired",
    category: "lifecycle",
    tone: LIFECYCLE_EXPIRED,
    shape: "diamond",
    note: "oto stopped hearing about this. That is not the same as it being fixed.",
  },
  "case.acknowledged": {
    label: "Acknowledged",
    category: "ack",
    tone: LIFECYCLE_ACKED,
    shape: "ring",
    note: "A receipt on a signal — someone has seen it. It is still firing.",
  },
  "case.unacknowledged": {
    label: "Acknowledgement withdrawn",
    category: "ack",
    tone: NEUTRAL,
    shape: "ring",
  },

  // ⛔ THESE ARE THE AlertGroup'S EVENTS, AND THE LABELS SAY SO. A group is
  // Alertmanager's notification batch — the object that owns one chat thread —
  // and it is not a case: a Case is ONE alert's firing episode, whose own events
  // are the `case.*` kinds above. Labelling a group event "Case opened" put two
  // different objects behind one word on the one surface where an operator reads
  // both in the same column.
  "group.opened": { label: "Alert group opened", category: "group", tone: NEUTRAL, shape: "square" },
  "group.closed": { label: "Alert group closed", category: "group", tone: NEUTRAL, shape: "square" },
  // ⛔ RETIRED, AND KEPT ANYWAY. NOTHING JOINS A GROUP ANY MORE: migration 00051
  // deleted `alert_group_members`, `joined_at` and `left_at` outright, and
  // membership is now derived from `alert_cases.group_id` with `ended_at IS
  // NULL`. Nothing has written either of these since — `group.member_left` never
  // had a production writer at all. They stay
  // in this map because `alert_events` is retained thirteen months and old
  // timelines still carry `group.member_joined`; a kind missing from here renders
  // as nothing, which is the one outcome a timeline may not have. Delete them when
  // the last partition holding them is dropped.
  "group.member_joined": {
    label: "Added to an alert group",
    category: "group",
    tone: NEUTRAL,
    shape: "dot",
  },
  "group.member_left": {
    label: "Removed from an alert group",
    category: "group",
    tone: NEUTRAL,
    shape: "dot",
  },
  // ⛔ `group.storm_started` AND `group.storm_ended` WERE HERE AND ARE GONE FROM
  // THE ENUM (ADR 0042, migration `00060`). Storm damping is REMOVED, not paused:
  // a flood of two hundred real firings is a truthful report that something is
  // badly wrong, and withholding it made oto's silence indistinguishable from a
  // signal that never fired (§B.6). Unlike `group.member_joined` above — which is
  // RETIRED and stays, because `ev_type_ck` still admits it — the CHECK now
  // refuses these two spellings outright.
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
    note: "oto could not reach the rule's origin, so this case has no snapshot.",
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

  /**
   * Snooze events (§B.8.5).
   *
   * They render as their own category and are offered as `type=` filter values,
   * because `AlertEventType` now carries both — the checked-in client used to be
   * three features behind the contract and these two had to be smuggled in
   * through a forward-compatibility map. Gate G3 (`npm run generate:check`, wired
   * into CI) is what stops that recurring.
   *
   * Neither takes a Tier-B hue: a snooze is not a lifecycle state.
   */
  "alert.snoozed": {
    label: "Notifications held",
    category: "snooze",
    tone: NEUTRAL_STRONG,
    shape: "square",
    note: "oto stopped notifying about this alert until a fixed time. The alert itself is unchanged — still firing, still whatever severity it was. The channel was told it is going quiet.",
  },
  "alert.unsnoozed": {
    label: "Notifications resumed",
    category: "snooze",
    tone: NEUTRAL,
    shape: "square",
    note: "The quiet period ended. The wake-up message reflects the alert's state now rather than replaying what was suppressed.",
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
  snooze: "Quiet periods",
  comment: "Comments",
  rule: "Rule",
  enrichment: "Enrichment",
  notification: "Notifications",
  group: "Alert group",
  source: "Source",
};

/**
 * The categories offered as filter chips.
 *
 * Derived from `EVENT_KINDS` rather than from `CATEGORY_LABEL`, so a category
 * with no type in the server's closed enum is never offered as a filter.
 * Offering one would send `type=` with nothing in it, and the request would
 * quietly return *everything* while the chip claimed to be filtering.
 */
export const ALL_CATEGORIES: readonly EventCategory[] = (
  Object.keys(CATEGORY_LABEL) as EventCategory[]
).filter((c) => (Object.keys(EVENT_KINDS) as AlertEventType[]).some((t) => EVENT_KINDS[t].category === c));

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
 *
 * ⛔ Keyed by `ActorKind` rather than by `string`: an actor the server starts
 * writing must be a build failure here, not a timeline row attributed to a bare
 * wire token.
 */
export const ACTOR_LABEL: Record<ActorKind, string> = {
  system: "oto",
  ingest: "webhook ingest",
  reconciler: "reconciler",
  reaper: "reaper",
  enricher: "enricher",
  notifier: "notifier",
  user: "a person",
  slack: "Slack",
};

export function describeActor(kind: ActorKind, label: string | null | undefined): string {
  if (label !== null && label !== undefined && label !== "") return label;
  return ACTOR_LABEL[kind] ?? kind;
}

/** True when a human did this, which is the only case worth naming prominently. */
export function isHuman(kind: ActorKind): boolean {
  return kind === "user" || kind === "slack";
}

/**
 * The mark that stands for a whole CATEGORY in the event-kind filter menu.
 *
 * ⭐ THE MENU IS A LEGEND FOR THE TIMELINE, SO IT DRAWS WHAT THE TIMELINE DRAWS.
 * Three categories answer `dot` here — lifecycle, enrichment, notification —
 * because that is genuinely the mark their rows carry, and inventing a
 * distinguishing shape for the menu would make the legend disagree with the
 * thing it is a legend for. The label beside it is what tells the three apart;
 * the glyph is there so an operator who has seen a column of dots can find the
 * switch that turns them off.
 *
 * ⛔ AND IT CARRIES NO TONE. `EVENT_KINDS` spends Tier-B state hues on its
 * markers, which §M.7 permits on a timeline marker and nowhere else — a filter
 * row is chrome, so these are drawn in Tier A ink at the call site. This map is
 * therefore shape only, on purpose: a `tone` field here would be an invitation
 * to import one into a menu.
 */
export const CATEGORY_MARK: Record<EventCategory, MarkerShape> = {
  lifecycle: "dot",
  ack: "ring",
  snooze: "square",
  comment: "quote",
  rule: "diamond",
  enrichment: "dot",
  notification: "dot",
  group: "square",
  source: "bar",
};
