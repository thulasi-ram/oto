/**
 * Import writes the operator's pick into the copy, literally, in place of every
 * `<<choose:<name>>>` placeholder — and asks until every choice has a valid pick
 * (owner ruling of 2026-10-02: PagerDuty's fallback severity is the operator's).
 */
import { describe, expect, it } from "vitest";

import type { PayloadMappingCatalogEntry } from "~/api/types";

import { fillChoices, unansweredChoices } from "./catalogChoices";

const entry: PayloadMappingCatalogEntry = {
  id: "pagerduty",
  vendor: "PagerDuty",
  title: "PagerDuty Events API v2",
  summary: "x",
  docs: ["https://docs.pagerduty.com/developer/send-alert-event"],
  checked_on: "2026-10-02",
  setup: ["x"],
  commands: [{ field: "event_action", forbidden: ["resolve", "acknowledge"] }],
  choices: [
    {
      name: "default_severity",
      question: "Which severity?",
      field: "payload.severity",
      options: ["critical", "error", "warning", "info"],
    },
  ],
  secrets: ["routing_key"],
  mapping: {
    body: '{"severity": "{% if x %}{{ x }}{% endif %}{% unless x %}<<choose:default_severity>>{% endunless %}", "again": "<<choose:default_severity>>"}',
    facts: { quiet: '{"severity": "<<choose:default_severity>>"}' },
  },
};

describe("a catalog choice at import", () => {
  it("is unanswered until it holds one of its options", () => {
    expect(unansweredChoices(entry, {})).toEqual(["default_severity"]);
    expect(unansweredChoices(entry, { default_severity: "page" })).toEqual([
      "default_severity",
    ]);
    expect(unansweredChoices(entry, { default_severity: "warning" })).toEqual(
      [],
    );
  });

  it("writes the literal pick in place of every placeholder, in every source", () => {
    const filled = JSON.stringify(
      fillChoices(entry, { default_severity: "warning" }),
    );
    expect(filled).not.toContain("<<choose:");
    expect(filled).not.toContain("critical");
    expect(filled.match(/warning/g)?.length).toBe(3);
  });

  it("leaves a placeholder with no pick for the server to refuse", () => {
    expect(JSON.stringify(fillChoices(entry, {}))).toContain(
      "<<choose:default_severity>>",
    );
  });
});
