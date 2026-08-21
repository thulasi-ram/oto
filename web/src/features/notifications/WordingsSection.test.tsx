/**
 * The wordings screen, checked on the two things it exists to make visible.
 *
 * ⭐ ONE: THE SAME TEMPLATE, SPELLED TWICE. ADR 0048 makes a Wording's output
 * provider-neutral — a filter emits a mark and each Dialect spells it — and the
 * only way an author learns that markup is not theirs to write is by seeing
 * `*critical*` and `critical` produced by one line. A pane that showed one
 * spelling, or that put the second behind a tab, would be a pane that teaches the
 * opposite of the decision it exists to serve, and it would pass every assertion
 * anybody would think to write about "the preview renders".
 *
 * ⭐ TWO: WHICH ROW SPEAKS. ADR 0049 has two scopes with no cascade — a
 * channel-bound Wording is asked before an org-wide one, and within a scope the
 * LOWER priority is asked first. A list can be ordered correctly and still leave
 * "which of these speaks on #sre-alerts" unanswerable, so what is asserted below
 * is the ANSWER and not the ordering.
 *
 * ⛔ AND THE REFUSALS ARE THE SERVER'S SENTENCES, NEVER THIS SCREEN'S. Four of
 * the eight stanzas take no Wording; they are listed anyway, and picking one has
 * to put the reason on screen. A test that accepted "not allowed" here would let
 * the screen re-word the one explanation the feature promised to surface.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";
import * as v from "valibot";

import { WordingsSection } from "./WordingsSection";
import {
  CreateWordingRequestSchema,
  WordingSpellingDTOSchema,
} from "~/api/generated/validators";
import type { Wording, WordingPreview, WordingRendering } from "~/api/types";
import { enumValues, requestOptions } from "~/test/contract";
import { channel } from "~/test/fixtures";
import {
  expectNoUndefined,
  item,
  list,
  renderScreen,
  stubFetch,
  unpaged,
  until,
  type FetchStub,
  type RecordedCall,
} from "~/test/harness";

const WORDINGS = "/api/v1/wordings";
const PREVIEW = `${WORDINGS}/preview`;

const STANZAS = enumValues("WordingStanza");
/**
 * The providers a preview spells for.
 *
 * ⛔ READ OFF THE CONTRACT, NOT TYPED HERE. `dialect` is an INLINE enum on
 * `WordingSpellingDTO` and so has no name to import; re-typing `["slack",
 * "plain"]` would mean a Dialect the server gains is a column this test never
 * asks for — which is the quieter half of the very defect ADR 0048 records
 * against the preview's own hard-coded Dialect slice.
 */
const DIALECTS = requestOptions(WordingSpellingDTOSchema, "dialect");

/* -------------------------------------------------------------------------- */
/* Fixtures. Local, because nothing else in the suite reads a Wording yet.     */
/* -------------------------------------------------------------------------- */

const T0 = "2026-08-01T09:00:00Z";

function wording(patch: Partial<Wording> = {}): Wording {
  return {
    id: "1a2b3c4d-5e6f-4a1b-8c2d-3e4f50617283",
    channel_id: null,
    stanza: "body",
    template: "{{ alert.name }} is {{ alert.severity | bold }}",
    matchers: [],
    reasons: [],
    priority: 100,
    enabled: true,
    created_at: T0,
    updated_at: T0,
    deleted_at: null,
    ...patch,
  };
}

/** The org-wide house voice, and one destination's exception to it. */
const HOUSE = wording({ id: "aaaaaaaa-0000-4000-8000-000000000001", priority: 100 });
const BOUND = wording({
  id: "bbbbbbbb-0000-4000-8000-000000000002",
  channel_id: channel().id,
  template: "{{ alert.name }} — {{ alert.severity | upper }}",
  priority: 50,
});

