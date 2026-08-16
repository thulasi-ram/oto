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
 *
 * The same argument covers **patterns and length caps**, which is what the source
 * form's tests below are about. A regex is the worst thing to hand-copy, because a
 * copy can be wrong in ways that read as right: `SourcesSection.tsx` shipped
 * `/^https?:\/\/[^\s]+$/i` where the contract says
 * `/^https?:\/\/[^\s]+[^\/]$/`, and the difference — one flag — was a form that
 * accepted `HTTP://…` and a server that did not. So no candidate URL below is
 * judged by this file. Each one is put to the *generated schema* first, and the
 * form is only ever asserted to agree with that answer.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";
import * as v from "valibot";

import { ChannelsSection } from "./ChannelsSection";
import { SourcesSection } from "./SourcesSection";
import { TuningSection } from "./TuningSection";
import { enumValuesOf, patternOf } from "~/api/bounds";
import {
  AlertRollupDTOSchema,
  CreateSourceRequestSchema,
  UpdateOrgSettingsRequestSchema,
} from "~/api/generated/validators";
import { enumValues, integerBounds, requestField, requestMaxLength } from "~/test/contract";
import { channel, cluster, orgSettings, source } from "~/test/fixtures";
import type { Bound } from "~/test/contract";
import {
  expectNoUndefined,
  flush,
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

    await until(() => expect(document.querySelector('[role="dialog"]')).toBeTruthy());
    // The Verbosity control is Kobalte's `Select` now, not a native `<select>` —
    // there is no `HTMLSelectElement` to read `.options` off directly. Kobalte
    // still renders one real hidden `<select>` with real `<option>`s underneath
    // (`SelectHiddenSelect`, id="ch-verbosity"), kept around for browser autofill
    // and native form submission, and that is what this assertion reads instead.
    const select = document.querySelector("#ch-verbosity") as HTMLSelectElement;
    expect(select, "no hidden native <select> found for the Verbosity control").toBeTruthy();

    // `VERBOSITIES` is hand-written in `ChannelsSection.tsx` — and copied a
    // third time in `tuningCopy.ts`. Derived here so that the day `Verbosity`
    // grows a member, the copies are a test failure rather than a channel that
    // silently cannot be set to it.
    //
    // The hidden select carries a leading blank `<option />` (Kobalte's own
    // placeholder-for-autofill option, always present regardless of selection),
    // so it is filtered out before comparing rather than asserted as part of
    // the contract's own list.
    expect(Array.from(select.options).map((o) => o.value).filter((v) => v !== "")).toEqual([
      ...enumValues("Verbosity"),
    ]);
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

/* -------------------------------------------------------------------------- */
/* Sources                                                                    */
/* -------------------------------------------------------------------------- */

/**
 * Open the register-a-source dialog over one cluster and no sources.
 *
 * No sources on purpose: a source row mounts a drill panel of its own, and this
 * file has nothing to say about that.
 */
async function mountSourceDialog(): Promise<void> {
  stubFetch({
    "GET /api/v1/sources": list([]),
    "GET /api/v1/clusters": list([cluster()]),
  });
  renderScreen(() => <SourcesSection />);

  const open = (): HTMLButtonElement =>
    screen.getByRole("button", { name: "Register an Alertmanager" }) as HTMLButtonElement;
  // The button is disabled until a cluster exists, so waiting on it is also
  // waiting for the clusters query to land.
  await until(() => expect(open().disabled).toBe(false));
  fireEvent.click(open());
  await until(() => expect(document.querySelector("#src-url")).toBeTruthy());
}

/** Type `value` into one field and return the complaint it now carries, if any. */
async function complaint(id: string, value: string): Promise<string> {
  const input = document.querySelector(`#${id}`) as HTMLInputElement;
  fireEvent.input(input, { target: { value } });
  await flush();
  return document.querySelector(`#${id}-error`)?.textContent ?? "";
}

/** What the generated schema itself says about one value of one property. */
function contractAccepts(key: string, value: unknown): boolean {
  const schema = requestField(CreateSourceRequestSchema, key) as v.GenericSchema;
  return v.safeParse(schema, value).success;
}

describe("the source form", () => {
  it("⛔ refuses exactly the base URLs the generated schema refuses", async () => {
    const max = requestMaxLength(CreateSourceRequestSchema, "base_url");

    // A list of strings, not a list of rules. Every one is put to the contract
    // first; this file never states which of them ought to fail.
    const candidates: readonly string[] = [
      "https://alertmanager.example.com",
      // The `/i` drift. Uppercase scheme: the server refuses it, and the form's
      // case-insensitive copy of the pattern used to wave it through.
      "HTTP://alertmanager.example.com",
      // Reconstructed for years by a second `v.check`; the contract's pattern
      // has always said it in one.
      "https://alertmanager.example.com/",
      "ftp://alertmanager.example.com",
      "alertmanager.example.com",
      // The cap that was missing outright, so this URL reached the API.
      `https://x.example.com/${"a".repeat(max)}`,
    ];

    await mountSourceDialog();

    let refused = 0;
    for (const candidate of candidates) {
      const accepted = contractAccepts("base_url", candidate);
      const shown = await complaint("src-url", candidate);
      expect(
        shown !== "",
        `the contract ${accepted ? "accepts" : "refuses"} \`${candidate.slice(0, 48)}\`, the form did not agree`,
      ).toBe(!accepted);
      if (!accepted) refused += 1;
    }

    // A form that complained about nothing would otherwise pass the loop above
    // by agreeing with a contract nobody asked anything of.
    expect(refused, "no candidate was refused — the contract read is vacuous").toBeGreaterThan(0);
  });

  it("caps the name at the contract's own maxLength, in the control and in the schema", async () => {
    const max = requestMaxLength(CreateSourceRequestSchema, "name");
    await mountSourceDialog();

    expect((document.querySelector("#src-name") as HTMLInputElement).maxLength).toBe(max);

    // The attribute alone is not the guarantee — a paste is not typing — so the
    // schema must complain too, and say the contract's number while it does.
    expect(contractAccepts("name", "n".repeat(max + 1))).toBe(false);
    const shown = await complaint("src-name", "n".repeat(max + 1));
    expect(shown).toContain(String(max));
    expect(await complaint("src-name", "n".repeat(max))).toBe("");
  });

  it("offers every source kind the contract publishes and no other", async () => {
    await mountSourceDialog();
    // Same story as the Verbosity control in the channel editor above: `#src-kind`
    // is Kobalte's hidden native `<select>` behind the Kind combobox, kept for
    // autofill/native form submission, and it carries a leading blank `<option />`
    // that isn't part of the contract's own enum.
    const select = document.querySelector("#src-kind") as HTMLSelectElement;
    expect(select, "no hidden native <select> found for the Kind control").toBeTruthy();
    expect(Array.from(select.options).map((o) => o.value).filter((v) => v !== "")).toEqual([
      ...enumValues("SourceKind"),
    ]);
    expectNoUndefined(document.body);
  });
});

/* -------------------------------------------------------------------------- */
/* The accessors the screens read through                                     */
/* -------------------------------------------------------------------------- */

/**
 * `~/api/bounds` is the only thing standing between a screen and a second copy
 * of a server rule, so its two newest accessors get the same treatment as the
 * screens: measured against the contract, and required to fail loudly rather than
 * to return nothing when the generator stops emitting what they read.
 */
describe("the contract accessors", () => {
  it("hands back the generated pattern object, flags and all", () => {
    const pattern = patternOf(CreateSourceRequestSchema, "base_url");

    // Asserted as properties rather than as the pattern's text, because the text
    // is the contract's to change and these two are what the copy got wrong: it
    // is case-sensitive, and it already forbids the trailing slash.
    expect(pattern.flags).toBe("");
    expect(pattern.test("HTTP://x.example.com")).toBe(false);
    expect(pattern.test("https://x.example.com/")).toBe(false);
    expect(pattern.test("https://x.example.com")).toBe(true);

    // A property with no pattern must throw with the name it looked for, not
    // return `undefined` and silently unvalidate the control.
    expect(() => patternOf(CreateSourceRequestSchema, "name")).toThrow(/no `regex`/);
  });

  it("reads an inline picklist the generator gave no name to", () => {
    // The roll-up axis is emitted inline on the DTO, so there is no
    // `RollupAxisSchema` to import and this is the only way to derive it.
    expect([...enumValuesOf(AlertRollupDTOSchema, "group_by")]).toEqual([
      ...enumValues("group_by"),
    ]);
    expect(() => enumValuesOf(AlertRollupDTOSchema, "key")).toThrow(/not a picklist/);
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
