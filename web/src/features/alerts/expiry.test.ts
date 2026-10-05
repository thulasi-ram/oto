/**
 * ADR 0056 §1, "staleness is shown", as a rule: given the facts a Case carries,
 * can it expire, and when it cannot, why. The rule is the reaper's (owner ruling
 * R1) — at least one live source, EVERY one healthy, and an end time or the
 * cluster's max silence to run out — so every shape the reaper distinguishes is
 * pinned here, each with what the screen says. `silent` and `source_removed`
 * ship behind a deployment flag the API does not expose, so their forecasts say
 * "if enabled" and never promise.
 */
import { describe, expect, it } from "vitest";

import { caseSource, caseSources, clusterSources } from "~/test/fixtures";
import { caseExpiry, expiryNote, isHeld, type StalenessInput } from "./expiry";

const HEARD = "2026-08-09T09:00:00.000Z";
const HOUR = 3_600_000;
const NOW = Date.parse(HEARD) + 3 * HOUR;

function open(patch: Partial<StalenessInput> = {}): StalenessInput {
  return { state: "open", last_observed_at: HEARD, sources: caseSources(), ...patch };
}

describe("whether an open case can expire", () => {
  it("can expire as silent under one healthy source with a max silence — if enabled", () => {
    const e = caseExpiry(open());
    expect(e).toEqual({
      kind: "can_expire",
      live: 1,
      maxSilenceS: 86_400,
      silentAt: "2026-08-10T09:00:00.000Z",
      endsAt: null,
      silenceOff: [],
    });
    expect(isHeld(e)).toBe(false);

    const note = expiryNote(e, NOW);
    expect(note?.short).toBe("expires as silent after 1d without word, if enabled");
    // ⛔ The flag is not on the wire, so the forecast is conditional, and says on what.
    expect(note?.long).toMatch(
      /^If this deployment has turned on the silent and source-removed expiries \(jobs\.expire_silent_and_removed, off by default in this release\), it expires as silent after 1d without word: in 21h /,
    );
  });

  it("says a silence that has run out expires on the reaper's next pass, still if enabled", () => {
    const note = expiryNote(caseExpiry(open()), Date.parse(HEARD) + 25 * HOUR);
    expect(note?.long).toMatch(/^If this deployment has turned on/);
    expect(note?.long).toMatch(/that has passed, so it expires on the reaper's next pass/);
  });

  it("forecasts a future end time as that time plus the org's resolve grace", () => {
    const e = caseExpiry(
      open({
        source_ends_at: "2026-08-09T12:05:00.000Z",
        sources: caseSources({}, { max_silence_seconds: null }),
      }),
    );
    expect(e?.kind).toBe("can_expire");
    const note = expiryNote(e, NOW);
    expect(note?.short).toBe("upstream end time in 5m");
    expect(note?.long).toMatch(
      /Once that time and then the org's resolve grace have passed with no word, oto expires it as timed out\./,
    );
    // `timeout` is not behind the flag: no condition on it.
    expect(note?.long).not.toMatch(/^If this deployment/);
    expect(note?.long).toMatch(
      /Max silence is off for prod-eu alertmanager, so it cannot expire as silent\.$/,
    );
  });

  it("forecasts a past end time as a reaper pass once the resolve grace has passed", () => {
    const e = caseExpiry(
      open({
        source_ends_at: "2026-08-09T11:00:00.000Z",
        sources: caseSources({}, { max_silence_seconds: null }),
      }),
    );
    const note = expiryNote(e, NOW);
    expect(note?.short).toBe("upstream end time 1h ago");
    expect(note?.long).toMatch(
      /has passed\. oto expires it as timed out on a reaper pass once the org's resolve grace has passed, unless upstream speaks first\./,
    );
  });

  it("leads a row with the timeout, which needs no flag, when both clocks run", () => {
    const e = caseExpiry(open({ source_ends_at: "2026-08-09T12:05:00.000Z" }));
    const note = expiryNote(e, NOW);
    expect(note?.short).toBe("upstream end time in 5m");
    expect(note?.long).toMatch(/expires as silent after 1d without word/);
  });

  it("cannot expire with no upstream end time and max silence off", () => {
    const e = caseExpiry(open({ sources: caseSources({}, { max_silence_seconds: null }) }));
    expect(e).toEqual({ kind: "no_end_time", live: 1, silenceOff: ["prod-eu alertmanager"] });
    expect(isHeld(e)).toBe(true);
    expect(expiryNote(e, NOW)?.long).toBe(
      "Cannot expire: upstream gave no end time, and max silence is off for prod-eu alertmanager. It stays open until upstream resolves it.",
    );
  });

  it("is held while its source is not healthy, whatever its clocks say", () => {
    const e = caseExpiry(
      open({
        source_ends_at: "2026-08-09T09:05:00.000Z",
        sources: caseSources({}, { healthy: false }),
      }),
    );
    expect(e).toEqual({ kind: "not_healthy", live: 1, sources: ["prod-eu alertmanager"] });
    expect(isHeld(e)).toBe(true);
    expect(expiryNote(e, NOW)?.short).toBe("held: prod-eu alertmanager is not healthy");
    expect(expiryNote(e, NOW)?.long).toMatch(/cannot tell silence from an outage/);
  });
});

describe("an HA cluster (ruling R1)", () => {
  const am0 = caseSource({ id: "a0", name: "am-0" });
  const am1 = caseSource({ id: "a1", name: "am-1", max_silence_seconds: 7_200 });

  it("⭐ can expire when every live source is healthy, on the LONGEST max silence", () => {
    const e = caseExpiry(open({ sources: clusterSources([am0, am1]) }));
    expect(e).toMatchObject({ kind: "can_expire", live: 2, maxSilenceS: 86_400 });
    expect(isHeld(e)).toBe(false);
    expect(expiryNote(e, NOW)?.short).toBe("expires as silent after 1d without word, if enabled");
  });

  it("is held by the one replica that is not healthy, and names only that one", () => {
    const e = caseExpiry(
      open({ sources: clusterSources([am0, { ...am1, healthy: false }]) }),
    );
    expect(e).toEqual({ kind: "not_healthy", live: 2, sources: ["am-1"] });
    expect(isHeld(e)).toBe(true);
    const note = expiryNote(e, NOW);
    expect(note?.short).toBe("held: am-1 is not healthy");
    expect(note?.long).toMatch(/^Held: am-1 is not healthy\. oto expires a case only while every live source on its cluster is healthy/);
    expect(note?.long).not.toMatch(/am-0/);
  });

  it("names every unhealthy replica when there are several", () => {
    const e = caseExpiry(
      open({
        sources: clusterSources([
          { ...am0, healthy: false },
          { ...am1, healthy: false },
          caseSource({ id: "a2", name: "am-2" }),
        ]),
      }),
    );
    expect(expiryNote(e, NOW)?.short).toBe("held: am-0 and am-1 are not healthy");
  });

  it("says a source holds it even when the unhealthy one is past the listed ten", () => {
    const e = caseExpiry(
      open({ sources: { ...clusterSources([am0, am1]), live: 12, all_healthy: false } }),
    );
    expect(e).toEqual({ kind: "not_healthy", live: 12, sources: [] });
    expect(expiryNote(e, NOW)?.short).toBe("held: a source is not healthy");
  });

  it("⛔ cannot expire as silent when ANY replica turned max silence off", () => {
    const off = { ...am1, max_silence_seconds: null };
    const e = caseExpiry(open({ sources: clusterSources([am0, off]) }));
    expect(e).toEqual({ kind: "no_end_time", live: 2, silenceOff: ["am-1"] });
    expect(expiryNote(e, NOW)?.long).toBe(
      "Cannot expire: upstream gave no end time, and max silence is off for am-1, which turns it off for its whole cluster. It stays open until upstream resolves it.",
    );
  });

  it("cannot expire with no live source and none removed", () => {
    const e = caseExpiry(open({ sources: clusterSources([]) }));
    expect(e).toEqual({ kind: "no_live_source" });
    expect(isHeld(e)).toBe(true);
    expect(expiryNote(e, NOW)?.short).toBe("cannot expire: no live source");
  });

  it("expires as source removed a resolve grace after the cluster's last source is gone — if enabled", () => {
    const e = caseExpiry(open({ sources: clusterSources([], 1) }));
    expect(e).toEqual({ kind: "source_removed" });
    expect(isHeld(e)).toBe(false);
    const note = expiryNote(e, NOW);
    expect(note?.short).toBe("source removed: expires if enabled");
    expect(note?.long).toMatch(/^Its cluster's last source was removed\. If this deployment has turned on/);
    expect(note?.long).toMatch(
      /expires it as source removed on a reaper pass once the org's resolve grace has passed since the removal\. Otherwise it stays open/,
    );
  });
});

describe("what is not said", () => {
  it("says nothing about expiry when the sources were not read — never a guess", () => {
    const e = caseExpiry(open({ sources: null }));
    expect(e).toEqual({ kind: "unknown" });
    expect(isHeld(e)).toBe(false);
    expect(expiryNote(e, NOW)).toBeNull();
  });

  it("says nothing about expiry once the case has ended: its chip names how", () => {
    expect(caseExpiry(open({ state: "closed" }))).toBeNull();
    expect(expiryNote(null, NOW)).toBeNull();
  });
});
