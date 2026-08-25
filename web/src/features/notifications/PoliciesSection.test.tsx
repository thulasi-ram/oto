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
import { CreatePolicyRequestSchema, UpdatePolicyRequestSchema } from "~/api/generated/validators";
import type { NotificationSuppressedReason, PolicyPreview } from "~/api/types";
import { enumValues, requestMaxLength, requestOptions, requestRange } from "~/test/contract";
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
  // `PolicyDialog` is now `Modal` (Kobalte), not the native-`<dialog>`-based
  // `Dialog.tsx` — presence-based `ModalContent` renders a `[role="dialog"]`
  // node rather than a `<dialog open>` one (see `Actions.test.tsx`'s own
  // `openDialogEl` for the same swap).
  const dialog = document.querySelector('[role="dialog"]');
  expect(dialog, "the policy editor did not open").not.toBeNull();
  return within(dialog as HTMLElement);
}

/**
 * Opens a `Select`'s real, accessible listbox — the way a keyboard user opens
 * it (`SelectTrigger`'s own `onKeyDown` treats `ArrowDown` as "open, focus
 * first") — rather than reaching past the visible trigger into
 * `SelectHiddenSelect`'s `aria-hidden` native shim. `SelectContent` renders
 * through a `Portal`, so its options are not descendants of the dialog
 * element and have to be found from `screen`, not from `editor`; and it is
 * presence-gated the same way `Modal` is, so the options are awaited rather
 * than assumed to exist the instant the trigger opens.
 */
