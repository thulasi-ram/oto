/**
 * The org tuning screen's copy, and the arithmetic that ties it to Alertmanager.
 *
 * This file is deliberately mostly prose. The screen it feeds is the one place
 * an operator changes how loud oto is, and a knob whose description is "how long
 * to wait before regrouping" is worse than no knob at all: it invites a number
 * chosen by vibes, and the failure it produces looks nothing like the setting
 * that caused it.
 *
 * Three rules held throughout:
 *
 *   1. **Every knob states BOTH failure modes.** Not "what it does" — what
 *      breaks if it is too small, and what breaks if it is too large. Those are
 *      different outages and an operator picking a value is choosing between
 *      them.
 *   2. **Nothing here is invented.** Every sentence and every inequality is from
 *      `docs/setup/tuning.md`, which is the document this screen must not
 *      contradict. Where the doc gives an arithmetic rule, it is implemented as
 *      a `guide` and evaluated live; where the doc explicitly declines to give a
 *      rule ("sanity-check against how long your incidents actually last"), the
 *      screen says that and does not manufacture a threshold.
 *   3. **The values are relative to the operator's own `alertmanager.yml`.**
 *      That is the single most valuable thing this screen can say, so it is said
 *      inline at each knob rather than in a help page. oto has no access to
 *      `alertmanager.yml`, so the four numbers everything depends on are entered
 *      by the operator and held in this browser — see `AmRef`.
 *
 * Vocabulary here is bound by SCOPE-BOUNDARY §3 and enforced by
 * `tools/lintvocab`. Notably: the unacked reminder is a reminder, never a
 * ladder; oto measures a signal's **firing duration**, never anyone's response.
 */
import type { ReminderMention, ReminderMentionSeverity, Verbosity } from "~/api/types";
import { duration } from "~/lib/format";

/* -------------------------------------------------------------------------- */
/* The Alertmanager reference                                                 */
/* -------------------------------------------------------------------------- */

/**
 * The four upstream numbers every oto duration is a multiple of.
 *
 * oto cannot read these. It has no access to `alertmanager.yml` and no access to
 * the rule files, and the read API it does have (`listSilences`, the rules
 * mirror) does not carry route timing. So they are entered here, stored in this
 * browser only, and never sent anywhere — which is also why `confirmed` exists:
 * guidance computed from *assumed* defaults must say so.
 */
export interface AmRef {
  /** `route.group_wait` — delay before the FIRST notification for a new group. */
  readonly group_wait_s: number;
  /** `route.group_interval` — the clock rate of oto's whole view of the world. */
  readonly group_interval_s: number;
  /** `route.repeat_interval` — gap before re-sending an UNCHANGED group. */
  readonly repeat_interval_s: number;
  /** The `for:` of the rules that actually misbehave, not the average. */
  readonly rule_for_s: number;
  /** False until an operator has actually entered their own numbers. */
  readonly confirmed: boolean;
}

/** Alertmanager's own defaults (`dispatch/route.go`), plus a typical `for:`. */
export const AM_DEFAULTS: AmRef = {
  group_wait_s: 30,
  group_interval_s: 300,
  repeat_interval_s: 14_400,
  rule_for_s: 300,
  confirmed: false,
};

export interface AmFieldCopy {
  readonly key: "group_wait_s" | "group_interval_s" | "repeat_interval_s" | "rule_for_s";
  readonly label: string;
  readonly source: string;
  readonly why: string;
}

export const AM_FIELDS: readonly AmFieldCopy[] = [
  {
    key: "group_wait_s",
    label: "group_wait",
    source: "route.group_wait in alertmanager.yml",
    why: "A floor on alert-to-Slack latency that oto cannot improve. It also hides the fastest flaps entirely: an alert that resolves before group_wait elapses produces no notification at all, and oto cannot damp, count or report what it is never told about.",
  },
  {
    key: "group_interval_s",
    label: "group_interval",
    source: "route.group_interval",
    why: "The clock rate of oto's whole view of the world. oto never learns about a change to an existing group faster than this, so every duration below should be read as a multiple of it rather than as an absolute time.",
  },
  {
    key: "repeat_interval_s",
    label: "repeat_interval",
    source: "route.repeat_interval",
    why: "Produces the notification oto delivers as an update rather than a new message — the single largest noise reduction oto provides, and it needs no tuning. Its consequence is that an unacknowledged critical is re-sent only this often, which is why oto runs its own unacked-reminder clock.",
  },
  {
    key: "rule_for_s",
    label: "rule for:",
    source: "the for: clause of the rules that actually misbehave",
    why: "The hard floor on how fast an alert can possibly oscillate, and what makes the flap thresholds either meaningful or dead code. If your rules range from 0s to 1h, no single global flap threshold is correct for all of them — tune for the ones that misbehave.",
  },
];

