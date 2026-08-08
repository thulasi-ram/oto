import { describe, expect, it } from "vitest";

import { classifyRegexValue, formatMatchers, parseMatchers } from "./matchers";

/**
 * The matcher parser is the one place the UI decides what the SERVER will be
 * asked. ADR 0017 is explicit that an untranslatable matcher is rejected at parse
 * time and never degraded into a scan, and the server enforces the same rule with
 * a 422 — so a client that quietly accepts a matcher the server will refuse
 * produces a request whose only possible outcome is an error nobody can act on.
 *
 * These are round-trip and refusal tests: what the parser accepts must survive
 * being formatted and re-parsed, and what it refuses must be refused loudly.
 */
describe("parseMatchers", () => {
  it("parses the four supported operators", () => {
    const r = parseMatchers('severity="critical", team!="infra", pod=~"api-.*", job!~"^cron"');
    expect(r.errors).toEqual([]);
    expect(r.matchers).toEqual([
      { name: "severity", op: "=", value: "critical" },
      { name: "team", op: "!=", value: "infra" },
      { name: "pod", op: "=~", value: "api-.*" },
      { name: "job", op: "!~", value: "^cron" },
    ]);
  });

  it("round-trips through formatMatchers, so the wire form never depends on typing", () => {
    const typed = 'severity = "critical" ,pod=~"api-.*"';
    const first = parseMatchers(typed);
    expect(first.errors).toEqual([]);

    const rendered = formatMatchers(first.matchers);
    const second = parseMatchers(rendered);

    expect(second.errors).toEqual([]);
    expect(second.matchers).toEqual(first.matchers);
    expect(formatMatchers(second.matchers)).toBe(rendered);
  });

  it("names every metacharacter that would force a scan (ADR 0017)", () => {
    // An alternation of literals is answerable from the label index.
    expect(classifyRegexValue("critical|warning")).toEqual({ servable: true, offending: [] });
    // Anchors are stripped before judging, because they cost nothing.
    expect(classifyRegexValue("^critical$")).toEqual({ servable: true, offending: [] });

    // Anything that can match an unbounded set cannot, and the refusal names the
    // characters so the message can teach rather than just deny.
    const verdict = classifyRegexValue("api-.*");
    expect(verdict.servable).toBe(false);
    expect(verdict.offending).toEqual([".", "*"]);
  });

  it("refuses a label name outside the Prometheus charset (bound B9)", () => {
    const r = parseMatchers('9lives="cat"');
    expect(r.errors.length).toBeGreaterThan(0);
  });

  it("treats empty input as no filter, not as an error", () => {
    const r = parseMatchers("   ");
    expect(r.errors).toEqual([]);
    expect(r.matchers).toEqual([]);
  });
});
