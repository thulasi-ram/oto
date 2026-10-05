/**
 * A source's max silence (ADR 0056 §3), judged on what an operator is told next
 * to it and on what the screen sends.
 *
 * ⛔ THE FAILURE THIS FILE EXISTS FOR IS SILENT. A max silence shorter than the
 * Alertmanager's `repeat_interval` ends long-firing cases while they are still
 * firing, and nothing anywhere else says why. So the warning is asserted as
 * text, and when oto has read the source's own `repeat_interval` off its config
 * and it is the longer one, the row must say so in those numbers.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { SourcesSection } from "./SourcesSection";
import { cluster, source } from "~/test/fixtures";
import { item, list, renderScreen, stubFetch, until } from "~/test/harness";

const SOURCE_ID = "2d8e4a5b-3c6f-4d8e-9f0a-1b2c3d4e5f60";

describe("a source's max silence", () => {
  it("states the repeat_interval warning beside the field", async () => {
    stubFetch({
      "GET /api/v1/sources": list([source()]),
      "GET /api/v1/clusters": list([cluster()]),
    });
    renderScreen(() => <SourcesSection />);

    await until(() => expect(screen.getByText("Max silence (hours)")).toBeTruthy());
    expect(document.body.textContent).toMatch(/still firing/);
    // The fixture's repeat_interval is 4h, inside a day: nothing to flag.
    expect(document.body.textContent).not.toMatch(/longer than its max silence/);
  });

  it("states the default precisely: a day for a new source, off for an existing one until set", async () => {
    stubFetch({
      "GET /api/v1/sources": list([source()]),
      "GET /api/v1/clusters": list([cluster()]),
    });
    renderScreen(() => <SourcesSection />);

    await until(() => expect(screen.getByText("Max silence (hours)")).toBeTruthy());
    const text = document.body.textContent ?? "";
    expect(text).toMatch(/A source registered now starts at 1 day/);
    expect(text).toMatch(/starts\s+with it off, until someone sets it here/);
    // R1: the HA rule, and B2: the flag — neither left for the operator to guess.
    expect(text).toMatch(/off on any one source turns it off for the whole cluster/);
    expect(text).toMatch(/jobs\.expire_silent_and_removed/);
  });

  it("names the numbers when this Alertmanager repeats less often than its max silence", async () => {
    stubFetch({
      "GET /api/v1/sources": list([source({ max_silence_seconds: 86_400 }, { repeatIntervalS: 172_800 })]),
      "GET /api/v1/clusters": list([cluster()]),
    });
    renderScreen(() => <SourcesSection />);

    await until(() =>
      expect(document.body.textContent).toMatch(/longer than its max silence/),
    );
  });

  it("sends the new value in seconds", async () => {
    const net = stubFetch({
      "GET /api/v1/sources": list([source()]),
      "GET /api/v1/clusters": list([cluster()]),
    });
    net.on(`PATCH /api/v1/sources/${SOURCE_ID}`, {
      json: item(source({ max_silence_seconds: 172_800 })),
    });
    renderScreen(() => <SourcesSection />);

    await until(() =>
      expect(document.querySelector(`#source-${SOURCE_ID}-silence`)).toBeTruthy(),
    );
    const input = document.querySelector(`#source-${SOURCE_ID}-silence`) as HTMLInputElement;
    fireEvent.input(input, { target: { value: "48" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await until(() => expect(net.to(`/sources/${SOURCE_ID}`).length).toBeGreaterThan(0));
    const patch = net.to(`/sources/${SOURCE_ID}`).find((c) => c.method === "PATCH");
    expect(patch?.body).toEqual({ max_silence_seconds: 172_800 });
  });
});

/* -------------------------------------------------------------------------- */
/* What a source is doing to endings (ADR 0056 §1, ADR 0006)                  */
/* -------------------------------------------------------------------------- */

/** The fixture source with its health patched. */
function withHealth(
  patch: Partial<NonNullable<ReturnType<typeof source>["health"]>>,
  extra: Parameters<typeof source>[0] = {},
): ReturnType<typeof source> {
  const s = source(extra);
  return { ...s, health: { ...s.health!, ...patch } };
}

function mountSources(rows: readonly ReturnType<typeof source>[]): void {
  stubFetch({
    "GET /api/v1/sources": list([...rows]),
    "GET /api/v1/clusters": list([cluster()]),
  });
  renderScreen(() => <SourcesSection />);
}

