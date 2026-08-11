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

import { PoliciesSection } from "./PoliciesSection";
import type { NotificationSuppressedReason, PolicyPreview } from "~/api/types";
import { enumValues } from "~/test/contract";
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

function mount(): FetchStub {
  const net = stubFetch({
    "GET /api/v1/notification-policies": list([policy()]),
    "GET /api/v1/channels": list([channel()]),
    "GET /api/v1/alerts": list([alert()]),
    "GET /api/v1/labels": { json: { data: [], meta: { request_id: "r" } } },
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
