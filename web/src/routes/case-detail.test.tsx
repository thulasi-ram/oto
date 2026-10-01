/**
 * The screen an operator acts on, and the one subject its control has.
 *
 * ⛔ THE ACK IS CASE-ADDRESSED AND THE PATH IS ASSERTED. `POST
 * /api/v1/cases/{id}/ack`, never `/alerts/{id}/ack`. The alert-addressed
 * spelling had to resolve "whatever is open right now" server-side, which made
 * the subject of the receipt a race: the firing the operator looked at and the
 * firing the receipt landed on could differ.
 *
 * ⛔ AND IT IS ONE CONTROL WITH TWO WORDS. `ack_state` has two values, so a
 * separate Acknowledge and Withdraw left one of the two dead on every paint. The
 * toggle reads what the case IS and does the other thing — which is why the tests
 * below assert the WORD as much as the request.
 *
 * ⛔ SNOOZE IS NOT ON THIS SCREEN AT ALL, AND THE LAST DESCRIBE GUARDS THAT. A
 * snooze holds oto's notifications for the IDENTITY: it outlives this case and
 * covers whatever fires next under the same labels, so it is taken from the
 * alert's own screen (`/alerts/:id`) and ended from the Quiet tab. A control here
 * would put an alert-scoped decision behind a case-shaped heading.
 *
 * ⛔ AND THE ROLLBACK TESTS ARE WHY THE FILE IS LONG. A UI that shows an ack as
 * written when the server refused it is the same lie as a chat message that
 * says "delivered" when nothing was delivered.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import CaseDetailRoute from "./case-detail";
import type { Incident } from "~/api/types";
import { alertRef, caseDetail, incident, incidentDetail } from "~/test/fixtures";
import {
  item,
  list,
  problem,
  renderScreen,
  stubFetch,
  until,
  type FetchStub,
  type RecordedCall,
} from "~/test/harness";

const ID = "0f8fad5b-d9cb-469f-a165-70867728950e";
const ALERT_ID = "8b1f0d38-6ae4-4f2d-9d3f-1f6b1f0d38ae";
const PATH = `/api/v1/cases/${ID}`;

/**
 * Mount the Case page. `inIncident` is what `GET /incidents?case_id=` answers —
 * the Incident this Case is in now, or none — and only for THIS Case's id: a
 * screen asking about any other Case is asking the wrong question, and gets an
 * empty answer that would make the in-an-Incident tests fail.
 */
function mount(patch = {}, inIncident: Incident | null = null): FetchStub {
  const net = stubFetch({
    [`GET ${PATH}`]: () => ({ json: item(caseDetail({ id: ID, ...patch })) }),
    [`GET ${PATH}/events`]: () => ({ json: list([]) }),
    "GET /api/v1/incidents": (call: RecordedCall) => ({
      json: list(call.search.get("case_id") === ID && inIncident !== null ? [inIncident] : []),
    }),
  });

  renderScreen(() => <CaseDetailRoute />, { path: `/cases/${ID}`, routePath: "/cases/:id" });
  return net;
}

function barButtons(name: string | RegExp): readonly HTMLElement[] {
  return screen
    .queryAllByRole("button", { name })
    .filter((el) => el.closest('[role="dialog"]') === null);
}

function barButton(name: string | RegExp): HTMLElement {
  const found = barButtons(name);
  expect(found, `the header has no \`${String(name)}\` button`).toHaveLength(1);
  return found[0]!;
}

function openDialog(): ReturnType<typeof within> {
  const dialog = document.querySelector('[role="dialog"]');
  expect(dialog, "no dialog is open").not.toBeNull();
  return within(dialog as HTMLElement);
}

async function ready(name: string | RegExp): Promise<HTMLElement> {
  await until(() => expect(barButtons(name)).toHaveLength(1));
  return barButton(name);
}

/* -------------------------------------------------------------------------- */
/* Acknowledging this firing                                                  */
/* -------------------------------------------------------------------------- */

