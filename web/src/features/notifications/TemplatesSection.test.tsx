/**
 * The templates screen, checked on the three things it exists to make visible.
 *
 * ⭐ ONE: THE SAME DOCUMENT, SPELLED TWICE. One Markdown source compiles to
 * Slack's `*critical*` and to a webhook consumer's plain `critical`, and the only
 * way an author learns that markup is not theirs to write is by seeing both
 * produced by one line. A pane that showed one spelling, or put the second behind
 * a tab, would teach the opposite of the decision it exists to serve — and would
 * pass every assertion anybody would think to write about "the preview renders".
 *
 * ⭐ TWO: A WARNING IS NOT AN ERROR. A card with no `{{ actions }}` carries no
 * Acknowledge button. That is the operator's choice to make, so the screen has to
 * say so AND leave the save button live. A test that only checked the sentence
 * appeared would pass against a screen that also blocked the save, which is the
 * failure that matters.
 *
 * ⭐ THREE: A REFUSAL IS THE SERVER'S SENTENCE, NEVER THIS SCREEN'S. A table is
 * refused with a reason and a suggested fix, and a screen that re-worded it to
 * "invalid template" would throw away the one explanation the feature promised.
 *
 * ⭐ FOUR: A REASON THE REPLY BODY SKIPS IS OTO'S OWN REPLY, NOT AN EMPTY ONE. The
 * contract spells that as a spelling with neither text nor error, and a pane that
 * drew it as a blank box would tell the author their thread goes silent.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import * as v from "valibot";
import { describe, expect, it } from "vitest";

import { TemplatesSection } from "./TemplatesSection";
import {
  CreateNotificationTemplateRequestSchema,
  TemplateSpellingDTOSchema,
  UpdateNotificationTemplateRequestSchema,
} from "~/api/generated/validators";
import type { NotificationTemplate, TemplatePreview, TemplateRendering } from "~/api/types";
import { requestOptions } from "~/test/contract";
import {
  item,
  list,
  renderScreen,
  stubFetch,
  unpaged,
  until,
  type FetchStub,
  type RecordedCall,
} from "~/test/harness";

const TEMPLATES = "/api/v1/notification-templates";
const PREVIEW = `${TEMPLATES}/preview`;

/**
 * The providers a preview spells for.
 *
 * ⛔ READ OFF THE CONTRACT, NOT TYPED HERE. `dialect` has no named enum to
 * import, and re-typing `["slack", "plain"]` would mean a Dialect the server
 * gains is a column this test never asks for.
 */
const DIALECTS = requestOptions(TemplateSpellingDTOSchema, "dialect");

const T0 = "2026-08-01T09:00:00Z";

const SOURCE = "# {{ alert.name }}\n\n{{ alert.severity | bold }}\n\n{{ actions }}";

/** A reply body that handles one reason and lets every other fall through. */
const REPLY_SOURCE = `{% if reason == 'all_resolved' %}{ "text": "all clear" }{% endif %}`;

function template(patch: Partial<NotificationTemplate> = {}): NotificationTemplate {
  return {
    id: "1a2b3c4d-5e6f-4a1b-8c2d-3e4f50617283",
    name: "house voice",
    provider: "slack",
    format: "card",
    source: SOURCE,
    reply_source: null,
    version: 1,
    enabled: true,
    created_at: T0,
    updated_at: T0,
    ...patch,
  };
}

/**
 * One fixture spelled by every Dialect the contract publishes.
 *
 * ⛔ THE TEXTS DIFFER BY THEIR PUNCTUATION AND BY NOTHING ELSE, which is the
 * whole shape of the thing under test: a pane that rendered one column, or the
 * same string twice, must not be able to pass.
 */
function rendering(patch: Partial<TemplateRendering> = {}): TemplateRendering {
  return {
    fixture: "firing",
    representative: true,
    has_actions: true,
    spellings: [
      { dialect: "slack", text: "checkout-latency\n*critical*" },
      { dialect: "plain", text: "checkout-latency\ncritical" },
    ],
    ...patch,
  };
}

function preview(patch: Partial<TemplatePreview> = {}): TemplatePreview {
  return {
    format: "card",
    source: SOURCE,
    problems: [],
    renderings: [rendering()],
    reply_renderings: [],
    ...patch,
  };
}

/**
 * What the server answers for {@link REPLY_SOURCE}: the reason it handles, and
 * one it does not — spelled with neither text nor error, which is the contract's
 * way of saying "oto's own reply goes out".
 */
function replyRenderings(): TemplateRendering[] {
  return [
    rendering({
      fixture: "all_resolved",
      spellings: [
        { dialect: "slack", text: '{ "text": "all clear" }' },
        { dialect: "plain", text: "all clear" },
      ],
    }),
    rendering({
      fixture: "acked",
      representative: false,
      spellings: [
        { dialect: "slack", text: "" },
        { dialect: "plain", text: "" },
      ],
    }),
  ];
}

