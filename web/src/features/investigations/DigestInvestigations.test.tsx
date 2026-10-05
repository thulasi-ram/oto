/**
 * A policy's digest Investigations (review D4): the runs that never ran are on screen
 * with the reason they did not, and nothing is read until the list is opened.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { DigestInvestigations } from "./DigestInvestigations";
import { REASON_SENTENCE, STATUS_LABEL } from "./copy";
import { investigation } from "~/test/fixtures";
import { expectNoUndefined, list, problem, renderScreen, stubFetch, until } from "~/test/harness";
import { absoluteTime } from "~/lib/format";

const POLICY = "8d1f6a3e-1b2c-4d5e-8f90-a1b2c3d4e5f6";
const LIST = `/api/v1/notification-policies/${POLICY}/investigations`;

const digestRun = (patch: Parameters<typeof investigation>[0]) =>
  investigation({
    subject_kind: "digest",
    subject_id: POLICY,
    requested_by_label: "oto: the digest window of payments digest",
    digest_window_start: "2026-08-09T09:00:00.000Z",
    digest_window_end: "2026-08-09T10:00:00.000Z",
    ...patch,
  });

function open(): HTMLElement {
  const el = document.querySelector<HTMLDetailsElement>("[data-digest-investigations]")!;
  el.open = true;
  fireEvent(el, new Event("toggle"));
  return el;
}

describe("a policy's digest Investigations", () => {
  it("⛔ shows a run skipped for the day's budget, with why, and reads nothing until opened", async () => {
    const skipped = digestRun({
      id: "1a2b3c4d-0000-4000-8000-000000000001",
      status: "skipped",
      reason: "budget",
      reason_detail: "the org had spent 2000000 of its 2000000 tokens today",
      finding: null,
      started_at: null,
      tokens_in: 0,
      tokens_out: 0,
      tool_calls: 0,
    });
    const done = digestRun({ id: "1a2b3c4d-0000-4000-8000-000000000002" });
    const net = stubFetch({ [`GET ${LIST}`]: () => ({ json: list([skipped, done]) }) });
    renderScreen(() => <DigestInvestigations policyId={POLICY} />);

    expect(screen.getByText("Digest Investigations")).toBeTruthy();
    await new Promise((r) => setTimeout(r, 30));
    expect(net.to(LIST)).toHaveLength(0);

    const root = open();
    await until(() => expect(root.querySelector('[data-digest-run="skipped"]')).not.toBeNull());
    const row = root.querySelector<HTMLElement>('[data-digest-run="skipped"]')!;
    expect(row.textContent).toContain(STATUS_LABEL.skipped);
    expect(row.textContent).toContain(REASON_SENTENCE.budget);
    expect(row.textContent).toContain("the org had spent 2000000");
    expect(row.textContent).toContain(absoluteTime(skipped.digest_window_start));
    expect(row.querySelector("[data-finding]")).toBeNull();

    // A Finding is shown with the instant it was reached, never as the state now.
    const reached = root.querySelector<HTMLElement>('[data-digest-run="completed"] [data-finding]')!;
    expect(reached.textContent).toContain(absoluteTime(done.ended_at));
    expect(reached.textContent).toContain("crash-looping");
    expectNoUndefined(root);
  });

  it("says so when no window has asked yet", async () => {
    stubFetch({ [`GET ${LIST}`]: () => ({ json: list([]) }) });
    renderScreen(() => <DigestInvestigations policyId={POLICY} />);
    const root = open();
    await until(() => expect(root.textContent).toMatch(/No digest window of this policy/));
  });

  it("shows a refused read in the open", async () => {
    stubFetch({
      [`GET ${LIST}`]: () => problem(404, "policy_deleted", { detail: "this policy has been deleted" }),
    });
    renderScreen(() => <DigestInvestigations policyId={POLICY} />);
    const root = open();
    await until(() => expect(root.textContent).toMatch(/this policy has been deleted/));
  });
});
