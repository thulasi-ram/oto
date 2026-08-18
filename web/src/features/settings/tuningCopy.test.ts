/**
 * The nine `guide()` closures — the arithmetic behind a one-click button.
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
 * `for: 15m` rule could never reach it.
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
  refire_grace_s: 1200,
  group_close_delay_s: 1200,
  resolve_grace_s: 300,
  flap_threshold: 5,
  flap_window_s: 7200,
  flap_digest_interval_s: 900,
  // DefaultUnackedReminderAfter is ZERO, and the zero is the decision: oto ships
  // no org-level reminder default at all (`identity/domain/org.go:246-250`).
  unacked_reminder_after_s: 0,
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
 * `refire_grace` "tight" band is 300 wide out of an 85 800 range. Above the dense
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
  it("is the seven the screen computes for, and the other seven are the ones the doc declines to rule on", () => {
    // The count is asserted rather than trusted because `KnobKey` is derived from
    // the write contract: the day the contract grows a setting, this number is the
    // thing that should be reconsidered. It was nine and eight until ADR 0042
    // deleted `storm_threshold`, `storm_window_s` and `storm_cooldown_s`.
    expect(GUIDED).toHaveLength(7);

    const unguided = (Object.keys(KNOBS) as KnobKey[]).filter(
      (k) => KNOBS[k].guide === undefined,
    );
    // Each of these is unguided for a stated reason, and none of the reasons is
    // "nobody got round to it": the mention rows are Slack questions; retention and
    // verbosity have no Alertmanager term at all. Inventing a threshold for any of
    // them is the one thing this screen must not do.
    expect([...unguided].sort()).toEqual([
      "broadcast_on_resolved",
      "default_verbosity",
      "event_retention_months",
      "raw_retention_days",
      "unacked_reminder_mention",
      "unacked_reminder_mention_list",
      "unacked_reminder_mention_min_severity",
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
      refire_grace_s: ["groupInterval"],
      group_close_delay_s: ["groupInterval"],
      resolve_grace_s: [],
      flap_threshold: ["groupInterval"],
      flap_window_s: ["groupInterval"],
      flap_digest_interval_s: ["groupInterval"],
      unacked_reminder_after_s: ["repeatInterval"],
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

    const worded = guideOf("flap_digest_interval_s")(60, asDefault, N);
    expect(worded?.text).toContain("Alertmanager's default group_interval");
    expect(guideOf("flap_digest_interval_s")(60, AM, N)?.text).toContain("your group_interval");
  });

  it("calls every shipped default consistent against Alertmanager's own defaults", () => {
    // This is the UI half of `identity/domain/defaults_derivation_test.go`. ADR
    // 0026 moved three defaults and two mirrored copies were missed; only a test
    // caught it. A default that this screen calls "Inert" the moment an operator
    // opens it is the same class of drift, arrived at from the other side.
    for (const key of GUIDED) {
      const shipped = SHIPPED[key];
      expect(shipped, `${key} has no shipped default named here`).toBeTypeOf("number");
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
    // floor it was about to check — `group_close_delay_s` offering `group_interval`
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

describe("refire_grace_s", () => {
  const g = guideOf("refire_grace_s");

  it("degrades in one direction only: unreachable, then tight, then consistent", () => {
    expect(shapeOf("refire_grace_s")).toEqual(["inert", "tight", "ok"]);
  });

  it("requires the RULE floor, not just the transport floor — the regression that shipped", () => {
    // ⛔ THE BUG NAMED AT `tuningCopy.ts:365-369`. The old closure compared the
    // value only against `group_interval`, so 600s — twice the 5m transport floor
    // — read as comfortably fine, while the modal `for: 15m` rule could never
    // reach it. 600 is also this knob's minimum, so an operator can still type it.
    expect(boundsOf("refire_grace_s").min).toBe(600);
    const verdict = g(600, AM, N);
    expect(verdict?.level).toBe("inert");
    expect(verdict?.text).toMatch(/Unreachable for an ordinary rule/);
  });

  it("clears both floors at once, and takes whichever is larger", () => {
    // Rule floor binds while group_interval ≤ for:. 15m + 5m = 20m.
    const slow = g(1199, AM, N);
    expect(slow?.level).toBe("tight");
    expect(slow?.suggest).toBe(ASSUMED_RULE_FOR_S + AM_GROUP_INTERVAL_S);
    expect(g(1200, AM, N)?.level).toBe("ok");

    // Transport floor binds once group_interval exceeds for:, and then the
    // suggestion is 2 × group_interval rather than for: + group_interval.
    const wide = amRef({ groupInterval: observed(1800) });
    const w = g(2500, wide, N);
    expect(w?.level).toBe("tight");
    expect(w?.suggest).toBe(3600);
    expect(g(3600, wide, N)?.level).toBe("ok");
  });

  it("suggests one number regardless of how wrong the current value is, and it clears both floors", () => {
    // The suggestion is a property of the cluster, not of the mistake. Three
    // different failing values, one answer — and that answer is the smallest
    // value the closure itself accepts.
    const suggestions = [600, 700, 899, 900, 1000, 1199].map((v) => g(v, AM, N)?.suggest);
    expect(new Set(suggestions).size).toBe(1);
    const want = suggestions[0]!;
    expect(want).toBeGreaterThanOrEqual(ASSUMED_RULE_FOR_S + AM_GROUP_INTERVAL_S);
    expect(want).toBeGreaterThanOrEqual(2 * AM_GROUP_INTERVAL_S);
    expect(g(want, AM, N)?.level).toBe("ok");
    expect(g(want - 1, AM, N)?.level).not.toBe("ok");
  });

  it("scales with the operator's own group_interval rather than a constant", () => {
    let prev = 0;
    for (const gi of [30, 60, 300, 600, 900, 1800]) {
      const s = g(boundsOf("refire_grace_s").min, amRef({ groupInterval: observed(gi) }), N)
        ?.suggest;
      expect(s, `group_interval ${gi}`).toBeGreaterThanOrEqual(prev);
      prev = s!;
    }
  });
});

describe("group_close_delay_s", () => {
  const g = guideOf("group_close_delay_s");

  it("degrades in one direction only: unreachable, then tight, then consistent", () => {
    expect(shapeOf("group_close_delay_s")).toEqual(["inert", "tight", "ok"]);
  });

  it("calls a value consistent only when it clears BOTH stated inequalities", () => {
    // `amRule`: at or above group_interval, AND at or above the re-fire grace —
    // "the second one is not a suggestion". oto shipped 5m against a 10m grace and
    // defeated half its own grace that way.
    const b = boundsOf("group_close_delay_s");
    for (const refire of [600, 1200, 3600]) {
      const num = nums({ ...SHIPPED, refire_grace_s: refire });
      for (const v of sweep(b.min, b.max)) {
        if (g(v, AM, num)?.level !== "ok") continue;
        expect(v, `ok at ${v} below group_interval`).toBeGreaterThanOrEqual(AM_GROUP_INTERVAL_S);
        expect(v, `ok at ${v} below a ${refire}s grace`).toBeGreaterThanOrEqual(refire);
      }
    }
  });

  it("accepts equality with the re-fire grace, because the two clocks start at different moments", () => {
    // ⭐ Equal is safe rather than racy: this clock runs from the group's last
    // ACTIVITY (the resolve as oto observed it) while the grace runs from the
    // upstream `ended_at`, which is the same instant or earlier. So the group
    // always closes at or after the grace expires. The shipped pair is equal.
    const num = nums({ ...SHIPPED, refire_grace_s: 1200 });
    expect(g(1200, AM, num)?.level).toBe("ok");
    expect(g(1199, AM, num)?.level).toBe("tight");
  });

  it("withholds the grace comparison when the grace field does not parse", () => {
    // An empty or mid-edit `refire_grace_s` box hands this closure NaN
    // (`TuningSection.tsx:335-338`). It guards with `Number.isFinite` and falls
    // back to the group_interval floor alone rather than comparing against NaN.
    const blank = nums({ flap_window_s: 7200, flap_threshold: 5 });
    expect(g(600, AM, blank)?.level).toBe("ok");
    expect(g(299, AM, blank)?.level).toBe("inert");
  });

  it("suggests a number that clears both floors, not the one branch it happens to be in", () => {
    // `amRule` calls the grace floor "not a suggestion", so the first branch must
    // not offer `group_interval` alone: with the shipped pair (group_interval 5m,
    // grace 20m) that button read "use 300", and clicking it landed the operator in
    // the very next branch — a second warning and a second button, from a fix that
    // was known-insufficient when it was offered. `Math.max(gi, refire)` is what the
    // stated rule asks for, and the same number serves both failing branches because
    // the button writes one value.
    const want = Math.max(AM_GROUP_INTERVAL_S, SHIPPED.refire_grace_s!);
    for (const v of [100, 299, AM_GROUP_INTERVAL_S, 600, want - 1]) {
      const verdict = g(v, AM, N);
      expect(verdict?.level, `at ${v}`).not.toBe("ok");
      expect(verdict?.suggest, `at ${v}`).toBe(want);
    }
    expect(g(want, AM, N)?.level).toBe("ok");
  });
});

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

describe("flap_threshold", () => {
  const g = guideOf("flap_threshold");

  it("is the one guide that degrades UPWARD: consistent, then tight, then unreachable", () => {
    // Every other guide's failure is a value too small. This one's is a value too
    // large, and the shape is what says so.
    expect(shapeOf("flap_threshold")).toEqual(["ok", "tight", "inert"]);
  });

  it("measures against 2 × floor(window / cycle), which is even and monotone in the window", () => {
    // `amRule`: one cycle yields exactly TWO counted transitions, so the ceiling
    // in a window W is 2 × floor(W / cycle). A ceiling that rose and fell as the
    // window widened, or an odd one, would mean the `2 ×` had been lost.
    //
    // The ceiling is RECOVERED from the boundary the closure itself draws — the
    // largest threshold it does not call unreachable — rather than read off the
    // formula, so this measures the closure and not a second copy of its
    // arithmetic. Only windows whose ceiling falls inside the knob's own range are
    // measurable; outside it the boundary is the bound, not the ceiling.
    const cycle = observableCycleS(AM_GROUP_INTERVAL_S, ASSUMED_RULE_FOR_S);
    const b = boundsOf("flap_threshold");
    let prev = -1;
    let measured = 0;
    for (const w of [300, 1199, 1200, 2400, 3000, 3600, 6000, 7200, 12_000, 86_400]) {
      const num = nums({ ...SHIPPED, flap_window_s: w });
      let ceiling = 0;
      for (let v = b.min; v <= b.max; v += 1) {
        if (g(v, AM, num)?.level !== "inert") ceiling = v;
      }
      const predicted = 2 * Math.floor(w / cycle);
      if (predicted >= b.min && predicted <= b.max) {
        expect(ceiling, `window ${w}`).toBe(predicted);
        expect(ceiling % 2, `window ${w} gave an odd ceiling`).toBe(0);
        expect(ceiling, `window ${w} narrowed the ceiling`).toBeGreaterThanOrEqual(prev);
        prev = ceiling;
        measured += 1;
      }
    }
    // Guards the guard: a `requestRange` that silently changed shape would make
    // every branch above vacuous.
    expect(measured).toBeGreaterThan(4);
  });

  it("calls every threshold unreachable when the window cannot hold one cycle", () => {
    // A window below one observable cycle has a ceiling of zero, so the damper is
    // dead code that looks configured — the exact failure the old 30m default had.
    const cycle = observableCycleS(AM_GROUP_INTERVAL_S, ASSUMED_RULE_FOR_S);
    const num = nums({ ...SHIPPED, flap_window_s: cycle - 1 });
    for (const v of [3, 5, 25, 100]) {
      expect(g(v, AM, num)?.level, `threshold ${v}`).toBe("inert");
    }
  });

  it("refuses to suggest anything when the threshold is unreachable, because the fix is the window", () => {
    // ⭐ `amRule`: "Widen the window rather than lowering the threshold." A
    // one-click button here would do the wrong thing very conveniently, so the
    // `inert` branch is the only failing branch on the screen with no `suggest`.
    const num = nums({ ...SHIPPED, flap_window_s: 1200 });
    const verdict = g(100, AM, num);
    expect(verdict?.level).toBe("inert");
    expect(verdict?.suggest).toBeUndefined();
    expect(verdict?.text).toContain("Widen the window rather than lowering the threshold");
  });

  it("puts the shipped default at roughly half the observable ceiling", () => {
    // `defaults.go`: "at the window below it sits at 42% of the observable
    // ceiling, which is the 'roughly half the ceiling' rule". 5 of 12.
    const cycle = observableCycleS(AM_GROUP_INTERVAL_S, ASSUMED_RULE_FOR_S);
    const ceiling = 2 * Math.floor(SHIPPED.flap_window_s! / cycle);
    expect(ceiling).toBe(12);
    const shipped = SHIPPED.flap_threshold!;
    expect(shipped / ceiling).toBeGreaterThan(0.33);
    expect(shipped / ceiling).toBeLessThanOrEqual(0.5);
    expect(g(shipped, AM, N)?.level).toBe("ok");
    // And half the ceiling is exactly the boundary it draws.
    expect(g(6, AM, N)?.level).toBe("ok");
    expect(g(7, AM, N)?.level).toBe("tight");
  });

  it("never suggests below the floor of 3 that keeps a rolling deploy off the flapping list", () => {
    // `amRule`: "do not lower the threshold to 2 — two transitions is a normal
    // deploy". The floor coincides with the knob's own minimum, and that is the
    // reason the minimum is 3.
    expect(boundsOf("flap_threshold").min).toBe(3);
    for (const w of [2400, 3000, 3600, 4800, 7200, 86_400]) {
      const num = nums({ ...SHIPPED, flap_window_s: w });
      for (const v of [3, 5, 12, 50, 100]) {
        const s = g(v, AM, num)?.suggest;
        if (s !== undefined) expect(s, `window ${w}, threshold ${v}`).toBeGreaterThanOrEqual(3);
      }
    }
  });

  it("⚠️ DOUBT: on a narrow window the button offers the number already in the field", () => {
    // ⚠️ When the ceiling is 4, half of it is 2, and the floor of 3 raises the
    // suggestion back to 3 — which is both the knob's minimum and, at that
    // ceiling, still "Tight". So an operator sitting at 3 is shown a "use 3"
    // button that changes nothing and leaves the same warning on screen. The
    // remedy `amRule` actually prescribes for this case is the one the `inert`
    // branch already gives — "widen the window" — and this branch never says it.
    const num = nums({ ...SHIPPED, flap_window_s: 3000 });
    const verdict = g(3, AM, num);
    expect(verdict?.level).toBe("tight");
    expect(verdict?.suggest).toBe(3);
    expect(verdict?.suggest).toBe(boundsOf("flap_threshold").min);
    expect(g(verdict!.suggest!, AM, num)?.level).toBe("tight");
  });

  it("withholds the verdict entirely when the window does not parse, rather than calling it consistent", () => {
    // ⛔ THE WORST OF THE LOT, AND THE REASON IT WAS WORST IS THE TONE. Its two
    // sibling cross-referencing guides both guarded (`group_close_delay_s` with
    // `Number.isFinite(refire)`, `flap_window_s` with `Number.isFinite(t)`); this one
    // did not guard `w`. An empty or mid-edit flap-window box yielded ceiling = NaN,
    // both comparisons then read false, and control fell through to `ok()` — the
    // LEAST safe direction. The operator was told "Consistent · About half the
    // observable ceiling of NaN", an all-clear derived from no computation, and the
    // tone is what they act on.
    //
    // Unlike the two siblings there is no partial verdict to fall back to: EVERY
    // number in this closure comes from the window. So the discipline stated at
    // `tuningCopy.ts:320-327` applies in full — return null and render nothing.
    const blank = nums({ flap_threshold: 5 });
    for (const v of [boundsOf("flap_threshold").min, 5, boundsOf("flap_threshold").max]) {
      expect(g(v, AM, blank), `threshold ${v} with an unparsed window`).toBeNull();
    }
    // And the guard is on the window alone: a window that parses still gets a verdict
    // with no threshold field of its own to read.
    expect(g(5, AM, nums({ flap_window_s: 7200 }))?.level).toBe("ok");
  });
});

describe("flap_window_s", () => {
  const g = guideOf("flap_window_s");

  it("degrades in one direction only: unreachable, then tight, then consistent", () => {
    expect(shapeOf("flap_window_s")).toEqual(["inert", "tight", "ok"]);
  });

  it("agrees with flap_threshold exactly, because they are one inequality solved two ways", () => {
    // ⛔ `tuningCopy.ts:561-563`: this closure once carried an extra `× 2`, which
    // demanded a quarter of the ceiling and disagreed with the threshold knob's
    // own verdict on the same two numbers. The screen then contradicted itself in
    // two adjacent rows.
    //
    // The identity: ceiling = 2 × floor(W / cycle), "half the ceiling" gives
    // threshold = floor(W / cycle), and solving for W gives W = threshold × cycle.
    // So W = t × cycle must be the FIRST window the pair both accept, and one
    // second less must be rejected by both.
    const threshold = guideOf("flap_threshold");
    for (const gi of [30, 300, 900, 1800]) {
      const am = amRef({ groupInterval: observed(gi) });
      const cycle = observableCycleS(gi, ASSUMED_RULE_FOR_S);
      for (const t of [3, 5, 12, 25]) {
        const need = t * cycle;
        const num = nums({ ...SHIPPED, flap_threshold: t, flap_window_s: need });
        expect(g(need, am, num)?.level, `gi ${gi}, t ${t}`).toBe("ok");
        expect(threshold(t, am, num)?.level, `gi ${gi}, t ${t}`).toBe("ok");

        const short = nums({ ...SHIPPED, flap_threshold: t, flap_window_s: need - 1 });
        expect(g(need - 1, am, short)?.level, `gi ${gi}, t ${t}`).not.toBe("ok");
        expect(threshold(t, am, short)?.level, `gi ${gi}, t ${t}`).not.toBe("ok");
      }
    }
  });

  it("derives the shipped 2h from the shipped threshold and the modal rule", () => {
    // `defaults.go`: 5 × (15m + 5m) = 100m, rounded UP to 2h. Rounding up is the
    // safe direction — a window that is too wide fails visibly and self-heals,
    // one that is too narrow fails as silence where a damper should be.
    const cycle = observableCycleS(AM_GROUP_INTERVAL_S, ASSUMED_RULE_FOR_S);
    expect(SHIPPED.flap_threshold! * cycle).toBe(6000);
    expect(SHIPPED.flap_window_s!).toBeGreaterThanOrEqual(6000);
    expect(g(SHIPPED.flap_window_s!, AM, N)?.level).toBe("ok");
    expect(g(5999, AM, N)?.level).toBe("tight");
    expect(g(6000, AM, N)?.level).toBe("ok");
  });

  it("rounds its threshold-derived suggestion to a whole second", () => {
    // ⭐ THE ONLY `Math.round` IN ANY OF THE NINE, and it is needed: `value_ms` is
    // milliseconds because `group_wait: 500ms` is legal upstream, so a
    // sub-second `group_interval` makes the cycle fractional. The knob is
    // `v.integer()`.
    const am = amRef({ groupInterval: observed(0.5) });
    const num = nums({ ...SHIPPED, flap_threshold: 5 });
    const verdict = g(1000, am, num);
    expect(verdict?.level).toBe("tight");
    expect(Number.isInteger(verdict!.suggest!)).toBe(true);
  });

  it("withholds the threshold comparison when the threshold field does not parse, and says so instead of printing NaN", () => {
    const blank = nums({ flap_window_s: 7200 });
    const verdict = g(7200, AM, blank);
    expect(verdict?.level).toBe("ok");
    // ⛔ The `ok` branch interpolated `${t}` unguarded, so a blank threshold box
    // rendered "Wide enough for a threshold of NaN" — the same class of defect as
    // `flap_threshold`'s ceiling of NaN, on the branch that sounds most settled.
    // Unlike `flap_threshold` there IS a partial verdict here (the cycle floor needs
    // no second field), so the fix is to state what was and was not checked.
    expect(verdict?.text).not.toContain("NaN");
    expect(verdict?.text).toContain("one observable cycle");
    expect(verdict?.text).toContain("no threshold to size this against");
    // The cycle floor still applies — it needs no second field.
    const short = g(1199, AM, blank);
    expect(short?.level).toBe("inert");
    expect(short?.text).not.toContain("NaN");
    // Nor does the suggestion become NaN: with no threshold, one whole cycle is the
    // most that can honestly be offered, and it is the value that earns the `ok`.
    expect(short?.suggest).toBe(observableCycleS(AM_GROUP_INTERVAL_S, ASSUMED_RULE_FOR_S));
    expect(g(short!.suggest!, AM, blank)?.level).toBe("ok");
  });

  it("computes its threshold-derived suggestion unclamped, and the button clamps it", () => {
    // ⭐ RULED CORRECT AS IT STANDS. `need = threshold × cycle` is not checked
    // against the write bound: flap_threshold's own maximum (100) with Alertmanager's
    // own default group_interval gives 100 × 20m = 120 000s against a maximum of
    // 86 400. That is not a 422 and not a broken button — `TuningSection.tsx:1393-1400`
    // clamps every suggestion into the knob's own bounds before rendering it, so the
    // button reads "use 86400" and the save succeeds. The arithmetic here answers
    // "what would this threshold need"; the bound belongs to the control, which is
    // the only place that knows it. Keeping the raw number means the two ratio
    // sentences and the suggestion stay the same computation.
    const num = nums({ ...SHIPPED, flap_threshold: boundsOf("flap_threshold").max });
    const verdict = g(7200, AM, num);
    expect(verdict?.level).toBe("tight");
    expect(verdict?.suggest).toBe(120_000);
    expect(verdict!.suggest!).toBeGreaterThan(boundsOf("flap_window_s").max);
  });

  it("below one cycle it suggests the window the current threshold needs, not a constant", () => {
    // ⛔ The `inert` branch used to suggest `cycle × 3` — a 3 appearing in neither
    // `amRule` nor `docs/setup/tuning.md`. The knob's whole stated purpose is to be
    // wide enough for the CURRENT threshold, and the very next branch computes that
    // as `threshold × cycle`: with the shipped threshold of 5 the suggestion landed
    // in "Tight", and for any threshold above 3 it was insufficient by construction.
    // One rule, one suggestion, and it is the same one both failing branches offer.
    const cycle = observableCycleS(AM_GROUP_INTERVAL_S, ASSUMED_RULE_FOR_S);
    for (const t of [3, 5, 12]) {
      const num = nums({ ...SHIPPED, flap_threshold: t });
      const verdict = g(600, AM, num);
      expect(verdict?.level, `threshold ${t}`).toBe("inert");
      expect(verdict?.suggest, `threshold ${t}`).toBe(t * cycle);
      expect(g(verdict!.suggest!, AM, num)?.level, `threshold ${t}`).toBe("ok");
    }
  });
});

describe("flap_digest_interval_s", () => {
  const g = guideOf("flap_digest_interval_s");

  it("is the only two-sided guide: tight, consistent, tight", () => {
    // Both failure modes are live for this knob — below group_interval it cannot
    // produce more digests than the upstream produces batches, above 4× the
    // summary arrives after anyone cared — so the shape is a band rather than a
    // floor. No other guide has an upper bound at all.
    expect(shapeOf("flap_digest_interval_s")).toEqual(["tight", "ok", "tight"]);
  });

  it("holds the band the doc states, inclusive at both ends", () => {
    // `amRule`: "Keep it at or above group_interval. Two to four times
    // group_interval is the useful range."
    expect(g(AM_GROUP_INTERVAL_S - 1, AM, N)?.level).toBe("tight");
    expect(g(AM_GROUP_INTERVAL_S, AM, N)?.level).toBe("ok");
    expect(g(4 * AM_GROUP_INTERVAL_S, AM, N)?.level).toBe("ok");
    expect(g(4 * AM_GROUP_INTERVAL_S + 1, AM, N)?.level).toBe("tight");
  });

  it("suggests the middle of the useful range from both sides, and the shipped default is it", () => {
    // 3 × group_interval is the midpoint of the 2×–4× band, so the same
    // suggestion serves a value that is too small and one that is too large.
    const low = g(60, AM, N);
    const high = g(86_400, AM, N);
    expect(low?.suggest).toBe(3 * AM_GROUP_INTERVAL_S);
    expect(high?.suggest).toBe(3 * AM_GROUP_INTERVAL_S);
    expect(low?.suggest).toBe(SHIPPED.flap_digest_interval_s);
    expect(g(low!.suggest!, AM, N)?.level).toBe("ok");
  });

  it("claims the 2×–4× range only where it holds, and the floor everywhere else", () => {
    // ⛔ The `ok` branch rendered "{n} x group_interval — inside the useful 2 x to
    // 4 x range" for anything from 1× upward, so at exactly `group_interval` the
    // sentence read "1.0 x group_interval — inside the useful 2 x to 4 x range" and
    // refuted itself in eight words. `amRule` states TWO things — "keep it at or
    // above group_interval" (the level, which was right) and "two to four times
    // group_interval is the useful range" (the recommendation) — and the copy
    // conflated them. The level is unchanged; the sentence now matches the band it
    // is in.
    const floor = g(AM_GROUP_INTERVAL_S, AM, N);
    expect(floor?.level).toBe("ok");
    expect(floor?.text).toContain("1.0 x group_interval");
    expect(floor?.text).not.toContain("inside the useful 2 x to 4 x range");
    expect(floor?.text).toContain("the useful range starts at 2 x");

    const inside = g(2 * AM_GROUP_INTERVAL_S, AM, N);
    expect(inside?.level).toBe("ok");
    expect(inside?.text).toContain("2.0 x group_interval");
    expect(inside?.text).toContain("inside the useful 2 x to 4 x range");

    // The shipped default is inside the band, so the claim it makes is the true one.
    expect(g(SHIPPED.flap_digest_interval_s!, AM, N)?.text).toContain(
      "inside the useful 2 x to 4 x range",
    );
  });

  it("would divide by a group_interval of 0, which no running Alertmanager can report", () => {
    // ⭐ RULED NOT REACHABLE, AND THE CODE IS LEFT ALONE. `RouteTimingDTO.value_ms`
    // does have `minimum: 0` and does say "`0` is a real setting and is never a
    // stand-in for 'not known'" — but these numbers are read off a RUNNING
    // Alertmanager's own config, and Alertmanager refuses to start with
    // `group_interval: 0` ("group_interval cannot be zero"). So no source can serve
    // one, and guarding it here would be a branch no operator can enter, written into
    // the file whose whole discipline is that every sentence is reachable.
    //
    // This pins what the arithmetic WOULD do, so that the day something else makes a
    // zero reachable the shape is already written down. No guide reads `group_wait`
    // any more — `storm_window_s` was the only one, and ADR 0042 deleted it — so a
    // legal upstream `group_wait: 0` now reaches no arithmetic at all.
    const zero = amRef({ groupInterval: observed(0) });
    expect(g(0, zero, N)?.text).toContain("NaN x group_interval");
    const positive = g(900, zero, N);
    expect(positive?.level).toBe("tight");
    expect(positive?.suggest).toBe(0);
    expect(positive!.suggest!).toBeLessThan(boundsOf("flap_digest_interval_s").min);
  });
});

/* -------------------------------------------------------------------------- */
/* What reaches the channel                                                   */
/* -------------------------------------------------------------------------- */

