/**
 * The filter bar, as an operator drives it.
 *
 * `filters.test.ts` proves the state transformations; this proves the controls
 * are wired to them — which is a different failure. A `<select>` bound to the
 * wrong field, or an option list that offers a value the server refuses, is
 * invisible to a pure-function test and completely visible at 3am.
 *
 * The enumerable controls are checked against the contract's own enums, so the
 * bar cannot quietly stop offering a lifecycle state the server still serves.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it, vi } from "vitest";

import { FilterBar } from "./FilterBar";
import { DEFAULT_FILTERS, type AlertFilters } from "./filters";
import { enumValues } from "~/test/contract";
import { cluster } from "~/test/fixtures";
import { expectNoUndefined, list, renderScreen, stubFetch, unpaged, until } from "~/test/harness";

function mount(filters: Partial<AlertFilters> = {}) {
  const onChange = vi.fn<(next: AlertFilters) => void>();
  const onReset = vi.fn();
  const rendered = renderScreen(() => (
    <FilterBar
      filters={{ ...DEFAULT_FILTERS, ...filters }}
      onChange={onChange}
      onReset={onReset}
    />
  ));
  return { ...rendered, onChange, onReset };
}

function stubReferenceData(): void {
  stubFetch({
    "GET /api/v1/clusters": list([cluster()]),
    "GET /api/v1/labels": unpaged([{ name: "namespace", alert_count: 12 }]),
  });
}

/**
 * Opens a `Select`'s real, accessible listbox and returns the option named
 * `optionName` — the way a keyboard user opens it (`SelectTrigger`'s own
 * `onKeyDown` treats `ArrowDown` as "open, focus first"), not by reaching past
 * the visible control into `SelectHiddenSelect`'s `aria-hidden` native shim.
 * `SelectContent` is presence-gated the same way `Modal` is, so the option is
 * awaited rather than assumed to exist the instant the trigger opens.
 */
async function openOption(trigger: HTMLElement, optionName: string | RegExp): Promise<HTMLElement> {
  fireEvent.keyDown(trigger, { key: "ArrowDown" });
  await until(() => expect(screen.getByRole("option", { name: optionName })).toBeTruthy());
  return screen.getByRole("option", { name: optionName });
}

/**
 * Opens one of the toolbar's menus by its trigger.
 *
 * ⛔ THIS STEP IS THE COST OF THE TOOLBAR, AND IT IS DELIBERATELY EXPLICIT.
 * Until ADR 0033 every control here was in the DOM at mount, so a test could
 * reach straight for `getByRole` — and so could a screen reader. A closed
 * Kobalte popover renders nothing, so both now have to open it first. What is
 * *not* behind a menu is asserted separately below ("says on each trigger's
 * face what it is narrowing by"), because that is the property that makes the
 * hiding acceptable.
 *
 * Matched by a `^`-anchored pattern: a trigger's accessible name grows to
 * include its value ("Severity critical +1"), so anchoring names the axis
 * without pinning whatever it happens to be filtering for.
 */
async function openMenu(axis: string): Promise<HTMLElement> {
  fireEvent.click(screen.getByRole("button", { name: new RegExp(`^${axis}`) }));
  await until(() => expect(screen.getByRole("dialog")).toBeTruthy());
  return screen.getByRole("dialog");
}

describe("the enumerable filters", () => {
  it("offers every lifecycle state the contract serves, each with a word", async () => {
    stubReferenceData();
    mount();
    await openMenu("Status");

    const group = screen.getByRole("group", { name: "Lifecycle state" });
    const labels = within(group)
      .getAllByRole("button")
      .map((el) => el.textContent?.trim() ?? "");

    expect(labels).toHaveLength(enumValues("State").length);
    for (const label of labels) expect(label).not.toBe("");
    expectNoUndefined(group as HTMLElement);
  });

  it("toggles a state without disturbing the rest of the filter set", async () => {
    stubReferenceData();
    const { onChange } = mount({ severity: ["critical"] });
    await openMenu("Status");

    const group = screen.getByRole("group", { name: "Lifecycle state" });
    fireEvent.click(within(group).getAllByRole("button")[0]!);

    expect(onChange).toHaveBeenCalledTimes(1);
    const next = onChange.mock.calls[0]?.[0];
    expect(next?.state).toEqual([enumValues("State")[0]]);
    expect(next?.severity).toEqual(["critical"]);
  });

  it("keeps a severity that arrived from a link even though it is not one of the common three", async () => {
    // `AlertDTO.severity` is a free vocabulary, not an enum. A deployment using
    // `sev1` must see its own word back rather than have it silently dropped.
    stubReferenceData();
    mount({ severity: ["sev1"] });
    await openMenu("Severity");

    const group = screen.getByRole("group", { name: "Severity" });
    expect(within(group).getByText("sev1")).toBeTruthy();
  });
});

