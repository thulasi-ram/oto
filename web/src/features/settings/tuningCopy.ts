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
 *      inline at each knob rather than in a help page. The three numbers
 *      everything depends on are READ off the source — `SourceHealthDTO.
 *      route_timings` — with the provenance of each carried beside it, so a
 *      verdict can say whether it is arguing from the operator's own setting or
 *      from Alertmanager's documented default. They were typed into a form and
 *      kept in one browser's localStorage until this screen was rebuilt; see
 *      `AmRef`.
 *
 * Vocabulary here is bound by SCOPE-BOUNDARY §3 and enforced by
 * `tools/lintvocab`. Notably: the unacked reminder is a reminder, never a
 * ladder; oto measures a signal's **firing duration**, never anyone's response.
 */
import { maxLengthOf } from "~/api/bounds";
import {
  ReminderMentionSchema,
  ReminderMentionSeveritySchema,
  UpdateOrgSettingsRequestSchema,
  VerbositySchema,
} from "~/api/generated/validators";
import type {
  ReceiverBasis,
  ReceiverRoute,
  ReminderMention,
  ReminderMentionSeverity,
  TimingProvenance,
  UpdateOrgSettingsRequest,
  Verbosity,
} from "~/api/types";
import { duration } from "~/lib/format";

/* -------------------------------------------------------------------------- */
/* The Alertmanager reference                                                 */
/* -------------------------------------------------------------------------- */

/**
 * One upstream timing, with the provenance oto served for it.
 *
 * ⛔ THE PROVENANCE IS NOT A FOOTNOTE. `observed` and `default_applies` produce
 * identical arithmetic — a 2m re-fire grace is just as unreachable under a
 * defaulted 5m `group_interval` as under a configured one — but they call for
 * different actions. Under `observed` there is a line in `alertmanager.yml` to
 * change; under `default_applies` there is no such line, and the operator either
 * adds one or moves the oto knob instead. Rendering them the same throws away
 * the only part of this screen that is advice.
 */
export interface AmTiming {
  /** The duration in force. `null` exactly when `provenance` is `unknown`. */
  readonly seconds: number | null;
  readonly provenance: TimingProvenance;
}

/**
 * The upstream numbers every oto duration is a multiple of, for ONE source.
 *
 * ⭐ THEY ARE READ, NEVER TYPED IN. This used to be four inputs whose values were
 * kept in one browser's `localStorage`: unshared, so the person beside you saw
 * different guidance; unvalidated, so nothing checked them against the cluster;
 * and silently wrong the moment somebody edited `alertmanager.yml`. oto reads all
 * three off `config.original` on the status call it already makes, and serves
 * them on `SourceHealthDTO.route_timings`.
 *
 * ⚠️ THE THREE ARE PER-ROUTE AND INHERITED, so the numbers governing the alerts
 * OTO is sent are the ones on the route delivering to oto's own receiver — not
 * the top-level route, on any Alertmanager that overrides anything. `route` says
 * which of the two these three are, `routes` is the whole resolved tree, and
 * `routesAgree` is false when several routes reach oto and disagree, in which
 * case there IS no single answer and this screen must not pretend otherwise.
 */
export interface AmRef {
  readonly sourceId: string;
  readonly sourceName: string;
  readonly groupWait: AmTiming;
  readonly groupInterval: AmTiming;
  readonly repeatInterval: AmTiming;
  /**
   * Which route the three above describe: `oto_receiver` (the route oto's own
   * receiver hangs off) or `top_level` (the fallback — what governs every alert
   * matching nothing more specific).
   */
  readonly route: RouteBasis;
  /** How many descendant routes exist, and how many state a timing of their own. */
  readonly childRoutes: number;
  readonly childRoutesWithTimings: number;
  /** The receiver oto believes is its own, and how it decided. */
  readonly receiver: string | null;
  readonly receiverBasis: ReceiverBasis;
  /** Every receiver in the config with a webhook integration — the candidates. */
  readonly webhookReceivers: readonly string[];
  /** Every delivering route in the tree, in the order Alertmanager evaluates them. */
  readonly routes: readonly ReceiverRoute[];
  /** False only when several routes reach oto's receiver and state different timings. */
  readonly routesAgree: boolean;
  /** How many routes the parser's cap discarded; non-zero means `routes` is partial. */
  readonly routesDropped: number;
  /** When the configuration was last read off the source. Null if never. */
  readonly observedAt: string | null;
  /** The Alertmanager version any defaulted field is attributed to. */
  readonly defaultsFromVersion: string | null;
  /** False when the source is newer than the release oto checked the constants against. */
  readonly defaultsVerified: boolean;
}

