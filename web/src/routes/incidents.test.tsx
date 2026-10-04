/**
 * The Incident list, and the one property of it that is easy to get wrong in a
 * way nothing on screen would show.
 *
 * ⛔ THE STATE IS A READING, NEVER A CONTROL. An Incident is `active` while any
 * member Case is open and `quiet` otherwise, derived by the server from the Cases
 * (ADR 0052 §3). The list renders the word the server sent and offers no way to
 * change it — a button, select or toggle over that word would be a status a hand
 * can set, which is the response the owner ruled out of oto.
 *
 * ⭐ AND THE ROW SHOWS ITS WORKING: the open count beside the word, so the rule can
 * be seen to hold rather than taken on trust.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import IncidentsRoute from "./incidents";
import { incident } from "~/test/fixtures";
import { list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const PATH = "/api/v1/incidents";

function mount(rows = [incident()]): FetchStub {
  const net = stubFetch({ [`GET ${PATH}`]: () => ({ json: list(rows) }) });
  renderScreen(() => <IncidentsRoute />, { path: "/incidents" });
  return net;
}

describe("the Incident list", () => {
  it("leads each row with the Incident's number and links to it by number", async () => {
    mount([incident({ number: 7 })]);
    await until(() => expect(screen.getByText("#7")).toBeTruthy());
    expect(screen.getByText("#7").closest("a")?.getAttribute("href")).toBe("/incidents/7");
  });

  it("⭐ renders the DERIVED state the server sent, with the open count beside it", async () => {
    mount([
      incident({ number: 4, state: "active", member_count: 3, open_member_count: 1 }),
      incident({
        id: "6b1f9d9f-4d20-4c9f-8b62-7a1e3d8c2f55",
        number: 3,
        state: "quiet",
        member_count: 2,
        open_member_count: 0,
        alertnames: ["KubePodCrashLooping"],
      }),
    ]);

    await until(() => expect(screen.getByText("#3")).toBeTruthy());
    const states = [...document.querySelectorAll("[data-incident-state]")].map((el) =>
      el.getAttribute("data-incident-state"),
    );
    expect(states).toEqual(["active", "quiet"]);
    expect(screen.getByText(/3 Cases, 1 open/)).toBeTruthy();
    expect(screen.getByText(/2 Cases, 0 open/)).toBeTruthy();
  });

  it("says who drew it — a person by their frozen label, or a Correlator", async () => {
    mount([
      incident({ number: 4 }),
      incident({
        id: "6b1f9d9f-4d20-4c9f-8b62-7a1e3d8c2f55",
        number: 5,
        drawn_by: {
          kind: "correlator",
          label: null,
          correlator_id: "7c2a0e1f-5e31-4daf-9c73-8b2f4e9d3a66",
        },
      }),
    ]);
    await until(() => expect(screen.getByText("#5")).toBeTruthy());
    expect(screen.getByText(/drawn by Priya R\./)).toBeTruthy();
    expect(screen.getByText(/drawn by a Correlator/)).toBeTruthy();
  });

  it("⛔ offers no control that sets an Incident's state", async () => {
    mount([incident({ state: "active" })]);
    await until(() => expect(screen.getByText("#4")).toBeTruthy());

    // The chip is a span: a reading. Nothing on the list can change it.
    const chip = document.querySelector("[data-incident-state]");
    expect(chip?.tagName).toBe("SPAN");
    for (const role of ["button", "combobox", "checkbox", "switch"] as const) {
      for (const el of screen.queryAllByRole(role)) {
        expect(el.textContent ?? "").not.toMatch(/active|quiet|status|resolve|close/i);
      }
    }
  });

  it("says where an Incident comes from when there are none, and that empty ones are hidden", async () => {
    mount([]);
    // With empty Incidents hidden, "none have been drawn" could be false.
    await until(() => expect(screen.getByText("No Incidents with Cases in them.")).toBeTruthy());
    expect(document.body.textContent).toMatch(/by selecting Cases on the Cases screen/);
    expect(document.body.textContent).toMatch(/Show empty Incidents lists it/);

    fireEvent.click(screen.getByLabelText("Show empty Incidents"));
    await until(() => expect(screen.getByText("No Incidents have been drawn.")).toBeTruthy());
  });

  it("⭐ hides empty Incidents by default and asks for them only when the toggle is on", async () => {
    const net = mount();
    await until(() => expect(net.to(PATH).length).toBeGreaterThan(0));
    expect(net.to(PATH)[0]!.search.has("include_empty")).toBe(false);

    fireEvent.click(screen.getByLabelText("Show empty Incidents"));
    await until(() =>
      expect(net.to(PATH).some((r) => r.search.get("include_empty") === "true")).toBe(true),
    );
  });

  it("names an emptied Incident for what it is when it is shown", async () => {
    mount([
      incident({ number: 9, state: "quiet", member_count: 0, open_member_count: 0, alertnames: [] }),
    ]);
    await until(() => expect(screen.getByText("#9")).toBeTruthy());
    expect(screen.getByText("No Cases left in it")).toBeTruthy();
  });

  it("asks for nothing but a page", async () => {
    const net = mount();
    await until(() => expect(net.to(PATH).length).toBeGreaterThan(0));
    const q = net.to(PATH)[0]!.search;
    expect([...q.keys()].sort()).toEqual(["limit"]);
  });
});
