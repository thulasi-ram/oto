/**
 * What the alert detail's action bar offers — and, just as much, what it no
 * longer offers.
 *
 * ⛔ ACKNOWLEDGE AND SNOOZE MOVED TO THE CASE, AND THE FIRST DESCRIBE BELOW IS
 * THE GUARD THAT THEY STAY THERE. Their behaviour — the rollback assertions, the
 * 412 sentences, the one-idempotency-key-per-gesture rule — did not go away; it
 * lives in `routes/case-detail.test.tsx` against the case-scoped endpoints. What
 * is asserted here is that this bar cannot grow them back by accident, because
 * an Acknowledge button on one alert is how thirty-nine of forty members end up
 * unacknowledged while the channel reads "a human has seen this".
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

  it("offers no snooze, and no resume even while a hold is running", async () => {
    mount(firing({ snooze: snooze() }));
    await commentButton();

    // ⛔ `Resume notifications` is the one that matters. A per-alert wake control
    // beside a case-wide snooze would let an operator quietly reverse one member
    // of a decision they made about the whole case, and nothing on the case
    // screen would say so.
    expect(barButtons("Snooze")).toHaveLength(0);
    expect(barButtons("Resume notifications")).toHaveLength(0);
  });

  it("leaves Comment as the only control on the bar", async () => {
    mount(firing({ snooze: snooze() }));
    await commentButton();

    expect(barButtons(/.*/)).toHaveLength(1);
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