/** Which route an `AmRef`'s three headline numbers came from. */
export type RouteBasis = "top_level" | "oto_receiver";

/**
 * How the identification of oto's own receiver was decided.
 *
 * ⛔ IT IS AN INFERENCE AND MUST NEVER BE RENDERED AS A READING. oto's ingest
 * path is `/api/v1/ingest/alertmanager/{source_id}`, so an operator's webhook URL
 * contains the id of the source oto is probing and would identify oto's receiver
 * exactly — but Alertmanager marshals `webhook_config.url` as `<secret>`, so that
 * URL never reaches oto. One webhook receiver means that one is oto's; several
 * means the screen shows every candidate and claims none.
 */
export const RECEIVER_BASIS_COPY: Record<ReceiverBasis, { label: string; detail: string }> = {
  sole_webhook: {
    label: "one webhook receiver",
    detail:
      "This configuration has exactly one receiver with a webhook integration, so that is oto's. Alertmanager redacts the webhook URL itself (it comes back as <secret>), so this is an inference from the shape of the config rather than a reading of oto's own address — but with a single candidate there is nothing to confuse it with.",
  },
  ambiguous: {
    label: "several webhook receivers",
    detail:
      "More than one receiver has a webhook integration and Alertmanager redacts the URL that would tell them apart, so oto cannot say which is its own. Every route in the tree is listed below and none is claimed: picking one would be a coin toss shown as a reading.",
  },
  no_webhook: {
    label: "no webhook receiver",
    detail:
      "No receiver in this configuration has a webhook integration, so nothing here can push alerts to oto. If this source is meant to feed oto, the receiver block is missing.",
  },
  unknown: {
    label: "configuration not read",
    detail:
      "oto has not managed to read this source's configuration, so there is not even a receiver list to reason about. Every verdict that depends on these numbers is withheld.",
  },
};

/**
 * The rules' `for:`, as a STATED ASSUMPTION rather than an input.
 *
 * ⛔ oto does not read your rule files, and this screen no longer pretends it can
 * be told. A per-browser number for this was the same unshared, unvalidated
 * localStorage value as the other three, and it fed the flap guidance — so two
 * operators could be given contradictory verdicts about the same rule. Every
 * verdict that depends on this number says so in the same sentence.
 *
 * ⭐ IT IS 15 MINUTES, AND THAT IS MEASURED RATHER THAN ASSERTED. It used to say
 * "five minutes is the commonest `for:` in the wild", which is false: across the
 * 155 alerting rules kube-prometheus-stack 88.2.0 ships — kubernetes-mixin, the
 * node-exporter mixin and the Prometheus/Alertmanager/etcd/KSM mixins — `15m` is
 * both the MODE (69 rules, 44.5%) and the MEDIAN, while `5m` is 12.9%. oto's own
 * rule pack (`deploy/prometheus/oto-rules.yaml`) agrees: `15m` is the mode of its
 * non-instantaneous rules. See ADR 0026 and docs/setup/tuning.md.
 */
export const ASSUMED_RULE_FOR_S = 900;

/**
 * How long one fire → resolve → fire cycle takes to be OBSERVED, in seconds.
 *
 * ⛔ THE `Math.max` IS THE WHOLE POINT AND IT USED TO BE MISSING. A cycle has two
 * independent floors and pays the larger: the RULE floor (the condition must hold
 * for `for:` all over again) and the TRANSPORT floor (Alertmanager will not send
 * two notifications for one group closer together than `group_interval`, and a
 * cycle needs two — one resolved, one firing). The old arithmetic was
 * `for + group_interval`, which claims an alert with no `for:` can cycle in one
 * `group_interval`. It cannot: it needs two flushes. That missing term is why the
 * shipped 5-in-30m flap default was unreachable even for the rule shape it was
 * supposedly written for.
 *
 * One cycle yields exactly TWO counted transitions, which is where the `2 ×` in
 * every ceiling below comes from.
 */
