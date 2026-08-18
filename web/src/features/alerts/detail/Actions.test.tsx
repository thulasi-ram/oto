/**
 * What the alert detail's action bar offers — and, just as much, what it no
 * longer offers.
 *
 * ⛔ ACKNOWLEDGE MOVED TO THE CASE, AND THE FIRST DESCRIBE BELOW IS THE GUARD
 * THAT IT STAYS THERE. A receipt belongs to ONE FIRING — an identity outlives
 * its firings, so "seen" written on the identity would go on being true about a
 * firing nobody has looked at — and the endpoint is addressed by case id for
 * exactly that reason. Its behaviour (the rollback assertions, the 412
 * sentences, the one-idempotency-key-per-gesture rule) lives in
 * `routes/case-detail.test.tsx`. What is asserted here is that this bar cannot
 * grow an alert-addressed ack back by accident.
 *
 * ⭐ SNOOZE **IS** ON THIS BAR, AND THE SECOND DESCRIBE IS THE GUARD THAT IT STAYS
 * HERE. A snooze holds oto's notifications for the IDENTITY until a fixed time, so
 * it outlives the firing it was taken from — which makes the alert the only screen
 * whose heading is its subject. `Resume` is deliberately NOT beside it: ending a
 * hold belongs to the Quiet tab of `/alerts`, the list of everything oto is
 * currently not saying, so nothing can leave that list from a screen that never
 * showed it was on it.
 *
 * Comment stays, and so does the dialog-labelling suite: the wrong-label bug this
 * file was written around was a property of several dialogs sharing one document,
 * and the comment dialog is still one of them.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { useQuery } from "@tanstack/solid-query";
import { describe, expect, it } from "vitest";
import * as v from "valibot";

import { AlertActions } from "./Actions";
import { getAlert } from "~/api/endpoints";
import { qk } from "~/api/keys";
import { CommentRequestSchema } from "~/api/generated/validators";
import type { AlertDetail } from "~/api/types";
import { requestMaxLength } from "~/test/contract";
import { alertDetail, alertCase, snooze } from "~/test/fixtures";
import { item, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const ID = "8b1f0d38-6ae4-4f2d-9d3f-1f6b1f0d38ae";
const PATH = `/api/v1/alerts/${ID}`;

/**
 * Render the action bar over the alert the *server* currently says exists.
 *
 * The alert on screen is read back out of the query cache rather than held in a
 * prop, because that is where a lie about a mutation would live.
 */
function mount(initial: AlertDetail): { readonly net: FetchStub } {
  const served = initial;
  const net = stubFetch({ [`GET ${PATH}`]: () => ({ json: item(served) }) });

  const Screen = () => {
    const alert = useQuery(() => ({
      queryKey: qk.alerts.detail(ID),
      queryFn: ({ signal }: { signal: AbortSignal }) => getAlert(ID, { signal }),
    }));
    return <>{alert.data ? <AlertActions alert={alert.data} /> : <p>loading</p>}</>;
  };

  renderScreen(() => <Screen />);
  return { net };
}

/**
 * ⭐ ACK IS PATCHED ON THE EPISODE, NOT ON THE ALERT. `current_case` is one
 * firing episode and `alerts` carries no ack column, so passing `acked` here is
 * how a test says "this episode has a receipt on it".
 */
const firing = (patch: Partial<AlertDetail> = {}): AlertDetail =>
  alertDetail({ id: ID, current_case: alertCase({ ack_state: "unacked" }), ...patch });

/** Queries scoped to the action bar rather than to the document. */
function barButtons(name: string | RegExp): readonly HTMLElement[] {
  return screen
    .queryAllByRole("button", { name })
    .filter((el) => el.closest('[role="dialog"]') === null);
}

function barButton(name: string): HTMLElement {
  const found = barButtons(name);
  expect(found, `the action bar has no \`${name}\` button`).toHaveLength(1);
  return found[0]!;
}

function openDialogEl(): HTMLElement {
  const dialog = document.querySelector('[role="dialog"]');
  expect(dialog, "no dialog is open").not.toBeNull();
  return dialog as HTMLElement;
}

function openDialog(): ReturnType<typeof within> {
  return within(openDialogEl());
}

async function commentButton(): Promise<HTMLElement> {
  await until(() => expect(barButtons("Comment")).toHaveLength(1));
  return barButton("Comment");
}

/* -------------------------------------------------------------------------- */
/* What the bar deliberately does not offer                                   */
/* -------------------------------------------------------------------------- */

describe("the controls that belong to the case, not to one alert", () => {
  it("offers no acknowledgement here, whatever state the episode is in", async () => {
    mount(firing());
    await commentButton();

    // Neither half of the old toggle, and not for an acked episode either — the
    // receipt is written on the case now.
    expect(barButtons("Acknowledge")).toHaveLength(0);
    expect(barButtons("Withdraw acknowledgement")).toHaveLength(0);
  });

  it("offers no acknowledgement for an already-acked episode either", async () => {
    mount(firing({ current_case: alertCase({ ack_state: "acked" }) }));
    await commentButton();

    expect(barButtons("Withdraw acknowledgement")).toHaveLength(0);
    expect(barButtons("Acknowledge")).toHaveLength(0);
  });

  it("offers no resume, even while a hold is running", async () => {
    mount(firing({ snooze: snooze() }));
    await commentButton();

    // ⛔ THE WAKE CONTROL IS NOT HERE, AND IT IS NOT AN OVERSIGHT. Resuming is
    // offered from the Quiet tab of `/alerts`, where the whole set of held-back
    // alerts is on screen; a button here would let one leave that list from a
    // screen that never showed it was on it.
    expect(barButtons("Resume notifications")).toHaveLength(0);
    expect(barButtons("Resume")).toHaveLength(0);
  });

  it("leaves Comment and Snooze as the only controls on the bar", async () => {
    mount(firing({ snooze: snooze() }));
    await commentButton();

    expect(barButtons(/.*/)).toHaveLength(2);
  });
});

