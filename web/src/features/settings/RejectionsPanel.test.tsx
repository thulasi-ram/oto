/**
 * The panel that answers "my alert never appeared".
 *
 * Three things are worth a test here and nothing else is. The reason filter is
 * an enum-backed control, and this repo has already shipped one screen whose
 * hand-copied enum drifted from the contract — so the list it offers is measured
 * against the generated contract rather than against a second copy of it. The
 * empty feed is the panel's most important sentence, because "nothing was
 * refused" is the finding that redirects a diagnosis, and a blank box says the
 * opposite. And a `reason` the wrapper failed to put on the wire would return an
 * unfiltered page that looks filtered.
 *
 * A fourth was added after the fact: that same most-important sentence has to
 * reach a screen reader, and the way it fails to is invisible to `getByText`.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { RejectionsPanel } from "./RejectionsPanel";
import { RejectionReasonSchema } from "~/api/generated/validators";
import { enumValues } from "~/test/contract";
import { expectNoUndefined, list, renderScreen, stubFetch, until } from "~/test/harness";

const SOURCE_ID = "0192f3a1-0000-7000-8000-000000000001";

/** Open the panel over an empty feed and an empty batch list. */
function mount(): ReturnType<typeof stubFetch> {
  const stub = stubFetch({
    [`GET /api/v1/sources/${SOURCE_ID}/rejections`]: list([]),
    [`GET /api/v1/sources/${SOURCE_ID}/failed-batches`]: list([]),
  });
  renderScreen(() => <RejectionsPanel sourceID={SOURCE_ID} />);
  fireEvent.click(screen.getByRole("button", { name: "Why an alert never appeared" }));
  return stub;
}

describe("the reason filter", () => {
  it("offers every reason the contract publishes, each in words rather than as a wire token", async () => {
    mount();
    const group = await screen.findByRole("group", { name: "Reason" });

    const labels = within(group)
      .getAllByRole("button")
      .map((el) => el.textContent?.trim() ?? "");

    // The picklist the control is built from is the contract's own, so the count
    // is derived twice from the same place and never typed here.
    expect([...RejectionReasonSchema.options]).toEqual([...enumValues("RejectionReason")]);
    expect(labels).toHaveLength(enumValues("RejectionReason").length);
    for (const label of labels) {
      expect(label).not.toBe("");
      // `too_many_labels` on screen is the drift symptom, not a label.
      expect(label).not.toContain("_");
    }
    expectNoUndefined(group as HTMLElement);
  });

  it("puts the chosen reason on the wire, comma-joined as the contract asks", async () => {
    const stub = mount();
    const group = await screen.findByRole("group", { name: "Reason" });

    fireEvent.click(within(group).getAllByRole("button")[0]!);

    await until(() => {
      const sent = stub.to("/rejections").map((c) => c.search.get("reason"));
      expect(sent).toContain(enumValues("RejectionReason")[0]);
    });
  });
});

describe("an empty feed", () => {
  it("reads as an answer about this source, not as an absence of data", async () => {
    mount();

    // Both halves say what their silence means, because a blank box would let an
    // operator conclude the opposite of what oto actually knows.
    await until(() =>
      expect(screen.getByText(/oto has never refused anything from this source/i)).toBeTruthy(),
    );
    await until(() =>
      expect(screen.getByText(/every batch from this source finished/i)).toBeTruthy(),
    );
    expectNoUndefined(document.body);
  });

  it("is announced by a region that was already there, not by one that arrives holding it", async () => {
    mount();

    // A live region that enters the DOM in the same mutation as its words is
    // commonly announced by nothing at all — the panel's whole finding, said to
    // no one. So both halves mount their region while the answer is still in
    // flight, and it is those same nodes that carry it when it lands: were
    // either half remounting, the node held here would go stale on "Loading…"
    // while a fresh one took its place.
    const regions = Array.from(document.querySelectorAll("[aria-live]"));
    expect(regions.map((el) => el.textContent)).toEqual(["Loading…", "Loading…"]);

    await until(() => {
      expect(regions[0]!.textContent).toMatch(/never refused anything from this source/i);
      expect(regions[1]!.textContent).toMatch(/every batch from this source finished/i);
    });
    for (const el of regions) expect(el.isConnected).toBe(true);
    expect(document.querySelectorAll("[aria-live]")).toHaveLength(regions.length);
  });
});
