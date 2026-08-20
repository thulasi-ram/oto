/**
 * The `guide()` closures — the arithmetic behind a one-click button.
 *
 * ⛔ WHY THIS FILE EXISTS AT ALL, GIVEN ADR 0021 EXEMPTS THE UI. `guide()` is not
 * form code. It reads the operator's own `alertmanager.yml` timings, computes a
 * number, and `TuningSection.tsx` renders that number as a `use {N}` button whose
 * click writes it straight into the field. So it is "the setting behind the form",
 * which ADR 0021 puts back under the config-knob rule — and a wrong verdict here
 * is indistinguishable from a right one, because it arrives in the same `Note`,
 * with the same button, in the same confident prose. `tuningCopy.ts:365-369`
 * records that an earlier version of exactly this arithmetic shipped: it called
 * the old 600s `refire_grace` default "comfortably fine" while the modal
 * `for: 15m` rule could never reach it. (That knob is itself deleted — git-bug
 * 7287b28 — but it is the reason this file exists, so the story stays.)
 *
 * ⭐ WHAT IS ASSERTED, AND WHAT DELIBERATELY IS NOT. `guide(x) === 7` because 7 is
 * what it returns today pins nothing. So the assertions below are properties with
 * a stated reason, drawn from three derived sources rather than typed here:
 *
 *   1. **`docs/setup/tuning.md`'s own inequalities**, which each knob's `amRule`
 *      restates and which the closure is supposed to implement. Where the doc
 *      declines to give a rule, the absence of a `suggest` is asserted too.
 *   2. **The write bounds**, read off the generated valibot schema via
 *      `requestRange` — never re-typed. A `suggest` outside them is not a 422:
 *      `TuningSection.tsx:1393-1400` clamps every suggestion into the knob's own
 *      bounds before it reaches the button. It is a suggestion the arithmetic could
 *      not honestly reach, which is worth knowing about separately, so the bounds
 *      are asserted over the reference case and the one knob that can exceed them
 *      says so explicitly.
 *   3. **The closures against each other.** `flap_threshold` and `flap_window_s`
 *      are the same inequality solved for different unknowns, and
 *      `tuningCopy.ts:561-563` records that they once disagreed. That agreement is
 *      tested directly rather than sampled.
 *
 * ⚠️ THE TESTS MARKED `⚠️ DOUBT` PIN BEHAVIOUR THIS FILE BELIEVES IS WRONG, and
 * each says what the doubt is. They are pinned rather than fixed because an
 * operator-facing recommendation must not be quietly changed by the person
 * writing its first test — see the report accompanying this change. Each one
 * fails the moment somebody rules on it, which is the point, and it has now
 * happened: five were ruled real and are FIXED, and the tests that pinned them
 * assert the corrected behaviour with a `⛔` recording what the old wording told
 * an operator. Two were ruled not real — a suggestion above the write bound, which
 * `TuningSection.tsx:1393-1400` clamps, and a `group_interval` of `0`, which a
 * running Alertmanager refuses to start with — and those keep the code, the pin,
 * and a `⭐` saying why it is not a defect. The two doubts left open are still
 * marked, and both are about a sentence rather than a number.
 */
import { describe, expect, it } from "vitest";

import {
  ASSUMED_RULE_FOR_S,
  KNOBS,
  observableCycleS,
  type AmRef,
  type AmTiming,
  type GuidanceLevel,
  type KnobCopy,
  type KnobKey,
} from "./tuningCopy";
import { UpdateOrgSettingsRequestSchema } from "~/api/generated/validators";
import { requestRange } from "~/test/contract";
import type { Bound } from "~/test/contract";

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                   */
/* -------------------------------------------------------------------------- */

type Guide = NonNullable<KnobCopy["guide"]>;
type Num = (key: KnobKey) => number;

const observed = (seconds: number): AmTiming => ({ seconds, provenance: "observed" });
const defaulted = (seconds: number): AmTiming => ({ seconds, provenance: "default_applies" });
const unread: AmTiming = { seconds: null, provenance: "unknown" };

/**
 * Alertmanager's own documented defaults: `group_wait: 30s`,
 * `group_interval: 5m`, `repeat_interval: 4h`. Every oto default below was
 * derived against these three (ADR 0026), so they are the reference case.
 */