/* -------------------------------------------------------------------------- */
/* The snooze, whose subject is this alert                                    */
/* -------------------------------------------------------------------------- */

describe("holding this alert's notifications", () => {
  async function snoozeButton(): Promise<HTMLElement> {
    await until(() => expect(barButtons("Snooze")).toHaveLength(1));
    return barButton("Snooze");
  }

  it("posts the snooze to THIS alert, with one idempotency key and one window", async () => {
    const { net } = mount(firing());
    net.on(`POST ${PATH}/snooze`, () => ({ json: item(firing()) }));

    fireEvent.click(await snoozeButton());
    // The dialog opens on a preset, so committing needs no further input.
    fireEvent.click(openDialog().getByRole("button", { name: /^Hold notifications until/ }));

    await until(() => expect(net.to("/snooze")).toHaveLength(1));
    const posts = net.to("/snooze");
    expect(posts[0]?.path).toBe(`${PATH}/snooze`);
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
    // Exactly one of the two forms, never both — there is no indefinite snooze.
    expect(posts[0]?.body).toEqual({ duration_seconds: 3600 });
  });

  it("⭐ says the hold is on the alert and outlasts whichever firing it came from", async () => {
    mount(firing());
    fireEvent.click(await snoozeButton());
    expect(
      openDialog().getByText(/outlasts whichever firing you took it from/i),
      "the dialog does not say whose quiet period this is",
    ).toBeTruthy();
  });

  it("keeps offering the snooze while one is already running, because it is not a no-op", async () => {
    // §B.8.6: the contract closes the incumbent hold and opens a new window, so
    // the control has something to do and stays live rather than going inert.
    mount(firing({ snooze: snooze() }));
    expect(await snoozeButton()).not.toBeDisabled();
  });
});

/* -------------------------------------------------------------------------- */
/* What the open dialog is announced as                                       */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ THIS BAR IS WHERE THE WRONG-LABEL BUG ONCE LIVED. Back when ack, comment and
 * snooze were built on `Dialog.tsx`'s native `<dialog>` — which has to stay
 * mounted whether or not it is open, since the platform's `showModal()`/`close()`
 * need an element to act on — all three existed in the document from the first
 * paint. While the label ids were the constants `oto-dialog-title` and
 * `oto-dialog-desc`, all three shared them, and IDREF resolution takes the FIRST
 * match in document order. So the comment form's `aria-labelledby` resolved to
 * the *ack* dialog's heading and a screen reader opened it with "Acknowledge this
 * alert". A wrong label is worse than a missing one: the operator acts on it.
 *
 * `Modal` (Kobalte) only renders a `[role="dialog"]` node for an instance whose
 * own `open` is true, so a single dialog no longer has any unopened sibling to
 * collide with. The check that survives the move is that the name comes from
 * INSIDE the open dialog — which is the assertion that would have failed then and
 * is still the one worth keeping.
 */
describe("the dialog a screen reader is told about", () => {
  /** Resolve an IDREF the way a screen reader does, and say where it landed. */
  const target = (dialog: HTMLElement, attr: string): HTMLElement => {
    const id = dialog.getAttribute(attr);
    expect(id, `the open dialog has no ${attr}`).toBeTruthy();
    const el = document.getElementById(id as string);
    expect(el, `${attr} points at #${id as string}, which is not in the document`).not.toBeNull();
    expect(
      dialog.contains(el),
      `${attr} resolves to an element outside the open dialog`,
    ).toBe(true);
    return el as HTMLElement;
  };

  it("names the comment dialog with its own heading, not the first one in the document", async () => {
    mount(firing());
    fireEvent.click(await commentButton());

    const dialog = openDialogEl();
    expect(target(dialog, "aria-labelledby").textContent).toBe("Add a comment");
    expect(target(dialog, "aria-describedby").textContent).toMatch(/Comments are events like any/);
  });
});

/* -------------------------------------------------------------------------- */
/* The bounds this form enforces are the server's, not this file's             */
/* -------------------------------------------------------------------------- */

describe("the comment bound", () => {
  it("caps the comment box at the contract's length, and refuses an empty one", async () => {
    // Derived, because a hand-copied cap that is one digit out is a form that
    // refuses what the server accepts and nobody finds out until someone pastes
    // a stack trace at 3am.
    const max = requestMaxLength(CommentRequestSchema, "body");

    mount(firing());
    fireEvent.click(await commentButton());

    const body = openDialog().getByLabelText(/^Comment/) as HTMLTextAreaElement;
    expect(body.maxLength).toBe(max);

    // And the contract's `minLength: 1` is enforced before anything is sent:
    // the timeline is the record, and a blank entry in it says nothing.
    expect(v.safeParse(CommentRequestSchema, { body: "" }).success).toBe(false);
    expect(openDialog().getByRole("button", { name: "Add comment" })).toBeDisabled();
  });

  it("posts the comment once, with an idempotency key", async () => {
    const net = mount(firing());
    net.net.on(`POST ${PATH}/comments`, () => ({ json: item({ id: "e1" }) }));

    fireEvent.click(await commentButton());
    fireEvent.input(openDialog().getByLabelText(/^Comment/), {
      target: { value: "rolled back the deploy" },
    });
    fireEvent.click(openDialog().getByRole("button", { name: "Add comment" }));

    await until(() => expect(net.net.to("/comments")).toHaveLength(1));
    const posts = net.net.to("/comments");
    expect(posts[0]?.body).toEqual({ body: "rolled back the deploy" });
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
  });
});
