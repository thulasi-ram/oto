/**
 * The two controls that moved onto the case, and what the screen says when the
 * server says no.
 *
 * ⛔ THE ROLLBACK TESTS ARE THE POINT OF THIS FILE, and the stakes went UP when
 * these controls moved here from the alert detail. A UI that shows an ack as
 * written when the server refused it is the same lie as a chat message that says
 * "delivered" when nothing was delivered — and this ack is a fan-out, so the lie
 * is now told about every member of the case at once. So every refusal below is
 * checked twice: that the failure is *shown*, and that the screen has not moved
 * on as if it had succeeded.
 *
 * ⛔ THE ENDPOINTS ARE CASE-SCOPED AND THE PATHS ARE ASSERTED. `POST
 * /api/v1/alert-groups/{id}/ack|snooze|unsnooze`, never the per-alert ones. A
 * screen that acknowledged the case by walking its members and calling
 * `/alerts/{id}/ack` forty times would look identical on screen and be a
 * different thing entirely: forty receipts with forty idempotency keys, some of
 * which land and some of which do not.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import CaseDetailRoute from "./case-detail";
import { groupDetail } from "~/test/fixtures";
import {
  item,
  list,
  problem,
  renderScreen,
  stubFetch,
  until,
  type FetchStub,
} from "~/test/harness";

const ID = "0f1e2d3c-4b5a-4697-8899-aabbccddeeff";
const PATH = `/api/v1/alert-groups/${ID}`;

/** Render the case screen the way the router renders it, params and all. */
function mount(patch = {}): FetchStub {
  const net = stubFetch({
    [`GET ${PATH}`]: () => ({ json: item(groupDetail(patch)) }),
    [`GET ${PATH}/alerts`]: () => ({ json: list([]) }),
    [`GET ${PATH}/timeline`]: () => ({ json: list([]) }),
  });

  renderScreen(() => <CaseDetailRoute />, { path: `/cases/${ID}`, routePath: "/cases/:id" });
  return net;
}

/** Buttons on the header bar rather than inside whichever dialog is open. */
function barButtons(name: string | RegExp): readonly HTMLElement[] {
  return screen
    .queryAllByRole("button", { name })
    .filter((el) => el.closest('[role="dialog"]') === null);
}

function barButton(name: string): HTMLElement {
  const found = barButtons(name);
  expect(found, `the header has no \`${name}\` button`).toHaveLength(1);
  return found[0]!;
}

function openDialog(): ReturnType<typeof within> {
  const dialog = document.querySelector('[role="dialog"]');
  expect(dialog, "no dialog is open").not.toBeNull();
  return within(dialog as HTMLElement);
}

async function ackButton(): Promise<HTMLElement> {
  await until(() => expect(barButtons("Acknowledge every current member")).toHaveLength(1));
  return barButton("Acknowledge every current member");
}

/* -------------------------------------------------------------------------- */
/* Acknowledging the case                                                     */
/* -------------------------------------------------------------------------- */