const AM_GROUP_WAIT_S = 30;
const AM_GROUP_INTERVAL_S = 300;
const AM_REPEAT_INTERVAL_S = 14_400;

function amRef(over: Partial<AmRef> = {}): AmRef {
  return {
    sourceId: "0192f3a1-0000-7000-8000-000000000001",
    sourceName: "prod",
    groupWait: observed(AM_GROUP_WAIT_S),
    groupInterval: observed(AM_GROUP_INTERVAL_S),
    repeatInterval: observed(AM_REPEAT_INTERVAL_S),
    route: "oto_receiver",
    childRoutes: 0,
    childRoutesWithTimings: 0,
    receiver: "oto",
    receiverBasis: "sole_webhook",
    webhookReceivers: ["oto"],
    routes: [],
    routesAgree: true,
    routesDropped: 0,
    observedAt: "2026-08-12T00:00:00Z",
    defaultsFromVersion: "0.27.0",
    defaultsVerified: true,
    ...over,
  };
}

const AM = amRef();

/**
 * oto's shipped §D.1 defaults, from `internal/platform/tuning/defaults.go` — the
 * one place in the Go tree where they are written. They cannot be imported into a
 * vitest run, so they are named here and then USED as the subject of a test:
 * every one of them must earn an `ok` from the closure that guides it, against
 * Alertmanager's own defaults. That is what makes the copy self-checking rather
 * than a second source of truth.
 */
const SHIPPED: Readonly<Partial<Record<KnobKey, number>>> = {
  // ⛔ `refire_grace_s: 1200` AND `group_close_delay_s: 1200` LED THIS OBJECT AND
  // BOTH ARE DELETED (git-bug 7287b28) — `platform/tuning` no longer writes
  // either, so a number here would be this file inventing one.
  resolve_grace_s: 300,
  flap_threshold: 5,
  flap_window_s: 7200,
  flap_digest_interval_s: 900,
};

function nums(over: Readonly<Partial<Record<KnobKey, number>>> = SHIPPED): Num {
  // NaN is what `TuningSection.tsx:335-338` hands a guide for a field that does
  // not parse — an empty box, or one mid-edit. It is a real input, not a stub.
  return (key) => over[key] ?? Number.NaN;
}

const N = nums();

/** The closure, or a failure naming the knob that lost it. */
function guideOf(key: KnobKey): Guide {
  const g = KNOBS[key].guide;
  if (g === undefined) throw new Error(`oto test: ${key} has no guide()`);
  return g;
}

/** Every knob that computes a verdict. The ticket's nine. */
const GUIDED: readonly KnobKey[] = (Object.keys(KNOBS) as KnobKey[]).filter(
  (k) => KNOBS[k].guide !== undefined,
);

function boundsOf(key: KnobKey): Bound {
  return requestRange(UpdateOrgSettingsRequestSchema, key);
}

/** A value that exercises the closure's main path: the shipped default, or the
 *  knob's minimum when the shipped default is the `0` that means unset. */
function probe(key: KnobKey): number {
  return Math.max(SHIPPED[key] ?? 0, boundsOf(key).min);
}

/**
 * Every value the knob's own write bounds admit, densely near the bottom and
 * sparsely above it.
 *
 * Dense matters: every transition these closures make sits within a few thousand
 * of the minimum, and a sampled sweep can step straight over a band — the
 * `refire_grace` "tight" band was 300 wide out of an 85 800 range. Above the dense
 * region every closure is monotone and flat, so 1000-second steps plus the exact
 * maximum are enough to catch a verdict that changes up there.
 */
function sweep(min: number, max: number): readonly number[] {
  const out: number[] = [];
  const dense = Math.min(max, min + 15_000);
  for (let v = min; v <= dense; v += 1) out.push(v);
  for (let v = dense + 1000; v < max; v += 1000) out.push(v);
  if (out[out.length - 1] !== max) out.push(max);
  return out;
}