export function observableCycleS(groupIntervalS: number, forS: number): number {
  return groupIntervalS + Math.max(groupIntervalS, forS);
}

/** The three fields, with why each one governs what it governs. */
export interface AmFieldCopy {
  readonly key: "groupWait" | "groupInterval" | "repeatInterval";
  readonly label: string;
  readonly source: string;
  readonly why: string;
}

export const AM_FIELDS: readonly AmFieldCopy[] = [
  {
    key: "groupWait",
    label: "group_wait",
    source: "route.group_wait in alertmanager.yml",
    why: "A floor on alert-to-Slack latency that oto cannot improve. It also hides the fastest flaps entirely: an alert that resolves before group_wait elapses produces no notification at all, and oto cannot damp, count or report what it is never told about.",
  },
  {
    key: "groupInterval",
    label: "group_interval",
    source: "route.group_interval",
    why: "The clock rate of oto's whole view of the world. oto never learns about a change to an existing group faster than this, so every duration below should be read as a multiple of it rather than as an absolute time.",
  },
  {
    key: "repeatInterval",
    label: "repeat_interval",
    source: "route.repeat_interval",
    why: "Produces the notification oto delivers as an update rather than a new message — the single largest noise reduction oto provides, and it needs no tuning. Its consequence is that an unacknowledged critical is re-sent only this often, which is why oto runs its own unacked-reminder clock.",
  },
];

/** The duration to compute with, or null when oto genuinely cannot say. */
export function amSeconds(t: AmTiming): number | null {
  return t.provenance === "unknown" ? null : t.seconds;
}

/**
 * How a timing is named inside a verdict.
 *
 * "your group_interval of 5m" and "Alertmanager's default group_interval of 5m"
 * are the same number and different instructions, and this is the one function
 * that keeps them apart in every sentence on the screen.
 */
export function amPhrase(label: string, t: AmTiming): string {
  const value = duration(t.seconds ?? 0);
  return t.provenance === "observed"
    ? `your ${label} of ${value}`
    : `Alertmanager's default ${label} of ${value}`;
}

/* -------------------------------------------------------------------------- */
/* Knob descriptions                                                          */
/* -------------------------------------------------------------------------- */

/**
 * Every knob this screen can edit — DERIVED from the write contract itself.
 *
 * ⛔ IT WAS SEVENTEEN LITERALS, AND `KNOBS` BELOW IS A `Record<KnobKey, …>`. So
 * the day the contract grows a setting, this union grows with it and `KNOBS`
 * stops compiling until somebody writes the two failure modes for it — which is
 * exactly the outcome worth having on the one screen where a badly-explained
 * control does real damage. A hand-written union would instead have quietly
 * omitted it, and the new setting would be invisible and unreachable here.
 *
 * `reset` is excluded because it is not a setting: it is the verb that removes
 * one, and the screen models it as `resets[]` rather than as a knob.
 */