describe("what a source is doing to endings", () => {
  it("warns loudly that resolves will never arrive from a send_resolved:false receiver", async () => {
    mountSources([
      withHealth({
        send_resolved: false,
        warnings: [
          {
            code: "send_resolved_false",
            message:
              "this receiver has send_resolved disabled: alerts routed to it will expire rather than resolve",
            subject: "pager-only",
          },
        ],
      }),
    ]);

    await until(() =>
      expect(document.querySelector('[data-warning="send_resolved_false"]')).toBeTruthy(),
    );
    const warning = document.querySelector('[data-warning="send_resolved_false"]')!;
    expect(warning.textContent).toMatch(/^Resolves will never arrive\. This receiver has send_resolved disabled/);
    expect(warning.textContent).toMatch(/receiver pager-only$/);
  });

  it("shows a warning whose code it does not know, under that code", async () => {
    mountSources([withHealth({ warnings: [{ code: "something_new", message: "a new fact" }] })]);
    await until(() =>
      expect(document.querySelector('[data-warning="something_new"]')?.textContent).toBe(
        "something_new. A new fact",
      ),
    );
  });

  it("says how many open cases the source is holding, and why", async () => {
    mountSources([
      withHealth({ status: "degraded" }, { open_case_count: 12, held_case_count: 12 }),
    ]);

    await until(() => expect(screen.getByText("12 open cases")).toBeTruthy());
    const held = document.querySelector("[data-held-cases]")!;
    expect(held.textContent).toBe("12 held");
    expect(held.getAttribute("title")).toMatch(/can expire while this source is degraded/);
    expect(held.getAttribute("title")).toMatch(
      /only while every live source on its cluster is healthy/,
    );
  });

  it("⛔ no longer blames a healthy source's HA sibling: an HA pair holds nothing by being one", async () => {
    // Owner ruling R1: a healthy source's count is zero, so it shows no marker.
    mountSources([source({ open_case_count: 3, held_case_count: 0 })]);
    await until(() => expect(screen.getByText("3 open cases")).toBeTruthy());
    expect(document.querySelector("[data-held-cases]")).toBeNull();
    expect(document.body.textContent).not.toMatch(/another live source/);
  });

  it("says nothing about cases the count did not reach — not counted is not none", async () => {
    mountSources([source()]);
    await until(() => expect(screen.getByText("Max silence (hours)")).toBeTruthy());
    // The max-silence help says "an open case" in prose; a count is a number.
    expect(document.body.textContent).not.toMatch(/\d+ open cases?\b/);
    expect(document.querySelector("[data-held-cases]")).toBeNull();
  });

  it("shows when a webhook batch was last accepted", async () => {
    // Five minutes back, matched by shape: the shared clock ticks every 10 s, so
    // an exact minute would race it.
    mountSources([withHealth({ last_push_at: new Date(Date.now() - 300_000).toISOString() })]);
    await until(() => expect(document.body.textContent).toMatch(/last push \d+m ago/));
    const at = document.querySelector("time[title^='Last push: ']");
    expect(at?.getAttribute("datetime")).toBeTruthy();
  });

  it("⛔ reads a push this browser's clock places in the future as `just now`, never `in 2m ago`", async () => {
    const ahead = new Date(Date.now() + 120_000).toISOString();
    mountSources([withHealth({ last_push_at: ahead, last_reconcile_at: ahead })]);
    await until(() => expect(document.body.textContent).toMatch(/last push just now/));
    expect(document.body.textContent).toMatch(/reconciled just now/);
    expect(document.body.textContent).not.toMatch(/in \d+m ago/);
    const at = document.querySelector("time[title^='Last push: ']");
    expect(at?.getAttribute("datetime")).toBe(ahead);
  });

  it("says so when no webhook batch has been accepted", async () => {
    mountSources([withHealth({ last_push_at: null })]);
    await until(() => expect(document.body.textContent).toMatch(/no webhook received yet/));
  });

  it("says what degraded covers: a failed probe, or an HA cluster that is not ready", async () => {
    mountSources([withHealth({ status: "degraded" })]);
    await until(() => expect(screen.getByText("degraded")).toBeTruthy());
    const title = screen.getByText("degraded").getAttribute("title") ?? "";
    expect(title).toMatch(/HA cluster is not ready/);
    expect(title).not.toMatch(/some reconciles are failing/);
  });
});