describe("acknowledging the case", () => {
  it("posts to the CASE's own ack, with one idempotency key and the trimmed note", async () => {
    const net = mount();
    net.on(`POST ${PATH}/ack`, () => ({ json: item(caseDetail({ ack_state: "acked" })) }));

    fireEvent.click(await ready("Acknowledge"));
    fireEvent.input(openDialog().getByLabelText("Note (optional)"), {
      target: { value: "  known deploy, rolling back  " },
    });
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() => expect(net.to("/ack")).toHaveLength(1));
    const posts = net.to("/ack");
    // ⛔ THE CASE'S ENDPOINT. The alert-addressed one would have made the subject
    // of the receipt "whatever is open now", resolved server-side, which is a race.
    expect(posts[0]?.path).toBe(`${PATH}/ack`);
    expect(posts[0]?.body).toEqual({ note: "known deploy, rolling back" });
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
    expect(net.calls.filter((c) => c.path.startsWith("/api/v1/alerts/"))).toHaveLength(0);
  });

  it("says a receipt does not change the signal, at the moment of committing", async () => {
    mount();
    fireEvent.click(await ready("Acknowledge"));
    expect(openDialog().getByText(/it stays firing until the upstream says otherwise/i)).toBeTruthy();
  });

  it("⛔ keeps the dialog open and writes nothing when the server refuses", async () => {
    const net = mount();
    net.on(`POST ${PATH}/ack`, () => problem(500, "internal"));

    fireEvent.click(await ready("Acknowledge"));
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));
    await until(() => expect(net.to("/ack")).toHaveLength(1));

    await until(() => expect(document.querySelector('[role="dialog"]')).not.toBeNull());
    expect(net.calls.filter((c) => c.method === "GET" && c.path === PATH)).toHaveLength(1);
  });

  it("refuses to offer the receipt on a case that has already ended", async () => {
    mount({ ended_at: "2026-08-09T09:30:00.000Z", state: "resolved" });
    await until(() => expect(barButtons("Acknowledge")).toHaveLength(1));
    // Acking an ended case is a 412 by contract; saying so first is kinder than
    // sending a request whose only possible answer is a refusal.
    expect(barButton("Acknowledge")).toBeDisabled();
  });
});

/* -------------------------------------------------------------------------- */
/* Taking it back                                                             */
/* -------------------------------------------------------------------------- */

describe("withdrawing the acknowledgement", () => {
  it("posts to the case's own unack and names the verb that was refused", async () => {
    const net = mount({ ack_state: "acked" });
    net.on(`POST ${PATH}/unack`, () => problem(412, "no_open_case"));

    fireEvent.click(await ready("Withdraw acknowledgement"));
    fireEvent.click(openDialog().getByRole("button", { name: "Withdraw" }));

    await until(() => expect(net.to("/unack")).toHaveLength(1));
    expect(net.to("/unack")[0]?.path).toBe(`${PATH}/unack`);
    // ⛔ "Nothing here to acknowledge" said of a withdrawal tells the operator
    // the opposite of what happened; `no_open_case` is the refusal both
    // directions share, so the sentence comes from the mode.
    await until(() => expect(openDialog().getByText(/no receipt left to withdraw/i)).toBeTruthy());
  });

  it("⛔ is the SAME control, so an unacked case offers no withdrawal at all", async () => {
    // One control with two words: a `Withdraw` sitting permanently beside an
    // `Acknowledge` meant one of the two was dead on every paint, and a dead
    // control is one an operator has to read to discard. The way back is not
    // hidden — it IS this button, the moment there is a receipt to take back.
    mount({ ack_state: "unacked" });
    await ready("Acknowledge");
    expect(barButtons("Withdraw acknowledgement")).toHaveLength(0);
  });

  it("⛔ and an acked case offers no second Acknowledge", async () => {
    mount({ ack_state: "acked" });
    await ready("Withdraw acknowledgement");
    expect(barButtons("Acknowledge")).toHaveLength(0);
  });
});

/* -------------------------------------------------------------------------- */
/* The snooze, which is not this screen's to offer                            */
/* -------------------------------------------------------------------------- */

describe("holding the alert's notifications", () => {
  it("⛔ is offered nowhere on this screen, and posts nothing to the alert", async () => {
    const net = mount();
    await ready("Acknowledge");

    // A snooze outlives this case and covers whatever fires next under the same
    // labels, so its subject is the identity — offered from the alert's own
    // screen, ended from the Quiet tab of the alert list. Here it would be an
    // alert-scoped decision behind a case-shaped heading.
    expect(barButtons(/^Snooze/)).toHaveLength(0);
    expect(barButtons(/^Resume/)).toHaveLength(0);
    expect(net.calls.filter((c) => c.path.startsWith("/api/v1/alerts/"))).toHaveLength(0);
  });

  it("⭐ still points at the identity, so the hold is one hop away", async () => {
    mount();
    await ready("Acknowledge");
    // Removing the control must not strand the operator: the panel naming the
    // alert links out to the screen that does offer it.
    expect(
      screen.getByRole("link", { name: /Every firing of this alert/ }).getAttribute("href"),
    ).toBe(`/alerts/${ALERT_ID}`);
  });
});