/**
 * One fixture spelled by every Dialect the contract publishes.
 *
 * ⛔ THE TEXTS DIFFER BY THEIR PUNCTUATION AND BY NOTHING ELSE, which is the
 * whole shape of the thing under test: a pane that rendered one column, or that
 * rendered the same string twice, must not be able to pass.
 */
function rendering(patch: Partial<WordingRendering> = {}): WordingRendering {
  return {
    fixture: "firing",
    representative: true,
    spellings: [
      { dialect: "slack", text: "checkout-latency is *critical*", error: null },
      { dialect: "plain", text: "checkout-latency is critical", error: null },
    ],
    ...patch,
  };
}

function preview(patch: Partial<WordingPreview> = {}): WordingPreview {
  return {
    stanza: "body",
    template: "{{ alert.name }} is {{ alert.severity | bold }}",
    problems: [],
    renderings: [rendering()],
    ...patch,
  };
}

/** The server's own sentence for a stanza that is a grid rather than prose. */
const FIELDS_REFUSAL =
  "the fields grid is up to ten separately-budgeted cells in a binding order that decides " +
  "what is dropped on overflow, so one line of prose would replace the grid rather than re-word it";

/* -------------------------------------------------------------------------- */

interface World {
  readonly net: FetchStub;
}

function mount(rows: readonly Wording[] = [HOUSE, BOUND]): World {
  const net = stubFetch({
    [`GET ${WORDINGS}`]: list([...rows]),
    "GET /api/v1/channels": list([channel()]),
    "GET /api/v1/labels": unpaged([]),
    // ⛔ THE STANZA IS READ OFF THE REQUEST, NOT ASSUMED. The refusal is the
    // server's answer to a particular stanza, so a stub that answered the same
    // way whatever was asked would make the refusal test prove nothing.
    [`POST ${PREVIEW}`]: (call: RecordedCall) => {
      const body = call.body as { stanza: string; template: string };
      if (body.stanza === "fields") {
        return {
          json: item(
            preview({
              stanza: "fields",
              template: body.template,
              problems: [
                { field: "stanza", code: "unsupported_stanza", message: FIELDS_REFUSAL },
              ],
              // A refused stanza never compiles, so there is nothing to show.
              renderings: [],
            }),
          ),
        };
      }
      return { json: item(preview({ template: body.template })) };
    },
    [`POST ${WORDINGS}`]: () => ({ status: 201, json: item(wording()) }),
  });
  renderScreen(() => <WordingsSection />);
  return { net };
}

/** Every GET of the list, which is how an invalidation is observed from outside. */
function reads(net: FetchStub): readonly RecordedCall[] {
  return net.calls.filter((c) => c.method === "GET" && c.path === WORDINGS);
}

/** Open a Kobalte listbox the way a keyboard user does, and wait for its options. */
async function openListbox(trigger: HTMLElement): Promise<void> {
  fireEvent.keyDown(trigger, { key: "ArrowDown" });
  await until(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
}

/** Pick the option whose text is exactly `label`. `SelectContent` portals. */
async function pick(trigger: HTMLElement, label: string): Promise<void> {
  await openListbox(trigger);
  const option = screen.getAllByRole("option").find((o) => o.textContent?.trim() === label);
  expect(option, `no option called \`${label}\``).toBeTruthy();
  fireEvent.click(option as HTMLElement);
}

/** Open the create dialog and hand back its scope. */
async function openEditor(name = "Add a wording"): Promise<ReturnType<typeof within>> {
  await until(() => expect(screen.getByRole("button", { name })).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name }));
  const dialog = document.querySelector('[role="dialog"]');
  expect(dialog, "the wording editor did not open").not.toBeNull();
  return within(dialog as HTMLElement);
}

/** Wait for the debounced preview to have answered at least once. */
async function awaitPreview(editor: ReturnType<typeof within>): Promise<void> {
  await until(() => expect(editor.getAllByText("firing").length).toBeGreaterThan(0));
}

/* -------------------------------------------------------------------------- */