describe("the orthogonal axes", () => {
  /**
   * ⛔ THE TOOLBAR OFFERS NO SNOOZE CONTROL, AND THAT IS THE ASSERTION.
   *
   * Snooze did not lose its home, it got a better one: it is a TAB. A tri-state
   * buried three levels into this popover could not do the one job that makes
   * hiding snoozed alerts safe — say, permanently and at rest, how many are being
   * held back and whether any of them is still firing. Two controls for one axis
   * would also let the toolbar and the tab bar disagree about which list is on
   * screen, and only one of them is in the URL.
   */
  it("offers no snooze control, because snooze is a tab and not a facet", async () => {
    stubReferenceData();
    mount();
    await openMenu("Status");

    expect(screen.queryByTitle(/currently holding its notifications/i)).toBeNull();
    expect(screen.queryByText("Notifications held")).toBeNull();
    expect(screen.queryByText("Snoozed")).toBeNull();
  });

  /**
   * ⛔ THE TOOLBAR OFFERS NO ACK CONTROL, AND THAT IS THE ASSERTION. `?ack=`
   * filtered `alerts.ack_state`, a column that no longer exists: an ack is a
   * receipt for one firing episode, so a filter over the Alert answered a
   * question about some earlier firing while reading as a question about this
   * one. Re-adding the control is the regression this test refuses.
   */
  it("offers no acknowledgement filter, because ack is not an Alert-scoped fact", async () => {
    stubReferenceData();
    mount();
    await openMenu("Status");

    expect(screen.queryByTitle(/A receipt on a signal/i)).toBeNull();
    expect(screen.queryByText("Seen by someone")).toBeNull();
    expect(screen.queryByRole("button", { name: "Acked" })).toBeNull();
  });
});

describe("clusters", () => {
  it("stays out of the way until the org has one", async () => {
    stubFetch({
      "GET /api/v1/clusters": list([]),
      "GET /api/v1/labels": unpaged([]),
    });
    mount();
    await until(() => expect(screen.queryByText("Cluster")).toBeNull());
  });

  it("offers the display name and filters by the cluster key", async () => {
    stubReferenceData();
    const { onChange } = mount();

    await until(() => expect(screen.getByText("Cluster")).toBeTruthy());
    const trigger = document.getElementById("alert-cluster") as HTMLElement;
    const option = await openOption(trigger, "Production (EU)");

    fireEvent.click(option);
    expect(onChange.mock.calls[0]?.[0]?.cluster).toEqual(["prod-eu"]);
  });
});