/* -------------------------------------------------------------------------- */
/* Drawing an Incident over this Case                                          */
/* -------------------------------------------------------------------------- */

describe("drawing an Incident", () => {
  it("posts this one Case to createIncident and goes to the Incident it drew", async () => {
    const net = mount();
    net.on("POST /api/v1/incidents", () => ({
      status: 201,
      json: item({
        id: "11111111-1111-4111-8111-111111111111",
        number: 4,
        state: "active",
        drawn_at: "2026-08-09T09:05:00.000Z",
        drawn_by: { kind: "human", label: "Priya R.", correlator_id: null },
        member_count: 1,
        open_member_count: 1,
        alertnames: ["HighErrorRate"],
        members: [],
      }),
    }));

    fireEvent.click(await ready("Draw Incident"));

    // The page also GETs this path, to learn which Incident the Case is in.
    const draws = () => net.to("/api/v1/incidents").filter((c) => c.method === "POST");
    await until(() => expect(draws()).toHaveLength(1));
    const post = draws()[0]!;
    // ⛔ ONLY CASE IDS. There is no status, lead or severity to send, and the
    // contract has no field that would take one.
    expect(post.body).toEqual({ case_ids: [ID] });
    expect(post.headers["Idempotency-Key"]).toBeTruthy();
  });

  it("⛔ shows the server's own sentence when the Case is already in an Incident", async () => {
    const net = mount();
    net.on("POST /api/v1/incidents", () =>
      problem(409, "case_in_incident", {
        detail:
          "Case #412 is already in Incident #3; move it with POST /api/v1/incidents/3/cases/" +
          `${ID}/move.`,
      }),
    );

    fireEvent.click(await ready("Draw Incident"));

    // The detail IS the pointer to the move; rewording it would lose it.
    await until(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/already in Incident #3.*\/move/),
    );
  });
});

/* -------------------------------------------------------------------------- */
/* The Incident this Case is in, and the ways to change that                  */
/* -------------------------------------------------------------------------- */

