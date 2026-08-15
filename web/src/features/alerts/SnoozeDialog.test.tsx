/**
 * oto's quiet button, and the two ways a quiet button goes wrong.
 *
 * The first is a snooze that never ends, which is a mute; §B.8 forbids one and
 * the window below is checked against the contract's own `duration_seconds`
 * range rather than against the constants the dialog happens to export.
 *
 * ⛔ The second is a snooze the server refused that the screen shows as held.
 * The dialog's whole job on failure is to stay open, name the refusal and call
 * nothing back — because `onSuccess` is what repaints the alert as snoozed, and
 * a snoozed-looking alert whose notifications are still flowing is a promise of
 * quiet that oto did not make.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it, vi } from "vitest";

import { SNOOZE_MAX_SECONDS, SNOOZE_MIN_SECONDS, SNOOZE_PRESETS, SnoozeDialog } from "./SnoozeDialog";
import { snoozeAlert } from "~/api/endpoints";
import { SnoozeRequestSchema } from "~/api/generated/validators";
import type { SnoozeRequest } from "~/api/types";
import { requestMaxLength, requestRange } from "~/test/contract";
import { alertDetail } from "~/test/fixtures";
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
const PATH = `/api/v1/alerts/${ID}/snooze`;

/** The window the *server* enforces. Everything below is measured against it. */
const WINDOW = requestRange(SnoozeRequestSchema, "duration_seconds");

function mount(): {
  readonly net: FetchStub;
  readonly onSuccess: ReturnType<typeof vi.fn>;
  readonly onClose: ReturnType<typeof vi.fn>;
} {
  const onSuccess = vi.fn();
  const onClose = vi.fn();
  const net = stubFetch({ [`POST ${PATH}`]: () => ({ json: item(alertDetail()) }) });

  renderScreen(() => (
    <SnoozeDialog
      open
      onClose={onClose}
      subject="alert"
      onSubmit={(body: SnoozeRequest, key: string) => snoozeAlert(ID, body, key)}
      onSuccess={onSuccess}
    />
  ));
  return { net, onSuccess, onClose };
}

function dialog(): ReturnType<typeof within> {
  const el = document.querySelector("dialog[open]");
  expect(el, "the snooze dialog is not open").not.toBeNull();
  return within(el as HTMLElement);
}

function submit(): void {
  fireEvent.click(dialog().getByRole("button", { name: /^Hold notifications until/ }));
}

function minutesBox(): HTMLInputElement {
  return dialog().getByLabelText("Or a custom number of minutes") as HTMLInputElement;
}

/* -------------------------------------------------------------------------- */
/* The window is the contract's, not this file's                              */
/* -------------------------------------------------------------------------- */

describe("the snooze window", () => {
  it("is exactly the window the contract enforces", () => {
    // The dialog exports its own constants and writes them into `min`/`max` and
    // into three sentences. Derived here so that moving `duration_seconds`
    // server-side fails the build's tests rather than leaving a form that
    // refuses what the server would take — or offers what it would not.
    expect(SNOOZE_MIN_SECONDS).toBe(WINDOW.min);
    expect(SNOOZE_MAX_SECONDS).toBe(WINDOW.max);
  });

  it("offers no preset the server would refuse, and no indefinite one", () => {
    expect(SNOOZE_PRESETS.length).toBeGreaterThan(0);
    for (const preset of SNOOZE_PRESETS) {
      expect(preset.seconds).toBeGreaterThanOrEqual(WINDOW.min);
      expect(preset.seconds).toBeLessThanOrEqual(WINDOW.max);
      // Every option ends. There is no "until I say so" anywhere in the form.
      expect(Number.isFinite(preset.seconds)).toBe(true);
    }
    mount();
    // And the one submit control names the instant it ends, so there is no
    // reading of this form under which notifications simply stop.
    expect(dialog().getByRole("button", { name: /^Hold notifications until \d/ })).toBeTruthy();
  });

  it("bounds the minutes box with the contract's numbers, in minutes", () => {
    mount();
    const box = minutesBox();
    expect(Number(box.min)).toBe(WINDOW.min / 60);
    expect(Number(box.max)).toBe(WINDOW.max / 60);
  });

  it("caps the note where the contract caps it", () => {
    mount();
    const note = dialog().getByLabelText("Note (optional)") as HTMLTextAreaElement;
    expect(note.maxLength).toBe(requestMaxLength(SnoozeRequestSchema, "note"));
  });
});

/* -------------------------------------------------------------------------- */
/* What actually reaches the wire                                             */
/* -------------------------------------------------------------------------- */

