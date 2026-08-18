/**
 * The banners that tell an operator whether a calm list is evidence of calm.
 *
 * Two of them are queries the shell reads — a source oto cannot reach, and a
 * quiet period oto is holding. The third, `ResyncBanner`, lives in `AppShell` and
 * is driven by the live stream instead; it is tested here because what all three
 * share is `ShellBanner`, and the property that matters most is a property of the
 * strip rather than of the fact behind it.
 *
 * ⛔ THE PROPERTY UNDER TEST IS SYMMETRIC, AND ONLY ONE HALF OF IT IS OBVIOUS.
 * A banner that shows its content on the degraded path is easy. A banner that is
 * genuinely *absent* on the healthy path is the half that decays: it is what
 * keeps the header one flat line, and it is what stops the strip from becoming
 * the chrome everybody has learned to read past. So every case here settles both
 * requests first — otherwise "absent" would only mean "has not loaded yet",
 * which is a test that passes for the wrong reason forever.
 *
 * A third property sits between them and is invisible to both: whether the
 * strip is ever actually *spoken*. A live region that arrives already holding
 * its words is one screen readers commonly never announce, so "the text is on
 * screen" and "the operator was told" are different claims, and only the second
 * one is what these strips are for. It is asserted the only way jsdom can — by
 * node identity, holding the region before the news and requiring the news to
 * land in that same node.
 *
 * The last describe is the one ADR 0012 exists for: neither banner may reach for
 * a Tier-B state hue. Losing sight of a source and holding a notification are
 * facts about oto, not the state of an alert, and spending a state colour on them
 * would blunt the scarcity that makes a firing row loud.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it, vi } from "vitest";

import { SnoozeBanner, SourceReachBanner } from "./ShellBanner";
import { LiveProvider } from "~/api/live";
import { qk } from "~/api/keys";
import { ResyncBanner } from "~/components/AppShell";
import {
  expectNoUndefined,
  flush,
  item,
  list,
  renderScreen,
  stubFetch,
  until,
} from "~/test/harness";
import { frame, snooze, source, sse, statsOverview } from "~/test/fixtures";
import type { QueryClient } from "@tanstack/solid-query";
import type { ActiveSnooze, Source } from "~/api/types";
import type { FetchStub } from "~/test/harness";

/* -------------------------------------------------------------------------- */
/* Fixtures the shipped ones do not cover                                     */
/* -------------------------------------------------------------------------- */

/** `source()` is healthy by construction; this is the same source, unreached. */
function unreachable(name: string, id: string, lastError: string): Source {
  const s = source({ id, name });
  return {
    ...s,
    health: { ...s.health!, source_id: id, status: "unreachable", last_error: lastError },
  };
}

function activeSnooze(id: string, alertname: string, by = "Ada Lovelace"): ActiveSnooze {
  return {
    ...snooze({ id, snoozed_by_label: by }),
    snoozed_until: new Date(Date.now() + 3_600_000).toISOString(),
    alert_id: `alert-of-${id}`,
    alert_key: `key-of-${id}`,
    alert: {
      id: `alert-of-${id}`,
      alert_key: `key-of-${id}`,
      alertname,
      cluster_key: "prod-eu",
      state: "firing",
    },
    remaining_seconds: 3600,
  };
}

/* -------------------------------------------------------------------------- */
/* Mounting                                                                   */
/* -------------------------------------------------------------------------- */

interface World {
  readonly sources?: readonly Source[];
  /** The org has more sources than the page above — `page.has_more` on `GET /sources`. */
  readonly moreSources?: boolean;
  /**
   * `sources.unreachable` on the dashboard roll-up: the org-wide count, over
   * sources the page above may not contain.
   *
   * It defaults to zero rather than to something interesting, because a world
   * that does not say otherwise is a world where the roll-up must never be read.
   */
  readonly overviewUnreachable?: number;
  readonly snoozes?: readonly ActiveSnooze[];
  readonly hasMore?: boolean;
}