/* -------------------------------------------------------------------------- */
/* Knob descriptions                                                          */
/* -------------------------------------------------------------------------- */

export type KnobKey =
  | "refire_grace_s"
  | "resolve_grace_s"
  | "group_close_delay_s"
  | "flap_threshold"
  | "flap_window_s"
  | "flap_digest_interval_s"
  | "storm_threshold"
  | "storm_window_s"
  | "storm_cooldown_s"
  | "raw_retention_days"
  | "event_retention_months"
  | "unacked_reminder_after_s"
  | "default_verbosity"
  | "broadcast_on_resolved"
  | "unacked_reminder_mention"
  | "unacked_reminder_mention_list"
  | "unacked_reminder_mention_min_severity";

/** How the control renders and how the value is parsed for the PATCH. */
export type KnobKind =
  | "seconds"
  | "count"
  | "days"
  | "months"
  | "verbosity"
  | "boolean"
  | "mentionMode"
  | "mentionList"
  | "severity";

/**
 * How wrong looks in each direction. Both are always shown: an operator picking
 * a number is choosing between two failures, not avoiding one.
 */
export interface Risk {
  /** "Too short", "Too low", "If it is on" — the direction, in two words. */
  readonly label: string;
  readonly text: string;
}

/** The verdict of a live check against the operator's Alertmanager numbers. */
export type GuidanceLevel = "inert" | "tight" | "ok";

export interface Guidance {
  readonly level: GuidanceLevel;
  readonly text: string;
  /** A value the arithmetic actually supports, offered as a one-click fix. */
  readonly suggest?: number;
}

export interface KnobCopy {
  readonly key: KnobKey;
  readonly kind: KnobKind;
  readonly label: string;
  /** The noun after the number, when `kind` alone does not name it. */
  readonly unit?: string;
  /**
   * `0` is a legal *read* value meaning "unset", even though the write bounds
   * start above it. `unacked_reminder_after_s` is the only such key: the org
   * settings schema admits 0, the update schema does not, and the way back to
   * unset is `reset` rather than writing a zero.
   */
  readonly zeroIsUnset?: boolean;
  /** What the knob does, in the product's own terms. Two sentences at most. */
  readonly what: string;
  /** Exactly two: the low-side failure and the high-side failure. */
  readonly risks: readonly [Risk, Risk];
  /** The standing relationship to the customer's own config. Always shown. */
  readonly amRule: string;
  /** Evaluated against the entered Alertmanager numbers on every keystroke. */
  readonly guide?: (value: number, am: AmRef, num: (key: KnobKey) => number) => Guidance;
}

export interface KnobGroup {
  readonly id: string;
  readonly title: string;
  readonly blurb: string;
  readonly keys: readonly KnobKey[];
}

/* -------------------------------------------------------------------------- */

const ok = (text: string): Guidance => ({ level: "ok", text });

/* -------------------------------------------------------------------------- */
/* The knobs                                                                  */
/* -------------------------------------------------------------------------- */