describe("the Incident this Case is in", () => {
  it("asks which Incident THIS Case is in, by its own id", async () => {
    const net = mount();
    await ready("Draw Incident");
    const asked = net.calls.filter((c) => c.path === "/api/v1/incidents" && c.method === "GET");
    expect(asked.length).toBeGreaterThan(0);
    expect(asked[0]!.search.get("case_id")).toBe(ID);
  });

  it("⭐ names the Incident and offers a move — never a draw or an add", async () => {
    // A Case is in at most one Incident (ADR 0052 §4). Offering "Draw" or "Add"
    // here would be offering the one gesture the rule refuses; the honest verb
    // for a Case that is already in a story is MOVE.
    mount({}, incident({ number: 3, state: "active" }));
    await ready("Move to Incident…");

    const link = screen.getByRole("link", { name: /Incident #3/ });
    expect(link.getAttribute("href")).toBe("/incidents/3");
    expect(barButtons("Draw Incident")).toHaveLength(0);
    expect(barButtons("Add to Incident…")).toHaveLength(0);
  });

  it("moves it out of the Incident it is in, addressed by THAT Incident, and shows the new one", async () => {
    const net = mount({}, incident({ number: 3 }));
    net.on(`POST /api/v1/incidents/3/cases/${ID}/move`, () => {
      // The server's truth after the move, for the refetch the write triggers.
      net.on("GET /api/v1/incidents", () => ({ json: list([incident({ number: 7 })]) }));
      return { json: item(incidentDetail({ number: 7, members: [] })) };
    });

    fireEvent.click(await ready("Move to Incident…"));
    fireEvent.input(screen.getByLabelText("Move to Incident #"), { target: { value: "7" } });
    fireEvent.click(screen.getByRole("button", { name: "Move Case" }));

    await until(() => expect(net.to("/move")).toHaveLength(1));
    const post = net.to("/move")[0]!;
    // ⭐ THE PATH NAMES WHERE IT IS LEAVING, the body where it goes — so a screen
    // a frame behind gets a 404 rather than a move from somewhere it is not.
    expect(post.path).toBe(`/api/v1/incidents/3/cases/${ID}/move`);
    expect(post.body).toEqual({ to_number: 7 });
    expect(post.headers["Idempotency-Key"]).toBeTruthy();
    await until(() =>
      expect(screen.getByRole("link", { name: /Incident #7/ }).getAttribute("href")).toBe(
        "/incidents/7",
      ),
    );
  });

  it("⛔ will not move it into the Incident it is already in", async () => {
    mount({}, incident({ number: 3 }));
    fireEvent.click(await ready("Move to Incident…"));
    fireEvent.input(screen.getByLabelText("Move to Incident #"), { target: { value: "3" } });
    expect(screen.getByRole("button", { name: "Move Case" })).toBeDisabled();
  });

  it("adds a Case that is in none to an existing Incident, by its number", async () => {
    const net = mount();
    net.on("POST /api/v1/incidents/7/cases", () => {
      net.on("GET /api/v1/incidents", () => ({ json: list([incident({ number: 7 })]) }));
      return { json: item(incidentDetail({ number: 7 })) };
    });

    fireEvent.click(await ready("Add to Incident…"));
    fireEvent.input(screen.getByLabelText("Add to Incident #"), { target: { value: "#7" } });
    fireEvent.click(screen.getByRole("button", { name: "Add Case" }));

    await until(() => expect(net.to("/incidents/7/cases")).toHaveLength(1));
    const post = net.to("/incidents/7/cases")[0]!;
    expect(post.method).toBe("POST");
    expect(post.body).toEqual({ case_id: ID });
    expect(post.headers["Idempotency-Key"]).toBeTruthy();
    // Now in one, so the header offers the move and no longer the add.
    await ready("Move to Incident…");
    expect(barButtons("Add to Incident…")).toHaveLength(0);
  });

  it("⛔ shows the server's own sentence when the add is refused", async () => {
    const net = mount();
    net.on("POST /api/v1/incidents/7/cases", () =>
      problem(409, "case_in_incident", {
        detail: `Case #412 is already in Incident #3; move it with POST /api/v1/incidents/3/cases/${ID}/move.`,
      }),
    );

    fireEvent.click(await ready("Add to Incident…"));
    fireEvent.input(screen.getByLabelText("Add to Incident #"), { target: { value: "7" } });
    fireEvent.click(screen.getByRole("button", { name: "Add Case" }));

    await until(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/already in Incident #3.*\/move/),
    );
  });
});

/* -------------------------------------------------------------------------- */
/* The vocabulary the screen is allowed to use                                */
/* -------------------------------------------------------------------------- */

describe("the word on screen", () => {
  it("⛔ never calls this firing an incident, a correlation or a group", async () => {
    mount({ alert: alertRef() });
    await ready("Acknowledge");
    // The membership controls mount once the screen knows which Incident this
    // Case is in, and the scan below has to see them to take them out.
    await ready("Draw Incident");

    const text = document.body.textContent ?? "";
    // A Case is one firing of one alert — the thing that is acknowledged.
    expect(text).toContain("One firing of one alert");
    // ⛔ AND THE ALERT GROUP IS GONE (git-bug 7570090). The screen used to carry
    // a "Notified in" panel naming Alertmanager's batching; the object it named
    // no longer exists, so neither the panel nor the word may come back.
    expect(text).not.toMatch(/\bgroup\b/i);
    expect(text).not.toMatch(/currently-joined/i);
    // ⭐ THE ONE PLACE THE WORD MAY APPEAR IS THE MEMBERSHIP CONTROLS (ADR 0052,
    // git-bug f89c9cc): draw, add, move, and the Incident this Case is in. An
    // Incident is a different object — a set of Cases — so those controls name
    // it; what must never happen is the screen calling THIS FIRING an incident.
    // So the assertion is made on the text with those controls taken out.
    let withoutMembership = text;
    for (const el of document.querySelectorAll("[data-incident-membership]")) {
      withoutMembership = withoutMembership.replace(el.textContent ?? "", "");
    }
    expect(withoutMembership).not.toMatch(/\bincident\b/i);
    expect(text).not.toMatch(/\bcorrelat/i);
  });
});
