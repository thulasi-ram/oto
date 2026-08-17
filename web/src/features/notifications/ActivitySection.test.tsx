/**
 * The activity log, checked on the one property it exists for.
 *
 * ⛔ A SUPPRESSED INTENT IS A ROW, WITH A SENTENCE ON IT. Everything else here —
 * the filters, the paging — is ordinary list machinery, and if it broke someone
 * would notice within a minute. The failure this suite is written against is the
 * quiet one: a log that showed only what was *sent* would render perfectly, pass
 * every layout assertion anybody would write, and answer "why did nobody hear
 * about this?" with an empty page. That is exactly the silence §B.6 forbids,
 * reproduced one layer up from the database that was careful to record it.
 *
 * The vocabulary expectations are derived from the generated contract rather
 * than re-typed, for the reason `PoliciesSection.test.tsx`'s header gives at
 * length: a hand-copied enum agrees with the screen, agrees with nothing else,
 * and passes forever while the server moves underneath both.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { ActivitySection } from "./ActivitySection";
import type { NotificationSuppressedReason } from "~/api/types";
import { enumValues } from "~/test/contract";
import { notification } from "~/test/fixtures";
import { expectNoUndefined, list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const SUPPRESSED = enumValues("NotificationSuppressedReason");
const PATH = "/api/v1/notifications";

function mount(rows: readonly ReturnType<typeof notification>[], page = {}): FetchStub {
  const net = stubFetch({ [`GET ${PATH}`]: list(rows, page) });
  renderScreen(() => <ActivitySection />);
  return net;
}

/* -------------------------------------------------------------------------- */

describe("what the log admits to", () => {
  it("⛔ puts every suppression the contract can record into words", async () => {
    mount(
      SUPPRESSED.map((reason, i) =>
        notification({
          id: `0000000${i}-0000-4000-8000-00000000000${i}`,
          status: "suppressed",
          suppressed_reason: reason as NonNullable<NotificationSuppressedReason>,
        }),
      ),
    );

    await until(() => expect(screen.getAllByText(/Not sent/)).toHaveLength(SUPPRESSED.length));

    const sentences = screen.getAllByText(/Not sent/).map((p) => p.textContent ?? "");
    for (const reason of SUPPRESSED) {
      // The wire token is never the explanation. `snoozed` is the one a map like
      // this has already lost once, and it is the one an operator most needs
      // said out loud: a silence somebody chose, not one oto imposed.
      expect(
        sentences.some((s) => s.includes(`Not sent — ${reason}.`)),
        `\`${reason}\` is rendered as its raw wire value instead of a sentence`,
      ).toBe(false);
    }
    for (const s of sentences) expect(s).toMatch(/Not sent — .+\. Recorded rather than dropped/);
    expectNoUndefined(document.body);
  });

  it("says an empty log is an answer, not a gap", async () => {
    mount([]);
    await until(() =>
      expect(screen.getByText(/oto records an intent to communicate even when/)).toBeTruthy(),
    );
  });

  it("tells a filter that matched nothing apart from a product that has said nothing", async () => {
    mount([]);
    await until(() => expect(screen.getByText(/never formed a notification intent/)).toBeTruthy());

    // Narrow to one status; the answer is now about the filter, and it must not
    // read as "oto has never notified anybody".
    fireEvent.click(screen.getByRole("button", { name: "deliberately not sent" }));
    await until(() => expect(screen.getByText(/No notification matches those filters/)).toBeTruthy());
    expect(screen.queryByText(/never formed a notification intent/)).toBeNull();
  });
});

/* -------------------------------------------------------------------------- */

describe("the filters", () => {
  it("sends the chosen status to the server rather than filtering on screen", async () => {
    const net = mount([notification()]);
    await until(() => expect(net.to(PATH)).toHaveLength(1));

    fireEvent.click(screen.getByRole("button", { name: "nothing landed" }));

    // A client-side filter over one page would be a filter that lies: it can
    // only ever narrow the fifty rows already fetched, and the row being looked
    // for is usually not in them.
    await until(() =>
      expect(net.to(PATH).some((c) => c.search.get("status") === "failed")).toBe(true),
    );
  });

  it("⛔ never carries a cursor across a filter change", async () => {
    const net = mount([notification()], { has_more: true, next_cursor: "page-two" });
    await until(() => expect(screen.getByRole("button", { name: "Load 50 more" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Load 50 more" }));
    await until(() => expect(net.to(PATH).some((c) => c.search.get("cursor") !== null)).toBe(true));

    fireEvent.click(screen.getByRole("button", { name: "deliberately not sent" }));

    // §E.3 answers a cursor minted under different filters with `400
    // cursor_filter_mismatch`, and an effect that reset it would be too late —
    // solid-query builds the request in Solid's pure phase.
    await until(() =>
      expect(net.to(PATH).some((c) => c.search.get("status") === "suppressed")).toBe(true),
    );
    for (const call of net.to(PATH)) {
      if (call.search.get("status") === null) continue;
      expect(
        call.search.get("cursor"),
        "a request paired the new filter with the previous keyset's cursor",
      ).toBeNull();
    }
  });
});
