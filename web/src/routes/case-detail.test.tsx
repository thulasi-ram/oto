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
import { alertRef, caseDetail } from "~/test/fixtures";
import {
  item,
  list,
  problem,
  renderScreen,
  stubFetch,
  until,
  type FetchStub,
} from "~/test/harness";

const ID = "0f8fad5b-d9cb-469f-a165-70867728950e";
const ALERT_ID = "8b1f0d38-6ae4-4f2d-9d3f-1f6b1f0d38ae";
const PATH = `/api/v1/cases/${ID}`;

function mount(patch = {}): FetchStub {
  const net = stubFetch({
    [`GET ${PATH}`]: () => ({ json: item(caseDetail({ id: ID, ...patch })) }),
    [`GET ${PATH}/events`]: () => ({ json: list([]) }),
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
/* The vocabulary the screen is allowed to use                                */
/* -------------------------------------------------------------------------- */

describe("the word on screen", () => {
  it("⛔ never calls this firing a group, and never calls its group a case", async () => {
    mount({
      alert: alertRef(),
      group: {
        id: "0f1e2d3c-4b5a-4697-8899-aabbccddeeff",
        group_key: "alertname=HighErrorRate",
        generation: 1,
        title: "HighErrorRate in payments",
        state: "open",
      },
    });
    await ready("Acknowledge");

    const text = document.body.textContent ?? "";
    // A Case is one firing of one alert — the thing that is acknowledged.
    expect(text).toContain("One firing of one alert");
    // An AlertGroup is Alertmanager's batching, and the panel says so rather
    // than letting a link called "group" read as this case's parent.
    expect(text).toContain("Alertmanager batched this firing");
    expect(text).not.toMatch(/currently-joined/i);
    expect(text).not.toMatch(/\bincident\b/i);
    expect(text).not.toMatch(/\bcorrelat/i);
  });
});