async function openListbox(trigger: HTMLElement): Promise<void> {
  fireEvent.keyDown(trigger, { key: "ArrowDown" });
  await until(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
}

/**
 * Picks the one episode the dry-run picker was seeded with.
 *
 * `PolicyPreviewPanel` lives inside the Modal's presence-gated content, so
 * `recentAlertsQuery()` only starts fetching once the dialog actually opens —
 * and the trigger stays honestly disabled (with a "Loading…" placeholder)
 * until that query resolves. A test driving the real trigger has to wait for
 * it exactly as an operator would, rather than reaching past it.
 *
 * ⛔ THE LABEL IS "Against this episode" AND THE ROW IS ONLY OFFERED BECAUSE IT
 * HAS AN OPEN CASE. The preview endpoint takes a `case_id` and nothing else
 * (git-bug 7570090), so the picker drops any recent alert whose `current_case`
 * is absent — `alert()` carries one, which is what keeps this seed pickable.
 */
async function pickTheRecentAlert(editor: ReturnType<typeof within>): Promise<void> {
  const trigger = editor.getByLabelText("Against this episode");
  await until(() => expect(trigger).not.toBeDisabled());
  await openListbox(trigger);
  fireEvent.click(screen.getAllByRole("option")[0]!);
}

/* -------------------------------------------------------------------------- */

describe("the reasons a policy may carry", () => {
  it("offers every reason the contract publishes, and each one in words", async () => {
    mount();
    const editor = await openEditor();

    // The "Simulating" select renders the same list the toggle group does.
    // It is Kobalte's real listbox now, not a native `<select>` — opened the
    // way a keyboard user opens it, with each `role="option"` counted rather
    // than an `HTMLSelectElement`'s `.options` (that is `SelectHiddenSelect`'s
    // job, and it is `aria-hidden` — not the surface under test here).
    await openListbox(editor.getByLabelText("Simulating"));
    const options = screen.getAllByRole("option");
    expect(options).toHaveLength(REASONS.length);

    for (const option of options) {
      const key = option.getAttribute("data-key");
      expect(key, "an option with no key").toBeTruthy();
      expect(option.textContent?.trim(), `\`${key ?? ""}\` has no label`).toBeTruthy();
    }
    // Derived: a reason the server adds must appear here, not in a `?? raw`.
    expect(options.map((o) => o.getAttribute("data-key")).sort()).toEqual([...REASONS].sort());
    expectNoUndefined(document.body);
  });

  it("lets an operator pick facts without the toggle group losing any of them", async () => {
    mount();
    const editor = await openEditor();
    const group = editor.getByRole("group", { name: "About these facts" });
    // `ToggleGroupItem` (Kobalte) renders a real `<button aria-pressed>`, not a
    // checkbox — see the "refuses a policy that communicates nothing" test
    // below for the same swap.
    expect(within(group).getAllByRole("button")).toHaveLength(REASONS.length);
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
    // every notification as suppressed, so the form will not build one. The
    // picker is a combobox now, so "untick" is "remove the chip" — and the chip
    // is the reason the control can be a search box at all: what a policy sends
    // to stays visible without opening anything.
    fireEvent.click(editor.getByRole("button", { name: `Do not tell ${channel().name}` }));
    await until(() => expect(save()).toBeDisabled());

    fireEvent.click(save());
    expect(net.to(EDIT_PATH)).toHaveLength(0);
  });

  it("refuses a policy that communicates nothing", async () => {
    const net = mount();
    const editor = await openEditor();
    const facts = editor.getByRole("group", { name: "About these facts" });

    // `reasons` has `minItems: 1`. Untick everything the seeded policy carries.
    // `ToggleGroupItem` (Kobalte) renders a real `<button aria-pressed>`, not a
    // checkbox — `.checked` has no meaning on it, so the pressed state is read
    // off `aria-pressed` instead.
    for (const box of within(facts).getAllByRole("button")) {
      if (box.getAttribute("aria-pressed") === "true") fireEvent.click(box);
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
    //
    // ⛔ THE **UPDATE** SCHEMA, BECAUSE THIS IS A PATCH. The two shapes were
    // interchangeable until `template_id` arrived, and they are not any more:
    // absent on a create means "oto's own card", while absent on a patch means
    // "leave it alone" — so clearing a template requires an explicit `null` that
    // the create schema rightly refuses. Asserting the create schema here would
    // pass by accident today and forbid the clear-a-template request tomorrow.
    const parsed = v.safeParse(UpdatePolicyRequestSchema, sent);
    expect(parsed.success, JSON.stringify(parsed.issues?.map((i) => i.message))).toBe(true);
    expect((sent as { priority: number }).priority).toBe(PRIORITY.max);
    // And the name is trimmed on the way out, not stored with its whitespace.
    expect((sent as { name: string }).name).toBe("critical → #sre");
  });
});

/* -------------------------------------------------------------------------- */
/* Where the policy sends                                                     */
/* -------------------------------------------------------------------------- */

/**
 * The picker is a combobox rather than a checkbox per channel, and the two
 * properties that trade has to keep are asserted here rather than assumed:
 * **what is already chosen is visible without opening anything**, and **typing
 * narrows the list**. A search box that hid the current selection would be a
 * worse control than the wall of checkboxes it replaced, however much shorter.
 */
describe("the channel picker", () => {
  const OTHER = channel({
    id: "6b2c8e9f-7a0d-4b2c-3e4f-506172839405",
    name: "#platform-alerts",
    type: "webhook",
  });

  function mountWithBoth(): void {
    stubFetch({
      "GET /api/v1/notification-policies": list([policy()]),
      "GET /api/v1/channels": list([channel(), OTHER]),
      "GET /api/v1/alerts": list([alert()]),
      "GET /api/v1/labels": { json: { data: [], meta: { request_id: "r" } } },
      [`PATCH ${EDIT_PATH}`]: () => ({ json: item(policy()) }),
    });
    renderScreen(() => <PoliciesSection />);
  }

  /**
   * The search box inside the control.
   *
   * By ROLE and not by `getByLabelText`: Kobalte points the label at the control
   * *and* at the input inside it, so the label alone matches two nodes — and the
   * one a person types into is the `combobox`.
   */
  function search(editor: ReturnType<typeof within>): HTMLElement {
    return editor.getByRole("combobox", { name: "Tell these channels" });
  }

  it("shows what the policy already sends to, without being opened", async () => {
    mountWithBoth();
    const editor = await openEditor();

    // The seeded policy carries exactly one channel, and its chip is on screen
    // with the listbox shut.
    expect(editor.getByText(channel().name)).toBeTruthy();
    expect(editor.queryByRole("listbox")).toBeNull();
  });

  it("narrows the list to what was typed", async () => {
    mountWithBoth();
    const editor = await openEditor();

    fireEvent.input(search(editor), { target: { value: "platform" } });

    // `ComboboxContent` portals (see `ui/Combobox.tsx`), so the options are
    // found from `screen` rather than from the dialog element.
    await until(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    expect(screen.getAllByRole("option")[0]?.textContent).toContain(OTHER.name);
  });

  it("searches the type as well as the name, because the row shows both", async () => {
    mountWithBoth();
    const editor = await openEditor();

    // A row reads `#platform-alerts webhook status_changes`. Filtering on the
    // name alone would render three words and find the option by only one.
    fireEvent.input(search(editor), { target: { value: "webhook" } });
    await until(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    expect(screen.getAllByRole("option")[0]?.textContent).toContain(OTHER.name);
  });

  it("adds the channel that was picked to the ones already there", async () => {
    const net = stubFetch({
      "GET /api/v1/notification-policies": list([policy()]),
      "GET /api/v1/channels": list([channel(), OTHER]),
      "GET /api/v1/alerts": list([alert()]),
      "GET /api/v1/labels": { json: { data: [], meta: { request_id: "r" } } },
      [`PATCH ${EDIT_PATH}`]: () => ({ json: item(policy()) }),
    });
    renderScreen(() => <PoliciesSection />);
    const editor = await openEditor();

    fireEvent.input(search(editor), { target: { value: "platform" } });
    await until(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    fireEvent.click(screen.getAllByRole("option")[0]!);

    fireEvent.click(editor.getByRole("button", { name: "Save" }));
    await until(() => expect(net.to(EDIT_PATH)).toHaveLength(1));

    // ⛔ ADDED TO, NOT REPLACED. A multiple-selection control that handed back
    // only the last click would quietly delete every other destination on the
    // policy — the most expensive silent edit this form could make.
    const sent = net.to(EDIT_PATH)[0]?.body as { channel_ids: readonly string[] };
    expect([...sent.channel_ids].sort()).toEqual([channel().id, OTHER.id].sort());
  });
});

/* -------------------------------------------------------------------------- */
/* The three axes that used to be reachable only through the API              */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ THIS BLOCK IS A SECOND SHAPE OF THE SAME BUG, WRITTEN DOWN.
 *
 * `subject_kinds` (migration `00072`), the count condition and the digest window
 * are policy fields with a wire name, a generated validator and a settings screen
 * that rendered none of them: an API-configured policy read as a plainer policy
 * than it was, and no sequence of clicks could set, change or remove any of the
 * three. Nothing was silently CLEARED — the create body omitted them and a PATCH
 * treats absent as "leave alone" — which is exactly why the omission was easy to
 * keep: the screen was lossy on the way in and inert on the way out.
 *
 * ⛔ AND THE TWO RULES A JSON SCHEMA CANNOT STATE WERE REACHABLE ONLY AS A 422
 * AFTER SAVE — a count must bind exactly `case`, a digest window must divide the
 * day — which is the failure this whole file is named after.
 *
 * ⚠️ THE DIALOG IS AUTHORITATIVE OVER ALL THREE NOW, and that is a real change of
 * behaviour rather than a widening. `subject_kinds` is always sent and a cleared
 * condition patches an explicit `null`, so a save from this screen OVERWRITES what
 * the API set. That is the only honest shape once the fields are on screen: a
 * dialog showing a digest removed and leaving it on the server is the worse bug.
 *
 * Everything below is derived. The altitudes come from the contract's own inline
 * picklist, the ranges from the generated request schema, and the misaligned
 * window is *computed* rather than picked, so a change to either end of the range
 * moves the test with it.
 */
describe("the altitude a policy is about", () => {
  const SUBJECT_KINDS = requestOptions(CreatePolicyRequestSchema, "subject_kinds");

  it("offers every altitude the contract publishes, each one in words", async () => {
    mount();
    const editor = await openEditor();
    const group = editor.getByRole("group", { name: "About which altitude" });

    const chips = within(group).getAllByRole("button");
    expect(chips).toHaveLength(SUBJECT_KINDS.length);
    for (const chip of chips) {
      expect(chip.textContent?.trim(), "an altitude with no words on it").toBeTruthy();
      // The wire token is never the label: `case` on a chip means nothing to
      // somebody who has not read `reason.go`.
      expect(SUBJECT_KINDS).not.toContain(chip.textContent?.trim());
    }
    expectNoUndefined(document.body);
  });

  it("⛔ sends the empty binding rather than dropping the field", async () => {
    const net = mount();
    const editor = await openEditor();

    fireEvent.click(editor.getByRole("button", { name: "Save" }));
    await until(() => expect(net.to(EDIT_PATH)).toHaveLength(1));

    // `subject_kinds` is the one new field with no `null` on the wire: the column
    // is NOT NULL DEFAULT '{}' and "claims every altitude" is an answer, not an
    // absence. A screen that omitted it when empty would make the create body and
    // the patch body disagree about a field that has nothing to disagree with.
    const sent = net.to(EDIT_PATH)[0]?.body as { subject_kinds?: readonly string[] };
    expect(sent.subject_kinds, "the binding was dropped from the request").toEqual([]);
  });

  it("carries a narrowed binding to the wire", async () => {
    const net = mount();
    const editor = await openEditor();
    const group = editor.getByRole("group", { name: "About which altitude" });

    const first = within(group).getAllByRole("button")[0]!;
    fireEvent.click(first);
    fireEvent.click(editor.getByRole("button", { name: "Save" }));

    await until(() => expect(net.to(EDIT_PATH)).toHaveLength(1));
    const sent = net.to(EDIT_PATH)[0]?.body as { subject_kinds: readonly string[] };
    expect(sent.subject_kinds).toHaveLength(1);
    expect(SUBJECT_KINDS).toContain(sent.subject_kinds[0]);

    const parsed = v.safeParse(UpdatePolicyRequestSchema, sent);
    expect(parsed.success, JSON.stringify(parsed.issues?.map((i) => i.message))).toBe(true);
  });
});

/* -------------------------------------------------------------------------- */

describe("the thresholds and windows", () => {
  const COUNT_MIN = requestRange(CreatePolicyRequestSchema, "count_min");
  const COUNT_WINDOW = requestRange(CreatePolicyRequestSchema, "count_window_seconds");
  const DIGEST_WINDOW = requestRange(CreatePolicyRequestSchema, "digest_window_seconds");

  const THROTTLE = "Send at most a fixed number per window";
  const COUNT = "Stay quiet until it has happened enough";
  const DIGEST = "Send one summary per window";

  /**
   * A window inside the contract's range that does NOT divide the day.
   *
   * Computed rather than picked: `301` is the answer today and would stop being
   * one the moment the floor moved, and a test that hardcodes it would then be
   * asserting the alignment rule against a number that satisfies it.
   */
  function misalignedWindow(): number {
    for (let w = DIGEST_WINDOW.min; w <= DIGEST_WINDOW.max; w += 1) {
      if (DIGEST_WINDOW.max % w !== 0) return w;
    }
    throw new Error(
      "oto test: every window in range divides the day, so the alignment rule constrains nothing",
    );
  }

  it("publishes the contract's ranges on the controls themselves", async () => {
    mount();
    const editor = await openEditor();

    fireEvent.click(editor.getByLabelText(COUNT));
    await until(() => expect(editor.getByLabelText("Once it has happened")).toBeTruthy());

    const threshold = editor.getByLabelText("Once it has happened") as HTMLInputElement;
    expect(threshold.type).toBe("number");
    expect(Number(threshold.min)).toBe(COUNT_MIN.min);
    expect(Number(threshold.max)).toBe(COUNT_MIN.max);

    const window = editor.getByLabelText("Within (seconds)") as HTMLInputElement;
    expect(Number(window.min)).toBe(COUNT_WINDOW.min);
    expect(Number(window.max)).toBe(COUNT_WINDOW.max);
  });

  it("⛔ arrives and leaves in pairs, so half a condition is unbuildable", async () => {
    const net = mount();
    const editor = await openEditor();

    fireEvent.click(editor.getByLabelText(COUNT));
    await until(() => expect(editor.getByLabelText("Within (seconds)")).toBeTruthy());
    fireEvent.click(editor.getByRole("button", { name: "Save" }));

    await until(() => expect(net.to(EDIT_PATH)).toHaveLength(1));
    const sent = net.to(EDIT_PATH)[0]?.body as {
      count_min: number | null;
      count_window_seconds: number | null;
    };
    // One click sets both halves. `policies_count_pair_ck` refuses a row carrying
    // one, and the point of the checkbox is that no sequence of clicks makes one.
    expect(sent.count_min).toBe(COUNT_MIN.min);
    expect(sent.count_window_seconds).toBe(COUNT_WINDOW.min);
  });

  it("⛔ binds the count's unit when the count is switched on", async () => {
    const net = mount();
    const editor = await openEditor();

    fireEvent.click(editor.getByLabelText(COUNT));
    await until(() => expect(editor.getByLabelText("Once it has happened")).toBeTruthy());
    fireEvent.click(editor.getByRole("button", { name: "Save" }));

    await until(() => expect(net.to(EDIT_PATH)).toHaveLength(1));
    const sent = net.to(EDIT_PATH)[0]?.body as { subject_kinds: readonly string[] };

    // `policies_count_case_ck`: a count counts Cases, so the binding must be
    // exactly one kind and that kind must be `case`. An operator who ticks a count
    // has not been anywhere near the altitude control, so leaving it to the server
    // means a 422 naming a field they never touched.
    expect(sent.subject_kinds).toEqual(["case"]);
  });

  it("⛔ refuses a digest window that does not divide the day, and names two that do", async () => {
    const net = mount();
    const editor = await openEditor();

    fireEvent.click(editor.getByLabelText(DIGEST));
    await until(() => expect(editor.getByLabelText("Every (seconds)")).toBeTruthy());

    const bad = misalignedWindow();
    fireEvent.input(editor.getByLabelText("Every (seconds)"), { target: { value: String(bad) } });

    // The rule JSON Schema cannot state, stated in the dialog. Without it this is
    // a 422 after Save — the contract says so itself: "a window that is in range
    // but not a divisor comes back as a 422".
    await until(() =>
      expect(
        editor
          .getAllByRole("alert")
          .some((el: HTMLElement) => /divide the day evenly/.test(el.textContent ?? "")),
        "nothing told the operator the window must divide the day",
      ).toBe(true),
    );

    // And it says which windows would work, because "must divide 86400" is not a
    // sum anybody does in their head.
    const complaint = editor
      .getAllByRole("alert")
      .map((el: HTMLElement) => el.textContent ?? "")
      .find((t: string) => t.includes("divide the day evenly"));
    const suggested = [...(complaint ?? "").matchAll(/\b(\d+)\b/g)].map((m) => Number(m[1]));
    expect(suggested.length, "no admissible window was suggested").toBeGreaterThan(0);
    for (const w of suggested) expect(DIGEST_WINDOW.max % w).toBe(0);

    expect(editor.getByRole("button", { name: "Save" })).toBeDisabled();
    fireEvent.click(editor.getByRole("button", { name: "Save" }));
    expect(net.to(EDIT_PATH), "a misaligned digest window reached the wire").toHaveLength(0);
  });

  it("⛔ adds the fact a digest is routed by, so the policy is not silently silent", async () => {
    const net = mount();
    const editor = await openEditor();

    fireEvent.click(editor.getByLabelText(DIGEST));
    await until(() => expect(editor.getByLabelText("Every (seconds)")).toBeTruthy());
    fireEvent.click(editor.getByRole("button", { name: "Save" }));

    await until(() => expect(net.to(EDIT_PATH)).toHaveLength(1));
    const sent = net.to(EDIT_PATH)[0]?.body as {
      reasons: readonly string[];
      digest_window_seconds: number | null;
    };
    // `policies_digest_reason_ck`. A policy with a window and without the reason
    // records a suppressed `no_policy` once per window, forever — configured on
    // the screen and silent in the channel.
    expect(sent.digest_window_seconds).toBe(DIGEST_WINDOW.min);
    expect(sent.reasons).toContain("digest");
  });

  it("⛔ patches an explicit null to turn a condition off", async () => {
    const withThrottle = policy({ throttle: { max: 5, window_seconds: 3600 } });
    const net = stubFetch({
      "GET /api/v1/notification-policies": list([withThrottle]),
      "GET /api/v1/channels": list([channel()]),
      "GET /api/v1/alerts": list([alert()]),
      "GET /api/v1/labels": { json: { data: [], meta: { request_id: "r" } } },
      [`PATCH ${EDIT_PATH}`]: () => ({ json: item(withThrottle) }),
    });
    renderScreen(() => <PoliciesSection />);
    const editor = await openEditor();

    // It arrives ticked, because the policy has one.
    const toggle = editor.getByLabelText(THROTTLE) as HTMLInputElement;
    expect(toggle.checked, "an existing throttle is not shown as set").toBe(true);

    fireEvent.click(toggle);
    fireEvent.click(editor.getByRole("button", { name: "Save" }));
    await until(() => expect(net.to(EDIT_PATH)).toHaveLength(1));

    // ⛔ `null`, NOT ABSENT. On a PATCH absent means "leave it alone", so omitting
    // the field would show the throttle removed in the dialog and keep it on the
    // server — the most expensive silent edit this form could make.
    const sent = net.to(EDIT_PATH)[0]?.body as Record<string, unknown>;
    expect(Object.hasOwn(sent, "throttle"), "the cleared throttle was omitted").toBe(true);
    expect(sent.throttle).toBeNull();

    const parsed = v.safeParse(UpdatePolicyRequestSchema, sent);
    expect(parsed.success, JSON.stringify(parsed.issues?.map((i) => i.message))).toBe(true);
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
    await pickTheRecentAlert(editor);
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
    await pickTheRecentAlert(editor);
    fireEvent.click(editor.getByRole("button", { name: "Preview" }));

    await until(() => expect(editor.getByText(/it would go unreported/)).toBeTruthy());
  });
});