/** The server's own sentence for the missing action row. */
const NO_ACTIONS =
  "this template has no `{{ actions }}`, so its cards carry no Acknowledge or Snooze button. " +
  "Alerts can still be acknowledged from the console or the API.";

/** The server's own sentence for a construct no provider can draw. */
const TABLE_REFUSAL =
  "tables are not supported: Slack has no table block and a table degrades to unreadable " +
  "prose. Use a `:::fields` grid instead.";

interface World {
  readonly net: FetchStub;
}

function mount(rows: readonly NotificationTemplate[] = [template()]): World {
  const net = stubFetch({
    [`GET ${TEMPLATES}`]: list([...rows]),
    "GET /api/v1/channels": unpaged([]),
    // ⛔ THE ANSWER IS READ OFF THE REQUEST. A stub that replied the same way
    // whatever was asked would make both the warning test and the refusal test
    // prove nothing about the screen's handling of either.
    [`POST ${PREVIEW}`]: (call: RecordedCall) => {
      const body = call.body as { format: string; source: string; reply_source?: string };
      // ⛔ `reply_renderings` only when a reply body was SENT, as the contract
      // says — so a screen that forgot to send it cannot draw the reasons.
      if (body.reply_source !== undefined) {
        return {
          json: item(preview({ source: body.source, reply_renderings: replyRenderings() })),
        };
      }
      if (body.source.includes("|---|")) {
        return {
          json: item(
            preview({
              source: body.source,
              problems: [{ kind: "unsupported", field: "source", message: TABLE_REFUSAL }],
              renderings: [],
            }),
          ),
        };
      }
      if (!body.source.includes("{{ actions }}")) {
        return {
          json: item(
            preview({
              source: body.source,
              problems: [{ kind: "warning", field: "source", message: NO_ACTIONS }],
              renderings: [rendering({ has_actions: false })],
            }),
          ),
        };
      }
      return { json: item(preview({ source: body.source })) };
    },
    [`POST ${TEMPLATES}`]: { status: 201, json: item(template()) },
    [`PATCH ${TEMPLATES}/${template().id}`]: { json: item(template()) },
  });
  renderScreen(() => <TemplatesSection />);
  return { net };
}

async function openEditor(): Promise<void> {
  await until(() => screen.getByText("house voice"));
  fireEvent.click(screen.getByText("house voice"));
  await until(() => screen.getByLabelText(/the message/i));
}

function sourceBox(): HTMLTextAreaElement {
  return screen.getByLabelText(/the message/i) as HTMLTextAreaElement;
}

function retype(text: string): void {
  fireEvent.input(sourceBox(), { target: { value: text } });
}

async function openReplies(): Promise<HTMLTextAreaElement> {
  fireEvent.click(screen.getByRole("tab", { name: /thread replies/i }));
  await until(() => screen.getByLabelText(/the reply/i));
  return screen.getByLabelText(/the reply/i) as HTMLTextAreaElement;
}

