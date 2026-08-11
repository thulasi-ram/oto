import { describe, expect, it } from "vitest";

import { SUPPORTED_OPERATORS, classifyRegexValue, formatMatchers, parseMatchers } from "./matchers";
import { maxLengthOf } from "~/api/bounds";
import { LabelNameSchema } from "~/api/generated/validators";
import { enumValues } from "~/test/contract";

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

  it("refuses a label name past the contract's own cap, which it used to ignore (bound B9)", () => {
    // The cap comes off `LabelName` in the generated schemas, not from here: the
    // parser copied the charset half of that rule faithfully and dropped this
    // half, so a 2000-byte name parsed clean and came back a 422.
    const max = maxLengthOf(LabelNameSchema);
    expect(max).toBeGreaterThan(0);

    expect(parseMatchers(`${"a".repeat(max)}="x"`).errors).toEqual([]);

    const over = parseMatchers(`${"a".repeat(max + 1)}="x"`);
    expect(over.matchers).toEqual([]);
    expect(over.errors).toHaveLength(1);
    // The refusal states the server's number rather than a rounded-off sentence.
    expect(over.errors[0]?.message).toContain(String(max));
  });

  it("sends every operator the contract publishes, and tokenises the two-character ones first", () => {
    expect([...SUPPORTED_OPERATORS]).toEqual([...enumValues("MatcherOp")]);

    // `!=` must not be read as `!` followed by `=`, and `=~` must not be read as
    // `=` followed by `~`. Asserted through the parser, because the ordering that
    // guarantees it is derived rather than written down.
    const r = parseMatchers('a!="1", b=~"x|y", c!~"x", d="1"');
    expect(r.errors).toEqual([]);
    expect(r.matchers.map((m) => m.op)).toEqual(["!=", "=~", "!~", "="]);
  });

  it("treats empty input as no filter, not as an error", () => {
    const r = parseMatchers("   ");
    expect(r.errors).toEqual([]);
    expect(r.matchers).toEqual([]);
  });
});
