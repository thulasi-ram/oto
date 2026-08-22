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
import type { Delivery, DeliverySummary, NotificationSuppressedReason } from "~/api/types";
import { enumValues } from "~/test/contract";
import { notification } from "~/test/fixtures";
import { expectNoUndefined, list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const SUPPRESSED = enumValues("NotificationSuppressedReason");
const PATH = "/api/v1/notifications";

function mount(
  rows: readonly ReturnType<typeof notification>[],
  page = {},
  deliveries: readonly Delivery[] = [],
): FetchStub {
  const net = stubFetch({
    [`GET ${PATH}`]: list(rows, page),
    "GET /api/v1/deliveries": list([...deliveries]),
    "POST /api/v1/deliveries": { status: 200, json: { data: deliveries[0] ?? null, meta: {} } },
  });
  renderScreen(() => <ActivitySection />);
  return net;
}

const summary = (patch: Partial<DeliverySummary> = {}): DeliverySummary => ({
  total: 1,
  sent: 0,
  failed: 0,
  dead: 0,
  skipped: 0,
  pending: 0,
  ...patch,
});

const DEAD_DELIVERY: Delivery = {
  id: "3f2a5c19-7d4b-4e88-9a10-2c6b5e4d3a21",
  notification_id: "7c3d9f0a-8b1e-4c3d-4f50-617283940516",
  channel_id: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
  channel_name: "#payments-alerts",
  mode: "post_root",
  status: "dead",
  attempts: 1,
  ambiguous: false,
  error: "slack render invalid (V14): top-level text is empty",
  error_class: "config_invalid",
  created_at: "2026-02-01T09:00:00Z",
  updated_at: "2026-02-01T09:00:05Z",
};

const retryButtons = () => screen.queryAllByRole("button", { name: "Send it again" });

/* -------------------------------------------------------------------------- */

/**
 * Picks a delivery status out of the "Where it got to" menu.
 *
 * ⛔ THIS STEP IS THE COST OF THE DROPDOWN, AND IT IS DELIBERATELY EXPLICIT. The
 * six statuses used to be a strip of toggles in the DOM at mount, so a test — and
 * a screen reader — could reach straight for the word. A closed Kobalte popover
 * renders nothing, so both open it first. What is *not* behind the menu is the
 * trigger's own summary of what is on, which is what makes the hiding acceptable
 * (see `~/components/ui/FilterMenu`).
 */
async function pickStatus(name: string): Promise<void> {
  fireEvent.click(screen.getByRole("button", { name: /^Where it got to/ }));
  await until(() => expect(screen.getByRole("dialog")).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name }));
}

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
    await pickStatus("deliberately not sent");
    await until(() => expect(screen.getByText(/No notification matches those filters/)).toBeTruthy());
    expect(screen.queryByText(/never formed a notification intent/)).toBeNull();
  });
});

/* -------------------------------------------------------------------------- */

describe("the filters", () => {
  it("sends the chosen status to the server rather than filtering on screen", async () => {
    const net = mount([notification()]);
    await until(() => expect(net.to(PATH)).toHaveLength(1));

    await pickStatus("nothing landed");

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

    await pickStatus("deliberately not sent");

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

/* -------------------------------------------------------------------------- */

describe("acting on a delivery that gave up", () => {
  it("⭐ offers the retry in the log, not only on the alert page", async () => {
    // The log is where an operator LOOKS when a delivery dies. Making them
    // navigate to the alert page to act on it was friction with nothing behind
    // it, and this is the assertion that keeps the affordance here.
    mount([notification({ status: "partial", delivery_summary: summary({ total: 2, sent: 1, dead: 1 }) })], {}, [
      DEAD_DELIVERY,
    ]);
    await until(() => expect(retryButtons()).toHaveLength(1));
    await until(() => expect(screen.getByText("#payments-alerts")).toBeTruthy());
  });

  it("⛔ offers it on a `dispatched` row whose fan-out has a dead delivery", async () => {
    /*
     * THE CASE A STATUS GATE WOULD HAVE HIDDEN, and the reason the gate is the
     * count. `AggregateStatus` returns `dispatched` whenever anything is still in
     * flight, so one dead delivery beside one pending one reads `dispatched` —
     * not `failed`, not `partial`. Gating the button on those two would have
     * looked right, passed every test anybody would think to write, and silently
     * dropped the affordance on a mixed fan-out.
     */
    mount(
      [
        notification({
          status: "dispatched",
          delivery_summary: summary({ total: 2, pending: 1, dead: 1 }),
        }),
      ],
      {},
      [DEAD_DELIVERY],
    );
    await until(() => expect(retryButtons()).toHaveLength(1));
  });

  it("⛔ makes no delivery request at all for a log with nothing dead", async () => {
    // The gate has to be free. A log page that asked for every row's fan-out
    // would be one request per row, which is the cost that kept the roll-up off
    // this list in the first place.
    const net = mount([
      notification({ status: "delivered", delivery_summary: summary({ total: 1, sent: 1 }) }),
    ]);
    await until(() => expect(screen.getByText("delivered")).toBeTruthy());
    expect(net.calls.filter((c) => c.url.includes("/deliveries"))).toHaveLength(0);
    expect(retryButtons()).toHaveLength(0);
  });

  it("⛔ shows nothing to press when the row carries no roll-up", async () => {
    // An absent `delivery_summary` says "not computed here", never "nothing was
    // sent". Reading it as an all-zero fan-out would offer a button on rows the
    // server never made a claim about.
    mount([notification({ status: "partial" })], {}, [DEAD_DELIVERY]);
    await until(() => expect(screen.getByText("some channels only")).toBeTruthy());
    expect(retryButtons()).toHaveLength(0);
  });
});