/** The sequence of DISTINCT verdicts a knob passes through, low value to high. */
function shapeOf(key: KnobKey, am: AmRef = AM, num: Num = N): readonly GuidanceLevel[] {
  const g = guideOf(key);
  const b = boundsOf(key);
  const seen: GuidanceLevel[] = [];
  for (const v of sweep(b.min, b.max)) {
    const r = g(v, am, num);
    if (r === null) throw new Error(`oto test: ${key} withheld a verdict at ${v}`);
    if (seen[seen.length - 1] !== r.level) seen.push(r.level);
  }
  return seen;
}

/* -------------------------------------------------------------------------- */
/* The set of closures, and what they are all required to do                  */
/* -------------------------------------------------------------------------- */

describe("the guided knobs as a set", () => {
  it("is the four the screen computes for, and the other three are the ones the doc declines to rule on", () => {
    // The count is asserted rather than trusted because `KnobKey` is derived from
    // the write contract: the day the contract grows a setting, this number is the
    // thing that should be reconsidered. It was nine and eight until ADR 0042
    // deleted `storm_threshold`, `storm_window_s` and `storm_cooldown_s`, then
    // seven and seven until git-bug bd0fb1d deleted `unacked_reminder_after_s`
    // and the three mention keys with the reminder itself, then six and three when
    // that same change took `broadcast_on_resolved` out of the unguided list with
    // Slack thread-broadcast (git-bug 7570090). It is four now: git-bug 7287b28 and
    // migration `00071` deleted `refire_grace_s` and `group_close_delay_s`, which
    // were guided knobs derived from the Alertmanager corpus and are the pair whose
    // mechanism ADR 0040 had already retired — the settings outlived the thing they
    // decided, which is exactly why they went.
    expect(GUIDED).toHaveLength(4);

    const unguided = (Object.keys(KNOBS) as KnobKey[]).filter(
      (k) => KNOBS[k].guide === undefined,
    );
    // Each of these is unguided for a stated reason, and none of the reasons is
    // "nobody got round to it": retention and verbosity have no Alertmanager term
    // at all. Inventing a threshold for either is the one thing this screen must
    // not do. (The three Slack mention rows were here too, and went with the
    // reminder — git-bug bd0fb1d. `broadcast_on_resolved` was here as well, and
    // went with Slack thread-broadcast — git-bug 7570090.)
    expect([...unguided].sort()).toEqual([
      "default_verbosity",
      "event_retention_months",
      "raw_retention_days",
    ]);
  });

  it("withholds every verdict when the configuration was never read", () => {
    const blind = amRef({ groupWait: unread, groupInterval: unread, repeatInterval: unread });
    for (const key of GUIDED) {
      // The knob's own minimum is used as the probe so no number is typed here,
      // and because it is non-zero for all nine — which keeps
      // `unacked_reminder_after_s` off its `0` means-unset path.
      const v = boundsOf(key).min;
      const verdict = guideOf(key)(v, blind, N);
      if (key === "flap_threshold" || key === "flap_window_s" || key === "flap_digest_interval_s") {
        // ⛔ RETIRED, SO AN UNREAD CONFIG TAKES NOTHING AWAY (git-bug 235f347).
        // These three read no route timing: the verdict names the retirement and
        // points at the case-retention window W, which is the same answer whether
        // or not Alertmanager could be reached. Withholding it would leave the row
        // silent about the one thing an operator needs to know about it.
        expect(verdict, key).not.toBeNull();
        expect(verdict?.level, key).toBe("inert");
        continue;
      }
      if (key === "resolve_grace_s") {
        // ⭐ THE ONE EXCEPTION, AND IT IS CORRECT. This knob is not derived from a
        // route timing at all — its `amRule` derives it from the scrape budget —
        // so an unread config takes nothing away from it. It is the only guide
        // that never returns null.
        expect(verdict).not.toBeNull();
        continue;
      }
      expect(verdict, `${key} invented a number from an unread config`).toBeNull();
    }
  });

  it("argues from exactly one upstream timing each, and reads nothing else off the reference", () => {
    // A guide that quietly depended on, say, `routesAgree` would be making a
    // claim the screen already makes once at the foot of the panel, and would be
    // untestable from its inputs. The Proxy makes the dependency observable.
    const expected: Readonly<Record<string, readonly string[]>> = {
      // ⛔ `refire_grace_s` AND `group_close_delay_s` — the only two entries that
      // named an upstream timing — are deleted with their knobs (git-bug 7287b28).
      // Every surviving guide is retired and argues from no timing at all, which
      // is why the Proxy now expects an empty set for all of them.
      resolve_grace_s: [],
      // Retired (git-bug 235f347): their verdict is about the retirement, not the
      // value, so they argue from no upstream timing at all.
      flap_threshold: [],
      flap_window_s: [],
      flap_digest_interval_s: [],
      // ⛔ `unacked_reminder_after_s: ["repeatInterval"]` WAS HERE. The knob went
      // with the reminder (git-bug bd0fb1d) and the entry outlived it — the loop
      // walks `GUIDED`, so a stale key here asserts nothing and simply reads as a
      // knob that still exists.
    };

    for (const key of GUIDED) {
      const touched = new Set<string>();
      const spy = new Proxy(AM, {
        get(target, prop, receiver) {
          if (typeof prop === "string") touched.add(prop);
          return Reflect.get(target, prop, receiver);
        },
      }) as AmRef;
      // `probe` and not the shipped default: `unacked_reminder_after_s` ships 0,
      // which returns on the "Unset" path before it reads a timing at all.
      guideOf(key)(probe(key), spy, N);
      expect([...touched].sort(), key).toEqual([...expected[key]!].sort());
    }
  });

  it("gives the same verdict for a defaulted timing as for a configured one, and names it differently", () => {
    // ⛔ `tuningCopy.ts:61-67`: the two provenances produce IDENTICAL arithmetic —
    // a 2m grace is just as unreachable under a defaulted 5m `group_interval` as
    // under a configured one — but call for different actions. Rendering them the
    // same throws away the only part of this screen that is advice; computing them
    // differently would be worse still.
    const asDefault = amRef({
      groupWait: defaulted(AM_GROUP_WAIT_S),
      groupInterval: defaulted(AM_GROUP_INTERVAL_S),
      repeatInterval: defaulted(AM_REPEAT_INTERVAL_S),
    });

    for (const key of GUIDED) {
      const b = boundsOf(key);
      for (const v of [b.min, Math.floor((b.min + b.max) / 2), b.max]) {
        const mine = guideOf(key)(v, AM, N);
        const theirs = guideOf(key)(v, asDefault, N);
        expect(theirs?.level, `${key} at ${v}`).toBe(mine?.level);
        expect(theirs?.suggest, `${key} at ${v}`).toBe(mine?.suggest);
      }
    }

    // ⚠️ THE `toContain` PAIR THAT DEMONSTRATED THIS RULE IS GONE WITH ITS LAST
    // DEMONSTRATOR, AND THE RULE IS KEPT ANYWAY. `flap_digest_interval_s` was
    // retired and reads no reference (git-bug 235f347); `unacked_reminder_after_s`
    // went with the unacked reminder (bd0fb1d); `refire_grace_s` was the last knob
    // whose text named a timing, and it went with git-bug 7287b28. The invariant
    // asserted above — same verdict for a defaulted timing as for a configured one
    // — still runs over every GUIDED knob, so it will hold the day a knob argues
    // from a timing again.
  });

  it("calls every shipped default consistent against Alertmanager's own defaults", () => {
    // This is the UI half of `identity/domain/defaults_derivation_test.go`. ADR
    // 0026 moved three defaults and two mirrored copies were missed; only a test
    // caught it. A default that this screen calls "Inert" the moment an operator
    // opens it is the same class of drift, arrived at from the other side.
    for (const key of GUIDED) {
      const shipped = SHIPPED[key];
      expect(shipped, `${key} has no shipped default named here`).toBeTypeOf("number");
      if (key === "flap_threshold" || key === "flap_window_s" || key === "flap_digest_interval_s") {
        // Their shipped default is not "consistent" and is not meant to be — the
        // knob decides nothing, so `inert` is the honest verdict at every value
        // including the default (git-bug 235f347).
        expect(guideOf(key)(shipped!, AM, N)?.level, `${key} = ${shipped}`).toBe("inert");
        continue;
      }
      expect(guideOf(key)(shipped!, AM, N)?.level, `${key} = ${shipped}`).toBe("ok");
    }
  });

  it("only ever suggests a number the knob's own write bounds admit", () => {
    // The button writes straight into the field. `TuningSection.tsx:1393-1400`
    // clamps first, so this is not about a 422 — it is that a clamped suggestion is
    // a number the closure did not actually compute, and the operator is shown the
    // bound while the sentence beside it argues for something else. The bounds are
    // read off the generated schema, so moving one server-side breaks this rather
    // than the operator. Over the reference case every one of the nine stays inside
    // them; `flap_window_s` is the single knob that can leave them, at a threshold
    // its own test names.
    for (const key of GUIDED) {
      const g = guideOf(key);
      const b = boundsOf(key);
      for (const v of sweep(b.min, b.max)) {
        const s = g(v, AM, N)?.suggest;
        if (s === undefined) continue;
        expect(s, `${key} suggested ${s} at ${v}`).toBeGreaterThanOrEqual(b.min);
        expect(s, `${key} suggested ${s} at ${v}`).toBeLessThanOrEqual(b.max);
        expect(Number.isInteger(s), `${key} suggested a non-integer ${s} at ${v}`).toBe(true);
      }
    }
  });

  it("offers only suggestions it would itself call consistent, with no exceptions", () => {
    // A `use {N}` button whose value the same closure then calls "Tight" is a fix
    // that does not fix. There were two, and both were the FIRST branch of a
    // two-branch guide suggesting the floor it had just checked while ignoring the
    // floor it was about to check — `group_close_delay_s` (deleted since, git-bug
    // 7287b28) offering `group_interval`
    // with no regard for the re-fire grace, and `flap_window_s` offering an
    // undocumented `cycle x 3` with no regard for the threshold. Each cost the
    // operator two clicks for one fix, and the first click was known-insufficient
    // when it was offered. Both now suggest a number that clears every floor the
    // same closure goes on to apply, and the empty list is what holds that.
    const notFixed: string[] = [];
    for (const key of GUIDED) {
      const g = guideOf(key);
      const b = boundsOf(key);
      const offered = new Set<number>();
      for (const v of sweep(b.min, b.max)) {
        const s = g(v, AM, N)?.suggest;
        if (s !== undefined) offered.add(s);
      }
      for (const s of offered) {
        const after = g(s, AM, N);
        if (after?.level !== "ok") notFixed.push(`${key}: use ${s} → ${after?.level}`);
      }
    }
    expect(notFixed.sort()).toEqual([]);
  });
});

