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
