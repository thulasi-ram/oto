/**
 * What oto decided, in words — the shared vocabulary of the notification record.
 *
 * Two screens read the same rows and must say the same things about them: the
 * alert detail's "Who was told" panel (`features/alerts/detail/DeliveryPanel`)
 * and the org-wide activity log (`ActivitySection`). The maps lived on the first
 * of those, and a second copy on the second would have been the third copy in
 * the app — `PoliciesSection` already keeps one, deliberately (see below).
 *
 * ⛔ EVERY MAP HERE IS TYPED AGAINST THE CONTRACT'S OWN ENUM, never
 * `Record<string, string>`. That is not a style preference, it is the fix for a
 * shipped bug: the loose type let the suppression map lose `snoozed`, the lookup
 * fell through to `?? raw`, and the screen whose entire job is explaining a
 * silence rendered the wire token `snoozed` where a sentence belongs. With the
 * exhaustive type, a reason the server adds is a build failure here instead.
 *
 * ⭐ WHY `PoliciesSection` KEEPS ITS OWN REASON MAP. These are the labels of
 * things that HAVE HAPPENED — "started firing", "the rule changed" — read beside
 * a timestamp. The policy editor labels the same enum as things a policy MAY BE
 * TOLD ABOUT, which is a different sentence in the same words ("rule changed",
 * ticked or unticked). Sharing one map would force one phrasing to be wrong in
 * one of the two places, and the wrong one would be in the editor, where the
 * label is the whole explanation of what a checkbox does.
 */
import type {
  NotificationReason,
  NotificationStatus,
  NotificationSuppressedReason,
} from "~/api/types";

/** Every reason a notification can carry, phrased as the event it records. */
export const REASON_LABEL: Record<NotificationReason, string> = {
  fired: "started firing",
  new_alerts: "new alerts joined",
  some_resolved: "some resolved",
  all_resolved: "all resolved",
  repeat: "repeat",
  suppressed: "suppressed upstream",
  unsuppressed: "no longer suppressed",
  expired: "expired",
  refired: "fired again",
  acked: "acknowledged",
  unacked: "acknowledgement withdrawn",
  enriched: "enrichment arrived",
  rule_changed: "the rule changed",
  comment: "a comment was added",
  snoozed: "snoozed",
  unsnoozed: "snooze ended",
  // oto's own clock on an unacknowledged signal, and ONE stage that ends at a
  // channel. There is no ladder, no rota and no notion of who is next, which is
  // why this is not called what the rest of the industry calls it (§G.9.1, §A.1).
  unacked_reminder: "still firing and unacknowledged",
  storm: "storm",
};

/**
 * Every reason a notification can be suppressed, in plain language.
 *
 * A suppressed notification is **recorded with a reason, never silently
 * dropped**, so every one of these has to be sayable.
 */
export const SUPPRESSED_REASON: Record<NonNullable<NotificationSuppressedReason>, string> = {
  no_policy: "no notification policy matched, so nobody was told",
  // §B.8.2 ranks a snooze ahead of every automatic damper: it is a deliberate
  // human act and therefore the most actionable explanation of a silence.
  snoozed:
    "a person asked oto to hold its notifications for this alert until a fixed time — the alert itself kept firing",
  throttled: "the per-subject throttle was already spent",
  storm: "the case was in storm mode — one message with a count was posted instead",
  flapping: "this alert is damped as flapping, so updates are digested rather than sent one by one",
  verbosity: "the channel's verbosity setting does not carry this kind of update",
  channel_disabled: "every matching channel is disabled",
  duplicate_render: "the message would have been byte-identical to the one already posted",
};

/**
 * A reason oto has never heard of renders as its raw wire value rather than as
 * nothing. The published enum is closed, so this only fires when the server is
 * ahead of the client — which gate G3 (`npm run generate:check`, in CI) exists
 * to make a build failure rather than a blank in the UI.
 */
export function describeSuppression(reason: string): string {
  return SUPPRESSED_REASON[reason as NonNullable<NotificationSuppressedReason>] ?? reason;
}

/**
 * Where one intent got to, in the words an operator would use.
 *
 * ⭐ `suppressed` IS A STATUS LIKE ANY OTHER HERE, and reading it as a failure
 * would be the mistake this whole record exists to prevent: oto deciding not to
 * send is a decision it took on purpose and wrote down. `partial` is the one
 * worth noticing — some channels heard and some did not, which is the state most
 * easily mistaken for "delivered".
 */
export const STATUS_LABEL: Record<NotificationStatus, string> = {
  pending: "not sent yet",
  dispatched: "handed to the channels",
  partial: "some channels only",
  delivered: "delivered",
  failed: "nothing landed",
  suppressed: "deliberately not sent",
};