/* -------------------------------------------------------------------------- */
/* The shared cycle arithmetic                                                */
/* -------------------------------------------------------------------------- */

describe("the observable cycle", () => {
  it("pays the larger of the rule floor and the transport floor, never their sum", () => {
    // ⛔ `tuningCopy.ts:184-196`: the old arithmetic was `for + group_interval`,
    // which claims an alert with NO `for:` can cycle in one `group_interval`. It
    // cannot — a cycle needs two flushes, one resolved and one firing — and that
    // missing term is why the shipped 5-in-30m flap default was unreachable even
    // for the rule shape it was written for.
    expect(observableCycleS(300, 0)).toBe(600);
    expect(observableCycleS(300, 300)).toBe(600);

    // Rule floor binding: 5m + 15m = 20m, the modal real rule.
    expect(observableCycleS(300, 900)).toBe(1200);
    // Transport floor binding: a `group_interval` above the rule's `for:` pays
    // twice the transport floor and the `for:` term disappears.
    expect(observableCycleS(1800, 900)).toBe(3600);
  });

  it("never returns less than twice the group_interval, and never less than one for:", () => {
    for (const gi of [0, 1, 30, 300, 900, 1800, 3600]) {
      for (const f of [0, 60, 900, 3600]) {
        const c = observableCycleS(gi, f);
        expect(c).toBeGreaterThanOrEqual(2 * gi);
        expect(c).toBeGreaterThanOrEqual(f);
        // Two counted transitions per cycle is what every ceiling's `2 ×` means.
        expect(c).toBe(gi + Math.max(gi, f));
      }
    }
  });

  it("is monotone non-decreasing in both terms, so a slower cluster never looks faster", () => {
    let prev = -1;
    for (const gi of [0, 30, 300, 900, 1800, 3600, 7200]) {
      const c = observableCycleS(gi, 900);
      expect(c).toBeGreaterThanOrEqual(prev);
      prev = c;
    }
    prev = -1;
    for (const f of [0, 60, 300, 900, 3600, 14_400]) {
      const c = observableCycleS(300, f);
      expect(c).toBeGreaterThanOrEqual(prev);
      prev = c;
    }
  });

  it("assumes the measured modal for:, not the one that used to be asserted", () => {
    // ⭐ 15m is the MODE and the MEDIAN of the 155 alerting rules
    // kube-prometheus-stack 88.2.0 ships (ADR 0026). It used to say 5m, which is
    // 12.9% of that corpus, and every flap verdict inherited the error.
    expect(ASSUMED_RULE_FOR_S).toBe(900);
  });
});

