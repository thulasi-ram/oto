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
import { VerbositySchema } from "~/api/generated/validators";
import type {
  ReceiverBasis,
  ReceiverRoute,
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
    why: "The clock rate of oto's whole view of the world. oto never learns about a change to an existing case faster than this, so every duration below should be read as a multiple of it rather than as an absolute time.",
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
  | "boolean";

/* ⛔ `mentionMode`, `mentionList` and `severity` WERE KINDS HERE AND ARE DELETED
   (git-bug bd0fb1d). They existed only for the unacked reminder's mention
   audience, which the owner ruled goes with the reminder. No knob has those
   kinds any more, so their option tables and their controls in TuningSection.tsx
   went too — a control for a setting the server no longer has is worse than no
   control, because it saves and then does nothing. */

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

// ⛔ THE THREE FLAP KNOBS SHARE ONE VERDICT, AND IT IS NOT A VERDICT ABOUT THE
// VALUE (git-bug 235f347). Their `what` and `risks` copy already says the damper
// is gone; the guides went on computing the retired detector's arithmetic and
// phrasing the result as "Unreachable", "Reachable but only just" and "About half
// the observable ceiling" — three levels, a one-click `suggest`, and every word of
// it about arming and pacing a mechanism that no longer exists. A row that says
// "this changes nothing" in one paragraph and offers a tuned number in the next
// contradicts itself, and the operator acts on the confident half.
//
// ⭐ AND NO `suggest`. A suggestion is an invitation to click. Offering one on a
// key that decides nothing spends the operator's trust to change a number that
// changes no delivery.
//
// It reads nothing off the Alertmanager reference, deliberately: there is no
// arithmetic left to argue from, and withholding is what `KnobCopy.guide`'s own
// discipline calls for when there is no computation to stand on.
const retiredFlapGuide = (): Guidance => ({
  level: "inert",
  text:
    "Retired, and no value here changes what is delivered. The flap detector this " +
    "knob sized is gone: nothing recomputes flap_score or is_flapping (ADR 0041 " +
    "Amendment 1), and the writer refuses to record `flapping` as a suppression " +
    "reason at all. Flap noise is absorbed one layer earlier, at case formation, " +
    "by the case-retention window W — set per (namespace, alertname), 0 to 86400 " +
    "seconds, and 0 is what ships. W is the control that decides what a flap costs.",
});

/* -------------------------------------------------------------------------- */
/* The knobs                                                                  */
/* -------------------------------------------------------------------------- */

export const KNOBS: Readonly<Record<KnobKey, KnobCopy>> = {
  /* ---- threads and lifecycle -------------------------------------------- */

  refire_grace_s: {
    key: "refire_grace_s",
    kind: "seconds",
    label: "Re-fire grace",
    what: "An alert resolves, then the same alert fires again. This window no longer decides what happens to that alert: a case is terminal, so every re-fire opens the next case, unacknowledged, however fast it arrives. Nothing reopens a closed case. What the number still does is bound two others — its floor is twice the 5-minute ingest replay window, because a re-fire arriving inside that window is dropped as a duplicate delivery before anything can see it, and Group close delay ships pinned equal to it. Whether a re-fire gets a brand-new Slack root message and thread, or updates the open generation's existing card in its existing thread, is decided entirely by Group close delay.",
    risks: [
      {
        label: "Too short",
        text: "A window nothing can arrive in. Alertmanager will not report a changed group sooner than one group_interval after the last notification, and oto drops a replayed batch for 5 minutes, so a grace below either floor describes a band no re-fire can land in. Nothing breaks — the setting decides no transition — but it also stops describing anything, and because Group close delay is meant to sit at or above it, a low value here is the number an operator lowers that one against. That is the setting that fragments your Slack threads.",
      },
      {
        label: "Too long",
        text: "Case counts and durations are unaffected — two outages hours apart are always two cases, each with its own ack and its own firing duration. What a long value costs you is Slack: Group close delay is pinned at or above this one, so raising this raises the floor under how long a generation is held open, and this morning's thread grows a reply about tonight's incident. Above a day, two genuinely separate incidents share one root card.",
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
          text: `Unreachable. Shorter than ${named}, so the window has always expired before oto can hear about a re-fire at all. The case is new either way; what this value stops doing is describing anything, and it is the floor Group close delay is set against.`,
          suggest: want,
        };
      }
      if (v < ASSUMED_RULE_FOR_S) {
        return {
          level: "inert",
          text: `Unreachable for an ordinary rule. With ${basis}, a re-fire cannot be detected by Prometheus until ${duration(ASSUMED_RULE_FOR_S)} after the resolve, and oto hears about it up to one ${named} later still. The case is new either way; what this value stops doing is describing anything, and it is the floor Group close delay is set against.`,
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
    what: "How long an alert group stays open after its last member's case ends. A group is Alertmanager's batch of alerts, not one alert's firing — closing one is what makes the next fire open a new generation, and a new generation is a new Slack root message.",
    risks: [
      {
        label: "Too short",
        text: "A generation closes between two Alertmanager batches of the same problem, so the second half of it arrives as a brand-new group with a brand-new root card.",
      },
      {
        label: "Too long",
        text: "The generation spans genuinely separate incidents, and tonight's fire lands as an update to a card about something that ended this morning.",
      },
    ],
    amRule:
      "Keep it at or above group_interval, and at or above the re-fire grace — the second one is not a suggestion. A close delay shorter than the grace gives you a re-fire that oto correctly classified as the same problem coming back, and then posts a brand-new root card for it anyway, which is the entire thing the grace exists to prevent. oto shipped 5m against a 10m grace and defeated half its own grace that way; the two defaults are now equal. Equal is safe rather than racy: this clock starts at the group's last activity, which is the resolve as oto observed it, while the grace clock starts at the case's upstream ended_at, which is the same instant or earlier.",
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

  /* ---- flap keys, retained and inert (migration 00057, ADR 0041 Am. 1) --- */

  flap_threshold: {
    key: "flap_threshold",
    kind: "count",
    label: "Flap threshold",
    unit: "transitions",
    what: "Retained, and it changes no delivery. It sized the retired flap.score job, which counted an alert's case.opened and case.resolved events over the flap window and marked the alert flapping past this many transitions; nothing recomputes flap_score or is_flapping now (ADR 0041 Amendment 1), and nothing withholds or coalesces a notification because an alert oscillates. Flap noise is absorbed one layer earlier, at case formation, by the case-retention window W.",
    risks: [
      {
        label: "Too high",
        text: "Nothing fails to engage, because there is nothing left to engage. The damper this number sized was at delivery — a withheld reply plus a coalesced summary — and it is gone: the writer refuses to record `flapping` as a suppression reason at all, so no value here can make oto quieter about a firing.",
      },
      {
        label: "Too low",
        text: "Nothing is marked flapping either, at any value: the score that read this threshold is not computed. The one failure left is a person changing this number and believing they changed oto's behaviour. The control that actually decides what a flap costs is the case-retention window W, set per (namespace, alertname) from 0 to 86400 seconds, and 0 — no window, no damping — is what ships.",
      },
    ],
    amRule:
      "The arithmetic below is the retired detector's, kept because the live verdicts on this row still compute it; no value here changes what is delivered. The for: trap. One observable fire-resolve-fire cycle pays the larger of two floors — the rule's for: dwell, and one group_interval per notification, of which a cycle needs two — so a cycle costs group_interval + max(group_interval, for) and yields two counted transitions. The ceiling in a window W is about 2 x floor(W / cycle); set the threshold at roughly half of it. For long-for: rules do not lower the threshold to 2 — two transitions is a normal deploy. Widen the window instead.",
    guide: retiredFlapGuide,
  },

  flap_window_s: {
    key: "flap_window_s",
    kind: "seconds",
    label: "Flap window",
    what: "Retained, and it changes no delivery. It was the span the retired flap score counted transitions over. What absorbs a flap now is the case-retention window W: a resolve holds the case open for W and closes it only once the alert has stayed resolved for W, so a re-fire inside W joins the still-open case — one case across the flap, one notification, one thread reply. W is a different setting on a different axis, per (namespace, alertname), and is not this number.",
    risks: [
      {
        label: "Too short",
        text: "Nothing narrows. A window shorter than one group_interval could not hold two transitions oto is able to observe, which is what made the old 30m default unreachable for every rule shape; with nothing counting transitions, no width here suppresses, coalesces or delays anything.",
      },
      {
        label: "Too long",
        text: "Nothing stays marked flapping for longer, because nothing is marked flapping. alerts.flap_score and alerts.is_flapping keep the last value they were written and are never recomputed — they are readable and unwritable — so no width here can put an alert back on a flapping list.",
      },
    ],
    amRule:
      "The arithmetic below is the retired detector's, kept because the live verdicts on this row still compute it; no value here changes what is delivered. For a rule with a long for:, widen this rather than lowering the threshold: flap_window is about flap_threshold x the observable cycle, and the cycle is group_interval + max(group_interval, for). With for: 15m and group_interval 5m the cycle is 20m, so a threshold of 5 needs about 100 minutes — which is where the shipped 2h comes from, and why the old 30m window made the threshold unreachable for every rule shape.",
    guide: retiredFlapGuide,
  },

  flap_digest_interval_s: {
    key: "flap_digest_interval_s",
    kind: "seconds",
    label: "Flap digest interval",
    what: "Retained, and it changes no delivery. It paced the coalesced summary a flapping alert used to receive in place of its per-transition replies; that summary is deleted along with the flapping arm of the damper, and no notification is coalesced on flapping any more. A flap that the case-retention window W absorbs never produces the extra replies there would be something to summarise.",
    risks: [
      {
        label: "Too short",
        text: "Nothing lands more often. No flapping summary is produced at any interval, so this cannot outrun group_interval or anything else.",
      },
      {
        label: "Too long",
        text: "Nothing lands later, and nothing is quiet in the meantime: the replies this interval used to stand in for are no longer withheld. The failure left here is the name — a notification policy's digest window (digest_window_seconds, migration 00058) is a summary of a window over a namespace, on a different object, and this key does not pace it.",
      },
    ],
    amRule:
      "The arithmetic below is the retired damper's, kept because the live verdicts on this row still compute it; no value here changes what is delivered. Keep it at or above group_interval. Two to four times group_interval is the useful range.",
    guide: retiredFlapGuide,
  },

  /* ---- what reaches the channel ----------------------------------------- */

  /* ⛔ FOUR REMINDER KNOBS WERE HERE AND ARE DELETED (git-bug bd0fb1d):
     unacked_reminder_after_s and the three unacked_reminder_mention* keys. The
     owner withdrew the unacked reminder — oto sends nothing unprompted — and
     ruled the mention goes with it, because a mention was never a property of
     Slack delivery in general: it was the audience half of that one fact.

     The mention copy carried a research result worth not losing: Slack documents
     that @here and @channel do NOT notify from inside a thread, and oto's
     reminder was a thread reply, so those two modes were believed to be silent
     no-ops in the only position oto used them. That finding lives on in ADR 0020
     and is why the default was `none`. */

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
        text: "You lose the ability to reproduce an ingestion bug from the payload that caused it, and to replay a stored batch after a parser fix. A replay reads the stored bytes: past this boundary there are none, and no flag overrides that. An ingestion defect reported three weeks late can no longer be shown the bytes that caused it. The alerts themselves, their cases, acks and timelines are untouched.",
      },
      {
        label: "Too long",
        text: "Age is not what makes a replay unsafe, so a long window costs disk rather than correctness. A replay is refused when an alert the batch would touch has moved on since the batch arrived — a later batch already wrote to that alert, or its case has closed while this batch still says firing — and it is allowed at any age the bytes survive. The disk: thirty days of payloads is 51 MB at 1 000 alert firings a day and 510 MB at 10 000; ninety days is 153 MB and 1.5 GB. Each extra day also keeps a day of event-dedupe rows, and both windows are set by the longest window any single org asked for, so one org on 365 days sets them for every tenant.",
      },
    ],
    amRule:
      "Nothing in alertmanager.yml bears on this. The shipped thirty days is chosen, not derived: it was derived from oto's event-idempotency horizon until replay moved its gate from age to supersession, and nothing derives it now. What it is: the depth of the rejections and failed-batch feeds, which take no date range, and the window an ingestion defect gets to be found and replayed in. Raising it is free; lowering it drops days that cannot come back, and it reclaims payload bytes and nothing else — the thirty-day event-dedupe floor is held by the reconciler re-applying transitions, not by replay.",
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
    title: "Flap damping (retired)",
    blurb:
      "Nothing here damps a flap. Flap noise is absorbed at case formation by the case-retention window W (migration 00057, ADR 0041 Amendment 1): a re-fire inside W lands in the still-open case, so an oscillating alert is one case with one thread reply rather than one of each per flap. These three keys are kept because deleting a settings key is a contract change of its own, and whether they are renamed, re-homed or removed is left open on purpose — they change no delivery at any value.",
    keys: ["flap_threshold", "flap_window_s", "flap_digest_interval_s"],
  },
  {
    id: "channel",
    title: "What reaches the channel",
    blurb:
      "oto's primary verb is an in-place edit of a card, and an edit is completely silent — no notification, no unread, nothing rises in the channel. These settings decide what is allowed to be louder than that.",
    keys: [
      "default_verbosity",
      "broadcast_on_resolved",
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

/* ⛔ THE MENTION VOCABULARY WAS HERE AND IS DELETED (git-bug bd0fb1d):
   MENTION_MODE_LABEL, MENTION_MODE_OPTIONS, MENTION_TOKEN_HINT, SEVERITY_LABEL,
   SEVERITY_OPTIONS and MENTION_LIST_MAX.

   ⭐ ONE THING IN IT WAS A RESEARCH RESULT AND IS WORTH NOT LOSING TWICE: the
   labels said `here` and `channel` are "believed not to notify from a thread",
   because Slack documents exactly that and oto's reminder was a thread reply.
   `list` was "the only form documented to notify". That is why the default was
   `none`, it is recorded in ADR 0020, and `2078a07` notes the whole mention path
   was never once observed working against a real workspace. */

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