describe("the two scopes, and which of them speaks", () => {
  it("shows the house voice and the per-destination rows as different things", async () => {
    mount();

    await until(() => expect(screen.getByText(/Per destination/)).toBeTruthy());
    expect(screen.getByText(/The house voice/)).toBeTruthy();
    // The destination's own row is filed under its name, not mixed in with the
    // org-wide one — the grouping IS the precedence claim. `getAllBy`, because
    // the name is also an option of the "Which destination?" picker.
    expect(screen.getAllByText(channel().name).length).toBeGreaterThan(0);
    expectNoUndefined(document.body);
  });

  it("⭐ names the row that is asked first once a destination is chosen", async () => {
    mount();

    // With no destination chosen the question is "everywhere else", and only the
    // house voice is in the queue: a channel-bound row speaks nowhere else.
    await until(() => expect(screen.getByText(/Asked first on every other destination/)).toBeTruthy());
    expect(screen.getByText(/from the house voice/)).toBeTruthy();

    await pick(screen.getByLabelText("Which destination?"), channel().name);

    // ⛔ THE CHANNEL-BOUND ROW TAKES OVER, AND THE ONE IT DISPLACED IS COUNTED.
    // A rule naming one destination is more specific than one naming a whole
    // tenant (ADR 0049), so the answer changes even though `BOUND` has the same
    // stanza and a priority that only matters within its own scope.
    await until(() =>
      expect(
        screen.getByText(new RegExp(`from ${channel().name}, ahead of 1 other`)),
      ).toBeTruthy(),
    );
    expect(screen.getByText(`Asked first on ${channel().name}`)).toBeTruthy();

    // And the rows themselves say where they stand in that queue.
    expect(screen.getByText("asked first for body")).toBeTruthy();
    expect(screen.getByText("asked #2")).toBeTruthy();
  });

  it("says out loud that being asked first is not the same as speaking", async () => {
    mount();
    // The `when` clause can decline and pass the turn on, and no configuration
    // screen can know whether it will. Claiming a winner would be a lie the one
    // time it matters.
    await until(() =>
      expect(screen.getByText(/passes the turn to the next one for that stanza/)).toBeTruthy(),
    );
  });
});

/* -------------------------------------------------------------------------- */

describe("the preview", () => {
  it("⭐ shows the same line as BOTH providers spell it, side by side", async () => {
    mount();
    const editor = await openEditor();
    await awaitPreview(editor);

    // Every Dialect the contract publishes is on screen, labelled — and the list
    // is the RESPONSE's, so a provider the server gains appears here without this
    // screen being told about it.
    for (const dialect of DIALECTS) {
      expect(editor.getByText(dialect), `\`${dialect}\` is not spelled`).toBeTruthy();
    }

    // ⛔ THE POINT: one template, two punctuations. Slack's mrkdwn emphasis is
    // present; the webhook consumer's copy has the same words and none of it.
    expect(editor.getByText("checkout-latency is *critical*")).toBeTruthy();
    expect(editor.getByText("checkout-latency is critical")).toBeTruthy();

    // The ordinary cards are marked as such, because a template that renders
    // empty on one of them is refused at save time and on a hostile one is not.
    expect(editor.getByText("ordinary card")).toBeTruthy();
    expectNoUndefined(document.body);
  });

  it("⛔ shows the problems AND the output together, never one instead of the other", async () => {
    const world = mount();
    world.net.on(`POST ${PREVIEW}`, () => ({
      json: item(
        preview({
          problems: [
            {
              field: "template",
              code: "empty_render",
              message: "renders nothing (on a digest notification)",
            },
          ],
        }),
      ),
    }));

    const editor = await openEditor();
    await awaitPreview(editor);

    // The server answers `200` for a template it would refuse, precisely so that
    // both halves land in one round trip: the fix for "renders nothing on a
    // digest" is only visible beside what it did render on the others.
    expect(editor.getByText(/renders nothing \(on a digest notification\)/)).toBeTruthy();
    expect(editor.getByText("checkout-latency is *critical*")).toBeTruthy();
    expect(editor.getByText("checkout-latency is critical")).toBeTruthy();

    // And a refused template cannot be saved from under its own renderings.
    expect(editor.getByText(/Saving is refused while any of these stand/)).toBeTruthy();
  });
});

