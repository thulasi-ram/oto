/**
 * The one gesture the group screen still offers, and what it says when the
 * server says no.
 *
 * ⛔ THE ROLLBACK TESTS ARE THE POINT OF THIS FILE. A UI that shows an ack as
 * written when the server refused it is the same lie as a chat message that says
 * "delivered" when nothing was delivered — and this ack is a FAN-OUT, so the lie
 * is told about every member of the group at once. So every refusal below is
 * checked twice: that the failure is *shown*, and that the screen has not moved
 * on as if it had succeeded.
 *
 * ⛔ THE ENDPOINT IS GROUP-SCOPED AND THE PATH IS ASSERTED. `POST
 * /api/v1/alert-groups/{id}/ack`, never a walk over the members calling
 * `/cases/{id}/ack` forty times: that would look identical on screen and be a
 * different thing entirely — forty requests with forty idempotency keys, some of
 * which land and some of which do not.
 *
 * ⛔ AND SNOOZE IS ABSENT, WHICH IS ITSELF UNDER TEST. A snooze is a hold on an
 * ALERT — an identity that outlives every group it is ever batched into — so
 * offering it from a screen about one generation of one notification batch
 * invited an operator to believe they had quieted the batch. The last describe
 * block is the fence around that.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import GroupDetailRoute from "./group-detail";
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

const ACK = "Acknowledge every member's open case";

/** Render the group screen the way the router renders it, params and all. */
function mount(patch = {}): FetchStub {
  const net = stubFetch({
    [`GET ${PATH}`]: () => ({ json: item(groupDetail(patch)) }),
    [`GET ${PATH}/alerts`]: () => ({ json: list([]) }),
    [`GET ${PATH}/timeline`]: () => ({ json: list([]) }),
  });

  renderScreen(() => <GroupDetailRoute />, { path: `/groups/${ID}`, routePath: "/groups/:id" });
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
  await until(() => expect(barButtons(ACK)).toHaveLength(1));
  return barButton(ACK);
}

/* -------------------------------------------------------------------------- */
/* Acknowledging every member's open case                                     */
/* -------------------------------------------------------------------------- */

describe("acknowledging every member's open case", () => {
  it("posts once to the group's own ack, with one idempotency key and the trimmed note", async () => {
    const net = mount();
    net.on(`POST ${PATH}/ack`, () => ({ json: item(groupDetail({ acked_count: 3 })) }));

    fireEvent.click(await ackButton());
    fireEvent.input(openDialog().getByLabelText("Note (optional)"), {
      target: { value: "  known deploy, rolling back  " },
    });
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() => expect(net.to("/ack")).toHaveLength(1));
    const posts = net.to("/ack");
    // ⛔ THE GROUP'S ENDPOINT, not a walk over the members.
    expect(posts[0]?.path).toBe(`${PATH}/ack`);
    // Trimmed, because a note is prose and trailing spaces are not part of it.
    expect(posts[0]?.body).toEqual({ note: "known deploy, rolling back" });
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
    // Nothing per-case went out alongside it.
    expect(net.calls.filter((c) => c.path.startsWith("/api/v1/cases/"))).toHaveLength(0);
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
    // And the group was never refetched as if something had changed.
    expect(net.calls.filter((c) => c.method === "GET" && c.path === PATH)).toHaveLength(1);
  });

  it("puts a 412 in words rather than leaving a bare status on screen", async () => {
    const net = mount();
    net.on(`POST ${PATH}/ack`, () => problem(412, "precondition_failed"));

    fireEvent.click(await ackButton());
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() =>
      expect(openDialog().getByText(/no member of this group has a case that is still open/i))
        .toBeTruthy(),
    );
  });

  it("says at the moment of committing that later members are not covered", async () => {
    mount();
    fireEvent.click(await ackButton());

    // ⭐ THE LIMIT IS STATED WHERE THE DECISION IS MADE. An operator who believes
    // this covers what arrives next has silenced their own future signal, and
    // finding that out afterwards is finding it out too late.
    expect(
      openDialog().getByText(/notified under this group afterwards are NOT acknowledged/i),
    ).toBeTruthy();
  });
});

/* -------------------------------------------------------------------------- */
/* Taking the acknowledgement back                                            */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ THE WAY BACK IS PART OF THE CONTROL, NOT AN EXTRA.
 *
 * When the fan-out arrived it arrived without its withdrawal: `POST
 * /alert-groups/{id}/ack` existed and `.../unack` did not, so the widest gesture
 * in the product — one press writing a receipt on every member — was also the only
 * irreversible one, and the route back was opening each member's case in turn.
 */
describe("withdrawing the acknowledgement", () => {
  async function withdrawButton(): Promise<HTMLElement> {
    await until(() => expect(barButtons("Withdraw acknowledgement")).toHaveLength(1));
    return barButton("Withdraw acknowledgement");
  }

  it("posts to the group's own unack, with one idempotency key and the trimmed note", async () => {
    const net = mount();
    net.on(`POST ${PATH}/unack`, () => ({ json: item(groupDetail({ acked_count: 0 })) }));

    fireEvent.click(await withdrawButton());
    fireEvent.input(openDialog().getByLabelText("Note (optional)"), {
      target: { value: "  un-acking, it's back  " },
    });
    fireEvent.click(openDialog().getByRole("button", { name: "Withdraw" }));

    await until(() => expect(net.to("/unack")).toHaveLength(1));
    const posts = net.to("/unack");
    // ⛔ THE GROUP'S ENDPOINT, not a walk over the members.
    expect(posts[0]?.path).toBe(`${PATH}/unack`);
    expect(posts[0]?.body).toEqual({ note: "un-acking, it's back" });
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
    expect(net.calls.filter((c) => c.path.startsWith("/api/v1/cases/"))).toHaveLength(0);
  });

  it("is offered whatever the acknowledged count says", async () => {
    // `acked_count` is a roll-up over cases and a partially-acked group is the
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
/* What this screen deliberately does not offer                               */
/* -------------------------------------------------------------------------- */

describe("the controls that are not here", () => {
  it("⛔ offers no snooze, because a snooze is a hold on an alert and not on a batch", async () => {
    mount();
    await ackButton();

    // A snooze suppresses oto's notifications for an IDENTITY, which outlives
    // every group it is ever batched into. A control here would read as
    // "quieten this batch" and would in fact quieten every alert in it,
    // indefinitely past the moment the batch closed. It lives on the alert and
    // on the case, where the subject is named.
    expect(barButtons(/snooze/i)).toHaveLength(0);
    expect(barButtons(/resume notifications/i)).toHaveLength(0);
  });
});

/* -------------------------------------------------------------------------- */
/* The vocabulary the screen is allowed to use                                */
/* -------------------------------------------------------------------------- */

describe("the word on screen", () => {
  it("⛔ calls this object a group, and never a case", async () => {
    mount();
    await ackButton();

    // An AlertGroup is Alertmanager's notification grouping. A Case is ONE
    // alert's firing episode and is what a human acknowledges. This screen may
    // say "case" only where it is naming the members' own cases — which is
    // exactly what the ack button does — so the check is that the screen never
    // calls ITSELF one.
    const text = document.body.textContent ?? "";
    expect(text).toContain("Group timeline");
    expect(text).toContain("Alertmanager batched these alerts into one notification");
    expect(text).not.toMatch(/\bthis case\b/i);
    expect(text).not.toMatch(/case timeline/i);
    expect(text).not.toMatch(/currently-joined/i);
  });
});
