import { describe, expect, it } from "vitest";

import type { RuleChange } from "~/api/types";
import { exprDrift } from "./RuleDrift";

/**
 * `exprDrift` is the only door between a `RuleChangeDTO` and a sentence about
 * thresholds, and the sentence is the dangerous part. oto does not parse PromQL:
 * it says the numbers moved, or the shape moved, or that it will not say — and
 * the last of those is a real answer, not a degraded one. A UI that rendered
 * "5 → 10" for an edit that was actually `[5m]` → `[10m]` would tell an operator
 * the alert got less sensitive when it got more so.
 *
 * These are refusal tests as much as rendering tests: every payload that did not
 * come with a `numbers_moved` verdict must classify to a case that has no
 * numbers to render.
 */

/** A change carrying only the fields the classifier reads. */
function change(patch: Partial<RuleChange>): RuleChange {
  return {
    previous_snapshot_id: "0f8fad5b-d9cb-469f-a165-70867728950e",
    previous_fingerprint: "sha256:abc",
    previous_captured_at: "2026-03-01T12:00:00Z",
    expr_changed: false,
    for_changed: false,
    ...patch,
  };
}

describe("exprDrift", () => {
  it("reports an unchanged expression as unchanged, not as an empty verdict", () => {
    expect(exprDrift(change({ expr_changed: false, expr_diff: null }))).toEqual({
      kind: "unchanged",
    });
  });

  it("names the values that moved when oto vouched for them", () => {
    const d = exprDrift(
      change({
        expr_changed: true,
        previous_expr: "sum(rate(http_errors[5m])) > 0.05",
        new_expr: "sum(rate(http_errors[5m])) > 0.1",
        expr_diff: {
          verdict: "numbers_moved",
          numbers: [{ index: 0, previous_value: 0.05, new_value: 0.1 }],
        },
      }),
    );

    expect(d).toEqual({
      kind: "numbers",
      numbers: [{ index: 0, previous_value: 0.05, new_value: 0.1 }],
    });
  });

  it("calls numbers_moved with no numbers a reformat, not a threshold move", () => {
    // The contract's whitespace case. Rendering "the threshold moved" with an
    // empty list under it would read as a bug in oto rather than as a fact
    // about the rule.
    expect(exprDrift(change({ expr_changed: true, expr_diff: { verdict: "numbers_moved" } }))).toEqual(
      { kind: "reformat" },
    );
    expect(
      exprDrift(change({ expr_changed: true, expr_diff: { verdict: "numbers_moved", numbers: [] } })),
    ).toEqual({ kind: "reformat" });
  });

  it("carries no numeric claim out of a structural change", () => {
    const d = exprDrift(
      change({
        expr_changed: true,
        previous_expr: "up == 0",
        new_expr: "sum(rate(http_errors[5m])) > 0.05",
        expr_diff: { verdict: "structural" },
      }),
    );

    expect(d).toEqual({ kind: "structural" });
    expect(d).not.toHaveProperty("numbers");
  });

  it("keeps uncharacterised distinct from structural — they are different admissions", () => {
    const d = exprDrift(
      change({
        expr_changed: true,
        previous_expr: "rate(x[5m]) > 100",
        new_expr: "rate(x[10m]) > 100",
        expr_diff: { verdict: "uncharacterised" },
      }),
    );

    expect(d).toEqual({ kind: "uncharacterised" });
    expect(d).not.toHaveProperty("numbers");
  });

  it("degrades a payload with no verdict to no claim, never to a guess", () => {
    // A `rule.definition_changed` timeline payload carries the booleans but not
    // the diff, and an older server carries neither. Both must land on the
    // was/now rendering with nothing said about numbers.
    expect(exprDrift(change({ expr_changed: true }))).toEqual({ kind: "uncompared" });
    expect(exprDrift(change({ expr_changed: true, expr_diff: null }))).toEqual({
      kind: "uncompared",
    });
  });

  it("degrades a verdict from a newer server to no claim", () => {
    // Forward compatibility runs in the safe direction: a verdict this build
    // cannot interpret is, by definition, a verdict that did not vouch for a
    // number in a way this build understands.
    // Cast through NonNullable: under `exactOptionalPropertyTypes` the optional
    // property will not accept an explicit `undefined`, and the case under test
    // is a verdict that IS there and is unknown — not an absent one.
    const future = { verdict: "durations_moved" } as unknown as NonNullable<
      RuleChange["expr_diff"]
    >;
    expect(exprDrift(change({ expr_changed: true, expr_diff: future }))).toEqual({
      kind: "uncompared",
    });
  });
});