/* -------------------------------------------------------------------------- */

describe("the four stanzas that take no wording", () => {
  it("lists all eight rather than hiding the ones that will be refused", async () => {
    mount();
    const editor = await openEditor();

    await openListbox(editor.getByLabelText("Which part of the card"));
    expect(screen.getAllByRole("option").map((o) => o.textContent?.trim()).sort()).toEqual(
      [...STANZAS].sort(),
    );
  });

  it("⛔ answers a refused stanza with the server's own sentence, and blocks the save", async () => {
    const world = mount();
    const editor = await openEditor();
    await awaitPreview(editor);

    await pick(editor.getByLabelText("Which part of the card"), "fields");

    // The whole reason `fields` is in the menu: the explanation. A screen that
    // said "not allowed" — or that greyed the option out — would leave an author
    // who wanted a grid of their own with nothing to read and nowhere to go.
    // Twice, deliberately: beside the control that caused it, and in the
    // preview's own list of what the save-time gate would say. Neither is
    // redundant — one is where the eye is, the other is where the rest of the
    // template's problems are.
    await until(() => expect(editor.getAllByText(FIELDS_REFUSAL)).toHaveLength(2));

    const create = editor.getByRole("button", { name: "Create" });
    expect(create).toBeDisabled();
    fireEvent.click(create);
    expect(
      world.net.calls.filter((c) => c.method === "POST" && c.path === WORDINGS),
      "a stanza the server refuses reached the wire anyway",
    ).toHaveLength(0);
  });
});

/* -------------------------------------------------------------------------- */

describe("writing one", () => {
  it("sends a body the generated request schema accepts, and re-reads the list", async () => {
    const world = mount();
    const before = reads(world.net).length;

    const editor = await openEditor();
    await awaitPreview(editor);

    fireEvent.click(editor.getByRole("button", { name: "Create" }));

    const posted = (): readonly RecordedCall[] =>
      world.net.calls.filter((c) => c.method === "POST" && c.path === WORDINGS);
    await until(() => expect(posted()).toHaveLength(1));

    const sent = posted()[0]?.body;
    const parsed = v.safeParse(CreateWordingRequestSchema, sent);
    expect(parsed.success, JSON.stringify(parsed.issues?.map((i) => i.message))).toBe(true);

    // Omitting the destination is how the house voice is spelled on the wire.
    expect((sent as { channel_id: string | null }).channel_id).toBeNull();

    // ⛔ THE LIST IS RE-READ, AND THAT IS THE INVALIDATION UNDER TEST. `wordings.list`
    // declares itself kept fresh by a local write (`api/queries.ts`); a create that
    // did not invalidate would leave the operator looking at a list missing the row
    // they just wrote, with nothing on screen that looked wrong.
    await until(() => expect(reads(world.net).length).toBeGreaterThan(before));
  });

  it("offers no way to move a saved wording to another stanza or destination", async () => {
    mount();
    await until(() => expect(screen.getAllByRole("button", { name: "Edit" }).length).toBe(2));
    fireEvent.click(screen.getAllByRole("button", { name: "Edit" })[0]!);
    const editor = within(document.querySelector('[role="dialog"]') as HTMLElement);

    // The PATCH body has no field for either (ADR 0049), so a control offering one
    // could only ever be a control whose value is silently discarded.
    expect(editor.queryByLabelText("Which part of the card")).toBeNull();
    expect(editor.queryByLabelText("Where it applies")).toBeNull();
    expect(editor.getByText(/Delete this one and write it/)).toBeTruthy();
  });
});
