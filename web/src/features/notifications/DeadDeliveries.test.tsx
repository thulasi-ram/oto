/**
 * The retry that had a server, a client wrapper, and nobody to press it.
 *
 * `POST /api/v1/deliveries/{id}/retry` has existed since v1 and
 * `~/api/endpoints`'s `retryDelivery` has wrapped it for almost as long; grep for
 * a caller and there was exactly one hit, the definition itself. So a delivery
 * that gave up was a permanent fact in the UI: the panel said "nobody was told
 * through that channel" and then offered nothing to do about it, and the only way
 * to send the message was `curl`.
 *
 * The suite is mounted through `DeliveryPanel` rather than over the component
 * alone, because the interesting half is the GATE. A retry offered on a live
 * delivery is a `412 delivery_not_dead`, and a button that 409s or 412s is worse
 * than no button: it teaches an operator that the screen guesses.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { DeliveryPanel } from "~/features/alerts/detail/DeliveryPanel";
import type { Delivery, DeliverySummary, Notification } from "~/api/types";
import { notification } from "~/test/fixtures";
import { list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const NOTIFICATION_ID = "7c3d9f0a-8b1e-4c3d-4f50-617283940516";
const DELIVERY_ID = "3f2a5c19-7d4b-4e88-9a10-2c6b5e4d3a21";
const CHANNEL_ID = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d";
const DELIVERIES = "/api/v1/deliveries";

const summary = (patch: Partial<DeliverySummary> = {}): DeliverySummary => ({
  total: 1,
  sent: 0,
  failed: 0,
  dead: 0,
  skipped: 0,
  pending: 0,
  ...patch,
});

const deadDelivery = (patch: Partial<Delivery> = {}): Delivery => ({
  id: DELIVERY_ID,
  notification_id: NOTIFICATION_ID,
  channel_id: CHANNEL_ID,
  channel_name: "#payments-alerts",
  mode: "post_root",
  status: "dead",
  attempts: 1,
  ambiguous: false,
  error: "slack render invalid (V14): top-level text is empty",
  error_class: "config_invalid",
  created_at: "2026-02-01T09:00:00Z",
  updated_at: "2026-02-01T09:00:05Z",
  ...patch,
});

function mount(n: Notification, rows: readonly Delivery[] = [deadDelivery()]): FetchStub {
  const net = stubFetch({ [`GET ${DELIVERIES}`]: list(rows) });
  renderScreen(() => (
    <DeliveryPanel
      notifications={[n]}
      summary={n.delivery_summary ?? null}
      loading={false}
      error={null}
    />
  ));
  return net;
}

const retryButton = () => screen.queryAllByRole("button", { name: "Send it again" });

/* -------------------------------------------------------------------------- */

describe("retrying a delivery that gave up", () => {
  it("offers the retry, and posts it once with an idempotency key", async () => {
    const net = mount(
      notification({ id: NOTIFICATION_ID, status: "failed", delivery_summary: summary({ dead: 1 }) }),
    );
    net.on(`POST ${DELIVERIES}/${DELIVERY_ID}/retry`, {
      json: { data: deadDelivery({ status: "pending" }), meta: { request_id: "r" } },
    });

    await until(() => expect(retryButton()).toHaveLength(1));
    fireEvent.click(retryButton()[0]!);

    await until(() => expect(net.to("/retry")).toHaveLength(1));
    const posts = net.to("/retry");
    expect(posts[0]?.path).toBe(`${DELIVERIES}/${DELIVERY_ID}/retry`);
    // The server calls itself "already safe to repeat" and still requires the
    // header; a double-click must not become two sends.
    expect(posts[0]?.headers["Idempotency-Key"]).toBeTruthy();
  });

  it("⛔ asks only for dead rows, because anything else is a 412", async () => {
    const net = mount(
      notification({ id: NOTIFICATION_ID, status: "failed", delivery_summary: summary({ dead: 1 }) }),
    );

    await until(() => expect(net.to("/deliveries")).toHaveLength(1));
    const [q] = net.to("/deliveries");
    expect(q?.search.get("status")).toBe("dead");
    expect(q?.search.get("notification_id")).toBe(NOTIFICATION_ID);
  });

  it("names the channel and the error class, so a retry is a judgement and not a reflex", async () => {
    mount(
      notification({ id: NOTIFICATION_ID, status: "failed", delivery_summary: summary({ dead: 1 }) }),
    );

    await until(() => expect(retryButton()).toHaveLength(1));
    expect(screen.getByText("#payments-alerts")).toBeTruthy();
    // `auth_expired` and `config_invalid` need different things done first, and
    // the button cannot tell them apart on the operator's behalf.
    expect(screen.getByText("config_invalid")).toBeTruthy();
  });
});

describe("what the panel refuses to offer", () => {
  it("offers nothing, and asks for nothing, when no delivery gave up", async () => {
    const net = mount(
      notification({ id: NOTIFICATION_ID, delivery_summary: summary({ sent: 1 }) }),
      [],
    );

    await until(() => expect(screen.getAllByText("sent")).not.toHaveLength(0));
    expect(retryButton()).toHaveLength(0);
    // The ids cost a request. A healthy fan-out must not pay for one.
    expect(net.to("/deliveries")).toHaveLength(0);
  });

  it("offers nothing while the intent carries no fan-out roll-up at all", async () => {
    // `delivery_summary` is optional on the list schema — absent means "not
    // computed here", which is not the same as a fan-out of zero, and a control
    // must never be invented out of an unknown.
    const net = mount(notification({ id: NOTIFICATION_ID }), []);

    await until(() => expect(screen.getAllByText(/last moved/)).toHaveLength(1));
    expect(retryButton()).toHaveLength(0);
    expect(net.to("/deliveries")).toHaveLength(0);
  });
});
