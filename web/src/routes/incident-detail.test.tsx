/**
 * One Incident, and the three membership verbs a human has over it.
 *
 * ⭐ ADD, REMOVE, MOVE — AND EACH PATH IS ASSERTED. They are the whole edit
 * surface ADR 0052 §4 gives a person: add a Case to this Incident, take one out
 * (tombstoned, never deleted), or move one to another Incident in one step. The
 * move is addressed by the Incident the Case is LEAVING, and its answer is the
 * one it went to — so the screen follows it there.
 *
 * ⛔ A REMOVED CASE STAYS ON THE PAGE. "Removed by Priya" and "moved to #7" are
 * part of the story, and a Correlator must never re-add what a human took out,
 * which only means something while the removal is visible.
 *
 * ⛔ THE REFUSAL IS THE SERVER'S SENTENCE. `409 case_in_incident` carries the
 * pointer to the move in its `detail`; the screen renders it verbatim.
 *
 * ⛔ AND NOTHING HERE SETS THE STATE. `active`/`quiet` is read off the Cases.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import IncidentDetailRoute from "./incident-detail";
import { incidentDetail, incidentMember } from "~/test/fixtures";
import {
  item,
  problem,
  renderScreen,
  stubFetch,
  until,
  type FetchStub,
} from "~/test/harness";
import type { IncidentDetail } from "~/api/types";

const NUMBER = 4;
const PATH = `/api/v1/incidents/${NUMBER}`;
const CASE = "0f8fad5b-d9cb-469f-a165-70867728950e";
const OTHER = "9d4c2b1a-8e7f-4a6b-9c5d-3e2f1a0b9c8d";

function mount(detail: IncidentDetail = incidentDetail()): FetchStub {
  const net = stubFetch({ [`GET ${PATH}`]: () => ({ json: item(detail) }) });
  renderScreen(() => <IncidentDetailRoute />, {
    path: `/incidents/${NUMBER}`,
    routePath: "/incidents/:number",
  });
  return net;
}

async function ready(): Promise<void> {
  await until(() => expect(screen.getByText(`Incident #${NUMBER}`)).toBeTruthy());
}

function button(name: string | RegExp): HTMLElement {
  return screen.getByRole("button", { name });
}

/* -------------------------------------------------------------------------- */
/* What the page shows                                                        */
/* -------------------------------------------------------------------------- */

