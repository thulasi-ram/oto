/**
 * The two tabs of the alert list, and the badge that makes splitting them safe.
 *
 * ⛔ THIS FILE IS THE SAFETY ARGUMENT, ASSERTED. `filters.ts` used to carry a
 * rule — *"hiding snoozed alerts from the default list is how an incident is
 * lost"* — and this change reverses it. The reversal is defensible only because
 * of three properties of this strip, and each one is a test below:
 *
 *   1. **The tab is present at zero.** A surface that vanishes when the count
 *      reaches zero is a surface nobody learns is there, and a snooze may run for
 *      thirty days.
 *   2. **The count is hidden at zero.** "Quiet" says the same thing as
 *      "Quiet (0)" and says it more quietly.
 *   3. **The badge carries the worst state inside it.** `Quiet (12 · 2 firing)`,
 *      never `Quiet (12)`. This is the clause that replaces the per-row snooze
 *      badge, and without it the split loses exactly the incident the old rule
 *      was written to protect.
 *
 * The last describe is ADR 0012's: none of this may spend a Tier-B state hue.
 * Being quiet about an alert is a fact about oto's notifications, not the state
 * of a signal, and burning the firing colour on permanently-mounted chrome would
 * blunt the scarcity that makes a firing row loud.
 */
import { fireEvent, render, screen } from "@solidjs/testing-library";
import { describe, expect, it, vi } from "vitest";

import { AlertTabs, quietBadgeLabel, summariseQuiet } from "./AlertTabs";
import { alert } from "~/test/fixtures";
import type { Alert, State } from "~/api/types";

const quietAlerts = (...states: readonly State[]): readonly Alert[] =>
  states.map((state, i) => alert({ id: `a-${i}`, state }));

const mount = (quiet: Parameters<typeof AlertTabs>[0]["quiet"]) => {
  const onChange = vi.fn();
  render(() => <AlertTabs tab="active" onChange={onChange} quiet={quiet} />);
  return { onChange };
};

describe("summarising what is being held back", () => {
  it("names the liveliest state present, and how many hold it", () => {
    const q = summariseQuiet(quietAlerts("resolved", "firing", "suppressed", "firing"), false);
    expect(q.total).toBe(4);
    expect(q.worst).toEqual({ state: "firing", n: 2 });
  });

  it("prefers firing over suppressed and suppressed over expired", () => {
    expect(summariseQuiet(quietAlerts("expired", "suppressed"), false).worst?.state).toBe(
      "suppressed",
    );
    expect(summariseQuiet(quietAlerts("expired", "resolved"), false).worst?.state).toBe("expired");
  });

  it("has no worst state when everything held back has already resolved", () => {
    // ⭐ NOT AN OVERSIGHT — IT IS WHAT MAKES THE SECOND CLAUSE MEAN SOMETHING.
    // Twelve resolved alerts somebody is still being quiet about are worth
    // counting and are not worth alarming anybody with. The clause appears
    // exactly when there is something live behind it.
    const q = summariseQuiet(quietAlerts("resolved", "resolved"), false);
    expect(q.total).toBe(2);
    expect(q.worst).toBeNull();
    expect(quietBadgeLabel(q)).toBe("2");
  });

  it("marks a capped count rather than claiming a total it did not see", () => {
    const q = summariseQuiet(quietAlerts("firing", "resolved"), true);
    expect(quietBadgeLabel(q)).toBe("2+ · 1 firing");
  });

  it("says nothing at all when nothing is quiet, and nothing when the count is unknown", () => {
    expect(quietBadgeLabel(summariseQuiet([], false))).toBe("");
    expect(quietBadgeLabel(null)).toBe("");
  });
});

describe("the strip itself", () => {
  it("shows the Quiet tab even when nothing is quiet", () => {
    // ⛔ THE TAB IS THE SURFACE THE 30-DAY MAXIMUM REQUIRES. Without a list of
    // what you are currently not being told, a snooze becomes permanent by
    // forgetfulness — and a tab that disappears at zero is one nobody discovers.
    mount(summariseQuiet([], false));
    expect(screen.getByRole("tab", { name: /Quiet/ })).toBeTruthy();
  });

  it("draws no count at zero", () => {
    mount(summariseQuiet([], false));
    const tab = screen.getByRole("tab", { name: /Quiet/ });
    expect(tab.textContent).toBe("Quiet");
    expect(tab.textContent).not.toContain("0");
  });

  it("draws no count before the count is known", () => {
    // Pending is not zero. A tab that reads "Quiet (0)" while the request is
    // still in flight has told an operator something oto does not yet know.
    mount(null);
    expect(screen.getByRole("tab", { name: /Quiet/ }).textContent).toBe("Quiet");
  });

  it("carries the worst state inside it, not a bare total", () => {
    mount(summariseQuiet(quietAlerts(...Array<State>(10).fill("resolved"), "firing", "firing"), false));
    const tab = screen.getByRole("tab", { name: /Quiet/ });
    expect(tab.textContent).toContain("12");
    expect(tab.textContent).toContain("2 firing");
    // ⛔ `Quiet (12)` is the failure mode this test exists for: it cannot tell a
    // dozen resolved leftovers from a dozen hiding two live pages.
    expect(tab.textContent).not.toBe("Quiet (12)");
  });

  it("reports which tab was picked", () => {
    const { onChange } = mount(summariseQuiet(quietAlerts("firing"), false));
    fireEvent.click(screen.getByRole("tab", { name: /Quiet/ }));
    expect(onChange).toHaveBeenCalledWith("quiet");
  });
});

describe("ADR 0012: chrome spends no state hue", () => {
  it("draws the badge in Tier-A ink, whatever is behind it", () => {
    mount(summariseQuiet(quietAlerts("firing", "firing"), false));
    const tab = screen.getByRole("tab", { name: /Quiet/ });

    // The worst state is carried by the WORD — "2 firing" — which is unambiguous
    // without spending any colour on a strip that is on screen permanently.
    expect(tab.innerHTML).not.toMatch(/state-firing|text-state|bg-state/);
    expect(tab.textContent).toContain("firing");
  });
});
