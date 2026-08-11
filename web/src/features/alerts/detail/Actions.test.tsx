/**
 * Acknowledge, withdraw, snooze and resume — and what the screen says when the
 * server says no.
 *
 * ⛔ THE ROLLBACK TESTS ARE THE POINT OF THIS FILE. A UI that shows an alert as
 * acknowledged when the server refused the ack is the same lie as a chat message
 * that says "delivered" when nothing was delivered: the person walks away
 * believing a receipt exists, and the next person to look sees a receipt that
 * nobody wrote. So every refusal below is checked twice — that the failure is
 * *shown*, and that the screen has not moved on as if it had succeeded.
 *
 * The alert on screen is read back out of the query cache rather than held in a
 * prop, because that is where the lie would live. A refused mutation that still
 * invalidated `["alerts"]` would repaint the row from the server and self-heal;
 * one that patched the cache optimistically and never rolled back would not, and
 * only a test that renders from the cache can tell those apart.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { useQuery } from "@tanstack/solid-query";
import { describe, expect, it } from "vitest";
import * as v from "valibot";

import { AlertActions } from "./Actions";
import { getAlert } from "~/api/endpoints";
import { qk } from "~/api/keys";
import { AckRequestSchema, CommentRequestSchema } from "~/api/generated/validators";
import type { AlertDetail } from "~/api/types";
import { requestMaxLength } from "~/test/contract";
import { alertDetail, occurrence, snooze } from "~/test/fixtures";
import {
  expectNoUndefined,
  item,
  problem,
  renderScreen,
  stubFetch,
  until,
  validationFailed,
  type FetchStub,
} from "~/test/harness";

const ID = "8b1f0d38-6ae4-4f2d-9d3f-1f6b1f0d38ae";
const PATH = `/api/v1/alerts/${ID}`;

/**
 * Render the action bar over the alert the *server* currently says exists.
 *
 * `served` is the server's copy. A test flips it only when the server would
 * really have changed, so "the screen refetched and agreed" and "the screen
 * decided on its own" are distinguishable outcomes.
 */
function mount(initial: AlertDetail): {
  readonly net: FetchStub;
  readonly serve: (next: AlertDetail) => void;
} {
  let served = initial;
  const net = stubFetch({ [`GET ${PATH}`]: () => ({ json: item(served) }) });

  const Screen = () => {
    const alert = useQuery(() => ({
      queryKey: qk.alerts.detail(ID),
      queryFn: ({ signal }: { signal: AbortSignal }) => getAlert(ID, { signal }),
    }));
    return (
      <>
        {alert.data ? <AlertActions alert={alert.data} /> : <p>loading</p>}
      </>
    );
  };

  renderScreen(() => <Screen />);
  return {
    net,
    serve: (next) => {
      served = next;
    },
  };
}

const firing = (patch: Partial<AlertDetail> = {}): AlertDetail =>
  alertDetail({ id: ID, ack_state: "unacked", ...patch });

/**
 * Queries scoped to the action bar rather than to the document.
 *
 * A closed `<dialog>` is still in the DOM, and its submit button shares its verb
 * with the bar button that opened it — so an unscoped `getByRole("button",
 * {name: "Acknowledge"})` is ambiguous by construction. Scoping is not a
 * workaround here: "what does the bar offer" is precisely the question every
 * rollback assertion below is asking.
 */
function barButtons(name: string | RegExp): readonly HTMLElement[] {
  return screen.queryAllByRole("button", { name }).filter((el) => el.closest("dialog") === null);
}

function barButton(name: string): HTMLElement {
  const found = barButtons(name);
  expect(found, `the action bar has no \`${name}\` button`).toHaveLength(1);
  return found[0]!;
}

/** The one dialog currently open, which is where every form control lives. */
function openDialog(): ReturnType<typeof within> {
  const dialog = document.querySelector("dialog[open]");
  expect(dialog, "no dialog is open").not.toBeNull();
  return within(dialog as HTMLElement);
}

/** The button that starts an acknowledgement, once the query has settled. */
async function ackButton(): Promise<HTMLElement> {
  await until(() => expect(barButtons("Acknowledge")).toHaveLength(1));
  return barButton("Acknowledge");
}

/* -------------------------------------------------------------------------- */
/* Acknowledging                                                              */
/* -------------------------------------------------------------------------- */

