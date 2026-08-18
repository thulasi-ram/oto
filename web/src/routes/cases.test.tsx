/**
 * The primary list, and the two things about it that are easy to get wrong in a
 * way nothing on screen shows.
 *
 * ⛔ THE DEFAULT VIEW MUST SEND `state=open`, AND IT MUST NOT SEND `open=`. An
 * episode has one axis — `alert_cases.state`, which holds `open` and `closed` —
 * and the boolean that used to spell the same thing is gone from the endpoint
 * entirely: the allow-list answers it with a `400`, so a screen that still sent
 * it would render an error page and nothing else. `state=open` is also what the
 * ack index is partial on, so the queue's shape and the fast shape are the same
 * one.
 *
 * ⛔ AND `firing`/`suppressed`/`resolved`/`expired` ARE NOT VALUES ON THAT AXIS.
 * They are what an ALERT is; sending one as a case state is a `400`.
 *
 * ⛔ AND IT MUST NEVER SEND WHAT THE ENDPOINT REFUSES. `q`, `matcher`,
 * `label[…]`, `flapping` and `snoozed` are answered with `400`, and there is no
 * `sort` parameter at all — the order is fixed at `-started_at` because a keyset
 * cursor is only sound over an indexed total order. A control for any of them
 * would be a control whose only outcome is an error page.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import CasesRoute from "./cases";
import { alertRef, caseListItem } from "~/test/fixtures";
import { item, list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const PATH = "/api/v1/cases";

function mount(search = "", rows = [caseListItem()]): FetchStub {
  const net = stubFetch({ [`GET ${PATH}`]: () => ({ json: list(rows) }) });
  renderScreen(() => <CasesRoute />, { path: `/cases${search}` });
  return net;
}

/** The query string of the last list request the screen made. */
async function lastQuery(net: FetchStub): Promise<URLSearchParams> {
  await until(() => expect(net.to(PATH).length).toBeGreaterThan(0));
  const calls = net.to(PATH);
  return calls[calls.length - 1]!.search;
}

/* -------------------------------------------------------------------------- */
/* What the queue asks for                                                    */
/* -------------------------------------------------------------------------- */

describe("the default view is the queue", () => {
  it("⭐ sends `state=open`, the episode axis, and never the deleted `open=`", async () => {
    const net = mount();
    const q = await lastQuery(net);

    expect(q.get("state"), "the live queue is `state=open` and nothing else").toBe("open");
    expect(q.get("open"), "`open=` was deleted from the endpoint's allow-list").toBeNull();
  });

  it("⛔ never sends an ALERT state as a case state", async () => {
    // A case is `open` or `closed`. `firing` and `suppressed` are facts about
    // the identity — a silence mutes a label set, not one episode of it — and
    // the allow-list refuses them here, so a chip offering one would be a
    // control whose only outcome is an error page.
    const net = mount("?state=firing,suppressed");
    const q = await lastQuery(net);
    expect(q.get("state")).toBe("open");
  });

  it("⛔ sends none of the parameters the endpoint answers with a 400", async () => {
    const net = mount();
    const q = await lastQuery(net);

    for (const refused of ["q", "matcher", "flapping", "snoozed", "sort", "open"]) {
      expect(q.get(refused), `\`${refused}\` is refused by GET /cases`).toBeNull();
    }
    for (const key of [...q.keys()]) {
      expect(key.startsWith("label["), "a label selector is refused by GET /cases").toBe(false);
    }
  });

  it("⛔ offers no sort control, because the order is fixed and there is no parameter", async () => {
    mount();
    await until(() => expect(screen.getByText(/case/)).toBeTruthy());
    expect(screen.queryByLabelText(/^sort$/i)).toBeNull();
    expect(screen.queryByRole("button", { name: /sort/i })).toBeNull();
  });
});

/* -------------------------------------------------------------------------- */
/* The filters that ARE offered                                               */
/* -------------------------------------------------------------------------- */

describe("the filters the bar surfaces", () => {
  it("puts the acknowledgement facet on the wire, which is what this list is for", async () => {
    const net = mount("?ack=unacked");
    const q = await lastQuery(net);
    // ⭐ THIS IS THE ONE LIST THAT CAN BE FILTERED BY ACKNOWLEDGEMENT. `alerts`
    // carries no ack column, because a receipt belongs to the firing it was
    // given for and the identity outlives that firing.
    expect(q.get("ack")).toBe("unacked");
    expect(q.get("state")).toBe("open");
  });

  it("carries severity and the episode state through to the request", async () => {
    const net = mount("?severity=critical,warning&state=closed");
    const q = await lastQuery(net);
    expect(q.get("severity")).toBe("critical,warning");
    expect(q.get("state")).toBe("closed");
  });

  it("asks for both states only when the operator names both", async () => {
    // Naming both is the same as omitting the parameter, which is precisely why
    // the screen never omits it: absence would mean "everything in retention",
    // and that is a question nobody arrived with.
    const net = mount("?state=open,closed");
    const q = await lastQuery(net);
    expect(q.get("state")).toBe("open,closed");
  });

  it("⭐ shows a narrowing it has no control for, with its way off", async () => {
    // A group hands its cases over by linking here with `group_id`. A filter
    // that lived only in a query string would be a filter an operator cannot
    // tell is on.
    const net = mount("?group_id=0f1e2d3c-4b5a-4697-8899-aabbccddeeff");
    const q = await lastQuery(net);
    expect(q.get("group_id")).toBe("0f1e2d3c-4b5a-4697-8899-aabbccddeeff");

    // The chip names the filter it carries and says how to take it off, so its
    // accessible name is both halves rather than a bare `×`.
    const chip = screen.getByRole("button", { name: /Remove the group filter/ });
    expect(chip).toBeTruthy();
    fireEvent.click(chip);
    await until(() =>
      expect(net.to(PATH)[net.to(PATH).length - 1]!.search.get("group_id")).toBeNull(),
    );
  });
});