describe("the request it builds", () => {
  it("sends `duration_seconds` and only `duration_seconds`", async () => {
    const { net, onSuccess, onClose } = mount();
    const preset = SNOOZE_PRESETS[2]!;

    fireEvent.click(dialog().getByLabelText(preset.label));
    submit();

    await until(() => expect(net.to("/snooze")).toHaveLength(1));
    const call = net.to("/snooze")[0]!;
    // Exactly one of the two, because both is a 422 and neither is a 422.
    expect(call.body).toEqual({ duration_seconds: preset.seconds });
    expect(call.headers["Idempotency-Key"]).toBeTruthy();
    await until(() => expect(onSuccess).toHaveBeenCalledTimes(1));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("sends `until` and only `until` once an exact time is chosen", async () => {
    const { net } = mount();
    const at = new Date(Date.now() + 3 * 3_600_000);
    const local =
      `${at.getFullYear()}-${String(at.getMonth() + 1).padStart(2, "0")}` +
      `-${String(at.getDate()).padStart(2, "0")}T${String(at.getHours()).padStart(2, "0")}` +
      `:${String(at.getMinutes()).padStart(2, "0")}`;

    fireEvent.input(dialog().getByLabelText("Or until an exact time"), { target: { value: local } });
    submit();

    await until(() => expect(net.to("/snooze")).toHaveLength(1));
    const body = net.to("/snooze")[0]?.body as Record<string, unknown>;
    expect(Object.keys(body)).toEqual(["until"]);
    expect(new Date(String(body["until"])).getTime()).toBeGreaterThan(Date.now());
  });

  it("trims a note away rather than sending an empty one", async () => {
    const { net } = mount();
    fireEvent.input(dialog().getByLabelText("Note (optional)"), { target: { value: "   " } });
    submit();

    await until(() => expect(net.to("/snooze")).toHaveLength(1));
    expect(net.to("/snooze")[0]?.body).toEqual({ duration_seconds: 3600 });
  });

  it("refuses a window shorter than the contract's minimum without asking the server", async () => {
    const { net } = mount();
    fireEvent.input(minutesBox(), { target: { value: String(WINDOW.min / 60 - 1) } });
    submit();

    await until(() => expect(dialog().getByText(/shortest snooze is 5 minutes/)).toBeTruthy());
    expect(net.to("/snooze")).toHaveLength(0);
  });

  it("refuses a window longer than the contract's maximum, and says why there is one", async () => {
    const { net } = mount();
    fireEvent.input(minutesBox(), { target: { value: String(WINDOW.max / 60 + 1) } });
    submit();

    // The refusal explains itself: an unexpiring snooze is a mute.
    await until(() => expect(dialog().getByText(/longest snooze is 30 days/)).toBeTruthy());
    expect(dialog().getByText(/mutes are how channels go quiet forever/)).toBeTruthy();
    expect(net.to("/snooze")).toHaveLength(0);
  });
});

/* -------------------------------------------------------------------------- */
/* Refusals                                                                   */
/* -------------------------------------------------------------------------- */

describe("when the server says no", () => {
  it("⛔ tells nobody the alert is snoozed", async () => {
    const { net, onSuccess, onClose } = mount();
    net.on(`POST ${PATH}`, () => problem(500, "internal", { detail: "the database is unhappy" }));

    submit();
    await until(() => expect(net.to("/snooze")).toHaveLength(1));

    // `onSuccess` is what repaints the alert as held; `onClose` is what takes
    // the form away. Neither may run on a refusal, or the person is looking at
    // a quiet they were never granted.
    await until(() => expect(screen.getByText("the database is unhappy")).toBeTruthy());
    expect(onSuccess).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
    expect(document.querySelector("dialog[open]")).not.toBeNull();
    expectNoUndefined(document.body);
  });

  it("lands a 422 on the control that caused it, and keeps the form open", async () => {
    const { net, onSuccess } = mount();
    net.on(`POST ${PATH}`, () =>
      validationFailed({
        field: "duration_seconds",
        code: "maximum",
        message: "oto holds notifications for 30 days at most.",
      }),
    );

    submit();
    await until(() =>
      expect(dialog().getByText("oto holds notifications for 30 days at most.")).toBeTruthy(),
    );
    expect(minutesBox().getAttribute("aria-invalid")).toBe("true");
    expect(onSuccess).not.toHaveBeenCalled();
  });

  it("surfaces a violation that landed on no control instead of swallowing it", async () => {
    const { net } = mount();
    net.on(`POST ${PATH}`, () =>
      validationFailed({
        field: "org_id",
        code: "forbidden",
        message: "this org may not snooze.",
      }),
    );

    submit();
    await until(() => expect(dialog().getByText(/org_id: this org may not snooze\./)).toBeTruthy());
  });

  it("puts a 412 in words, and does not pretend the request was malformed", async () => {
    const { net } = mount();
    net.on(`POST ${PATH}`, () => problem(412, "precondition_failed"));

    submit();
    await until(() =>
      expect(dialog().getByText(/The request itself was fine; the entity moved/)).toBeTruthy(),
    );
  });
});
