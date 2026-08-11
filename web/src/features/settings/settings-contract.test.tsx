/**
 * The settings screens, measured against the server's own bounds and enums.
 *
 * ⛔ THE RULE THESE TESTS EXIST TO ENFORCE: a settings screen may not invent a
 * number or a word the server has an opinion about. `PoliciesSection.tsx` shipped
 * a reason list that had drifted from the contract, and the symptom was a 422 on
 * save and a raw wire token on screen — a class of bug that is invisible to any
 * test that types the same list a second time.
 *
 * So the expectations here come from two derived sources and never from this
 * file: `integerBounds()` reads `api/openapi/openapi.yaml`, and
 * `requestMaxLength()` / `enumValues()` read what `npm run generate` emitted from
 * it. A bound moved server-side breaks these tests; a bound copied into a screen
 * and then moved server-side would not.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { ChannelsSection } from "./ChannelsSection";
import { TuningSection } from "./TuningSection";
import { UpdateOrgSettingsRequestSchema } from "~/api/generated/validators";
import { enumValues, integerBounds, requestMaxLength } from "~/test/contract";
import { channel, orgSettings, source } from "~/test/fixtures";
import type { Bound } from "~/test/contract";
import {
  expectNoUndefined,
  item,
  list,
  renderScreen,
  stubFetch,
  unpaged,
  until,
} from "~/test/harness";

/** Every `minimum`/`maximum` the org-settings write declares, read from the YAML. */
const BOUNDS = integerBounds("UpdateOrgSettingsRequest");

/* -------------------------------------------------------------------------- */
/* Channels                                                                   */
/* -------------------------------------------------------------------------- */

function mountChannels(): void {
  stubFetch({
    "GET /api/v1/channel-types": unpaged([
      {
        type: "slack",
        display_name: "Slack",
        credential_kinds: ["bot_token"],
        renderers: ["slack.default"],
        config_schema: { type: "object", properties: {} },
      },
    ]),
    "GET /api/v1/channels": list([channel()]),
  });
  renderScreen(() => <ChannelsSection />);
}

describe("the channel editor", () => {
  it("offers every verbosity the contract publishes and no other", async () => {
    mountChannels();
    await until(() => expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));

    const dialog = within(document.querySelector("dialog[open]") as HTMLElement);
    const select = dialog.getByLabelText("Verbosity") as HTMLSelectElement;

    // `VERBOSITIES` is hand-written in `ChannelsSection.tsx` — and copied a
    // third time in `tuningCopy.ts`. Derived here so that the day `Verbosity`
    // grows a member, the copies are a test failure rather than a channel that
    // silently cannot be set to it.
    expect(Array.from(select.options).map((o) => o.value)).toEqual([...enumValues("Verbosity")]);
    expectNoUndefined(document.body);
  });
});

/* -------------------------------------------------------------------------- */
/* Tuning                                                                     */
/* -------------------------------------------------------------------------- */

/**
 * Mount the tuning screen over a chosen set of bounds.
 *
 * The default is the contract's own table. A test that passes a *different*
 * table is asking the sharper question: does the screen read the server's
 * numbers, or does it merely happen to agree with them?
 */
function mountTuning(bounds: ReadonlyMap<string, Bound> = BOUNDS): void {
  stubFetch({
    "GET /api/v1/org/settings": () => ({ json: item(orgSettings(bounds)) }),
    "GET /api/v1/sources": list([source()]),
  });
  renderScreen(() => <TuningSection />);
}

/** Every number input on screen, by the bound key it is bound to. */
function numberInputs(): Map<string, HTMLInputElement> {
  const out = new Map<string, HTMLInputElement>();
  for (const el of Array.from(document.querySelectorAll("input[type=number]"))) {
    const input = el as HTMLInputElement;
    // Knob controls are `id="knob-<key>"`; anything else on the screen is not
    // a knob and is not this test's business.
    const key = /^knob-(.+)$/.exec(input.id)?.[1];
    if (key !== undefined) out.set(key, input);
  }
  return out;
}

describe("the tuning knobs", () => {
  it("bounds every numeric knob with the range the server served for it", async () => {
    mountTuning();
    await until(() => expect(numberInputs().size).toBeGreaterThan(0));

    const inputs = numberInputs();
    let checked = 0;
    for (const [key, bound] of BOUNDS) {
      const input = inputs.get(key);
      if (input === undefined) continue;
      expect(Number(input.min), `\`${key}\` min`).toBe(bound.min);
      expect(Number(input.max), `\`${key}\` max`).toBe(bound.max);
      checked += 1;
    }
    // A screen that rendered no knob at all would otherwise pass the loop above
    // vacuously, which is the failure mode this whole file is about.
    expect(checked, "no bounded knob was found on the tuning screen").toBeGreaterThan(0);
  });

  it("⛔ follows the server when the server moves, rather than its own copy", async () => {
    // Deliberately not the contract's numbers. If any bound on screen were
    // hardcoded, this is where it would show: the screen must publish what oto
    // served, because oto is what will reject the save.
    const moved = new Map<string, Bound>();
    for (const [key, b] of BOUNDS) moved.set(key, { min: b.min + 1, max: b.max - 1 });

    mountTuning(moved);
    await until(() => expect(numberInputs().size).toBeGreaterThan(0));

    const inputs = numberInputs();
    let checked = 0;
    for (const [key, bound] of moved) {
      const input = inputs.get(key);
      if (input === undefined) continue;
      expect(Number(input.min), `\`${key}\` min`).toBe(bound.min);
      expect(Number(input.max), `\`${key}\` max`).toBe(bound.max);
      checked += 1;
    }
    expect(checked).toBeGreaterThan(0);
    // And it says so in words, so the range is legible without inspecting the
    // element: "oto accepts A–B".
    const first = [...moved].find(([key]) => inputs.has(key))!;
    expect(screen.getAllByTitle(/range oto enforces/).length).toBeGreaterThan(0);
    expect(document.body.textContent).toContain(`oto accepts ${first[1].min}–${first[1].max}`);
  });

  it("refuses a value outside the served range instead of letting the server refuse it", async () => {
    mountTuning();
    await until(() => expect(numberInputs().size).toBeGreaterThan(0));

    const [key, input] = [...numberInputs()].find(([k]) => BOUNDS.has(k))!;
    const bound = BOUNDS.get(key)!;
    // Nothing is complaining yet, so the complaint below is this edit's.
    expect(screen.queryAllByRole("alert")).toHaveLength(0);
    fireEvent.input(input, { target: { value: String(bound.max + 1) } });

    await until(() => expect(screen.getAllByRole("alert").length).toBeGreaterThan(0));
    expect(document.body.textContent).toContain(String(bound.max));
    expectNoUndefined(document.body);
  });
});

describe("the mention list cap", () => {
  it("is the contract's cap, not a number this screen chose", () => {
    // The one bound on the tuning screen that is not served in `bounds` — the
    // screen holds `MENTION_LIST_MAX` itself. Derived here so the copy cannot
    // outlive the contract.
    const max = requestMaxLength(UpdateOrgSettingsRequestSchema, "unacked_reminder_mention_list");
    expect(max).toBeGreaterThan(0);

    mountTuning();
    // The cap is stated in the copy wherever the list is edited; assert the
    // number rather than the sentence, which is free to be reworded.
    return until(() => expect(document.body.textContent).toContain(String(max)));
  });
});