describe("TemplatesSection", () => {
  it("⭐ shows the same document as BOTH providers spell it, side by side", async () => {
    mount();
    await openEditor();

    for (const dialect of DIALECTS) {
      await until(() => screen.getByText(dialect));
    }
    // The punctuation is the difference, and it is the whole lesson.
    await until(() => screen.getByText(/\*critical\*/));
    // ⭐ THE TWO COLUMNS MUST NOT AGREE. If they ever render the same bytes the
    // Dialect layer has gone inert and this screen has stopped teaching anything,
    // while still passing every assertion about "the preview renders".
    await until(() => expect(document.querySelectorAll("pre").length).toBe(DIALECTS.length));
    const columns = [...document.querySelectorAll("pre")].map((el) => el.textContent);
    expect(new Set(columns).size).toBe(DIALECTS.length);
    expect(columns.some((c) => c?.includes("*critical*"))).toBe(true);
    expect(columns.some((c) => c?.includes("critical") && !c.includes("*"))).toBe(true);
  });

  it("⛔ says a card has no Acknowledge button, and still lets it be saved", async () => {
    mount();
    await openEditor();
    retype("# {{ alert.name }}\n\nno buttons here");

    await until(() => screen.getByText(new RegExp("no Acknowledge or Snooze button")));

    // The half that matters: the operator's decision is not overruled.
    const save = screen.getByRole("button", { name: /save/i });
    expect(save).not.toBeDisabled();
  });

  it("⛔ answers a refusal with the server's own sentence, and blocks the save", async () => {
    mount();
    await openEditor();
    retype("| a | b |\n|---|---|");

    await until(() => screen.getByText(new RegExp("Use a `:::fields` grid instead")));
    expect(screen.getByRole("button", { name: /save/i })).toBeDisabled();
  });

  it("names the format's reach where the choice is made, not in a tooltip", async () => {
    mount();
    await openEditor();
    // `raw` is the one irreversible-feeling choice on the screen: finding out it
    // is Slack-only after writing 200 lines of Block Kit is the worst version of
    // this feature.
    fireEvent.click(screen.getByRole("button", { name: "raw" }));
    await until(() => screen.getByText(/Slack only/));
  });

  it("sends a body the generated request schema accepts, and re-reads the list", async () => {
    const world = mount([]);
    await until(() => screen.getByRole("button", { name: /write a template/i }));
    fireEvent.click(screen.getByRole("button", { name: /write a template/i }));

    await until(() => screen.getByLabelText(/^name$/i));
    fireEvent.input(screen.getByLabelText(/^name$/i), { target: { value: "house voice" } });
    fireEvent.click(screen.getByRole("button", { name: /create/i }));

    const posted = () =>
      world.net.calls.find((c) => c.method === "POST" && c.url.endsWith("/notification-templates"));
    await until(() => expect(posted()).toBeDefined());
    const body = posted()!.body as Record<string, unknown>;
    expect(body.name).toBe("house voice");
    expect(body.format).toBe("card");
    // The starter is what gets sent when nobody edits it, so it had better be a
    // template that actually renders.
    expect(String(body.source)).toContain("{{ actions }}");

    await until(() =>
      expect(
        world.net.calls.filter((c) => c.method === "GET" && c.url.includes("notification-templates"))
          .length,
      ).toBeGreaterThan(1),
    );
  });

  it("⭐ previews a reply body per reason, and names the reasons it skips as oto's own", async () => {
    mount();
    await openEditor();
    const replyBox = await openReplies();
    // The root card's box is behind the other tab, not beside this one.
    expect(screen.queryByLabelText(/the message/i)).toBeNull();
    await until(() => screen.getByText(/every reply is oto's own/i));

    fireEvent.input(replyBox, { target: { value: REPLY_SOURCE } });

    await until(() => screen.getByText("all_resolved"));
    screen.getByText("acked");
    expect(screen.getByText("all clear")).toBeTruthy();
    // One muted line per Dialect of the skipped reason, never a blank box.
    expect(screen.getAllByText("oto's own reply")).toHaveLength(DIALECTS.length);
    // The card-only chips mean nothing on a reply.
    expect(screen.queryByText("ordinary card")).toBeNull();
    expect(screen.getByRole("tab", { name: /thread replies/i }).textContent).toContain(
      "(custom)",
    );
  });

  it("sends a typed reply body on create, in a shape the generated schema accepts", async () => {
    const world = mount([]);
    await until(() => screen.getByRole("button", { name: /write a template/i }));
    fireEvent.click(screen.getByRole("button", { name: /write a template/i }));

    await until(() => screen.getByLabelText(/^name$/i));
    fireEvent.input(screen.getByLabelText(/^name$/i), { target: { value: "house voice" } });
    fireEvent.input(await openReplies(), { target: { value: REPLY_SOURCE } });
    fireEvent.click(screen.getByRole("button", { name: /create/i }));

    const posted = () =>
      world.net.calls.find((c) => c.method === "POST" && c.url.endsWith("/notification-templates"));
    await until(() => expect(posted()).toBeDefined());
    const body = posted()!.body as Record<string, unknown>;
    expect(body.reply_source).toBe(REPLY_SOURCE);
    expect(v.safeParse(CreateNotificationTemplateRequestSchema, body).success).toBe(true);
  });

  it("leaves reply_source out of a create nobody wrote replies for", async () => {
    const world = mount([]);
    await until(() => screen.getByRole("button", { name: /write a template/i }));
    fireEvent.click(screen.getByRole("button", { name: /write a template/i }));

    await until(() => screen.getByLabelText(/^name$/i));
    fireEvent.input(screen.getByLabelText(/^name$/i), { target: { value: "house voice" } });
    fireEvent.click(screen.getByRole("button", { name: /create/i }));

    const posted = () =>
      world.net.calls.find((c) => c.method === "POST" && c.url.endsWith("/notification-templates"));
    await until(() => expect(posted()).toBeDefined());
    const body = posted()!.body as Record<string, unknown>;
    expect("reply_source" in body).toBe(false);
    expect(v.safeParse(CreateNotificationTemplateRequestSchema, body).success).toBe(true);
  });

  it("⛔ clears a reply body with an explicit empty string, never by leaving it out", async () => {
    const world = mount([template({ reply_source: REPLY_SOURCE })]);
    await openEditor();
    const replyBox = await openReplies();
    expect(replyBox.value).toBe(REPLY_SOURCE);

    fireEvent.input(replyBox, { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    const patched = () => world.net.calls.find((c) => c.method === "PATCH");
    await until(() => expect(patched()).toBeDefined());
    const body = patched()!.body as Record<string, unknown>;
    // Absent would mean "unchanged" on a PATCH, and the old replies would stay.
    expect(body.reply_source).toBe("");
    expect(v.safeParse(UpdateNotificationTemplateRequestSchema, body).success).toBe(true);
  });
});
