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
    // A delivery drill hands its cases over by linking here with
    // `synthetic=true`. A filter that lived only in a query string would be a
    // filter an operator cannot tell is on.
    const net = mount("?synthetic=true");
    const q = await lastQuery(net);
    expect(q.get("synthetic")).toBe("true");

    // The chip names the filter it carries and says how to take it off, so its
    // accessible name is both halves rather than a bare `×`.
    const chip = screen.getByRole("button", { name: /Remove the synthetic filter/ });
    expect(chip).toBeTruthy();
    fireEvent.click(chip);
    await until(() =>
      expect(net.to(PATH)[net.to(PATH).length - 1]!.search.get("synthetic")).toBeNull(),
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
/* What the row does NOT say                                                  */
/* -------------------------------------------------------------------------- */

describe("the alert's own state", () => {
  it("⛔ is not on the row, because the row's subject is the EPISODE", async () => {
    // The row used to wear two badges: `Firing` for what the identity is doing
    // now, and `Open`/`Ended` for this episode. On one wrap row that reads as
    // three words about two subjects — and the loudest of them, the only Tier-B
    // hue on the screen, was about the thing this list does not contain. The eye
    // went to it first, every row, and read it as the row's status.
    mount("?state=open,closed", [
      caseListItem({
        state: "closed",
        ended_at: "2026-08-09T09:30:00.000Z",
        resolve_reason: "upstream",
        alert: alertRef({ state: "firing" }),
      }),
    ]);
    await until(() => expect(screen.getByText("HighErrorRate")).toBeTruthy());

    // The episode says what it is.
    expect(screen.getByText("Ended · resolved")).toBeTruthy();
    // The identity does not, here. `/alerts`, `/alerts/:id` and the case
    // detail's "The alert" panel are where `firing` has its subject on screen.
    expect(screen.queryByText("Firing")).toBeNull();
  });
});

/* -------------------------------------------------------------------------- */
/* Folding the loaded rows by identity                                        */
/* -------------------------------------------------------------------------- */

/** Two firings of one alert plus one of another, newest first as the API serves. */
function threeFirings() {
  const disk = alertRef({ id: "alert-disk", alertname: "DiskFillingUp" });
  const errors = alertRef({ id: "alert-errors", alertname: "HighErrorRate" });
  return [
    caseListItem({ id: "c-3", seq: 9, alert: disk, started_at: "2026-08-09T12:00:00.000Z" }),
    caseListItem({ id: "c-2", seq: 4, alert: errors, started_at: "2026-08-09T11:00:00.000Z" }),
    caseListItem({
      id: "c-1",
      seq: 8,
      alert: disk,
      state: "closed",
      ended_at: "2026-08-09T10:30:00.000Z",
      resolve_reason: "upstream",
      started_at: "2026-08-09T10:00:00.000Z",
    }),
  ];
}

describe("grouping by alert", () => {
  it("⛔ sends nothing new on the wire — it is a layout, not a filter", async () => {
    const net = mount("?group=alert&state=open,closed", threeFirings());
    const q = await lastQuery(net);
    // `GET /api/v1/cases` has no `group_by`, and it must not grow one here: a
    // grouped response would need a second ordering to page over and this list
    // has exactly one indexed total order.
    expect(q.get("group")).toBeNull();
    expect(q.get("group_by")).toBeNull();
    expect(q.get("state")).toBe("open,closed");
  });

  it("carries the newest firing as the row and folds the earlier ones away", async () => {
    mount("?group=alert&state=open,closed", threeFirings());

    // One row per identity, and it is the LATEST firing that names it: `#9`,
    // not the `#8` that ended earlier the same morning.
    await until(() => expect(screen.getByText("DiskFillingUp")).toBeTruthy());
    expect(screen.getByText("#9")).toBeTruthy();
    expect(screen.queryByText("firing #8")).toBeNull();

    // The earlier one is behind a closed handle that counts it.
    const handle = screen.getByRole("button", { name: /1 earlier firing loaded/ });
    expect(handle).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(handle);
    await until(() => expect(screen.getByText("firing #8")).toBeTruthy());
  });

  it("leaves an alert with one loaded firing without a handle at all", async () => {
    // A grouped list of a healthy estate must look like the flat one, not grow a
    // column of empty disclosure triangles.
    mount("?group=alert&state=open,closed", threeFirings());
    await until(() => expect(screen.getByText("HighErrorRate")).toBeTruthy());
    expect(screen.getAllByRole("button", { name: /earlier firing/ })).toHaveLength(1);
  });

  it("⭐ says so when the queue alone gives it nothing to fold", async () => {
    // An alert has at most ONE open episode, so grouping the default view folds
    // nothing. That is worth a sentence rather than leaving the operator to
    // conclude the feature is broken — and the screen offers the press instead of
    // quietly widening what is on the wire.
    const net = mount("?group=alert", [caseListItem()]);
    await until(() => expect(net.to(PATH).length).toBeGreaterThan(0));

    expect(screen.getByText(/an alert has at most one/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Include ended" }));
    await until(() =>
      expect(net.to(PATH)[net.to(PATH).length - 1]!.search.get("state")).toBe("open,closed"),
    );
  });

  it("⛔⛔ keeps the loaded pages when the layout changes", async () => {
    // ⭐ THIS IS THE ONE THAT PAYS FOR `DECORATIVE_PARAMS`. A cursor is minted
    // under a filter set and §E.3 refuses one carried across a change, so the
    // screen resets pagination whenever its fingerprint moves. `group` never
    // reaches the wire, so it cannot have changed the keyset — and if it were in
    // the fingerprint anyway, folding the list would throw away every page the
    // operator had loaded, which is precisely the set the fold is a view of.
    const net = stubFetch({
      [`GET ${PATH}`]: (call: { search: URLSearchParams }) =>
        call.search.get("cursor") === null
          ? { json: list(threeFirings(), { has_more: true, next_cursor: "page-two" }) }
          : {
              json: list([caseListItem({ id: "c-0", seq: 1, alert: alertRef({ id: "alert-old" }) })]),
            },
    });
    renderScreen(() => <CasesRoute />, { path: "/cases?state=open,closed" });

    await until(() => expect(screen.getByRole("button", { name: /Load 100 more/ })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Load 100 more/ }));
    await until(() => expect(net.to(PATH).length).toBe(2));
    const paged = net.to(PATH).length;

    // Fold the list. The rows on screen came from two pages and must survive it.
    fireEvent.click(screen.getByRole("button", { name: /^Group/ }));
    await until(() => expect(screen.getByRole("dialog")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "By alert" }));

    await until(() => expect(screen.getByRole("button", { name: /earlier firing/ })).toBeTruthy());
    // Four cases across three identities — the second page is still folded in.
    expect(screen.getByText(/4 cases across 3 alerts/)).toBeTruthy();
    // And no request was issued at all: nothing about the query changed.
    expect(net.to(PATH).length).toBe(paged);
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