export const KNOBS: Readonly<Record<KnobKey, KnobCopy>> = {
  /* ---- threads and lifecycle -------------------------------------------- */

  refire_grace_s: {
    key: "refire_grace_s",
    kind: "seconds",
    label: "Re-fire grace",
    what: "An alert resolves, then the same alert fires again. Inside this window the existing occurrence reopens, oto reuses the existing Slack thread and the card updates in place — the one case that produces no new root message, and therefore the one oto surfaces in the channel so it is not missed. Outside the window a new occurrence opens, and once the group has closed that means a new generation: a brand-new Slack root message and a brand-new thread.",
    risks: [
      {
        label: "Too short",
        text: "Every re-fire opens a new Slack thread. Alertmanager will not report a changed group sooner than one group_interval after the last notification, so if the grace window is shorter than that it has always expired by the time oto is even capable of hearing about the re-fire. You get the wall of near-identical messages oto exists to prevent, produced by a setting that looks like it should have prevented it.",
      },
      {
        label: "Too long",
        text: "History that lies. Two genuinely separate outages hours apart are recorded as one occurrence that reopened — one thread, one firing duration with a long gap in the middle, and this morning's thread grows a reply about tonight's incident. Occurrence counts under-report and duration statistics stop meaning anything.",
      },
    ],
    amRule:
      "Start from group_interval and give it real headroom: 2 x group_interval is the hard floor, 3 x is a reasonable default. Below one group_interval the knob does nothing at all. Then check the top end against how long your incidents actually last — if a typical one is genuinely gone in ten minutes, a ten-minute window will merge distinct incidents.",
    guide: (v, am) => {
      const gi = am.group_interval_s;
      if (v < gi) {
        return {
          level: "inert",
          text: `Unreachable. Shorter than your group_interval of ${duration(gi)}, so the window has always expired before oto can hear about a re-fire. Every re-fire will open a new Slack thread.`,
          suggest: gi * 3,
        };
      }
      if (v < gi * 2) {
        return {
          level: "tight",
          text: `Below the 2 x group_interval floor (${duration(gi * 2)}). Reachable only by a re-fire that lands in the very first batch after the resolve.`,
          suggest: gi * 3,
        };
      }
      return ok(
        `${(v / gi).toFixed(1)} x group_interval — above the 2 x floor. The doc's suggested default is 3 x (${duration(gi * 3)}).`,
      );
    },
  },

  group_close_delay_s: {
    key: "group_close_delay_s",
    kind: "seconds",
    label: "Group close delay",
    what: "How long an AlertGroup stays open after its last member stops firing. Closing a group is what makes the next fire open a new generation, and a new generation is a new Slack root message.",
    risks: [
      {
        label: "Too short",
        text: "A generation closes between two Alertmanager batches of the same incident, so the second half of one outage arrives as a brand-new group with a brand-new root card.",
      },
      {
        label: "Too long",
        text: "The generation spans genuinely separate incidents, and tonight's fire lands as an update to a card about something that ended this morning.",
      },
    ],
    amRule:
      "Keep it at or above group_interval, and consider aligning it with the re-fire grace. If it is much shorter than the re-fire grace, a re-fire inside the grace window still finds a closed group — and gets a new root message anyway.",
    guide: (v, am, num) => {
      const gi = am.group_interval_s;
      if (v < gi) {
        return {
          level: "inert",
          text: `Below group_interval (${duration(gi)}). A generation can close between two batches of one incident.`,
          suggest: gi,
        };
      }
      const refire = num("refire_grace_s");
      if (Number.isFinite(refire) && v < refire) {
        return {
          level: "tight",
          text: `Shorter than the re-fire grace (${duration(refire)}). A re-fire inside the grace window would still find a closed group, so it gets a new root message despite the grace.`,
          suggest: refire,
        };
      }
      return ok(`At or above group_interval (${duration(gi)}), and not shorter than the re-fire grace.`);
    },
  },

  resolve_grace_s: {
    key: "resolve_grace_s",
    kind: "seconds",
    label: "Resolve grace",
    what: "How long past an alert's upstream end-time lease oto waits before the reaper marks the occurrence expired. Expired is not resolved: it means oto stopped hearing about this, never that the problem went away.",
    risks: [
      {
        label: "Too short",
        text: "A single missed scrape looks like an expiry. Prometheus refreshes the end time on every send and the lease is commonly three to four minutes; below that, an alert that is still firing gets recorded as one oto lost sight of.",
      },
      {
        label: "Too long",
        text: "An alert whose source really did go away sits open longer than it should, and the timeline keeps implying something is still firing when nothing is left to say so.",
      },
    ],
    amRule:
      "This one is not derived from your route timing but from your scrape budget: the end-time lease is typically 4 x scrape_interval or evaluation_interval, commonly three to four minutes. Set this above it. Do not tune it down to make expiries arrive faster — losing sight of an alert is not the same as the alert resolving, and a fast, wrong expiry is worse than a slow, correct one.",
    guide: (v) => {
      if (v < 240) {
        return {
          level: "tight",
          text: "Below a typical end-time lease of three to four minutes. A single missed scrape can look like an expiry.",
          suggest: 300,
        };
      }
      return ok("Above a typical three-to-four-minute end-time lease.");
    },
  },

  /* ---- flap damping ------------------------------------------------------ */

  flap_threshold: {
    key: "flap_threshold",
    kind: "count",
    label: "Flap threshold",
    unit: "transitions",
    what: "Transitions inside the flap window before oto marks an alert flapping. A flapping alert still opens and closes occurrences and its card still updates; what stops is the per-transition thread replies, replaced by one coalesced digest. Flapping is a visible state, never a silent drop.",
    risks: [
      {
        label: "Too high",
        text: "The damper never engages. Every individual transition keeps its own reply for an alert that is oscillating — the noisiest possible outcome, and the exact thing the feature exists to stop. Worse, it looks correctly configured right up until someone asks why a wildly oscillating alert was never marked as flapping.",
      },
      {
        label: "Too low",
        text: "Real transitions get folded into a digest. An alert that legitimately fired, resolved and fired again during a rolling deploy is marked flapping, and the second firing arrives inside a digest instead of being announced — so you find out late. Flapping is shown in the UI, so a healthy alert is displayed as noisy and someone eventually deletes a useful rule because of it.",
      },
    ],
    amRule:
      "The for: trap. Every resolved-to-firing edge needs the rule condition to hold for its whole for: duration first, and oto only sees a change when Alertmanager sends one. The observable ceiling in a window W is about 2 x W / (for + group_interval); set the threshold at roughly half of it. For long-for: rules do not lower the threshold to 2 — two transitions is a normal deploy. Widen the window instead.",
    guide: (v, am, num) => {
      const w = num("flap_window_s");
      const cadence = am.rule_for_s + am.group_interval_s;
      const ceiling = Math.floor((2 * w) / cadence);
      if (v > ceiling) {
        return {
          level: "inert",
          text: `Unreachable. With for: ${duration(am.rule_for_s)} and group_interval ${duration(am.group_interval_s)}, a ${duration(w)} window can contain at most about ${ceiling} transition${ceiling === 1 ? "" : "s"} oto is able to observe. The damper can never engage — it is dead code that looks configured. Widen the window rather than lowering the threshold.`,
        };
      }
      if (v > Math.floor(ceiling / 2)) {
        return {
          level: "tight",
          text: `Reachable but only just: the observable ceiling in a ${duration(w)} window is about ${ceiling}, and the doc puts a workable threshold at roughly half of that.`,
          suggest: Math.max(3, Math.floor(ceiling / 2)),
        };
      }
      return ok(
        `About half the observable ceiling of ${ceiling} for a ${duration(w)} window at for: ${duration(am.rule_for_s)}.`,
      );
    },
  },

  flap_window_s: {
    key: "flap_window_s",
    kind: "seconds",
    label: "Flap window",
    what: "The span the transition count is measured over. Widening it is the correct response to a threshold that cannot be reached, because lowering the threshold instead labels ordinary deploys as flapping.",
    risks: [
      {
        label: "Too short",
        text: "A window shorter than one group_interval cannot contain two transitions oto is able to observe, so the threshold becomes unreachable at any value and the damper is dead code.",
      },
      {
        label: "Too long",
        text: "An alert that misbehaved once last week still counts toward today's score, and something long since fixed stays marked as flapping in the UI.",
      },
    ],
    amRule:
      "For a rule with a long for:, widen this rather than lowering the threshold: flap_window is about flap_threshold x (for + group_interval) x 2. With for: 10m and group_interval 5m, a 3h window makes a threshold of 5 describe something genuinely pathological instead of something impossible.",
    guide: (v, am, num) => {
      const gi = am.group_interval_s;
      if (v < gi) {
        return {
          level: "inert",
          text: `Shorter than group_interval (${duration(gi)}). The window cannot contain two transitions oto is able to observe, so no threshold is reachable.`,
          suggest: gi * 2,
        };
      }
      const t = num("flap_threshold");
      const need = Math.round(t * (am.rule_for_s + gi) * 2);
      if (Number.isFinite(t) && v < need) {
        return {
          level: "tight",
          text: `A threshold of ${t} needs roughly ${duration(need)} to be reachable at for: ${duration(am.rule_for_s)} and group_interval ${duration(gi)}.`,
          suggest: need,
        };
      }
      return ok(`Wide enough for a threshold of ${t} at for: ${duration(am.rule_for_s)}.`);
    },
  },

  flap_digest_interval_s: {
    key: "flap_digest_interval_s",
    kind: "seconds",
    label: "Flap digest interval",
    what: "How often a flapping alert is allowed one coalesced summary reply in place of the individual transition replies it would otherwise have posted.",
    risks: [
      {
        label: "Too short",
        text: "Not a digest. Below group_interval it cannot produce more digests than the upstream produces batches — it only adds jitter to when they land.",
      },
      {
        label: "Too long",
        text: "The summary arrives long after anyone cared, and in the meantime a flapping alert is effectively silent in the thread.",
      },
    ],
    amRule:
      "Keep it at or above group_interval. Two to four times group_interval is the useful range.",
    guide: (v, am) => {
      const gi = am.group_interval_s;
      if (v < gi) {
        return {
          level: "tight",
          text: `Below group_interval (${duration(gi)}). It cannot produce more digests than the upstream produces batches — it only jitters when they land.`,
          suggest: gi * 3,
        };
      }
      if (v > gi * 4) {
        return {
          level: "tight",
          text: `Above 4 x group_interval (${duration(gi * 4)}), which is the top of the useful range. The digest starts arriving after anyone cared.`,
          suggest: gi * 3,
        };
      }
      return ok(`${(v / gi).toFixed(1)} x group_interval — inside the useful 2 x to 4 x range.`);
    },
  },

  /* ---- storm collapse ---------------------------------------------------- */

  storm_threshold: {
    key: "storm_threshold",
    kind: "count",
    label: "Storm threshold",
    unit: "alerts in the window",
    what: "Distinct alerts joining one AlertGroup generation inside the storm window before the group collapses. In storm mode oto posts or updates exactly one root message carrying a count and a link, and suppresses every per-alert thread reply until the cooldown elapses with no new members. The channel is told once that oto has started withholding — once for the channel, not once per group, because a per-group announcement of going quiet would be the flood it is damping. Like flapping, it is a visible state, never a silent drop.",
    risks: [
      {
        label: "Too high",
        text: "The flood you configured this for. A node pool dies, two hundred pods alert, and oto faithfully posts two hundred thread replies — strictly worse than posting none. You will also hit Slack's roughly one-message-per-second-per-channel limit and spend twenty minutes delivering a backlog nobody will read.",
      },
      {
        label: "Too low",
        text: "oto goes quiet exactly when you want detail. A routine deploy touching six services collapses into one line saying six alerts, and the operator has to leave Slack to find out which six. Storm mode is a defence against a flood, not a summarisation strategy.",
      },
    ],
    amRule:
      "Your group_by decides whether this knob can do anything at all. Storm collapse counts alerts joining ONE group, so if group_by contains a high-cardinality label (instance, pod, container) no group ever accumulates enough members and storm collapse is unreachable at any threshold — you get one root card per alert instead, which is a flood by a different route. That fix is in alertmanager.yml, not here: group on the labels that describe the problem, not the ones that describe the instance. Otherwise set it above your largest normal group and below your smallest abnormal one; the replica count of your largest deployment is a useful proxy.",
  },

  storm_window_s: {
    key: "storm_window_s",
    kind: "seconds",
    label: "Storm window",
    what: "The span over which joining members are counted toward the storm threshold.",
    risks: [
      {
        label: "Too short",
        text: "A burst Alertmanager is still batching does not look like a burst to oto: the members arrive together in one delivery, after the window has already closed.",
      },
      {
        label: "Too long",
        text: "Alerts that arrived minutes apart for unrelated reasons are counted as one burst, and a slow trickle trips storm mode — which then suppresses the per-alert detail you wanted.",
      },
    ],
    amRule:
      "It must be longer than group_wait, and about 2 x group_wait is the shipped shape. If you have raised group_wait to reduce noise, raise this with it, or storm mode triggers inconsistently depending on where a burst falls relative to the batch boundary.",
    guide: (v, am) => {
      const gw = am.group_wait_s;
      if (v <= gw) {
        return {
          level: "inert",
          text: `Not longer than group_wait (${duration(gw)}). A burst Alertmanager is still batching arrives in a single delivery after this window closes, so it never looks like a burst.`,
          suggest: gw * 2,
        };
      }
      if (v < gw * 2) {
        return {
          level: "tight",
          text: `Above group_wait but below the 2 x shape the default uses (${duration(gw * 2)}). Storm detection will depend on where a burst falls relative to the batch boundary.`,
          suggest: gw * 2,
        };
      }
      return ok(`${(v / gw).toFixed(1)} x group_wait — comfortably past the batch boundary.`);
    },
  },

  storm_cooldown_s: {
    key: "storm_cooldown_s",
    kind: "seconds",
    label: "Storm cooldown",
    what: "How long the group must be quiet before per-alert behaviour resumes. It is also the gap oto holds between two channel-level storm notices, so that a storm's start and its end can each be announced while every other group's storm inside that span is collapsed into them.",
    risks: [
      {
        label: "Too short",
        text: "Storm mode flickers on and off across consecutive Alertmanager batches, and the channel gets a storm announcement every time it re-engages.",
      },
      {
        label: "Too long",
        text: "Per-alert replies stay suppressed long after the flood ended, so everything arriving in the tail of the event is invisible in the thread.",
      },
    ],
    amRule:
      "Keep it at or above group_interval, otherwise storm mode flickers across consecutive batches. The shipped 10m is 2 x the Alertmanager default.",
    guide: (v, am) => {
      const gi = am.group_interval_s;
      if (v < gi) {
        return {
          level: "inert",
          text: `Below group_interval (${duration(gi)}). Storm mode will flicker on and off across consecutive batches.`,
          suggest: gi * 2,
        };
      }
      return ok(`${(v / gi).toFixed(1)} x group_interval — no flicker across consecutive batches.`);
    },
  },

  /* ---- what reaches the channel ----------------------------------------- */

  unacked_reminder_after_s: {
    key: "unacked_reminder_after_s",
    kind: "seconds",
    label: "Unacked reminder default",
    zeroIsUnset: true,
    what: "The org default a notification policy's own reminder delay falls back to when the policy names none. A policy with an opinion always wins. Zero means this org sets no default — it does not mean immediately, and a policy with no delay of its own still produces no reminder.",
    risks: [
      {
        label: "Too short",
        text: "A reminder that arrives before anyone could reasonably have looked is noise, and it is always broadcast into the channel. A channel that learns to scroll past oto's reminders has lost the only mechanism oto has for genuine urgency.",
      },
      {
        label: "Too long",
        text: "The reminder lands after the alert stopped mattering. Its entire purpose is to be seen; one that arrives at the end of the day is a log line.",
      },
    ],
    amRule:
      "Read it against repeat_interval. Alertmanager re-sends an unchanged group only that often — four hours by default — which is exactly why oto runs its own clock instead of relying on it. There is one reminder stage, forever: it is a single number, never a ladder, and it never targets anything but the policy's own channels.",
    guide: (v, am) => {
      if (v === 0) {
        return ok(
          "Unset. A policy that names no delay of its own produces no reminder at all. This is not the same as zero seconds.",
        );
      }
      const ri = am.repeat_interval_s;
      if (v >= ri) {
        return {
          level: "tight",
          text: `At or beyond your repeat_interval (${duration(ri)}). Alertmanager will already have re-sent the unchanged group before oto's reminder fires, so the reminder adds nothing the channel was not just told.`,
        };
      }
      return ok(
        `Fires well inside your repeat_interval of ${duration(ri)}, which is the point of having this clock at all.`,
      );
    },
  },

  unacked_reminder_mention: {
    key: "unacked_reminder_mention",
    kind: "mentionMode",
    label: "Unacked reminder mention",
    what: "Who the one unacked reminder addresses. The reminder is delivered as a thread reply that also surfaces in the channel, and a thread reply notifies only people already following that thread — precisely the wrong audience for the one message whose purpose is to reach somebody who has not engaged.",
    risks: [
      {
        label: "If none",
        text: "The reminder arrives as an ordinary message in a busy channel. For the one notification meant to reach someone who has not looked, that is close to not sending it.",
      },
      {
        label: "If here or channel",
        text: "Slack documents that @here and @channel do not notify when used in threads, and oto's reminder is a thread reply. On the evidence these are silent no-ops from that position — worse than being obviously off, because a control that appears configured manufactures the belief that somebody was told. Nobody investigates a setting that looks like it is working.",
      },
    ],
    amRule:
      "Not an Alertmanager question — a Slack one. An explicit list of individuals and usergroups is the only form Slack documents as notifying from inside a thread, which is why it exists and why the shipped default is none rather than here. Nothing on this row is time-aware: it is a fixed audience an operator chose once, it will never learn who is awake, and there will never be a second stage after it.",
  },

  unacked_reminder_mention_list: {
    key: "unacked_reminder_mention_list",
    kind: "mentionList",
    label: "Unacked reminder mention list",
    what: "The explicit audience for list mode: Slack user ids and usergroup ids, at most ten. It is ignored unless the mention above is set to list.",
    risks: [
      {
        label: "If empty",
        text: "In list mode an empty list mentions nobody, so the reminder is exactly as quiet as none while appearing to be configured — the same failure as defaulting to here, arrived at from the other direction.",
      },
      {
        label: "If crowded",
        text: "A reminder that notifies ten people is at the outer edge of what a notification is; past that it is a page, and oto pages nobody. Ten is a cap, not a courtesy, and the server enforces it.",
      },
    ],
    amRule:
      "Not an Alertmanager question. This is a fixed audience, not a schedule: it has no notion of who is awake, whose week it is, or who is at a keyboard, and it will never become time-aware. If you need that, oto is the wrong layer — point a webhook channel at the tool that does it. Note also that @here and @channel are modes, not entries: a list that could contain them would let a five-person list quietly become a channel-wide ping.",
  },

  unacked_reminder_mention_min_severity: {
    key: "unacked_reminder_mention_min_severity",
    kind: "severity",
    label: "Mention only at or above",
    what: "The severity floor for attaching a mention at all. Below it the reminder still goes out — it simply arrives without a mention.",
    risks: [
      {
        label: "Too low",
        text: "A mention on every unacked warning is how a channel learns to mute oto, and a muted channel is how a real outage goes unseen. This is the setting that protects the one above it from being switched off in frustration.",
      },
      {
        label: "Too high",
        text: "Set it above the severities your rules actually emit and the mention never attaches. The mention above is then configured and inert, which looks exactly like being configured and working.",
      },
    ],
    amRule:
      "Not an Alertmanager route question, but it does read your alerts' own severity label — whatever your rules actually write there. The shipped default is critical.",
  },

  default_verbosity: {
    key: "default_verbosity",
    kind: "verbosity",
    label: "Default channel verbosity",
    what: "The fallback for a channel that names no verbosity of its own. A channel's own setting always wins, so an org default can never make a quiet channel loud.",
    risks: [
      {
        label: "Too loud",
        text: "Every channel that has not stated a preference receives every transition, including the ones nobody reads — and the volume is what teaches a channel to be ignored.",
      },
      {
        label: "Too quiet",
        text: "Channels that never set a verbosity stop reporting resolves and acks. A card that never visibly changes looks like an alert nobody handled, and the fact that it ended never reaches the channel.",
      },
    ],
    amRule:
      "Nothing in alertmanager.yml bears on this. Set it to the quietest setting most of your channels want, rather than repeating yourself on each one.",
  },

  broadcast_on_resolved: {
    key: "broadcast_on_resolved",
    kind: "boolean",
    label: "Broadcast when everything resolves",
    what: "Whether the all-resolved transition surfaces in the channel rather than being posted quietly in the thread. It is the only broadcast an org can configure. Two others are fixed by policy — a re-fire inside the grace window, and the unacked reminder — because for those two the quiet form of the fact is genuinely invisible, which is the only property that earns an irreversible channel post.",
    risks: [
      {
        label: "If it is on",
        text: "It doubles channel traffic for the least urgent fact oto has, on a busy channel. A broadcast cannot be un-sent — Slack documents nothing that removes a channel reference once made — and the bar is whether someone who was asleep would be angry to have missed it, not whether it is interesting. Nobody was ever woken because a resolve arrived quietly.",
      },
      {
        label: "If it is off",
        text: "On a quiet channel, closure is genuinely welcome and this withholds it. oto's primary verb is an in-place edit of the card, and an edit is completely silent: with this off, the only in-channel evidence that an outage ended is a message that changed and told nobody.",
      },
    ],
    amRule:
      "Nothing in alertmanager.yml bears on this. It interacts with verbosity instead: broadcast is decided per transition and then modulated by the destination channel's verbosity, so a channel that has opted out of thread replies does not receive louder ones.",
  },

  /* ---- retention --------------------------------------------------------- */

  raw_retention_days: {
    key: "raw_retention_days",
    kind: "days",
    label: "Raw payload retention",
    what: "How long raw ingested webhook payloads are kept. They age out by dropping whole partitions, never by deleting rows.",
    risks: [
      {
        label: "Too short",
        text: "A webhook you want to inspect has already had its partition dropped, and an ingestion bug reported on Friday cannot be reproduced on Monday.",
      },
      {
        label: "Too long",
        text: "Raw payloads are the largest thing oto stores. A year of them buys storage nobody will read.",
      },
    ],
    amRule:
      "Nothing in alertmanager.yml bears on this. It is storage, not behaviour — no alert changes shape because of it. Raise it if you debug ingestion often.",
  },

  event_retention_months: {
    key: "event_retention_months",
    kind: "months",
    label: "Event timeline retention",
    what: "How long the event timeline is kept — the record of what fired, what oto did about it and what a human annotated.",
    risks: [
      {
        label: "Too short",
        text: "The timeline ends before the question you are asking. Twelve months cannot answer whether this is worse than the same week last year, which is why the shipped default is thirteen.",
      },
      {
        label: "Too long",
        text: "The event table grows without bound at the far end, and nobody has read the far end.",
      },
    ],
    amRule:
      "Nothing in alertmanager.yml bears on this. Thirteen months is the default so that year-on-year comparisons have a full extra month to land in.",
  },
};

