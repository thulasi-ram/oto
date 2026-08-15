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

describe("the enumerable filters", () => {
  it("offers every lifecycle state the contract serves, each with a word", () => {
    stubReferenceData();
    mount();

    const group = screen.getByRole("group", { name: "Lifecycle state" });
    const labels = within(group)
      .getAllByRole("checkbox")
      .map((el) => el.closest("label")?.textContent?.trim() ?? "");

    expect(labels).toHaveLength(enumValues("State").length);
    for (const label of labels) expect(label).not.toBe("");
    expectNoUndefined(group as HTMLElement);
  });

  it("toggles a state without disturbing the rest of the filter set", () => {
    stubReferenceData();
    const { onChange } = mount({ severity: ["critical"] });

    const group = screen.getByRole("group", { name: "Lifecycle state" });
    fireEvent.click(within(group).getAllByRole("checkbox")[0]!);

    expect(onChange).toHaveBeenCalledTimes(1);
    const next = onChange.mock.calls[0]?.[0];
    expect(next?.state).toEqual([enumValues("State")[0]]);
    expect(next?.severity).toEqual(["critical"]);
  });

  it("keeps a severity that arrived from a link even though it is not one of the common three", () => {
    // `AlertDTO.severity` is a free vocabulary, not an enum. A deployment using
    // `sev1` must see its own word back rather than have it silently dropped.
    stubReferenceData();
    mount({ severity: ["sev1"] });

    const group = screen.getByRole("group", { name: "Severity" });
    expect(within(group).getByText("sev1")).toBeTruthy();
  });
});

describe("the orthogonal axes", () => {
  it("defaults snoozed to `any`, because hiding snoozed alerts is how an incident is lost", () => {
    stubReferenceData();
    mount();
    const select = screen.getByTitle(/currently holding its notifications/i) as HTMLSelectElement;
    expect(select.value).toBe("");
    expect(within(select).getByRole("option", { name: /Any \(default/ })).toBeTruthy();
  });

  it("maps the snooze control onto a tri-state, never onto a lifecycle state", () => {
    stubReferenceData();
    const { onChange } = mount();
    const select = screen.getByTitle(/currently holding its notifications/i);

    fireEvent.change(select, { target: { value: "true" } });
    expect(onChange.mock.calls[0]?.[0]?.snoozed).toBe(true);
    // And it never touches `state` — a snoozed alert is still firing.
    expect(onChange.mock.calls[0]?.[0]?.state).toEqual([]);
  });

  it("keeps acknowledgement orthogonal to state too", () => {
    stubReferenceData();
    const { onChange } = mount();
    fireEvent.change(screen.getByTitle(/A receipt on a signal/i), { target: { value: "acked" } });
    expect(onChange.mock.calls[0]?.[0]?.ack).toBe("acked");
    expect(onChange.mock.calls[0]?.[0]?.state).toEqual([]);
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
    const select = screen.getByText("Cluster").parentElement?.querySelector("select");
    expect(within(select!).getByRole("option", { name: "Production (EU)" })).toBeTruthy();

    fireEvent.change(select!, { target: { value: "prod-eu" } });
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
  it("offers one tab per axis the contract rolls up on, plus 'All'", () => {
    stubReferenceData();
    mount();
    const tabs = screen.getAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual([
      "All",
      "By alert name",
      "By namespace",
      "By fingerprint",
    ]);
  });

  it("marks exactly the current axis selected, and only it reachable by Tab", () => {
    stubReferenceData();
    mount({ groupBy: "namespace" });
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

  it("activates a tab on click", () => {
    stubReferenceData();
    const { onChange } = mount();
    fireEvent.click(screen.getByRole("tab", { name: "By alert name" }));
    expect(onChange.mock.calls[0]?.[0]?.groupBy).toBe("alertname");
  });

  it("moves focus on the arrow keys without activating — activation is Enter/Space only", () => {
    stubReferenceData();
    const { onChange } = mount();
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
