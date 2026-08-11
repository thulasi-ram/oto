/**
 * The routing screen, checked against the server's vocabulary rather than its
 * own.
 *
 * ⛔ THIS IS THE SCREEN THE ISSUE IS NAMED AFTER. `PoliciesSection.tsx` holds
 * three hand-written copies of server enums — the reasons a policy may carry,
 * the labels for them, and the reasons a message may be suppressed — and a copy
 * is only ever as good as the day it was written. One of them had already lost
 * `snoozed`, so the dry run answered "Would not send — snoozed." with a wire
 * token where a sentence belongs.
 *
 * So nothing below re-types a list. Every expectation is derived from the
 * generated contract, which means a `NotificationReason` or a
 * `NotificationSuppressedReason` added server-side fails here rather than
 * quietly narrowing what an operator can express.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";
import * as v from "valibot";

import { PoliciesSection } from "./PoliciesSection";
import { CreatePolicyRequestSchema } from "~/api/generated/validators";
import type { NotificationSuppressedReason, PolicyPreview } from "~/api/types";
import { enumValues, requestMaxLength, requestRange } from "~/test/contract";
import { alert, channel, policy } from "~/test/fixtures";
import {
  expectNoUndefined,
  item,
  list,
  renderScreen,
  stubFetch,
  until,
  type FetchStub,
} from "~/test/harness";

const REASONS = enumValues("NotificationReason");
const SUPPRESSED = enumValues("NotificationSuppressedReason");

const POLICY_PATH = "/api/v1/notification-policies";
const EDIT_PATH = `${POLICY_PATH}/${policy().id}`;

function mount(): FetchStub {
  const net = stubFetch({
    "GET /api/v1/notification-policies": list([policy()]),
    "GET /api/v1/channels": list([channel()]),
    "GET /api/v1/alerts": list([alert()]),
    "GET /api/v1/labels": { json: { data: [], meta: { request_id: "r" } } },
    [`PATCH ${EDIT_PATH}`]: () => ({ json: item(policy()) }),
  });
  renderScreen(() => <PoliciesSection />);
  return net;
}

/** Open the editor for the seeded policy, where the vocabulary lives. */
async function openEditor(): Promise<ReturnType<typeof within>> {
  await until(() => expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  const dialog = document.querySelector("dialog[open]");
  expect(dialog, "the policy editor did not open").not.toBeNull();
  return within(dialog as HTMLElement);
}

/* -------------------------------------------------------------------------- */

describe("the reasons a policy may carry", () => {
  it("offers every reason the contract publishes, and each one in words", async () => {
    mount();
    const editor = await openEditor();

    // The "Simulating" select renders the same list the toggle group does, and
    // one `<option>` per reason is the cleanest place to count them.
    const simulating = editor.getByLabelText("Simulating") as HTMLSelectElement;
    expect(simulating.options).toHaveLength(REASONS.length);

    for (const option of Array.from(simulating.options)) {
      expect(option.value, "an option with no value").not.toBe("");
      expect(option.textContent?.trim(), `\`${option.value}\` has no label`).toBeTruthy();
    }
    // Derived: a reason the server adds must appear here, not in a `?? raw`.
    expect(Array.from(simulating.options).map((o) => o.value).sort()).toEqual([...REASONS].sort());
    expectNoUndefined(document.body);
  });

  it("lets an operator pick facts without the toggle group losing any of them", async () => {
    mount();
    const editor = await openEditor();
    const group = editor.getByRole("group", { name: "About these facts" });
    expect(within(group).getAllByRole("checkbox")).toHaveLength(REASONS.length);
  });
});

/* -------------------------------------------------------------------------- */
/* The bounds the editor enforces are the contract's, not this screen's        */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ THIS BLOCK IS THE ORIGINAL BUG'S SHAPE, WRITTEN DOWN.
 *
 * The priority box shipped with no `min`/`max` at all against a contract that
 * says 0–10000, the four list and text limits were unenforced, and the draft was
 * handed to `createPolicy` **raw** — so every one of those bounds was discovered
 * as a 422 with the operator's work still in the dialog.
 *
 * Every expectation below is derived from `CreatePolicyRequestSchema`, which is
 * generated from `api/openapi/openapi.yaml` by gate G4. Re-typing `10000` here
 * would produce a test that agrees with the screen, agrees with nothing else,
 * and passes forever while the contract moves underneath both.
 */
describe("the policy editor's bounds", () => {
  const PRIORITY = requestRange(CreatePolicyRequestSchema, "priority");
  const NAME_MAX = requestMaxLength(CreatePolicyRequestSchema, "name");

  function priorityBox(editor: ReturnType<typeof within>): HTMLInputElement {
    return editor.getByLabelText("Priority") as HTMLInputElement;
  }

  it("publishes the contract's priority range on the control itself", async () => {
    mount();
    const editor = await openEditor();
    const box = priorityBox(editor);

    // The attributes, so the browser's own stepper and validation agree with
    // the server rather than offering a range nobody enforces.
    expect(box.type).toBe("number");
    expect(Number(box.min), "priority has no `min`").toBe(PRIORITY.min);
    expect(Number(box.max), "priority has no `max`").toBe(PRIORITY.max);
  });

  it("caps the name at the contract's length", async () => {
    mount();
    const editor = await openEditor();
    const name = editor.getByLabelText(/^Name/) as HTMLInputElement;
    expect(name.maxLength).toBe(NAME_MAX);
  });

  it("⛔ refuses a priority past the ceiling locally, and says what the range is", async () => {
    const net = mount();
    const editor = await openEditor();

    fireEvent.input(priorityBox(editor), { target: { value: String(PRIORITY.max + 1) } });

    // The complaint states the range rather than "invalid", and it is announced:
    // the operator should not have to guess which end they fell off.
    const stated = new RegExp(`${PRIORITY.min}.{1,3}${PRIORITY.max}`);
    await until(() =>
      expect(
        editor.getAllByRole("alert").some((el: HTMLElement) => stated.test(el.textContent ?? "")),
        "no alert states the range oto accepts",
      ).toBe(true),
    );
    expect(editor.getByRole("button", { name: "Save" })).toBeDisabled();

    fireEvent.click(editor.getByRole("button", { name: "Save" }));
    expect(net.to(EDIT_PATH), "an out-of-range priority reached the wire").toHaveLength(0);
    expectNoUndefined(document.body);
  });

  it("refuses a priority below the floor locally too", async () => {
    const net = mount();
    const editor = await openEditor();

    fireEvent.input(priorityBox(editor), { target: { value: String(PRIORITY.min - 1) } });
    await until(() => expect(editor.getByRole("button", { name: "Save" })).toBeDisabled());

    fireEvent.click(editor.getByRole("button", { name: "Save" }));
    expect(net.to(EDIT_PATH)).toHaveLength(0);
  });

  it("refuses a blank name and a policy with nowhere to send", async () => {
    const net = mount();
    const editor = await openEditor();
    const save = (): HTMLElement => editor.getByRole("button", { name: "Save" });

    // The seeded policy is legal, so the disabling below is this edit's doing.
    expect(save()).not.toBeDisabled();

    fireEvent.input(editor.getByLabelText(/^Name/), { target: { value: "   " } });
    await until(() => expect(save()).toBeDisabled());

    fireEvent.input(editor.getByLabelText(/^Name/), { target: { value: "still routed" } });
    await until(() => expect(save()).not.toBeDisabled());

    // `channel_ids` has `minItems: 1`: a policy with nowhere to send records
    // every notification as suppressed, so the form will not build one.
    const group = editor.getByRole("group", { name: "Tell these channels" });
    fireEvent.click(within(group).getAllByRole("checkbox")[0] as HTMLElement);
    await until(() => expect(save()).toBeDisabled());

    fireEvent.click(save());
    expect(net.to(EDIT_PATH)).toHaveLength(0);
  });

  it("refuses a policy that communicates nothing", async () => {
    const net = mount();
    const editor = await openEditor();
    const facts = editor.getByRole("group", { name: "About these facts" });

    // `reasons` has `minItems: 1`. Untick everything the seeded policy carries.
    for (const box of within(facts).getAllByRole("checkbox")) {
      if ((box as HTMLInputElement).checked) fireEvent.click(box);
    }

    await until(() => expect(editor.getByRole("button", { name: "Save" })).toBeDisabled());
    fireEvent.click(editor.getByRole("button", { name: "Save" }));
    expect(net.to(EDIT_PATH)).toHaveLength(0);
  });

  it("⛔ sends only a body the generated request schema accepts", async () => {
    const net = mount();
    const editor = await openEditor();

    fireEvent.input(editor.getByLabelText(/^Name/), { target: { value: "critical → #sre" } });
    fireEvent.input(priorityBox(editor), { target: { value: String(PRIORITY.max) } });
    fireEvent.click(editor.getByRole("button", { name: "Save" }));

    await until(() => expect(net.to(EDIT_PATH)).toHaveLength(1));
    const sent = net.to(EDIT_PATH)[0]?.body;

    // The point of the gate: whatever left the browser is something the
    // contract's own schema would have accepted. A raw draft could not promise
    // this, and the 422 this screen is named after is exactly what it looks like
    // when it does not.
    const parsed = v.safeParse(CreatePolicyRequestSchema, sent);
    expect(parsed.success, JSON.stringify(parsed.issues?.map((i) => i.message))).toBe(true);
    expect((sent as { priority: number }).priority).toBe(PRIORITY.max);
    // And the name is trimmed on the way out, not stored with its whitespace.
    expect((sent as { name: string }).name).toBe("critical → #sre");
  });
});

/* -------------------------------------------------------------------------- */

describe("the dry run", () => {
  /** One preview whose results cover every suppression the contract can send. */
  function everySuppression(): PolicyPreview {
    return {
      matched: true,
      results: SUPPRESSED.map((reason, i) => ({
        policy_id: "5a1b7d8e-6f9c-4a1b-2d3e-4f5061728394",
        policy_name: "critical → #sre-alerts",
        channel_id: "4f0a6c7d-5e8b-4f0a-1c2d-3e4f50617283",
        channel_name: `#channel-${i}`,
        channel_type: "slack",
        mode: "new_thread",
        would_send: false,
        suppressed_reason: reason as NonNullable<NotificationSuppressedReason>,
      })),
      warnings: [],
    } as unknown as PolicyPreview;
  }

  it("⛔ puts every suppression the contract can return into words", async () => {
    const net = mount();
    net.on("POST /api/v1/notification-policies/preview", () => ({
      json: item(everySuppression()),
    }));

    const editor = await openEditor();
    fireEvent.change(editor.getByLabelText("Against this alert"), {
      target: { value: "8b1f0d38-6ae4-4f2d-9d3f-1f6b1f0d38ae" },
    });
    fireEvent.click(editor.getByRole("button", { name: "Preview" }));

    await until(() => expect(editor.getAllByText(/Would not send/)).toHaveLength(SUPPRESSED.length));

    const sentences = editor
      .getAllByText(/Would not send/)
      .map((p: HTMLElement) => p.textContent ?? "");
    for (const reason of SUPPRESSED) {
      // The token itself must never be the explanation. `snoozed` is the one
      // this map had lost, and it is the one an operator most needs said out
      // loud: a silence somebody chose, not one oto imposed.
      expect(
        sentences.some((s: string) => s.includes(`Would not send — ${reason}.`)),
        `\`${reason}\` is rendered as its raw wire value instead of a sentence`,
      ).toBe(false);
    }
    // And every row explains itself rather than trailing off.
    for (const s of sentences) expect(s).toMatch(/Would not send — .+\. It would still be recorded/);
    expectNoUndefined(document.body);
  });

  it("says so plainly when nothing would be sent at all", async () => {
    const net = mount();
    net.on("POST /api/v1/notification-policies/preview", () => ({
      json: item({ matched: false, results: [], warnings: [] } as unknown as PolicyPreview),
    }));

    const editor = await openEditor();
    fireEvent.change(editor.getByLabelText("Against this alert"), {
      target: { value: "8b1f0d38-6ae4-4f2d-9d3f-1f6b1f0d38ae" },
    });
    fireEvent.click(editor.getByRole("button", { name: "Preview" }));

    await until(() => expect(editor.getByText(/it would go unreported/)).toBeTruthy());
  });
});