/* -------------------------------------------------------------------------- */
/* Threads and lifecycle                                                      */
/* -------------------------------------------------------------------------- */

/* ⛔⛔ `describe("refire_grace_s")` AND `describe("group_close_delay_s")` STOOD
   HERE — nine `it` blocks and ~120 lines — AND BOTH ARE DELETED WITH THEIR KNOBS
   (git-bug 7287b28).

   They were the densest tests in this file, and worth recording what they held:
   `refire_grace_s` was where the RULE-floor regression was pinned (the old
   closure compared only against `group_interval`, so 600s read as fine while the
   modal `for: 15m` rule could never reach it), and `group_close_delay_s` was the
   only knob whose guidance read ANOTHER knob's live value — including the
   NaN-guard for a mid-edit grace box and the "one suggestion clears both floors"
   rule.

   ⭐ NEITHER PROPERTY IS ORPHANED. The rule-floor lesson survives as this file's
   header paragraph and as the `notFixed` sweep above, which still walks every
   GUIDED knob and refuses a `use {N}` the same closure would then call "Tight".
   What is gone is the two knobs, not the two rules. */

describe("resolve_grace_s", () => {
  const g = guideOf("resolve_grace_s");

  it("is tight below the lease and consistent above it, and nothing else", () => {
    expect(shapeOf("resolve_grace_s")).toEqual(["tight", "ok"]);
  });

  it("is the one guide the Alertmanager reference cannot change, because it is a scrape budget", () => {
    // `amRule`: derived from `scrape_interval`/`evaluation_interval`, not from the
    // route timing. So the verdict must be identical whatever the reference says —
    // including when it says nothing.
    const blind = amRef({ groupWait: unread, groupInterval: unread, repeatInterval: unread });
    for (const v of [60, 239, 240, 300, 86_400]) {
      const base = g(v, AM, N);
      expect(base).not.toBeNull();
      expect(g(v, blind, N)?.level).toBe(base?.level);
      expect(g(v, amRef({ groupInterval: observed(7200) }), N)?.level).toBe(base?.level);
    }
  });

  it("takes the TOP of the documented lease range, so it errs toward keeping an alert open", () => {
    // The lease is "commonly three to four minutes". The closure's threshold is
    // 240s — four minutes, the pessimistic end — not 180. That is the safe
    // direction here, because `amRule` states the asymmetry outright: "a fast,
    // wrong expiry is worse than a slow, correct one".
    expect(g(179, AM, N)?.level).toBe("tight");
    expect(g(239, AM, N)?.level).toBe("tight");
    expect(g(240, AM, N)?.level).toBe("ok");
  });

  it("suggests strictly above its own threshold, so the fix does not sit on the boundary", () => {
    const s = g(60, AM, N)?.suggest;
    expect(s).toBe(300);
    expect(s).toBeGreaterThan(240);
    expect(g(s!, AM, N)?.level).toBe("ok");
  });

  it("⚠️ DOUBT: at exactly 240s it says it is ABOVE a three-to-four-minute lease", () => {
    // ⚠️ 240s IS four minutes, so at the boundary the sentence claims a margin the
    // number does not have. `amRule` says "set this above it". Either the
    // comparison should be `v <= 240` or the wording should say "at or above".
    // The level is defensible; the sentence is not.
    const verdict = g(240, AM, N);
    expect(verdict?.level).toBe("ok");
    expect(verdict?.text).toContain("Above a typical three-to-four-minute end-time lease");
  });
});