export type KnobKey = Exclude<keyof UpdateOrgSettingsRequest, "reset">;

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
  /**
   * Evaluated against the SOURCE'S OWN Alertmanager numbers on every keystroke.
   *
   * It returns `null` when the arithmetic cannot be done — a timing whose
   * provenance is `unknown` has no number, and inventing one is exactly what this
   * screen stopped doing. A defaulted timing DOES produce a verdict, because the
   * default is what governs; the wording says whose number it is.
   */
  readonly guide?: (value: number, am: AmRef, num: (key: KnobKey) => number) => Guidance | null;
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
    what: "An alert resolves, then the same alert fires again. Inside this window the existing case reopens, oto reuses the existing Slack thread and the card updates in place — the one case that produces no new root message, and therefore the one oto surfaces in the channel so it is not missed. Outside the window a new case opens, and once the group has closed that means a new generation: a brand-new Slack root message and a brand-new thread.",
    risks: [
      {
        label: "Too short",
        text: "Every re-fire opens a new Slack thread. Alertmanager will not report a changed group sooner than one group_interval after the last notification, so if the grace window is shorter than that it has always expired by the time oto is even capable of hearing about the re-fire. You get the wall of near-identical messages oto exists to prevent, produced by a setting that looks like it should have prevented it.",
      },
      {
        label: "Too long",
        text: "History that lies. Two genuinely separate outages hours apart are recorded as one case that reopened — one thread, one firing duration with a long gap in the middle, and this morning's thread grows a reply about tonight's incident. Case counts under-report and duration statistics stop meaning anything.",
      },
    ],
    amRule:
      "Two floors, and the RULE floor is usually the binding one. The grace clock starts at the case's ended_at, which oto takes from the upstream EndsAt — when Prometheus stopped considering the rule firing, not when oto heard about it. So the alert must hold its condition for the rule's whole for: all over again, and Alertmanager then batches the notification: the earliest re-fire oto can observe lands at for + up to one group_interval. The transport floor is 2 x group_interval. Set the grace above whichever is larger, then check the top end against how long your incidents actually last.",
    // ⛔ THE `for:` TERM IS THE ONE THAT MATTERS AND IT USED TO BE ABSENT. This
    // verdict compared the value only against `group_interval`, which is the
    // smaller floor for every rule whose `for:` exceeds it — i.e. for 82% of the
    // rules a real cluster runs. It reported the old 600s default as comfortably
    // fine while the modal `for: 15m` rule could never reach it.
    guide: (v, am) => {
      const gi = amSeconds(am.groupInterval);
      if (gi === null) return null;
      const named = amPhrase("group_interval", am.groupInterval);
      const basis = `an assumed for: of ${duration(ASSUMED_RULE_FOR_S)}`;
      const ruleFloor = ASSUMED_RULE_FOR_S + gi;
      const want = Math.max(ruleFloor, gi * 2);
      if (v < gi) {
        return {
          level: "inert",
          text: `Unreachable. Shorter than ${named}, so the window has always expired before oto can hear about a re-fire at all. Every re-fire will open a new Slack thread.`,
          suggest: want,
        };
      }
      if (v < ASSUMED_RULE_FOR_S) {
        return {
          level: "inert",
          text: `Unreachable for an ordinary rule. With ${basis}, a re-fire cannot be detected by Prometheus until ${duration(ASSUMED_RULE_FOR_S)} after the resolve, and oto hears about it up to one ${named} later still. Every re-fire will open a new Slack thread.`,
          suggest: want,
        };
      }
      if (v < want) {
        return {
          level: "tight",
          text: `Reachable only by a re-fire that lands in the very first batch after the resolve. With ${basis} and ${named}, the typical re-fire is observed ${duration(ruleFloor)} after the resolve.`,
          suggest: want,
        };
      }
      return ok(
        `Above both floors: the transport floor of 2 x group_interval (${duration(gi * 2)}) and the rule floor of for + group_interval (${duration(ruleFloor)}), with ${basis}.`,
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
      "Keep it at or above group_interval, and at or above the re-fire grace — the second one is not a suggestion. A close delay shorter than the grace gives you a re-fire that oto correctly classified as the same problem coming back, and then posts a brand-new root card for it anyway, which is the entire thing the grace exists to prevent. oto shipped 5m against a 10m grace and defeated half its own grace that way; the two defaults are now equal. Equal is safe rather than racy: this clock starts at the group's last activity, which is the resolve as oto observed it, while the grace clock starts at the upstream ended_at, which is the same instant or earlier.",
    // ⛔ ONE SUGGESTION CLEARS BOTH FLOORS, BECAUSE THERE ARE TWO AND THE BUTTON
    // WRITES ONE NUMBER. The first branch used to offer `group_interval` alone
    // while saying nothing about the grace — so against the shipped pair
    // (group_interval 5m, grace 20m) the button read "use 300", and clicking it
    // landed the operator straight in the second branch: a second warning and a
    // second button, from a fix that was known-insufficient when it was offered.
    guide: (v, am, num) => {
      const gi = amSeconds(am.groupInterval);
      if (gi === null) return null;
      const named = amPhrase("group_interval", am.groupInterval);
      const refire = num("refire_grace_s");
      // An empty or mid-edit grace box parses to NaN. It withdraws the second
      // comparison rather than poisoning the first one.
      const want = Number.isFinite(refire) ? Math.max(gi, refire) : gi;
      if (v < gi) {
        return {
          level: "inert",
          text: `Below ${named}. A generation can close between two batches of one incident.`,
          suggest: want,
        };
      }
      if (Number.isFinite(refire) && v < refire) {
        return {
          level: "tight",
          text: `Shorter than the re-fire grace (${duration(refire)}). A re-fire inside the grace window would still find a closed group, so it gets a new root message despite the grace.`,
          suggest: want,
        };
      }
      return ok(`At or above ${named}, and not shorter than the re-fire grace.`);
    },
  },

  resolve_grace_s: {
    key: "resolve_grace_s",
    kind: "seconds",
    label: "Resolve grace",
    what: "How long past an alert's upstream end-time lease oto waits before the reaper marks the case expired. Expired is not resolved: it means oto stopped hearing about this, never that the problem went away.",
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
    what: "Transitions inside the flap window before oto marks an alert flapping. A flapping alert still opens and closes cases and its card still updates; what stops is the per-transition thread replies, replaced by one coalesced digest. Flapping is a visible state, never a silent drop.",
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
      "The for: trap. One observable fire-resolve-fire cycle pays the larger of two floors — the rule's for: dwell, and one group_interval per notification, of which a cycle needs two — so a cycle costs group_interval + max(group_interval, for) and yields two counted transitions. The ceiling in a window W is about 2 x floor(W / cycle); set the threshold at roughly half of it. For long-for: rules do not lower the threshold to 2 — two transitions is a normal deploy. Widen the window instead.",
    guide: (v, am, num) => {
      const gi = amSeconds(am.groupInterval);
      if (gi === null) return null;
      const w = num("flap_window_s");
      // ⛔ WITHOUT THE WINDOW THERE IS NO ARITHMETIC, AND THIS USED TO SAY "ok"
      // ANYWAY. Every number below is derived from `w`: an empty or mid-edit
      // window box makes the ceiling NaN, both comparisons then read false, and
      // control fell through to `ok()` — the operator was told "About half the
      // observable ceiling of NaN" in the confident tone they act on, on the
      // strength of no computation at all. The discipline stated at the top of
      // `KnobCopy.guide` is to withhold instead.
      if (!Number.isFinite(w)) return null;
      const cadence = observableCycleS(gi, ASSUMED_RULE_FOR_S);
      const ceiling = 2 * Math.floor(w / cadence);
      // oto does not read rule files, so the `for:` half of this arithmetic is an
      // assumption and every verdict below says so in the same breath.
      const basis = `an assumed for: of ${duration(ASSUMED_RULE_FOR_S)} and ${amPhrase("group_interval", am.groupInterval)}`;
      if (v > ceiling) {
        return {
          level: "inert",
          text: `Unreachable. With ${basis}, a ${duration(w)} window can contain at most about ${ceiling} transition${ceiling === 1 ? "" : "s"} oto is able to observe. The damper can never engage — it is dead code that looks configured. Widen the window rather than lowering the threshold.`,
        };
      }
      if (v > Math.floor(ceiling / 2)) {
        return {
          level: "tight",
          text: `Reachable but only just: with ${basis}, the observable ceiling in a ${duration(w)} window is about ${ceiling}, and the doc puts a workable threshold at roughly half of that.`,
          suggest: Math.max(3, Math.floor(ceiling / 2)),
        };
      }
      return ok(
        `About half the observable ceiling of ${ceiling} for a ${duration(w)} window, with ${basis}.`,
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
      "For a rule with a long for:, widen this rather than lowering the threshold: flap_window is about flap_threshold x the observable cycle, and the cycle is group_interval + max(group_interval, for). With for: 15m and group_interval 5m the cycle is 20m, so a threshold of 5 needs about 100 minutes — which is where the shipped 2h comes from, and why the old 30m window made the threshold unreachable for every rule shape.",
    guide: (v, am, num) => {
      const gi = amSeconds(am.groupInterval);
      if (gi === null) return null;
      const named = amPhrase("group_interval", am.groupInterval);
      const cycle = observableCycleS(gi, ASSUMED_RULE_FOR_S);
      const t = num("flap_threshold");
      // `threshold ~ floor(W / cycle)` is the "half the ceiling" rule solved for W.
      // It used to carry an extra `x 2`, which demanded a quarter of the ceiling and
      // disagreed with the threshold knob's own verdict on the same two numbers.
      //
      // ⛔ IT IS ALSO WHAT THE `inert` BRANCH OFFERS, AND THAT BRANCH USED TO OFFER
      // `cycle x 3` — a 3 appearing in neither `amRule` nor `docs/setup/tuning.md`,
      // and insufficient by construction for any threshold above 3, so the operator
      // was asked to click twice. There is one rule for this knob and one suggestion
      // derived from it. When the threshold box does not parse there is no such
      // number, and the floor the `inert` branch checks — one whole cycle — is the
      // most that can honestly be offered. `Math.ceil` because `group_wait: 500ms`
      // is legal upstream, so a sub-second `group_interval` makes the cycle
      // fractional while the knob is `v.integer()`.
      const need = Math.max(Math.ceil(cycle), Number.isFinite(t) ? Math.round(t * cycle) : 0);
      if (v < cycle) {
        return {
          level: "inert",
          text: `Shorter than one observable cycle (${duration(cycle)}, from ${named} and an assumed for: of ${duration(ASSUMED_RULE_FOR_S)}). The window cannot contain a single fire-resolve-fire cycle, so no threshold is reachable.`,
          suggest: need,
        };
      }
      if (Number.isFinite(t) && v < need) {
        return {
          level: "tight",
          text: `A threshold of ${t} needs roughly ${duration(need)} to be reachable at an assumed for: of ${duration(ASSUMED_RULE_FOR_S)} and ${named}.`,
          suggest: need,
        };
      }
      return ok(
        Number.isFinite(t)
          ? `Wide enough for a threshold of ${t} at an assumed for: of ${duration(ASSUMED_RULE_FOR_S)}, against ${named}.`
          : `At least one observable cycle (${duration(cycle)}) wide, against ${named} and an assumed for: of ${duration(ASSUMED_RULE_FOR_S)}. The threshold box does not currently hold a number, so there is no threshold to size this against.`,
      );
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
      const gi = amSeconds(am.groupInterval);
      if (gi === null) return null;
      const named = amPhrase("group_interval", am.groupInterval);
      if (v < gi) {
        return {
          level: "tight",
          text: `Below ${named}. It cannot produce more digests than the upstream produces batches — it only jitters when they land.`,
          suggest: gi * 3,
        };
      }
      if (v > gi * 4) {
        return {
          level: "tight",
          text: `Above 4 x group_interval (${duration(gi * 4)}), measured against ${named}, which is the top of the useful range. The digest starts arriving after anyone cared.`,
          suggest: gi * 3,
        };
      }
      // ⛔ `amRule` STATES TWO DIFFERENT THINGS AND THE SENTENCE USED TO CONFLATE
      // THEM. The floor is "at or above group_interval" — that is what decides the
      // level, and it is right. The recommendation is "two to four times
      // group_interval is the useful range". Between 1 x and 2 x the old copy read
      // "1.0 x group_interval — inside the useful 2 x to 4 x range", which refutes
      // itself in eight words, on the screen whose entire purpose is prose.
      const ratio = `${(v / gi).toFixed(1)} x group_interval`;
      return ok(
        v < gi * 2
          ? `${ratio} — at or above ${named}, so it is a real digest, though the useful range starts at 2 x (${duration(gi * 2)}).`
          : `${ratio} — inside the useful 2 x to 4 x range, against ${named}.`,
      );
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
      const gw = amSeconds(am.groupWait);
      if (gw === null) return null;
      const named = amPhrase("group_wait", am.groupWait);
      if (v <= gw) {
        return {
          level: "inert",
          text: `Not longer than ${named}. A burst Alertmanager is still batching arrives in a single delivery after this window closes, so it never looks like a burst.`,
          suggest: gw * 2,
        };
      }
      if (v < gw * 2) {
        return {
          level: "tight",
          text: `Above ${named} but below the 2 x shape the default uses (${duration(gw * 2)}). Storm detection will depend on where a burst falls relative to the batch boundary.`,
          suggest: gw * 2,
        };
      }
      // ⛔ `group_wait: 0` IS A REAL SETTING AND THE RATIO DIVIDED BY IT. It is what
      // an operator writes to switch batching delay off entirely, oto's own parser
      // covers it (`amconfig_test.go:158`), and `RouteTimingDTO.value_ms` says
      // outright that `0` is never a stand-in for "not known" — so every legal
      // window here reported "Infinity x group_wait", a number nobody can act on,
      // for a value that is in fact fine. With no batching delay there is no batch
      // boundary to be past, and that is what the sentence says instead.
      return ok(
        gw > 0
          ? `${(v / gw).toFixed(1)} x group_wait — comfortably past the batch boundary, against ${named}.`
          : `No batching delay at all (${named}), so there is no batch boundary for a burst to fall either side of.`,
      );
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
      const gi = amSeconds(am.groupInterval);
      if (gi === null) return null;
      const named = amPhrase("group_interval", am.groupInterval);
      if (v < gi) {
        return {
          level: "inert",
          text: `Below ${named}. Storm mode will flicker on and off across consecutive batches.`,
          suggest: gi * 2,
        };
      }
      return ok(
        `${(v / gi).toFixed(1)} x group_interval — no flicker across consecutive batches, against ${named}.`,
      );
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
      const ri = amSeconds(am.repeatInterval);
      if (ri === null) return null;
      const named = amPhrase("repeat_interval", am.repeatInterval);
      if (v >= ri) {
        return {
          level: "tight",
          text: `At or beyond ${named}. Alertmanager will already have re-sent the unchanged group before oto's reminder fires, so the reminder adds nothing the channel was not just told.`,
        };
      }
      return ok(`Fires well inside ${named}, which is the point of having this clock at all.`);
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
    what: "How long the raw webhook bodies Alertmanager sent are kept. Past this age the whole day is dropped and the bytes are gone — there is no undo and no copy. Nothing an alert page shows comes from here: this is the debugging record, not the alert record.",
    risks: [
      {
        label: "Too short",
        text: "You lose the ability to reproduce an ingestion bug from the payload that caused it, and to replay a stored batch after a parser fix. An ingestion defect reported three weeks late can no longer be shown the bytes that caused it. The alerts themselves, their episodes, acks and timelines are untouched.",
      },
      {
        label: "Too long",
        text: "Past thirty days a stored batch can no longer be replayed safely anyway — oto's event-dedupe keys have aged out, so re-processing it would append the timeline a second time. Beyond that horizon you are paying disk for bytes nothing can act on.",
      },
    ],
    amRule:
      "Nothing in alertmanager.yml bears on this. The shipped thirty days is derived, not chosen: it is oto's event-idempotency horizon, the longest window in which replaying a stored batch still converges.",
  },

  event_retention_months: {
    key: "event_retention_months",
    kind: "months",
    label: "Event timeline retention",
    what: "How long the instant-by-instant timeline is kept. Past this age the whole month is dropped, permanently — this is the only setting on this screen that destroys something oto cannot rebuild. What it does NOT touch, at any value: the alert itself and its full label set, every firing episode with its start, end, outcome and who acknowledged it, what the rule said at the moment it fired, who was told on which channel in which thread and whether it landed, and the daily hygiene rollups. None of those are ever reaped.",
    risks: [
      {
        label: "Too short",
        text: "The timeline for the months you dropped is gone: every human comment and unack note — which live nowhere else and cannot be recovered — the ordered narrative of transitions, and who or what caused each one. A question asked after the boundary gets the episodes and the outcomes, but not the story between them.",
      },
      {
        label: "Too long",
        text: "oto stores everything in one Postgres and nothing else. Past roughly thirteen months at high alert volume a single org crosses the row count where that stops being comfortable, and the fix is a bigger conversation than this box.",
      },
    ],
    amRule:
      "Nothing in alertmanager.yml bears on this. Thirteen months is the longest default that keeps one org inside oto's single-Postgres design; year-on-year comparisons are served by the daily rollups, which are never deleted. There is no cold-storage export yet, so a month that ages out is not archived anywhere.",
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
    blurb:
      "The only settings here that delete something. Neither changes what oto says or when — they decide how far back you can still look, and lowering either one drops whole partitions permanently. There is no export and no undo. Read what each one destroys before you lower it.",
    keys: ["raw_retention_days", "event_retention_months"],
  },
];

/* -------------------------------------------------------------------------- */
/* Verbosity                                                                  */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ EVERY OPTION LIST BELOW IS AN EXHAUSTIVE `Record` OVER THE GENERATED ENUM,
 * ORDERED BY THE ENUM ITSELF.
 *
 * The distinction matters and it is the whole point of this change. A
 * `readonly {value, label}[]` typed against the union is checked for values it
 * must not contain and never for the one it forgot — a member the server ADDS is
 * simply not offered, and the screen looks perfectly fine. An exhaustive record
 * is a build failure instead, and the list is then generated from the picklist
 * so the two can never fall out of step.
 */
function labelled<T extends string>(
  options: readonly T[],
  labels: Record<T, string>,
): readonly { readonly value: T; readonly label: string }[] {
  return options.map((value) => ({ value, label: labels[value] }));
}

const VERBOSITY_LABEL: Record<Verbosity, string> = {
  all: "all — every transition, including comments and enrichments",
  status_changes: "status_changes — firing, resolved, acknowledged, snoozed",
  firing_and_resolved: "firing_and_resolved — the two ends, nothing between",
  firing_only: "firing_only — the quietest; resolves never reach the channel",
};

export const VERBOSITY_OPTIONS = labelled<Verbosity>(VerbositySchema.options, VERBOSITY_LABEL);

/**
 * The mention vocabulary. `here` and `channel` are offered because they are
 * expressible in Slack and an operator may know something about their workspace
 * that the documentation does not say — but the label says what the evidence
 * says, because a control that silently does nothing is the worst outcome here.
 */
const MENTION_MODE_LABEL: Record<ReminderMention, string> = {
  none: "none — no mention (oto's default)",
  here: "here — @here; believed not to notify from a thread",
  channel: "channel — @channel; believed not to notify from a thread",
  list: "list — the explicit audience below; the only form documented to notify",
};

export const MENTION_MODE_OPTIONS = labelled<ReminderMention>(
  ReminderMentionSchema.options,
  MENTION_MODE_LABEL,
);

/** The entries a mention list accepts, stated so the shape is not a guessing game. */
export const MENTION_TOKEN_HINT =
  "A Slack user id as <@U…> or a usergroup id as <!subteam^S…>. @here and @channel are modes, not entries.";

const SEVERITY_LABEL: Record<ReminderMentionSeverity, string> = {
  critical: "critical — only the loudest (oto's default)",
  warning: "warning — critical and warning",
  info: "info — every severity",
};

export const SEVERITY_OPTIONS = labelled<ReminderMentionSeverity>(
  ReminderMentionSeveritySchema.options,
  SEVERITY_LABEL,
);

/**
 * The list cap — READ from the write schema the server enforces it with.
 *
 * It is a cap, not a courtesy, and it was written here as `10`. That copy is the
 * one that decides what the screen refuses locally, so it is also the one that
 * can start refusing what the server accepts.
 */
export const MENTION_LIST_MAX = maxLengthOf(
  UpdateOrgSettingsRequestSchema,
  "unacked_reminder_mention_list",
);

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