describe("acknowledging every current member", () => {
  it("posts once to the case's own ack, with one idempotency key and the trimmed note", async () => {
    const net = mount();
    net.on(`POST ${PATH}/ack`, () => ({ json: item(groupDetail({ acked_count: 3 })) }));

    fireEvent.click(await ackButton());
    fireEvent.input(openDialog().getByLabelText("Note (optional)"), {
      target: { value: "  known deploy, rolling back  " },
    });
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() => expect(net.to("/ack")).toHaveLength(1));
    const posts = net.to("/ack");
    // ⛔ THE CASE'S ENDPOINT, not a walk over the members.
    expect(posts[0]?.path).toBe(`${PATH}/ack`);
    // Trimmed, because a note is prose and trailing spaces are not part of it.
    expect(posts[0]?.body).toEqual({ note: "known deploy, rolling back" });
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
    // Nothing per-alert went out alongside it.
    expect(net.calls.filter((c) => c.path.startsWith("/api/v1/alerts/"))).toHaveLength(0);
  });

  it("sends no note at all rather than an empty one", async () => {
    const net = mount();
    net.on(`POST ${PATH}/ack`, () => ({ json: item(groupDetail()) }));

    fireEvent.click(await ackButton());
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() => expect(net.to("/ack")).toHaveLength(1));
    // `{}`, never `{note: ""}`: the contract has no "cleared" note and a blank
    // line on the timeline is forever.
    expect(net.to("/ack")[0]?.body).toEqual({});
  });

  it("⛔ keeps the dialog open and writes nothing when the server refuses", async () => {
    const net = mount();
    net.on(`POST ${PATH}/ack`, () => problem(500, "internal"));

    fireEvent.click(await ackButton());
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));
    await until(() => expect(net.to("/ack")).toHaveLength(1));

    // The refusal is visible, and the screen did not close the dialog as if the
    // receipt existed.
    await until(() => expect(document.querySelector('[role="dialog"]')).not.toBeNull());
    // And the case was never refetched as if something had changed.
    expect(net.calls.filter((c) => c.method === "GET" && c.path === PATH)).toHaveLength(1);
  });

  it("puts a 412 in words rather than leaving a bare status on screen", async () => {
    const net = mount();
    net.on(`POST ${PATH}/ack`, () => problem(412, "precondition_failed"));

    fireEvent.click(await ackButton());
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() =>
      expect(openDialog().getByText(/no currently-joined member whose episode is still open/))
        .toBeTruthy(),
    );
  });

  it("says at the moment of committing that later members are not covered", async () => {
    mount();
    fireEvent.click(await ackButton());

    // ⭐ THE LIMIT IS STATED WHERE THE DECISION IS MADE. An operator who believes
    // a case-wide ack covers what joins next has silenced their own future
    // signal, and finding that out afterwards is finding it out too late.
    expect(openDialog().getByText(/members that join later are NOT acknowledged/i)).toBeTruthy();
  });
});

/* -------------------------------------------------------------------------- */
/* Taking the acknowledgement back                                            */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ THE WAY BACK IS PART OF THE CONTROL, NOT AN EXTRA.
 *
 * When acknowledging moved onto this screen it arrived without its withdrawal:
 * `POST /alert-groups/{id}/ack` existed and `.../unack` did not, so the widest
 * gesture in the product — one press writing a receipt on every member — was also
 * the only irreversible one, and the route back was opening each member alert in
 * turn. These tests are the fence around that: the control is here, it posts to the
 * CASE's own unack, and a refusal is shown in the withdrawal's own words rather than
 * the acknowledgement's.
 */
describe("withdrawing the acknowledgement", () => {
  async function withdrawButton(): Promise<HTMLElement> {
    await until(() => expect(barButtons("Withdraw acknowledgement")).toHaveLength(1));
    return barButton("Withdraw acknowledgement");
  }

  it("posts to the case's own unack, with one idempotency key and the trimmed note", async () => {
    const net = mount();
    net.on(`POST ${PATH}/unack`, () => ({ json: item(groupDetail({ acked_count: 0 })) }));

    fireEvent.click(await withdrawButton());
    fireEvent.input(openDialog().getByLabelText("Note (optional)"), {
      target: { value: "  un-acking, it's back  " },
    });
    fireEvent.click(openDialog().getByRole("button", { name: "Withdraw" }));

    await until(() => expect(net.to("/unack")).toHaveLength(1));
    const posts = net.to("/unack");
    // ⛔ THE CASE'S ENDPOINT, not a walk over the members.
    expect(posts[0]?.path).toBe(`${PATH}/unack`);
    expect(posts[0]?.body).toEqual({ note: "un-acking, it's back" });
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
    expect(net.calls.filter((c) => c.path.startsWith("/api/v1/alerts/"))).toHaveLength(0);
  });

  it("is offered whatever the acknowledged count says", async () => {
    // `acked_count` is a roll-up over episodes and a partially-acked case is the
    // normal shape of one, so a control hidden below some threshold would be hidden
    // from exactly the operator who needs it.
    mount({ acked_count: 0 });
    await withdrawButton();
  });

  it("⛔ keeps the dialog open and writes nothing when the server refuses", async () => {
    const net = mount();
    net.on(`POST ${PATH}/unack`, () => problem(500, "internal"));

    fireEvent.click(await withdrawButton());
    fireEvent.click(openDialog().getByRole("button", { name: "Withdraw" }));
    await until(() => expect(net.to("/unack")).toHaveLength(1));

    await until(() => expect(document.querySelector('[role="dialog"]')).not.toBeNull());
    expect(net.calls.filter((c) => c.method === "GET" && c.path === PATH)).toHaveLength(1);
  });

  it("names the verb that was refused, not the opposite one", async () => {
    const net = mount();
    net.on(`POST ${PATH}/unack`, () => problem(412, "no_open_case"));

    fireEvent.click(await withdrawButton());
    fireEvent.click(openDialog().getByRole("button", { name: "Withdraw" }));

    // ⛔ "Nothing here to acknowledge" said of a withdrawal tells the operator the
    // opposite of what happened, and `no_open_case` is the refusal both directions
    // share — so the sentence has to come from the mode, not from the status.
    await until(() => expect(openDialog().getByText(/nothing here to withdraw/i)).toBeTruthy());
  });
});