describe("acknowledging", () => {
  it("posts the note once, with one idempotency key, and shows the receipt afterwards", async () => {
    const net = mount(firing());
    net.net.on(`POST ${PATH}/ack`, () => ({ json: item(occurrence({ ack_state: "acked" })) }));

    fireEvent.click(await ackButton());
    fireEvent.input(openDialog().getByLabelText("Note (optional)"), {
      target: { value: "  known deploy, rolling back  " },
    });
    // The server agrees only because it accepted; the screen must refetch to
    // learn that, not assume it.
    net.serve(firing({ ack_state: "acked" }));
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() => expect(barButtons("Withdraw acknowledgement")).toHaveLength(1));

    const posts = net.net.to("/ack");
    expect(posts).toHaveLength(1);
    // Trimmed, because a note is prose and trailing spaces are not part of it.
    expect(posts[0]?.body).toEqual({ note: "known deploy, rolling back" });
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
  });

  it("sends no note at all rather than an empty one", async () => {
    const net = mount(firing());
    net.net.on(`POST ${PATH}/ack`, () => ({ json: item(occurrence({ ack_state: "acked" })) }));

    fireEvent.click(await ackButton());
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() => expect(net.net.to("/ack")).toHaveLength(1));
    // `{}`, never `{note: ""}`: the contract has no "cleared" note and a blank
    // line on the timeline is forever.
    expect(net.net.to("/ack")[0]?.body).toEqual({});
  });

  it("⛔ does not show the alert as acknowledged when the server refused", async () => {
    const net = mount(firing());
    net.net.on(`POST ${PATH}/ack`, () => problem(500, "internal"));

    fireEvent.click(await ackButton());
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));
    await until(() => expect(net.net.to("/ack")).toHaveLength(1));

    // The refusal is visible…
    await until(() => expect(document.querySelector("dialog[open]")).not.toBeNull());
    // …and nothing anywhere claims the receipt exists. `Withdraw
    // acknowledgement` only ever renders for an acked alert, so its absence is
    // the assertion that the screen did not move.
    expect(barButtons("Withdraw acknowledgement")).toHaveLength(0);
    expect(barButtons("Acknowledge")).toHaveLength(1);
    // And the alert was never refetched as if something had changed.
    expect(net.net.calls.filter((c) => c.method === "GET" && c.path === PATH)).toHaveLength(1);
  });

  it("puts a 412 in words rather than leaving a bare status on screen", async () => {
    const net = mount(firing());
    net.net.on(`POST ${PATH}/ack`, () => problem(412, "precondition_failed"));

    fireEvent.click(await ackButton());
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() =>
      expect(openDialog().getByText(/This episode ended before the request landed/)).toBeTruthy(),
    );
    expect(barButtons("Withdraw acknowledgement")).toHaveLength(0);
  });

  it("lands a server violation on the control that caused it", async () => {
    const net = mount(firing());
    net.net.on(`POST ${PATH}/ack`, () =>
      validationFailed({ field: "note", code: "max_length", message: "That note is too long." }),
    );

    fireEvent.click(await ackButton());
    fireEvent.input(openDialog().getByLabelText("Note (optional)"), { target: { value: "x" } });
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));

    await until(() => expect(openDialog().getByText("That note is too long.")).toBeTruthy());
    const note = openDialog().getByLabelText("Note (optional)");
    expect(note.getAttribute("aria-invalid")).toBe("true");
  });

  it("refuses to acknowledge an episode that has already ended, and says why", async () => {
    mount(firing({ current_occurrence: occurrence({ ended_at: "2026-08-09T09:30:00.000Z" }) }));
    const button = await ackButton();
    expect(button).toBeDisabled();
    expect(button.getAttribute("title")).toMatch(/already ended/);
  });

  it("offers withdrawal, not a second acknowledgement, once the alert is acked", async () => {
    const net = mount(firing({ ack_state: "acked" }));
    net.net.on(`POST ${PATH}/unack`, () => ({ json: item(occurrence()) }));

    await until(() => expect(barButtons("Withdraw acknowledgement")).toHaveLength(1));
    expect(barButtons("Acknowledge")).toHaveLength(0);

    fireEvent.click(barButton("Withdraw acknowledgement"));
    net.serve(firing({ ack_state: "unacked" }));
    fireEvent.click(openDialog().getByRole("button", { name: "Withdraw" }));

    await until(() => expect(net.net.to("/unack")).toHaveLength(1));
    await until(() => expect(barButtons("Acknowledge")).toHaveLength(1));
  });
});