export const KNOB_GROUPS: readonly KnobGroup[] = [
  {
    id: "threads",
    title: "Threads and lifecycle",
    blurb:
      "These three decide how many Slack threads a recurring problem generates, and when oto stops believing an alert is still there.",
    keys: ["refire_grace_s", "group_close_delay_s", "resolve_grace_s"],
  },
  {
    id: "flap",
    title: "Flap damping",
    blurb:
      "When an alert oscillates, individual transition replies are replaced by one coalesced digest. Flapping is a visible state in the UI, never a silent drop.",
    keys: ["flap_threshold", "flap_window_s", "flap_digest_interval_s"],
  },
  {
    id: "storm",
    title: "Storm collapse",
    blurb:
      "When many alerts join one generation at once, oto posts one root message with a count and suppresses the per-alert replies. Also a visible state.",
    keys: ["storm_threshold", "storm_window_s", "storm_cooldown_s"],
  },
  {
    id: "channel",
    title: "What reaches the channel",
    blurb:
      "oto's primary verb is an in-place edit of a card, and an edit is completely silent — no notification, no unread, nothing rises in the channel. These settings decide what is allowed to be louder than that.",
    keys: [
      "default_verbosity",
      "broadcast_on_resolved",
      "unacked_reminder_after_s",
      "unacked_reminder_mention",
      "unacked_reminder_mention_list",
      "unacked_reminder_mention_min_severity",
    ],
  },
  {
    id: "retention",
    title: "Retention",
    blurb: "Storage, not behaviour. Nothing here changes what oto says or when.",
    keys: ["raw_retention_days", "event_retention_months"],
  },
];