interface Mounted {
  readonly stub: FetchStub;
  readonly client: QueryClient;
}

/**
 * Mount both banners over one route table, exactly as the shell mounts them.
 *
 * Together rather than separately on purpose: they share a header, and "the
 * header is empty" is a claim about both of them at once.
 */
function mount(world: World = {}): Mounted {
  const stub = stubFetch({
    "GET /api/v1/sources": list(world.sources ?? [source()], {
      has_more: world.moreSources ?? false,
    }),
    // Registered in every world, so "the strip did not ask for it" is an
    // assertion a test can make rather than an unrouted call that throws. The
    // strip is only allowed to read it when `GET /sources` said `has_more`, and
    // the cases below assert both halves of that.
    "GET /api/v1/stats/overview": item(
      statsOverview({
        sources: {
          healthy: 0,
          degraded: 0,
          unreachable: world.overviewUnreachable ?? 0,
          unknown: 0,
        },
      }),
    ),
    "GET /api/v1/snoozes": list(world.snoozes ?? [], { has_more: world.hasMore ?? false }),
  });

  const { client } = renderScreen(() => (
    <>
      <SourceReachBanner />
      <SnoozeBanner />
    </>
  ));

  return { stub, client };
}

/** Both queries have answered, so anything still absent is absent on purpose. */
async function settled(stub: FetchStub): Promise<void> {
  await until(() => {
    expect(stub.to("/api/v1/sources").length).toBeGreaterThan(0);
    expect(stub.to("/api/v1/snoozes").length).toBeGreaterThan(0);
  });
  await flush();
}

const text = (): string => document.body.textContent ?? "";

/* -------------------------------------------------------------------------- */
/* The healthy path says nothing at all                                       */
/* -------------------------------------------------------------------------- */

// Nothing to reset between cases. The read set is `SnoozeBanner`'s own signal
// now that the shell is a layout route rather than a per-route wrapper, so it
// dies with the mount and one case's Dismiss cannot silence the next one's strip.
// It used to be module state with an exported reset hook, and both existed only
// because `AppShell` was rebuilt on every navigation.

describe("a healthy org", () => {
  it("renders no visible strip, nothing to dismiss, and announces nothing", async () => {
    const { stub } = mount({ sources: [source()], snoozes: [] });
    await settled(stub);

    expect(text()).not.toContain("cannot reach");
    expect(text()).not.toContain("holding notifications");
    expect(screen.queryByRole("button", { name: "Dismiss" })).toBeNull();
    expect(document.querySelectorAll("li")).toHaveLength(0);

    // And it cost exactly the two requests the two strips need: an org whose
    // page of sources IS the org has nothing to learn from the dashboard
    // roll-up, so it never asks for it.
    expect(stub.to("/stats/overview")).toHaveLength(0);

    // ⛔ WHAT IS MOUNTED HERE, AND WHY IT IS STILL "NOTHING". The announcement
    // region has to pre-date the news or the news is announced to no one, so it
    // is here — holding nothing, wearing nothing. Both halves are the
    // requirement: empty means a healthy org is never announced however often
    // the shell re-renders, and unstyled means zero pixels, because a *dressed*
    // empty strip would still cost the header a border and the table below an
    // offset. That is the whole reason the strip is not simply permanent.
    const region = document.querySelector("[aria-live]");
    expect(region).not.toBeNull();
    expect(region!.textContent).toBe("");
    expect(region!.getAttribute("class") ?? "").toBe("");
  });

  it("says nothing for a source that is merely degraded", async () => {
    // `degraded` is not what blocks the reaper; the third consecutive failure is.
    // A strip that fired on the first transient timeout would be ignorable inside
    // a week, and then useless on the day it mattered.
    const s = source();
    const { stub } = mount({
      sources: [{ ...s, health: { ...s.health!, status: "degraded", consecutive_failures: 1 } }],
    });
    await settled(stub);

    expect(text()).not.toContain("cannot reach");
    expect(document.querySelector("[aria-live]")!.textContent).toBe("");
  });
});