describe("unacked_reminder_after_s", () => {
  const g = guideOf("unacked_reminder_after_s");

  it("is consistent up to repeat_interval and tight from there on", () => {
    expect(shapeOf("unacked_reminder_after_s")).toEqual(["ok", "tight"]);
  });

  it("reads zero as UNSET before it looks at Alertmanager at all", () => {
    // ⭐ `zeroIsUnset`, and it is the only knob that carries it: the org settings
    // schema admits 0 on read while the write bounds start at 60, and the way back
    // to unset is `reset` rather than writing a zero. The check sits above the
    // provenance guard, so this is also the only guide that answers at all when
    // its timing was never read — correctly, because "unset" is a fact about oto
    // and needs no upstream number.
    expect(KNOBS.unacked_reminder_after_s.zeroIsUnset).toBe(true);
    const withZeroIsUnset = (Object.keys(KNOBS) as KnobKey[]).filter(
      (k) => KNOBS[k].zeroIsUnset === true,
    );
    expect(withZeroIsUnset).toEqual(["unacked_reminder_after_s"]);

    const blind = amRef({ repeatInterval: unread });
    const verdict = g(0, blind, N);
    expect(verdict?.level).toBe("ok");
    expect(verdict?.text).toContain("Unset");
    // And it says what unset is NOT, because "0 seconds" is the natural misreading.
    expect(verdict?.text).toContain("not the same as zero seconds");
    // One second above unset is an ordinary value again, and needs the number.
    expect(g(1, blind, N)).toBeNull();
  });

  it("turns tight AT repeat_interval, not past it", () => {
    // `amRule`: at or beyond repeat_interval, Alertmanager will already have
    // re-sent the unchanged group, so the reminder adds nothing the channel was
    // not just told. Equality is already too late.
    expect(g(AM_REPEAT_INTERVAL_S - 1, AM, N)?.level).toBe("ok");
    expect(g(AM_REPEAT_INTERVAL_S, AM, N)?.level).toBe("tight");
    expect(g(AM_REPEAT_INTERVAL_S + 1, AM, N)?.level).toBe("tight");
  });

  it("tracks the operator's own repeat_interval rather than the documented 4h", () => {
    const brisk = amRef({ repeatInterval: observed(1800) });
    expect(g(3600, AM, N)?.level).toBe("ok");
    expect(g(3600, brisk, N)?.level).toBe("tight");
  });

  it("never offers a number, because the doc declines to give one", () => {
    // ⭐ `tuningCopy.ts:16-21`: where the doc gives an arithmetic rule it is
    // implemented as a guide; where it declines ("sanity-check against how long
    // your incidents actually last") the screen says so and does not manufacture a
    // threshold. This knob has an upper bound argument and no formula, so it warns
    // and offers nothing — the only guide with a failing branch and no button.
    const b = boundsOf("unacked_reminder_after_s");
    for (const v of sweep(b.min, b.max)) {
      expect(g(v, AM, N)?.suggest, `at ${v}`).toBeUndefined();
    }
    expect(g(0, AM, N)?.suggest).toBeUndefined();
  });
});