/* -------------------------------------------------------------------------- */
/* Rows and the one gesture on them                                           */
/* -------------------------------------------------------------------------- */

describe("a row", () => {
  it("renders the alert's fields off `alert`, which the server batch-loads", async () => {
    mount("", [
      caseListItem({ alert: alertRef({ alertname: "DiskFillingUp", namespace: "storage" }) }),
    ]);
    await until(() => expect(screen.getByText("DiskFillingUp")).toBeTruthy());
    expect(screen.getByText(/namespace storage/)).toBeTruthy();
  });

  it("acknowledges through the CASE's own endpoint, with one idempotency key", async () => {
    const net = mount("", [caseListItem({ id: "case-1" })]);
    net.on("POST /api/v1/cases/case-1/ack", () => ({ json: item(caseListItem()) }));

    await until(() =>
      expect(screen.getByRole("button", { name: "Acknowledge HighErrorRate" })).toBeTruthy(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Acknowledge HighErrorRate" }));

    await until(() => expect(net.to("/ack")).toHaveLength(1));
    expect(net.to("/ack")[0]?.path).toBe("/api/v1/cases/case-1/ack");
    expect(net.to("/ack")[0]?.headers["Idempotency-Key"]).toBeTruthy();
  });

  it("⭐ turns into the way back once the firing carries a receipt", async () => {
    // ONE control with two words. `ack_state` has two values, so a separate
    // Acknowledge and Withdraw would leave one of the two dead on every row —
    // and the dead one was the enabled-looking half of the pair a tired operator
    // reads first.
    mount("", [caseListItem({ ack_state: "acked" })]);
    const name = "Withdraw the acknowledgement of HighErrorRate";
    await until(() => expect(screen.getByRole("button", { name })).toBeTruthy());
    expect(screen.getByRole("button", { name })).not.toBeDisabled();
    expect(screen.queryByRole("button", { name: "Acknowledge HighErrorRate" })).toBeNull();
  });

  it("withdraws through the CASE's own unack when that is the direction it is in", async () => {
    const net = mount("", [caseListItem({ id: "case-1", ack_state: "acked" })]);
    net.on("POST /api/v1/cases/case-1/unack", () => ({ json: item(caseListItem()) }));

    const name = "Withdraw the acknowledgement of HighErrorRate";
    await until(() => expect(screen.getByRole("button", { name })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name }));

    await until(() => expect(net.to("/unack")).toHaveLength(1));
    expect(net.to("/unack")[0]?.path).toBe("/api/v1/cases/case-1/unack");
    expect(net.to("/unack")[0]?.headers["Idempotency-Key"]).toBeTruthy();
    // ⛔ And nothing was acknowledged on the way: the direction is read at the
    // click, not captured when the row rendered.
    expect(net.to("/ack")).toHaveLength(0);
  });

  it("⛔ shows the control on a row it cannot be used on, disabled and explained", async () => {
    // §0.4: never hover-revealed, and never absent. A disabled button that says
    // why in its tooltip is information; a button that appears and disappears as
    // the list re-sorts is a 3am misclick generator. An ENDED case is the one
    // state in which neither direction is possible.
    mount("?state=open,closed", [
      caseListItem({
        ended_at: "2026-08-09T09:30:00.000Z",
        state: "closed",
        resolve_reason: "upstream",
      }),
    ]);
    await until(() =>
      expect(screen.getByRole("button", { name: "Acknowledge HighErrorRate" })).toBeTruthy(),
    );
    expect(screen.getByRole("button", { name: "Acknowledge HighErrorRate" })).toBeDisabled();
  });
});

/* -------------------------------------------------------------------------- */
/* Nothing to show                                                            */
/* -------------------------------------------------------------------------- */

describe("an empty list", () => {
  it("⭐ reads as an answer when nothing is narrowed, not as a failed search", async () => {
    mount("", []);
    await until(() => expect(screen.getByText("Nothing is firing.")).toBeTruthy());
  });

  it("blames the filters when there are filters to blame", async () => {
    mount("?ack=acked", []);
    await until(() =>
      expect(screen.getByText("No cases match these filters.")).toBeTruthy(),
    );
  });
});