/* -------------------------------------------------------------------------- */
/* A source oto cannot reach                                                  */
/* -------------------------------------------------------------------------- */

describe("an unreachable source", () => {
  it("names it, says the cases are held, and cannot be dismissed", async () => {
    const { stub } = mount({
      sources: [unreachable("prod-eu alertmanager", "src-1", "dial tcp: i/o timeout")],
    });
    await settled(stub);

    await until(() => expect(text()).toContain("cannot reach"));
    expect(text()).toContain("prod-eu alertmanager");
    // The whole point of the strip: quiet is not the same as blind.
    expect(text()).toContain("never resolved, never expired");
    expect(text()).toContain("may be one oto can no longer see");

    // One polite announcement, and no way to make it go away while it is true.
    // (That it is announced *at all* is the case below; here it is only that the
    // words landed in the region rather than beside it.)
    const regions = document.querySelectorAll("[aria-live]");
    expect(regions).toHaveLength(1);
    expect(regions[0]!.textContent).toContain("cannot reach");
    expect(screen.queryByRole("button", { name: "Dismiss" })).toBeNull();

    // The upstream error is one hover away rather than on screen.
    expect(screen.getByTitle("dial tcp: i/o timeout")).toBeTruthy();
    expectNoUndefined(document.body);
  });

  it("aggregates into ONE strip, however many sources are out", async () => {
    const { stub } = mount({
      sources: [
        unreachable("am-eu", "src-1", "i/o timeout"),
        unreachable("am-us", "src-2", "connection refused"),
        unreachable("am-ap", "src-3", "no route to host"),
        unreachable("am-sa", "src-4", "TLS handshake failure"),
        source({ id: "src-5", name: "am-well" }),
      ],
    });
    await settled(stub);

    await until(() => expect(text()).toContain("cannot reach"));
    // Four unreachable sources, still one strip: a strip per source would stack
    // until it pushed the alert table off screen. One region, therefore one
    // announcement, not four.
    expect(document.querySelectorAll("[aria-live]")).toHaveLength(1);
    expect(document.querySelectorAll("p")).toHaveLength(1);

    // Three named, and the fourth is counted rather than quietly dropped — the
    // strip leads with the number the moment it cannot spell the whole of it out.
    expect(text()).toContain("oto cannot reach 4 sources");
    expect(text()).toContain("of which it can name 3");
    expect(text()).toContain("am-eu");
    expect(text()).toContain("am-us");
    expect(text()).toContain("am-ap");
    expect(text()).not.toContain("am-sa");
    expect(text()).not.toContain("am-well");
    // Plural copy, because four sources are not "it".
    expect(text()).toContain("Their");
  });

  it("costs one request while the page it read was the whole org", async () => {
    // ⛔ THE GATE IS THE DESIGN, AND THIS IS THE HALF THAT DECAYS. `GET /sources`
    // came back with `has_more: false`, so the page IS the org and there is
    // nothing the roll-up could add. It is not asked — not once, and not every
    // sixty seconds — because it computes twenty-six columns across five tables,
    // including a `notification_deliveries` scan with two joins, to be read for
    // one of them.
    const { stub } = mount({ sources: [unreachable("am-eu", "src-1", "i/o timeout")] });
    await settled(stub);
    await until(() => expect(text()).toContain("cannot reach"));

    expect(stub.to("/stats/overview")).toHaveLength(0);
  });

  it("raises the strip for an unreachable source that sorts past the first page", async () => {
    // ⛔⛔ THE DEFECT THIS STRIP WAS SILENT ON. `GET /sources` is keyset with a
    // default `limit` of 50 (§E.1), so an org whose only unreachable source sorts
    // 51st used to get no strip — and could not tell that it was looking at part
    // of a list. An operator then read a held, frozen alert list as calm, which is
    // the one failure the §B.4 guard's only surface must not have.
    //
    // The page here is fifty healthy sources and `has_more`. The org-wide count
    // comes off the roll-up, whose source CTE now joins `alert_sources` and
    // filters `deleted_at IS NULL` — without that it would count sources the
    // operator deleted and pin this strip open forever.
    const wholePage = Array.from({ length: 50 }, (_, i) =>
      source({ id: `src-${i}`, name: `am-${i}` }),
    );
    const { stub } = mount({ sources: wholePage, moreSources: true, overviewUnreachable: 1 });
    await settled(stub);

    await until(() => expect(stub.to("/stats/overview").length).toBeGreaterThan(0));
    await until(() => expect(text()).toContain("cannot reach"));

    // One source, no name for it, and the copy says exactly that rather than
    // implying the strip has enumerated anything.
    expect(text()).toContain("oto cannot reach 1 source, which it cannot name here");
    expect(text()).toContain("never resolved, never expired");
    expect(screen.getByRole("link", { name: "Source health" })).toBeTruthy();
    // Singular copy: one source is "it", however the count was arrived at.
    expect(text()).toContain("Its");
    expectNoUndefined(document.body);

    // Announced through the one region, as every other strip is.
    const regions = document.querySelectorAll("[aria-live]");
    expect(regions).toHaveLength(1);
    expect(regions[0]!.textContent).toContain("cannot reach");
  });

  it("names what it saw and counts what it did not, without conflating the two", async () => {
    // ⛔ THE STRIP MAY NEVER CLAIM A SOURCE IT CANNOT NAME WITHOUT SAYING SO. The
    // page in hand holds one unreachable source; the org has three. Naming one
    // and stopping would understate the outage, and saying "three" beside one
    // name would read as if the other two were listed somewhere on this line.
    const page = [
      unreachable("am-eu", "src-1", "i/o timeout"),
      ...Array.from({ length: 49 }, (_, i) => source({ id: `src-${i + 2}`, name: `am-${i}` })),
    ];
    const { stub } = mount({ sources: page, moreSources: true, overviewUnreachable: 3 });
    await settled(stub);

    await until(() => expect(text()).toContain("cannot reach"));
    expect(text()).toContain("oto cannot reach 3 sources");
    expect(text()).toContain("of which it can name 1");
    expect(text()).toContain("am-eu");
    // Plural copy, because the count is what the sentence is about.
    expect(text()).toContain("Their");
    // The one source it did see keeps its upstream error, one hover away.
    expect(screen.getByTitle("i/o timeout")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Source health" })).toBeTruthy();
    expectNoUndefined(document.body);
  });

  it("never says less than it has seen for itself", async () => {
    // The roll-up is the org-wide authority, not a veto. While it is in flight —
    // or if it somehow answers with fewer than the page already shows — the page
    // in hand is still evidence, and the strip states what it has seen.
    const page = [
      unreachable("am-eu", "src-1", "i/o timeout"),
      unreachable("am-us", "src-2", "connection refused"),
      ...Array.from({ length: 48 }, (_, i) => source({ id: `src-${i + 3}`, name: `am-${i}` })),
    ];
    const { stub } = mount({ sources: page, moreSources: true, overviewUnreachable: 0 });
    await settled(stub);

    await until(() => expect(text()).toContain("cannot reach"));
    expect(text()).toContain("am-eu");
    expect(text()).toContain("am-us");
    expect(text()).not.toContain("cannot name");
  });
});

/* -------------------------------------------------------------------------- */
/* Whether the strip is ever actually spoken                                  */
/* -------------------------------------------------------------------------- */

describe("how a strip reaches a screen reader", () => {
  it("announces through a region that was already there, not one that arrives holding the news", async () => {
    // ⛔ THE ASSERTION IS ABOUT NODE IDENTITY, AND NOTHING ELSE WOULD CATCH THIS.
    // Assistive technology speaks mutations inside a region that already existed;
    // a region that enters the DOM already holding its words is commonly spoken
    // by nothing at all. jsdom has no announcement model, so `getByRole` and
    // `getByText` pass under either shape — what separates them is whether the
    // node holding the sentence is the node that was mounted before it.
    //
    // So the region is taken here while the source list is still in flight, and
    // it is THAT reference the sentence has to turn up in. Under the old shape —
    // a `<Show>` around the whole strip — the reference would not exist at all,
    // and under any future one that remounts the strip it would go stale while a
    // fresh sibling took the words.
    const { stub } = mount({ sources: [unreachable("am-eu", "src-1", "i/o timeout")] });

    const region = document.querySelector("[aria-live]");
    expect(region).not.toBeNull();
    expect(region!.textContent).toBe("");

    await settled(stub);
    await until(() => expect(region!.textContent).toContain("cannot reach"));

    // The same node, still in the document, and still the only one.
    expect(region!.isConnected).toBe(true);
    expect(document.querySelectorAll("[aria-live]")).toHaveLength(1);

    // Polite and whole: read once, read complete, and never over the top of
    // whatever the operator is already hearing. `aria-relevant` is left at its
    // default — text arriving is already covered by it, and the strip leaving is
    // not news worth speaking.
    expect(region!.getAttribute("aria-live")).toBe("polite");
    expect(region!.getAttribute("aria-atomic")).toBe("true");
    expect(region!.hasAttribute("aria-relevant")).toBe(false);
  });

  it("goes quiet again by emptying that same region rather than dropping it", async () => {
    // The other half. Health returns, the strip must leave the screen, and the
    // region must NOT leave with it: the next outage needs somewhere to be
    // announced. A region recreated per outage is the original bug on a delay.
    const { stub, client } = mount({ sources: [unreachable("am-eu", "src-1", "i/o timeout")] });
    const region = document.querySelector("[aria-live]")!;
    await settled(stub);
    await until(() => expect(region.textContent).toContain("cannot reach"));

    stub.on("GET /api/v1/sources", list([source({ id: "src-1", name: "am-eu" })]));
    await client.invalidateQueries({ queryKey: qk.settings.sources() });

    await until(() => expect(region.textContent).toBe(""));
    expect(region.isConnected).toBe(true);
    // Empty *and* undressed, so the header goes back to one flat line.
    expect(region.getAttribute("class") ?? "").toBe("");
    expect(text()).not.toContain("cannot reach");
  });
});

/* -------------------------------------------------------------------------- */
/* Every quiet period in force                                                */
/* -------------------------------------------------------------------------- */

describe("active snoozes", () => {
  it("enumerates each one with who asked and when it resumes", async () => {
    const { stub } = mount({
      snoozes: [
        activeSnooze("s1", "HighErrorRate", "Ada Lovelace"),
        activeSnooze("s2", "DiskFillingUp", "Grace Hopper"),
      ],
    });
    await settled(stub);

    await until(() => expect(text()).toContain("holding notifications on"));
    expect(text()).toContain("2 alerts");
    expect(text()).toContain("HighErrorRate");
    expect(text()).toContain("DiskFillingUp");
    expect(text()).toContain("Ada Lovelace");
    expect(text()).toContain("Grace Hopper");
    expect(document.querySelectorAll("li")).toHaveLength(2);

    // The copy must not imply the alerts themselves went calm.
    expect(text()).toContain("still firing");
    expectNoUndefined(document.body);
  });

  it("is not a live region, because its rows tick", async () => {
    // A polite region wrapped around a countdown re-reads the whole banner every
    // time the clock moves. The strip is standing chrome, not news.
    const { stub } = mount({ snoozes: [activeSnooze("s1", "HighErrorRate")] });
    await settled(stub);

    await until(() => expect(text()).toContain("holding notifications on"));
    expect(screen.queryAllByRole("status")).toHaveLength(0);
    // Asked from inside the strip rather than by counting regions on the page:
    // the healthy source strip keeps its own (empty) one mounted, and the claim
    // here is about this strip's rows, not about the header's total.
    const row = document.querySelector("li");
    expect(row).not.toBeNull();
    expect(row!.closest("[aria-live]")).toBeNull();
  });

  it("stops enumerating at five and hands the rest to the filtered list", async () => {
    const many = Array.from({ length: 8 }, (_, i) => activeSnooze(`s${i}`, `Alert${i}`));
    const { stub } = mount({ snoozes: many });
    await settled(stub);

    await until(() => expect(text()).toContain("holding notifications on"));
    // The count is honest about all eight; only five are spelled out, so the
    // table below keeps its screen.
    expect(text()).toContain("8 alerts");
    expect(document.querySelectorAll("li")).toHaveLength(5);

    const link = screen.getByRole("link", { name: /3 more/ });
    expect(link.getAttribute("href")).toBe("/alerts?snoozed=true");
  });

  it("dismisses the list it was shown, and comes back for a different one", async () => {
    const { stub, client } = mount({ snoozes: [activeSnooze("s1", "HighErrorRate")] });
    await settled(stub);
    await until(() => expect(text()).toContain("holding notifications on"));

    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    await flush();
    expect(text()).not.toContain("holding notifications on");

    // ⛔ A DIFFERENT SET OF HOLDS IS DIFFERENT NEWS. Dismissal is keyed to the
    // identity of the list, not to a boolean, so the next snooze somebody takes
    // is not silenced by a click that happened before it existed.
    stub.on("GET /api/v1/snoozes", list([activeSnooze("s2", "DiskFillingUp")]));
    await client.invalidateQueries({ queryKey: qk.alerts.activeSnoozes() });

    await until(() => expect(text()).toContain("DiskFillingUp"));
    expect(text()).toContain("holding notifications on");
  });

  it("stays dismissed when one of the holds simply ends", async () => {
    // ⛔ ATTRITION IS NOT NEWS. The server drops a snooze from this list the
    // moment it expires. Keying dismissal to a signature of the whole set — the
    // sorted ids joined — looks equivalent and is not: the first expiry changes
    // the signature and re-opens the strip over a STRICTLY SMALLER set the
    // operator has already read. Keyed by id, only an unseen hold re-opens it.
    const { stub, client } = mount({
      snoozes: [activeSnooze("s1", "HighErrorRate"), activeSnooze("s2", "DiskFillingUp")],
    });
    await settled(stub);
    await until(() => expect(text()).toContain("holding notifications on"));

    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    await flush();
    expect(text()).not.toContain("holding notifications on");

    // `s2` wakes up. Nothing new is being held.
    stub.on("GET /api/v1/snoozes", list([activeSnooze("s1", "HighErrorRate")]));
    await client.invalidateQueries({ queryKey: qk.alerts.activeSnoozes() });
    await flush();

    expect(text()).not.toContain("holding notifications on");
    expect(text()).not.toContain("HighErrorRate");
  });
});

/* -------------------------------------------------------------------------- */
/* ADR 0012 and §M.2                                                          */
/* -------------------------------------------------------------------------- */

describe("the two-tier colour rule", () => {
  /** Every Tier-B utility the token set publishes, as a class name would spell it. */
  const TIER_B = /\b(firing|acked|suppressed|resolved|expired|info)-(fill|border|text|solid)\b/;

  it("keeps both banners in Tier A, and out of motion", async () => {
    const { stub } = mount({
      sources: [unreachable("am-eu", "src-1", "i/o timeout")],
      snoozes: [activeSnooze("s1", "HighErrorRate")],
    });
    await settled(stub);
    await until(() => expect(text()).toContain("cannot reach"));
    expect(text()).toContain("holding notifications on");

    for (const el of Array.from(document.querySelectorAll("*"))) {
      const cls = el.getAttribute("class") ?? "";
      expect(cls, `a shell banner reached for a state hue: \`${cls}\``).not.toMatch(TIER_B);
      // No animation and no pulse: the only urgency motion oto ships is on the
      // unacked-critical dot, and neither of these strips is urgency.
      expect(cls, `a shell banner animates: \`${cls}\``).not.toMatch(/animate-|oto-pulse|oto-enter/);
    }
  });
});

/* -------------------------------------------------------------------------- */
/* The third strip, whose input is the stream itself                          */
/* -------------------------------------------------------------------------- */

/**
 * Push frames into a `text/event-stream` body the stream is already reading.
 *
 * `ResyncBanner` has no query behind it — `useLive()` is its only input — so the
 * only way to raise it is to be the server. The shape is the one `live.test.tsx`
 * uses: a stubbed `fetch` that answers with a `ReadableStream` the test holds the
 * controller for.
 */
async function mountResync(): Promise<(text: string) => Promise<void>> {
  const pushes: ((text: string) => void)[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(() => {
      const encoder = new TextEncoder();
      let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
      const body = new ReadableStream<Uint8Array>({
        start(c) {
          controller = c;
        },
      });
      pushes.push((t) => controller?.enqueue(encoder.encode(t)));
      return Promise.resolve({ ok: true, status: 200, body });
    }),
  );

  renderScreen(() => (
    <LiveProvider>
      <ResyncBanner />
    </LiveProvider>
  ));

  await until(() => expect(pushes.length).toBeGreaterThanOrEqual(1));
  const push = pushes[0]!;
  return async (t) => {
    push(t);
    await flush();
  };
}

describe("a resync the server ordered", () => {
  it("announces through a region that predates the news, because nothing else could", async () => {
    // ⛔ THIS STRIP DEPENDS ON THE MOUNTED REGION MORE COMPLETELY THAN THE OTHERS.
    // The other two are reachable by eye: an operator watching the source list can
    // see it stop changing. A resync is entirely out of band — it is the client
    // being told that everything already on screen may have been wrong — and the
    // one sentence that says so arrives with no visible cause at all. If the
    // region were created along with the sentence, the announcement most screen
    // readers make is none, and the only channel this fact has is gone.
    //
    // The text is also static, which is what makes a live region the right tool
    // here and the wrong one for `SnoozeBanner`: it arrives once, is read once,
    // and does not change again while it is up.
    const push = await mountResync();

    // Taken before the server has said anything: the strip is invisible, but the
    // node that will carry its words is already in the document and already empty.
    const region = document.querySelector("[aria-live]");
    expect(region).not.toBeNull();
    expect(region!.textContent).toBe("");
    expect(region!.getAttribute("class") ?? "").toBe("");

    await push(sse(frame(1, "resync", { reason: "replay_window_exceeded" })));

    // THAT node, not a sibling that arrived holding the sentence.
    await until(() => expect(region!.textContent).toContain("could not keep this page"));
    expect(region!.isConnected).toBe(true);
    expect(document.querySelectorAll("[aria-live]")).toHaveLength(1);

    // The reason is named rather than generalised: an operator who was away for a
    // day and one whose buffer overflowed have different things to do next.
    expect(region!.textContent).toContain("24-hour replay window");
    expect(region!.textContent).toContain("has been refetched");

    // Polite and whole, exactly as the source strip is.
    expect(region!.getAttribute("aria-live")).toBe("polite");
    expect(region!.getAttribute("aria-atomic")).toBe("true");
    expectNoUndefined(document.body);
  });

  it("empties that same region on Dismiss rather than dropping it", async () => {
    // The operator has read it and acknowledged it; the next resync still needs
    // somewhere to be announced, and a region recreated per resync is the bug
    // this whole shape exists to avoid.
    const push = await mountResync();
    const region = document.querySelector("[aria-live]")!;

    await push(sse(frame(1, "resync", { reason: "buffer_overflow" })));
    await until(() => expect(region.textContent).toContain("update buffer overflowed"));

    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));

    await until(() => expect(region.textContent).toBe(""));
    expect(region.isConnected).toBe(true);
    // Empty *and* undressed, so the header goes back to one flat line.
    expect(region.getAttribute("class") ?? "").toBe("");
  });
});