describe("the Incident", () => {
  it("asks for it by NUMBER, and shows the derived state and who drew it", async () => {
    const net = mount(incidentDetail({ state: "quiet", open_member_count: 0 }));
    await ready();

    expect(net.calls[0]?.path).toBe(PATH);
    expect(document.querySelector("[data-incident-state]")?.getAttribute("data-incident-state")).toBe(
      "quiet",
    );
    expect(document.body.textContent).toMatch(/drawn by\s*Priya R\./);
  });

  it("⭐ lists current members linking to their Case, and keeps removed ones as tombstones", async () => {
    mount(
      incidentDetail({
        members: [
          incidentMember(),
          incidentMember({
            case_id: OTHER,
            case_number: 413,
            case_state: "closed",
            alertname: "KubePodCrashLooping",
            removed_at: "2026-08-09T09:20:00.000Z",
            removed_by_label: "Sam K.",
            moved_to_number: null,
          }),
          incidentMember({
            case_id: "1e2d3c4b-5a69-4788-9a1b-2c3d4e5f6a7b",
            case_number: 414,
            removed_at: "2026-08-09T09:25:00.000Z",
            removed_by_label: "Sam K.",
            moved_to_number: 7,
          }),
        ],
      }),
    );
    await ready();

    expect(screen.getByRole("link", { name: "#412" }).getAttribute("href")).toBe(`/cases/${CASE}`);
    // A plain removal names who removed it…
    expect(document.body.textContent).toMatch(/removed by Sam K\./);
    // …and a move names where the Case went, as a link to that Incident.
    expect(document.body.textContent).toMatch(/moved to\s*#7\s*by Sam K\./);
    expect(screen.getByRole("link", { name: "#7" }).getAttribute("href")).toBe("/incidents/7");
    // Tombstones offer no verbs: only the one current member has Remove/Move.
    expect(screen.getAllByRole("button", { name: "Remove" })).toHaveLength(1);
  });

  it("⛔ offers no control that sets the Incident's state", async () => {
    mount();
    await ready();
    expect(document.querySelector("[data-incident-state]")?.tagName).toBe("SPAN");
    for (const el of screen.queryAllByRole("button")) {
      expect(el.textContent ?? "").not.toMatch(/active|quiet|status|severity|lead|resolve|close/i);
    }
  });
});

/* -------------------------------------------------------------------------- */
/* The membership verbs                                                       */
/* -------------------------------------------------------------------------- */

describe("changing which Cases are in it", () => {
  it("adds a Case by id to THIS Incident", async () => {
    const net = mount();
    net.on(`POST ${PATH}/cases`, () => ({
      json: item(incidentDetail({ member_count: 2 })),
    }));
    await ready();

    fireEvent.input(screen.getByLabelText("Add a Case by id"), { target: { value: ` ${OTHER} ` } });
    fireEvent.click(button("Add Case"));

    await until(() => expect(net.to(`${PATH}/cases`)).toHaveLength(1));
    const post = net.to(`${PATH}/cases`)[0]!;
    expect(post.method).toBe("POST");
    expect(post.body).toEqual({ case_id: OTHER });
    expect(post.headers["Idempotency-Key"]).toBeTruthy();
  });

  it("⛔ shows the server's pointer to the move when the Case is in another Incident", async () => {
    const net = mount();
    net.on(`POST ${PATH}/cases`, () =>
      problem(409, "case_in_incident", {
        detail: `Case #413 is already in Incident #3; move it with POST /api/v1/incidents/3/cases/${OTHER}/move.`,
      }),
    );
    await ready();

    fireEvent.input(screen.getByLabelText("Add a Case by id"), { target: { value: OTHER } });
    fireEvent.click(button("Add Case"));

    await until(() =>
      expect(screen.getByRole("alert").textContent).toMatch(
        /already in Incident #3; move it with POST \/api\/v1\/incidents\/3\/cases\/.*\/move/,
      ),
    );
  });

  it("removes a member through its own `/remove`, with no body", async () => {
    const net = mount();
    const after = incidentDetail({
      state: "quiet",
      member_count: 0,
      open_member_count: 0,
      members: [
        incidentMember({ removed_at: "2026-08-09T09:30:00.000Z", removed_by_label: "Priya R." }),
      ],
    });
    net.on(`POST ${PATH}/cases/${CASE}/remove`, () => ({ json: item(after) }));
    await ready();
    // The write invalidates the entry, so the refetch must see the world the
    // write produced — a stub still serving the old membership would make the
    // assertion below a race against the refetch.
    net.on(`GET ${PATH}`, () => ({ json: item(after) }));

    fireEvent.click(button("Remove"));

    await until(() => expect(net.to("/remove")).toHaveLength(1));
    expect(net.to("/remove")[0]?.body).toBeNull();
    // The answer is written straight into the page: the Case is now a tombstone.
    await until(() => expect(document.body.textContent).toMatch(/removed by Priya R\./));
  });

  it("⭐ moves a member from THIS Incident to the one named, and follows it there", async () => {
    const net = mount();
    const destination = incidentDetail({ number: 7, member_count: 2 });
    net.on(`POST ${PATH}/cases/${CASE}/move`, () => ({ json: item(destination) }));
    net.on("GET /api/v1/incidents/7", () => ({ json: item(destination) }));
    await ready();

    fireEvent.click(button("Move…"));
    fireEvent.input(screen.getByLabelText("Move to Incident #"), { target: { value: "7" } });
    fireEvent.click(button("Move Case"));

    await until(() => expect(net.to("/move")).toHaveLength(1));
    const post = net.to("/move")[0]!;
    // The path names where the Case is LEAVING; the body names where it goes.
    expect(post.path).toBe(`${PATH}/cases/${CASE}/move`);
    expect(post.body).toEqual({ to_number: 7 });
    await until(() => expect(screen.getByText("Incident #7")).toBeTruthy());
  });

  it("⛔ will not send a move to the Incident the Case is already in", async () => {
    const net = mount();
    await ready();

    fireEvent.click(button("Move…"));
    fireEvent.input(screen.getByLabelText("Move to Incident #"), {
      target: { value: String(NUMBER) },
    });
    expect(button("Move Case")).toBeDisabled();
    expect(net.to("/move")).toHaveLength(0);
  });
});
