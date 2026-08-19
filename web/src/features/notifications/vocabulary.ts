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
  // ⛔ `unacked_reminder` WAS HERE AND IS GONE FROM THE ENUM (git-bug bd0fb1d,
  // migration 00067). oto's own clock on an unacknowledged signal, one stage that
  // ended at a channel — no ladder, no rota, no notion of who was next. Withdrawn
  // by the owner: oto sends nothing unprompted. Unlike `storm` below it was not
  // kept for a cut as a retired reason, because oto is unreleased and the database
  // is being reset — there is no stored row left to explain.
  // ⛔ `storm` WAS HERE AND IS GONE FROM THE ENUM (ADR 0042, migration `00060`).
  // It was kept for one cut as a RETIRED reason, on the argument that
  // `notifications.reason` is what a STORED ROW SAYS IT WAS ABOUT and that
  // `notifications_reason_ck` still admitted it. 00060 narrows that CHECK with no
  // backfill and the database was reset, so no row spells it and there is nothing
  // for this map to have to say.
  // ⭐ THE ONE REASON NO TRANSITION PRODUCED, AND THE ONLY ONE WHOSE SUBJECT IS
  // NOT AN OBJECT. A digest is a WINDOW OVER A NAMESPACE (migration `00058`): at
  // each closed window boundary a tick counts the cases that OPENED inside the
  // window and sends one message if the count clears the policy's `digest_floor`.
  // So the label names the window, never an alert — this row is about no alert,
  // no case and no generation, and it is the one notification with no `group_id`.
  digest: "a window closed with enough new cases to report",
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
  verbosity: "the channel's verbosity setting does not carry this kind of update",
  channel_disabled: "every matching channel is disabled",
  duplicate_render: "the message would have been byte-identical to the one already posted",
};

/*
 * ⛔ THE SIX ARE THE WHOLE LIST, AND WHAT THEY HAVE IN COMMON IS THE ARGUMENT.
 * `channel_disabled` and `no_policy` mean there was nowhere to send; `snoozed`
 * and `verbosity` are a human asking for less; `throttled` is the world's rate
 * limit; `duplicate_render` is that nothing changed. Not one of them is oto's own
 * opinion that a real firing was not worth mentioning — and the two that were,
 * `storm` and `flapping`, are gone. Migration `00059` narrowed
 * `notifications_suppmap_ck` to these six with NO backfill, so it FAILS on a
 * database that ever recorded either rather than rewriting the reason into one
 * that never applied: there is no stored row left for this map to have to explain.
 * `REASON_LABEL` above lost its `storm` entry on the same terms one cut later
 * (migration `00060`), so the two maps now say the same thing about the dampers
 * rather than disagreeing on purpose.
 */

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