describe("the merged search box", () => {
  it("names the metacharacters that would force a scan instead of just refusing", () => {
    stubReferenceData();
    mount({ matcherText: 'pod=~"api-.*"' });

    const status = screen.getByText(/sequential scan/i);
    expect(status.textContent).toMatch(/index/);
    // The refusal is a teaching moment or it is nothing: it must show the
    // spelling that DOES work.
    expect(status.textContent).toMatch(/critical\|warning/);
  });

  it("says where a parse error is, so the caret has somewhere to go", () => {
    stubReferenceData();
    mount({ matcherText: "9lives=cat" });
    expect(screen.getByText(/not a valid label name/)).toBeTruthy();
  });

  it("renders a committed matcher as a removable chip", () => {
    stubReferenceData();
    mount({ matcherText: '{namespace="payments"}' });
    expect(screen.getByText('namespace="payments"')).toBeTruthy();
    expect(screen.getByRole("button", { name: 'Remove namespace="payments"' })).toBeTruthy();
  });

  it("commits a typed matcher into a chip on Space rather than on every keystroke", () => {
    stubReferenceData();
    const { onChange } = mount();
    const input = screen.getByLabelText("Search alerts, or type a label matcher");

    fireEvent.focus(input);
    fireEvent.input(input, { target: { value: 'namespace="payments"' } });
    expect(onChange).not.toHaveBeenCalled();

    fireEvent.keyDown(input, { key: " " });
    expect(onChange.mock.calls[0]?.[0]?.matcherText).toBe('{namespace="payments"}');
  });

  it("commits a typed matcher into a chip on Enter, same as Space", () => {
    stubReferenceData();
    const { onChange } = mount();
    const input = screen.getByLabelText("Search alerts, or type a label matcher");

    fireEvent.focus(input);
    fireEvent.input(input, { target: { value: 'namespace="payments"' } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onChange.mock.calls[0]?.[0]?.matcherText).toBe('{namespace="payments"}');
  });

  it("clicking a chip removes it and puts its exact text back into the box for editing", () => {
    stubReferenceData();
    const { onChange } = mount({ matcherText: '{namespace="payments"}' });

    fireEvent.click(screen.getByText('namespace="payments"'));
    expect(onChange.mock.calls[0]?.[0]?.matcherText).toBe("");

    const input = screen.getByLabelText("Search alerts, or type a label matcher") as HTMLInputElement;
    expect(input.value).toBe('namespace="payments"');
  });

  it("removes the last chip on Backspace when the box is empty", () => {
    stubReferenceData();
    const { onChange } = mount({ matcherText: '{namespace="payments"}' });
    const input = screen.getByLabelText("Search alerts, or type a label matcher");

    fireEvent.keyDown(input, { key: "Backspace" });
    expect(onChange.mock.calls[0]?.[0]?.matcherText).toBe("");
  });

  it("treats anything else typed and submitted via Enter as free-text search", () => {
    stubReferenceData();
    const { onChange } = mount();
    const input = screen.getByLabelText("Search alerts, or type a label matcher");

    fireEvent.focus(input);
    fireEvent.input(input, { target: { value: "high error rate" } });
    fireEvent.keyDown(input, { key: "Enter" });

    expect(onChange.mock.calls[0]?.[0]?.q).toBe("high error rate");
    expect(onChange.mock.calls[0]?.[0]?.matcherText).toBe("");
  });
});

describe("grouping, as tabs", () => {
  it("offers one tab per axis the contract rolls up on, plus 'All'", async () => {
    stubReferenceData();
    mount();
    await openMenu("Group");
    const tabs = screen.getAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual([
      "All",
      "By alert name",
      "By namespace",
      "By fingerprint",
    ]);
  });

  it("marks exactly the current axis selected, and only it reachable by Tab", async () => {
    stubReferenceData();
    mount({ groupBy: "namespace" });
    await openMenu("Group");
    const tabs = screen.getAllByRole("tab");

    const byNamespace = tabs.find((t) => t.textContent === "By namespace")!;
    expect(byNamespace).toHaveAttribute("aria-selected", "true");
    expect(byNamespace).toHaveAttribute("tabindex", "0");

    for (const t of tabs) {
      if (t !== byNamespace) {
        expect(t).toHaveAttribute("aria-selected", "false");
        expect(t).toHaveAttribute("tabindex", "-1");
      }
    }
  });

  it("activates a tab on click", async () => {
    stubReferenceData();
    const { onChange } = mount();
    await openMenu("Group");
    fireEvent.click(screen.getByRole("tab", { name: "By alert name" }));
    expect(onChange.mock.calls[0]?.[0]?.groupBy).toBe("alertname");
  });

  it("moves focus on the arrow keys without activating — activation is Enter/Space only", async () => {
    stubReferenceData();
    const { onChange } = mount();
    await openMenu("Group");
    const all = screen.getByRole("tab", { name: "All" });
    const byAlertName = screen.getByRole("tab", { name: "By alert name" });

    all.focus();
    fireEvent.keyDown(all, { key: "ArrowRight" });
    expect(document.activeElement).toBe(byAlertName);
    // Moving focus must not itself fire the rollup request the axis change would.
    expect(onChange).not.toHaveBeenCalled();

    fireEvent.keyDown(byAlertName, { key: "Enter" });
    expect(onChange.mock.calls[0]?.[0]?.groupBy).toBe("alertname");
  });
});

/**
 * ⭐ THIS IS THE SUITE THAT PAYS FOR THE MENUS.
 *
 * Four axes are behind a popover now (ADR 0033), which means their controls are
 * out of the accessibility tree while it is closed. That is only acceptable
 * because the *facts* are not: every axis states what it is narrowing by on its
 * own trigger, at rest, and the total is on a button beside them. Delete these
 * assertions and the menus stop being a layout decision and start being a
 * disclosure failure — "why is this list empty?" would be a click away for a
 * sighted operator and unanswerable for a screen-reader one.
 */
describe("what the toolbar says without being opened", () => {
  it("names each axis it is narrowing by, on the trigger itself", () => {
    stubReferenceData();
    mount({
      state: ["firing"],
      severity: ["critical", "warning"],
      since: new Date(Date.now() - 24 * 3_600_000).toISOString(),
      groupBy: "namespace",
    });

    // A view is three axes agreeing, so the trigger says the view's name rather
    // than reciting the axes it is made of.
    expect(screen.getByRole("button", { name: /^Status\s+Firing/ })).toBeTruthy();
    // Two severities: the first, and how many more. Never a bare count.
    expect(screen.getByRole("button", { name: /^Severity\s+critical \+1/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Group\s+namespace/ })).toBeTruthy();
    expect(screen.getByTitle(/Lower bound on last activity/)).toHaveTextContent("Last 24 hours");
  });

  it("says only the axis name when that axis is narrowing nothing", () => {
    stubReferenceData();
    mount();

    // Not "Status: any" and not a zero — an axis that is doing nothing says its
    // name and stops, so the ones that ARE doing something are the only words
    // on the band with a value attached.
    expect(screen.getByRole("button", { name: "Status" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Severity" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Group" })).toBeTruthy();
  });
});

describe("clearing", () => {
  it("shows the count of what is on, and nothing when nothing is", () => {
    stubReferenceData();
    const { onReset, unmount } = mount({ state: ["firing"], q: "err" });
    const button = screen.getByRole("button", { name: "Clear 2 filters" });
    fireEvent.click(button);
    expect(onReset).toHaveBeenCalled();
    unmount();

    stubReferenceData();
    mount();
    expect(screen.queryByRole("button", { name: /Clear/ })).toBeNull();
  });
});