/* -------------------------------------------------------------------------- */
/* Verbosity                                                                  */
/* -------------------------------------------------------------------------- */

// Every option list below is typed against the generated enum, so a value the
// contract drops fails the build here rather than rendering a control that
// offers something the server will refuse.
export const VERBOSITY_OPTIONS: readonly { readonly value: Verbosity; readonly label: string }[] = [
  { value: "all", label: "all — every transition, including comments and enrichments" },
  { value: "status_changes", label: "status_changes — firing, resolved, acknowledged, snoozed" },
  { value: "firing_and_resolved", label: "firing_and_resolved — the two ends, nothing between" },
  { value: "firing_only", label: "firing_only — the quietest; resolves never reach the channel" },
];

/**
 * The mention vocabulary. `here` and `channel` are offered because they are
 * expressible in Slack and an operator may know something about their workspace
 * that the documentation does not say — but the label says what the evidence
 * says, because a control that silently does nothing is the worst outcome here.
 */
export const MENTION_MODE_OPTIONS: readonly {
  readonly value: ReminderMention;
  readonly label: string;
}[] = [
  { value: "none", label: "none — no mention (oto's default)" },
  { value: "here", label: "here — @here; believed not to notify from a thread" },
  { value: "channel", label: "channel — @channel; believed not to notify from a thread" },
  { value: "list", label: "list — the explicit audience below; the only form documented to notify" },
];

