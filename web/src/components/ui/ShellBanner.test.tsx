/**
 * The two banners that tell an operator whether a calm list is evidence of calm.
 *
 * ⛔ THE PROPERTY UNDER TEST IS SYMMETRIC, AND ONLY ONE HALF OF IT IS OBVIOUS.
 * A banner that shows its content on the degraded path is easy. A banner that is
 * genuinely *absent* on the healthy path is the half that decays: it is what
 * keeps the header one flat line, and it is what stops the strip from becoming
 * the chrome everybody has learned to read past. So every case here settles both
 * requests first — otherwise "absent" would only mean "has not loaded yet",
 * which is a test that passes for the wrong reason forever.
 *
 * The last describe is the one ADR 0012 exists for: neither banner may reach for
 * a Tier-B state hue. Losing sight of a source and holding a notification are
 * facts about oto, not the state of an alert, and spending a state colour on them
 * would blunt the scarcity that makes a firing row loud.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { beforeEach, describe, expect, it } from "vitest";

import { SnoozeBanner, SourceReachBanner, resetDismissedSnoozes } from "./ShellBanner";
import { qk } from "~/api/keys";
import { expectNoUndefined, flush, list, renderScreen, stubFetch, until } from "~/test/harness";
import { snooze, source } from "~/test/fixtures";
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
      ack_state: "unacked",
    },
    remaining_seconds: 3600,
  };
}

/* -------------------------------------------------------------------------- */
/* Mounting                                                                   */
/* -------------------------------------------------------------------------- */

interface World {
  readonly sources?: readonly Source[];
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
    "GET /api/v1/sources": list(world.sources ?? [source()]),
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

// The read set outlives the mount on purpose (AppShell is remounted per route),
// so it must be cleared between cases or one Dismiss silences the next test.
beforeEach(() => resetDismissedSnoozes());

describe("a healthy org", () => {
  it("renders no banner, no live region and nothing to dismiss", async () => {
    const { stub } = mount({ sources: [source()], snoozes: [] });
    await settled(stub);

    expect(text()).not.toContain("cannot reach");
    expect(text()).not.toContain("holding notifications");
    // No strip at all — not an empty one. An empty strip would still cost the
    // header a border and the table below a few pixels of offset.
    expect(screen.queryAllByRole("status")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "Dismiss" })).toBeNull();
    expect(document.querySelectorAll("li")).toHaveLength(0);
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
    expect(screen.queryAllByRole("status")).toHaveLength(0);
  });
});

/* -------------------------------------------------------------------------- */
/* A source oto cannot reach                                                  */
/* -------------------------------------------------------------------------- */

describe("an unreachable source", () => {
  it("names it, says the occurrences are held, and cannot be dismissed", async () => {
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
    expect(screen.getAllByRole("status")).toHaveLength(1);
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
    // until it pushed the alert table off screen.
    expect(screen.getAllByRole("status")).toHaveLength(1);

    // Three named, the rest counted, and the reachable one never mentioned.
    expect(text()).toContain("am-eu");
    expect(text()).toContain("am-us");
    expect(text()).toContain("am-ap");
    expect(text()).toContain("and 1 more");
    expect(text()).not.toContain("am-sa");
    expect(text()).not.toContain("am-well");
    // Plural copy, because four sources are not "it".
    expect(text()).toContain("Their");
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