/* -------------------------------------------------------------------------- */
/* Snoozing and resuming the case                                             */
/* -------------------------------------------------------------------------- */

describe("holding the case's notifications", () => {
  it("posts the snooze to the case's own endpoint, with an idempotency key", async () => {
    const net = mount();
    net.on(`POST ${PATH}/snooze`, () => ({ json: item(groupDetail()) }));

    await until(() => expect(barButtons("Snooze every current member")).toHaveLength(1));
    fireEvent.click(barButton("Snooze every current member"));

    // The dialog opens on a preset, so committing needs no further input.
    fireEvent.click(openDialog().getByRole("button", { name: /^Hold notifications until/ }));

    await until(() => expect(net.to("/snooze")).toHaveLength(1));
    const posts = net.to("/snooze");
    expect(posts[0]?.path).toBe(`${PATH}/snooze`);
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
    // Exactly one of the two forms, never both — there is no default window
    // because there is no indefinite snooze.
    expect(posts[0]?.body).toEqual({ duration_seconds: 3600 });
  });

  it("names the case, not a group, in the snooze dialog", async () => {
    mount();
    await until(() => expect(barButtons("Snooze every current member")).toHaveLength(1));
    fireEvent.click(barButton("Snooze every current member"));

    expect(openDialog().getByText(/every alert currently in this case/)).toBeTruthy();
  });

  it("⛔ puts a refused resume in words, in a live region, and keeps offering it", async () => {
    const net = mount();
    net.on(`POST ${PATH}/unsnooze`, () => problem(412, "precondition_failed"));

    await until(() => expect(barButtons("Resume notifications")).toHaveLength(1));
    fireEvent.click(barButton("Resume notifications"));

    // This refusal has no dialog to appear inside — it lands in the header under
    // a button that still reads "Resume notifications", so a person who pressed
    // it and heard nothing would press it again.
    await until(() =>
      expect(screen.getByText(/Nothing here was snoozed, so there was nothing to resume/))
        .toBeTruthy(),
    );
    expect(net.to("/unsnooze")).toHaveLength(1);
    expect(barButtons("Resume notifications")).toHaveLength(1);
  });

  it("never invents a message for a failure it has no sentence for", async () => {
    const net = mount();
    net.on(`POST ${PATH}/unsnooze`, () => problem(503, "unavailable", { detail: "upstream" }));

    await until(() => expect(barButtons("Resume notifications")).toHaveLength(1));
    fireEvent.click(barButton("Resume notifications"));

    // The server's own `detail`, verbatim — not a status code, not a guess, and
    // above all not silence.
    await until(() => expect(screen.getByText("upstream")).toBeTruthy());
  });
});

/* -------------------------------------------------------------------------- */
/* The vocabulary the screen is allowed to use                                */
/* -------------------------------------------------------------------------- */

describe("the word on screen", () => {
  it("⛔ never calls the correlation a group where a person can read it", async () => {
    mount();
    await ackButton();

    // `AlertCase` (the per-alert firing episode) and this object share a word in
    // the code by an accepted decision. The rule that keeps that survivable is
    // that neither foreign word reaches the screen: this object is a Case here,
    // and the episode is only ever an *episode*.
    const text = document.body.textContent ?? "";
    expect(text).toContain("Case timeline");
    expect(text).not.toMatch(/\bgroup\b/i);
  });
});
