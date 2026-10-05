/**
 * `ago`, the sentence form of a past instant. Every instant it renders is one
 * the server recorded, so one that reads as in the future is this browser's
 * clock running behind — and `in 2m ago` is the bug this file pins shut.
 */
import { describe, expect, it } from "vitest";

import { ago } from "./format";

const AT = "2026-08-09T09:00:00.000Z";
const T = Date.parse(AT);

describe("how long ago something happened", () => {
  it("reads as `3h ago`", () => {
    expect(ago(AT, T + 3 * 3_600_000)).toBe("3h ago");
  });

  it("reads as `just now` rather than `now ago`", () => {
    expect(ago(AT, T + 1000)).toBe("just now");
    expect(ago(AT, T)).toBe("just now");
  });

  it("⛔ reads a future instant as `just now`, never `in 2m ago` — the client clock is behind", () => {
    expect(ago(AT, T - 2 * 60_000)).toBe("just now");
    expect(ago(AT, T - 3 * 3_600_000)).toBe("just now");
  });

  it("says nothing it cannot read, rather than `— ago`", () => {
    expect(ago(null, T)).toBe("—");
    expect(ago("not a time", T)).toBe("—");
  });
});