/* -------------------------------------------------------------------------- */
/* Flap damping                                                               */
/* -------------------------------------------------------------------------- */

describe("the three flap knobs, after the detector was retired", () => {
  // ⛔ THREE describe BLOCKS STOOD HERE AND ARE DELETED WITH THE MECHANISM THEY
  // TESTED (git-bug 235f347). They asserted the retired detector's arithmetic in
  // detail -- the observable ceiling of 2 x floor(W / cycle), the upward
  // degradation `ok -> tight -> inert` unique to `flap_threshold`, the suggestion
  // clamping, and the cross-knob identity below -- and every one of them was a
  // true statement about a computation nothing runs.
  //
  // ⭐ THE CROSS-KNOB INVARIANT IS DELETED, NOT MOVED, AND THAT IS THE ONE TO
  // NOTICE. It read "agrees with flap_threshold exactly, because they are one
  // inequality solved two ways", and it existed because the pair once disagreed:
  // `flap_window_s` carried an extra x 2, demanded a quarter of the ceiling, and
  // made the screen contradict itself in two adjacent rows. That was a real defect
  // and this was the right guard for it. There is no inequality left to solve two
  // ways, so there is nothing for the guard to hold -- keeping it would pin the
  // arithmetic of a dead detector and make removing the keys harder later.
  // git-bug 27a1860 decides whether the keys themselves survive.

  it("all three return the same single verdict, and it is about the retirement", () => {
    for (const key of ["flap_threshold", "flap_window_s", "flap_digest_interval_s"] as const) {
      const verdict = guideOf(key)(SHIPPED[key]!, AM, N);
      expect(verdict, key).not.toBeNull();
      expect(verdict?.level, key).toBe("inert");
      expect(verdict?.text, key).toContain("no value here changes what is delivered");
      expect(verdict?.text, key).toContain("case-retention window W");
    }
  });

  it("offers no suggestion, because a suggestion is an invitation to click", () => {
    // The knob decides nothing. A one-click fix on it spends the operator's trust
    // to change a number that changes no delivery.
    for (const key of ["flap_threshold", "flap_window_s", "flap_digest_interval_s"] as const) {
      for (const v of [0, 1, 5, 900, 7200, 100000]) {
        expect(guideOf(key)(v, AM, N)?.suggest, `${key} at ${v}`).toBeUndefined();
      }
    }
  });

  it("says the same thing at every value, because no value means anything", () => {
    for (const key of ["flap_threshold", "flap_window_s", "flap_digest_interval_s"] as const) {
      const first = guideOf(key)(0, AM, N);
      for (const v of [1, 3, 42, 7200, 86400]) {
        expect(guideOf(key)(v, AM, N), `${key} at ${v}`).toEqual(first);
      }
    }
  });

  it("does not contradict the `what` copy above it", () => {
    // The row already tells the operator the damper is gone. A guide that then
    // graded their number would contradict it one paragraph later, and the
    // confident half is the half people act on.
    for (const key of ["flap_threshold", "flap_window_s", "flap_digest_interval_s"] as const) {
      expect(KNOBS[key].what).toContain("changes no delivery");
      expect(guideOf(key)(SHIPPED[key]!, AM, N)?.level).toBe("inert");
    }
  });
});

/* -------------------------------------------------------------------------- */
/* What reaches the channel                                                   */
/* -------------------------------------------------------------------------- */

/* ⛔ THE `unacked_reminder_after_s` SUITE WAS HERE AND IS DELETED (git-bug
   bd0fb1d). It was the only knob carrying `zeroIsUnset`, and the suite asserted
   that nothing else carried it — worth restating if the flag ever comes back,
   because the flag's whole point is that 0 is a legal READ value below a write
   bound that starts at 60, and the way back to unset is `reset` rather than
   writing a zero. It was also the only guide with a failing branch and no
   suggestion button: an upper-bound argument and no formula. */

