/**
 * ADR 0056 §1, "staleness is shown", as a rule: given the facts a Case carries,
 * can it expire, and when it cannot, why. The rule is the reaper's — exactly one
 * live source, healthy, and an end time or a max silence to run out — so every
 * shape the reaper distinguishes is pinned here, each with what the screen says.
 */
import { describe, expect, it } from "vitest";

import { caseSources } from "~/test/fixtures";
import { ago, caseExpiry, expiryNote, isHeld, type StalenessInput } from "./expiry";

const HEARD = "2026-08-09T09:00:00.000Z";
const HOUR = 3_600_000;
const NOW = Date.parse(HEARD) + 3 * HOUR;

function open(patch: Partial<StalenessInput> = {}): StalenessInput {
  return { state: "open", last_observed_at: HEARD, sources: caseSources(), ...patch };
}

describe("whether an open case can expire", () => {
  it("can expire as silent under one healthy source with a max silence", () => {
    const e = caseExpiry(open());
    expect(e).toEqual({
      kind: "can_expire",
      source: "prod-eu alertmanager",
      maxSilenceS: 86_400,
      silentAt: "2026-08-10T09:00:00.000Z",
      endsAt: null,
    });
    expect(isHeld(e)).toBe(false);

    const note = expiryNote(e, NOW);
    expect(note?.short).toBe("expires as silent after 1d without word");
    expect(note?.long).toMatch(/^Expires as silent after 1d without word: in 21h /);
  });

  it("says a silence that has run out expires on the reaper's next pass", () => {
    const note = expiryNote(caseExpiry(open()), Date.parse(HEARD) + 25 * HOUR);
    expect(note?.long).toMatch(/that has passed, so it expires on the reaper's next pass/);
  });

  it("can expire as timed out on upstream's end time when max silence is off", () => {
    const e = caseExpiry(
      open({
        source_ends_at: "2026-08-09T12:05:00.000Z",
        sources: caseSources({}, { max_silence_seconds: null }),
      }),
    );
    expect(e?.kind).toBe("can_expire");
    const note = expiryNote(e, NOW);
    expect(note?.short).toBe("upstream end time in 5m");
    expect(note?.long).toMatch(/If it passes with no word, oto expires it as timed out\./);
  });

  it("cannot expire with no upstream end time and max silence off", () => {
    const e = caseExpiry(open({ sources: caseSources({}, { max_silence_seconds: null }) }));
    expect(e).toEqual({ kind: "no_end_time", source: "prod-eu alertmanager" });
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
    expect(e).toEqual({ kind: "not_healthy", source: "prod-eu alertmanager" });
    expect(isHeld(e)).toBe(true);
    expect(expiryNote(e, NOW)?.short).toBe("held: prod-eu alertmanager is not healthy");
    expect(expiryNote(e, NOW)?.long).toMatch(/cannot tell silence from an outage/);
  });

  it("is held under an HA pair, because the reaper acts only under one live source", () => {
    const e = caseExpiry(open({ sources: caseSources({ live: 2, source: null }) }));
    expect(e).toEqual({ kind: "several_sources", live: 2 });
    expect(isHeld(e)).toBe(true);
    expect(expiryNote(e, NOW)?.short).toBe("cannot expire: 2 live sources");
  });

  it("cannot expire with no live source and none removed", () => {
    const e = caseExpiry(open({ sources: caseSources({ live: 0, source: null }) }));
    expect(e).toEqual({ kind: "no_live_source" });
    expect(isHeld(e)).toBe(true);
    expect(expiryNote(e, NOW)?.short).toBe("cannot expire: no live source");
  });

  it("expires as source removed once its cluster's last source is gone", () => {
    const e = caseExpiry(open({ sources: caseSources({ live: 0, removed: 1, source: null }) }));
    expect(e).toEqual({ kind: "source_removed" });
    expect(isHeld(e)).toBe(false);
    expect(expiryNote(e, NOW)?.long).toMatch(/expires it as source removed on the reaper's next pass/);
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

describe("how long since upstream spoke", () => {
  it("reads as `3h ago`, and as `just now` rather than `now ago`", () => {
    expect(ago(HEARD, NOW)).toBe("3h ago");
    expect(ago(HEARD, Date.parse(HEARD) + 1000)).toBe("just now");
  });
});