/** The entries a mention list accepts, stated so the shape is not a guessing game. */
export const MENTION_TOKEN_HINT =
  "A Slack user id as <@U…> or a usergroup id as <!subteam^S…>. @here and @channel are modes, not entries.";

export const SEVERITY_OPTIONS: readonly {
  readonly value: ReminderMentionSeverity;
  readonly label: string;
}[] = [
  { value: "critical", label: "critical — only the loudest (oto's default)" },
  { value: "warning", label: "warning — critical and warning" },
  { value: "info", label: "info — every severity" },
];

/** The list cap, which the server also enforces. Ten is a cap, not a courtesy. */
export const MENTION_LIST_MAX = 10;

/* -------------------------------------------------------------------------- */
/* Units                                                                      */
/* -------------------------------------------------------------------------- */

/** How a raw number reads to a human. Seconds get the Prometheus spelling. */
export function readValue(kind: KnobKind, value: number): string {
  switch (kind) {
    case "seconds":
      return value === 0 ? "unset" : duration(value);
    case "days":
      return `${value} day${value === 1 ? "" : "s"}`;
    case "months":
      return `${value} month${value === 1 ? "" : "s"}`;
    case "count":
      return `${value}`;
    default:
      return `${value}`;
  }
}

export function unitSuffix(knob: KnobCopy): string {
  if (knob.unit !== undefined) return knob.unit;
  switch (knob.kind) {
    case "seconds":
      return "seconds";
    case "days":
      return "days";
    case "months":
      return "months";
    default:
      return "";
  }
}

/** Numeric knobs are the ones the server publishes bounds for. */
export function isNumeric(kind: KnobKind): boolean {
  return kind === "seconds" || kind === "count" || kind === "days" || kind === "months";
}