/* -------------------------------------------------------------------------- */
/* Resuming notifications                                                     */
/* -------------------------------------------------------------------------- */

describe("ending a snooze", () => {
  it("offers the control that ends the hold, never a second one that starts it", async () => {
    const net = mount(firing({ snooze: snooze() }));
    net.net.on(`POST ${PATH}/unsnooze`, () => ({ json: item(firing()) }));

    await until(() => expect(barButtons("Resume notifications")).toHaveLength(1));
    // §B.8.6: buttons are never no-ops. While a snooze holds, "Snooze" is gone.
    expect(barButtons("Snooze")).toHaveLength(0);

    net.serve(firing());
    fireEvent.click(barButton("Resume notifications"));
    await until(() => expect(barButtons("Snooze")).toHaveLength(1));
  });

  it("⛔ keeps showing the hold when the server refuses to end it", async () => {
    const net = mount(firing({ snooze: snooze() }));
    net.net.on(`POST ${PATH}/unsnooze`, () => problem(412, "precondition_failed"));

    await until(() => expect(barButtons("Resume notifications")).toHaveLength(1));
    fireEvent.click(barButton("Resume notifications"));

    await until(() => expect(screen.getByText(/not snoozed — it woke before/)).toBeTruthy());
    // The alert is still snoozed as far as anyone knows, so the control still
    // offers to end the hold rather than to start one.
    expect(barButtons("Resume notifications")).toHaveLength(1);
    expect(barButtons("Snooze")).toHaveLength(0);
    expectNoUndefined(document.body);
  });

  it("never invents a message for a failure it has no sentence for", async () => {
    const net = mount(firing({ snooze: snooze() }));
    net.net.on(`POST ${PATH}/unsnooze`, () => problem(503, "unavailable", { detail: "upstream" }));

    await until(() => expect(barButtons("Resume notifications")).toHaveLength(1));
    fireEvent.click(barButton("Resume notifications"));

    // The server's own `detail`, verbatim — not a status code, not a guess, and
    // above all not silence.
    await until(() => expect(screen.getByText("upstream")).toBeTruthy());
    expectNoUndefined(document.body);
  });
});

/* -------------------------------------------------------------------------- */
/* The bounds these forms enforce are the server's, not this file's            */
/* -------------------------------------------------------------------------- */

describe("the note bound", () => {
  it("caps the note at exactly the length the contract caps it at", async () => {
    mount(firing());
    fireEvent.click(await ackButton());

    const max = requestMaxLength(AckRequestSchema, "note");
    const note = openDialog().getByLabelText("Note (optional)") as HTMLTextAreaElement;
    // Derived, so widening `AckRequest.note` server-side fails here instead of
    // leaving a control that truncates what the server would have accepted.
    expect(note.maxLength).toBe(max);

    fireEvent.input(note, { target: { value: "x".repeat(max + 1) } });
    fireEvent.click(openDialog().getByRole("button", { name: "Acknowledge" }));
    // Refused locally: nothing reached the wire.
    expect(document.querySelector("dialog[open]")).not.toBeNull();
  });

  it("caps the comment box at the contract's length too, and refuses an empty one", async () => {
    // `Actions.tsx` writes `10_000` twice by hand — into `v.maxLength` and into
    // the control's `maxLength` attribute. Derived here, because a hand-copied
    // cap that is one digit out is a form that refuses what the server accepts
    // and nobody finds out until someone pastes a stack trace at 3am.
    const max = requestMaxLength(CommentRequestSchema, "body");

    mount(firing());
    await until(() => expect(barButtons("Comment")).toHaveLength(1));
    fireEvent.click(barButton("Comment"));

    const body = openDialog().getByLabelText(/^Comment/) as HTMLTextAreaElement;
    expect(body.maxLength).toBe(max);

    // And the contract's `minLength: 1` is enforced before anything is sent:
    // the timeline is the record, and a blank entry in it says nothing.
    expect(v.safeParse(CommentRequestSchema, { body: "" }).success).toBe(false);
    expect(openDialog().getByRole("button", { name: "Add comment" })).toBeDisabled();
  });
});
