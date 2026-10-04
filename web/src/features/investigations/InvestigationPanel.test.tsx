/**
 * A Case's Investigations, in every state a run can be in (git-bug e8e5ca8).
 *
 * ⛔ THE FINDING IS ASSERTED WITH ITS INSTANT, NEVER ALONE. ADR 0016 makes a
 * snapshot "what was seen at T", and the failure this suite exists to catch is a
 * Finding rendered as though it were the state now — so every test that finds a
 * Finding also finds the `ended_at` it was reached at, and the version that
 * reached it.
 *
 * ⛔ AND EVERY CONTROL A RUN HIT IS ASSERTED AS SHOWN. `exhausted` reads as
 * partial with its budget, `skipped` and `failed` say why, and a refused or
 * truncated Tool call wears its own word in the transcript (ADR 0053 §6:
 * "recorded, never silent").
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { InvestigationPanel } from "./InvestigationPanel";
import { REASON_SENTENCE, OUTCOME } from "./copy";
import type { Investigation, InvestigationDetail, Investigator, Suggestion } from "~/api/types";
import { investigation, investigationDetail, investigator, step } from "~/test/fixtures";
import {
  expectNoUndefined,
  item,
  list,
  problem,
  renderScreen,
  stubFetch,
  until,
  type FetchStub,
} from "~/test/harness";
import { absoluteTime, count } from "~/lib/format";

const CASE = "0f8fad5b-d9cb-469f-a165-70867728950e";
const LIST = `/api/v1/cases/${CASE}/investigations`;

interface World {
  readonly runs?: readonly Investigation[];
  readonly details?: readonly InvestigationDetail[];
  readonly investigators?: readonly Investigator[];
  /** Each run's Suggestions, by run id; none when absent. */
  readonly suggestions?: Readonly<Record<string, readonly Suggestion[]>>;
}

/** Mount the panel over a Case whose runs and Investigators are `world`. */
function mount(world: World = {}): FetchStub {
  const details = new Map((world.details ?? []).map((d) => [d.id, d]));
  const net = stubFetch({
    [`GET ${LIST}`]: () => ({ json: list(world.runs ?? []) }),
    "GET /api/v1/investigators": () => ({ json: list(world.investigators ?? [investigator()]) }),
  });
  for (const [id, d] of details) net.on(`GET /api/v1/investigations/${id}`, () => ({ json: item(d) }));
  for (const id of new Set([...(world.runs ?? []).map((r) => r.id), ...details.keys()])) {
    net.on(`GET /api/v1/investigations/${id}/suggestions`, () => ({
      json: list(world.suggestions?.[id] ?? []),
    }));
    // The Remedies sit under the Suggestions (ADR 0054); their own suite is RemediesView's.
    net.on(`GET /api/v1/investigations/${id}/remedies`, () => ({ json: list([]) }));
  }
  renderScreen(() => <InvestigationPanel subject={{ kind: "case", id: CASE }} pollMs={20} />);
  return net;
}

/** A button, once it has rendered — under `root` when given, else anywhere. */
async function button(name: string | RegExp, root?: () => HTMLElement): Promise<HTMLElement> {
  const find = (): HTMLElement =>
    (root !== undefined ? within(root()) : screen).getByRole("button", { name });
  await until(() => find());
  return find();
}

/** The run on screen. */
function shownRun(): HTMLElement {
  const el = document.querySelector<HTMLElement>("[data-investigation]");
  expect(el, "no Investigation is on screen").not.toBeNull();
  return el!;
}

function finding(): HTMLElement {
  const el = shownRun().querySelector<HTMLElement>("[data-finding]");
  expect(el, "the run on screen shows no Finding").not.toBeNull();
  return el!;
}

/** A run and its detail, from one patch. */
function run(patch: Partial<InvestigationDetail> = {}): {
  row: Investigation;
  detail: InvestigationDetail;
} {
  const detail = investigationDetail(patch);
  const { steps: _steps, ...row } = detail;
  void _steps;
  return { row, detail };
}

/* -------------------------------------------------------------------------- */
/* completed                                                                  */
/* -------------------------------------------------------------------------- */

describe("a completed Investigation", () => {
  it("⭐ shows the Finding labelled with the instant it was REACHED, and the version that reached it", async () => {
    const { row, detail } = run();
    mount({ runs: [row], details: [detail] });

    await until(() => expect(finding()).toBeTruthy());
    const caption = finding().querySelector("figcaption")!;
    // `ended_at`, not `requested_at`: a run that waited in the queue saw the
    // cluster later than it was asked.
    const time = caption.querySelector("time[datetime]")!;
    expect(time.getAttribute("datetime")).toBe(detail.ended_at);
    expect(caption.textContent).toContain(absoluteTime(detail.ended_at));
    expect(caption.textContent).not.toContain(absoluteTime(detail.requested_at));
    expect(caption.textContent).toContain("firstlook v3");
    expect(caption.textContent).toMatch(/not the state now/);
    expect(finding().textContent).toContain("crash-looping on a missing secret");
    expect(caption.textContent).not.toMatch(/partial/i);
    expectNoUndefined(shownRun());
  });

  it("⭐ says the class a Finding was given as the model's judgement, beside who concluded it", async () => {
    const { row, detail } = run({ classification: "deploy-regression" });
    mount({ runs: [row], details: [detail] });

    await until(() => expect(finding()).toBeTruthy());
    const cls = finding().querySelector("[data-classification]");
    expect(cls, "the Finding does not say its class").not.toBeNull();
    expect(cls!.textContent).toContain("deploy-regression");
    expect(cls!.textContent).toMatch(/by the model/);
    expect(cls!.textContent).toMatch(/a model's judgement/);
  });

  it("says `unclassified` as the answer it is, and says nothing when no class was asked for", async () => {
    const unclassified = run({ classification: "unclassified" });
    mount({ runs: [unclassified.row], details: [unclassified.detail] });
    await until(() => expect(finding()).toBeTruthy());
    expect(finding().querySelector("[data-classification]")!.textContent).toMatch(
      /unclassified.*none of this organisation's classes clearly fit/s,
    );
  });

  it("draws no classification for a Finding the org wrote no classes for", async () => {
    const { row, detail } = run(); // classification: null — oto ships no classes
    mount({ runs: [row], details: [detail] });
    await until(() => expect(finding()).toBeTruthy());
    expect(finding().querySelector("[data-classification]")).toBeNull();
    expect(finding().textContent).not.toMatch(/classif/i);
  });

  it("names the model and states the tokens it spent", async () => {
    const { row, detail } = run();
    mount({ runs: [row], details: [detail] });

    await until(() => expect(finding()).toBeTruthy());
    const r = within(shownRun());
    expect(r.getByText("gpt-4.1-mini")).toBeTruthy();
    expect(r.getByText(`${count(1540)} tokens spent`)).toBeTruthy();
    expect(r.getByText(`${count(1)} of ${count(20)} Tool calls`)).toBeTruthy();
  });

  it("opens its Step transcript, in order, model turns and Tool calls each as such", async () => {
    const { row, detail } = run();
    mount({ runs: [row], details: [detail] });

    const toggle = await button("Show the 3 Steps", shownRun);
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);

    const steps = [...shownRun().querySelectorAll<HTMLElement>("[data-step]")];
    expect(steps.map((s) => s.dataset["step"])).toEqual(["model_turn", "tool_call", "model_turn"]);
    expect(steps[0]!.textContent).toContain("asked for oto_case_timeline({})");
    expect(steps[1]!.textContent).toContain("oto_case_timeline");
    expect(steps[1]!.textContent).toContain("answered");
    expect(steps[1]!.textContent).toContain("[fired 09:02]");
  });

  it("⛔ shows a refused, timed-out and truncated Tool call — and a cut-off turn — as such", async () => {
    const { row, detail } = run({
      steps: [
        step({ seq: 1, kind: "tool_call", tool_name: "kubectl_delete", outcome: "refused", result: "not allowed" }),
        step({ seq: 2, kind: "tool_call", tool_name: "prom_query", outcome: "timeout" }),
        step({ seq: 3, kind: "tool_call", tool_name: "oto_case_timeline", outcome: "truncated", result: "[…" }),
        step({ seq: 4, text: "Partial thought", finish_reason: "length", tokens_in: 10, tokens_out: 4 }),
      ],
    });
    mount({ runs: [row], details: [detail] });

    fireEvent.click(await button("Show the 4 Steps", shownRun));

    const byOutcome = (o: string): HTMLElement =>
      shownRun().querySelector<HTMLElement>(`[data-outcome="${o}"]`)!;
    expect(byOutcome("refused").textContent).toContain(OUTCOME.refused.label);
    expect(byOutcome("refused").textContent).toContain(OUTCOME.refused.note);
    expect(byOutcome("timeout").textContent).toContain(OUTCOME.timeout.label);
    expect(byOutcome("truncated").textContent).toContain(OUTCOME.truncated.label);
    expect(byOutcome("truncated").textContent).toContain(OUTCOME.truncated.note);
    expect(shownRun().textContent).toContain("cut off at the model's output limit");
  });
});

/* -------------------------------------------------------------------------- */
/* exhausted, skipped, failed                                                 */
/* -------------------------------------------------------------------------- */

describe("a run that did not complete", () => {
  it("⛔ exhausted: keeps its Finding, marked PARTIAL, beside the budget that stopped it", async () => {
    const { row, detail } = run({
      status: "exhausted",
      reason: "token_budget",
      partial: true,
      finding: "So far: the secret was rotated at 09:01.",
    });
    mount({ runs: [row], details: [detail] });

    await until(() => expect(finding()).toBeTruthy());
    expect(shownRun().dataset["status"]).toBe("exhausted");
    expect(finding().querySelector("figcaption")!.textContent).toMatch(/^Partial Finding/);
    expect(finding().querySelector("figcaption")!.textContent).toContain(
      absoluteTime(detail.ended_at),
    );
    expect(finding().textContent).toContain("the secret was rotated at 09:01");
    expect(shownRun().textContent).toContain(REASON_SENTENCE.token_budget);
    expect(shownRun().textContent).toContain("stopped at a budget");
  });

  it("exhausted with nothing reached says so, rather than showing an empty Finding", async () => {
    const { row, detail } = run({
      status: "exhausted",
      reason: "step_budget",
      partial: true,
      finding: null,
    });
    mount({ runs: [row], details: [detail] });

    await until(() => expect(finding()).toBeTruthy());
    expect(finding().textContent).toContain("It reached no Finding before it stopped.");
    expect(shownRun().textContent).toContain(REASON_SENTENCE.step_budget);
  });

  it("⛔ skipped: says why nothing ran, and shows no Finding", async () => {
    const { row, detail } = run({
      status: "skipped",
      reason: "disabled",
      reason_detail: "Investigations are off for this org.",
      finding: null,
      started_at: null,
      tokens_in: 0,
      tokens_out: 0,
      tool_calls: 0,
      steps: [],
    });
    mount({ runs: [row], details: [detail] });

    await until(() => expect(shownRun().textContent).toContain("Skipped — nothing ran."));
    expect(shownRun().textContent).toContain(REASON_SENTENCE.disabled);
    expect(shownRun().textContent).toContain("Investigations are off for this org.");
    expect(shownRun().querySelector("[data-finding]")).toBeNull();
    await until(() =>
      expect(shownRun().textContent).toContain("No Steps are recorded for this run."),
    );
    expectNoUndefined(shownRun());
  });

  it("⛔ failed: shows the failure in the reason's words and the server's own", async () => {
    const { row, detail } = run({
      status: "failed",
      reason: "usage_missing",
      reason_detail: "turn 2 carried no usage block",
      finding: null,
    });
    mount({ runs: [row], details: [detail] });

    await until(() => expect(shownRun().textContent).toContain("Failed."));
    expect(shownRun().textContent).toContain(REASON_SENTENCE.usage_missing);
    expect(shownRun().textContent).toContain("turn 2 carried no usage block");
    expect(shownRun().querySelector("[data-finding]")).toBeNull();
  });
});

/* -------------------------------------------------------------------------- */
/* queued and running                                                         */
/* -------------------------------------------------------------------------- */

describe("a run in progress", () => {
  it("shows running as in progress, and polls until it has a Finding", async () => {
    const running = run({
      status: "running",
      finding: null,
      ended_at: null,
      tokens_in: 600,
      tokens_out: 40,
      steps: [step({ seq: 1, tokens_in: 600, tokens_out: 40 })],
    });
    const done = run();
    const net = mount({ runs: [running.row], details: [running.detail] });

    await until(() => expect(within(shownRun()).getByRole("status").textContent).toMatch(/In progress — running since/));
    expect(shownRun().querySelector("[data-finding]")).toBeNull();

    // The run ends server-side; nothing announces it but the poll.
    net.on(`GET ${LIST}`, () => ({ json: list([done.row]) }));
    net.on(`GET /api/v1/investigations/${done.row.id}`, () => ({ json: item(done.detail) }));

    await until(() => expect(finding().textContent).toContain("crash-looping"));
    expect(within(shownRun()).queryByRole("status")).toBeNull();

    // ...and once it is frozen, the polling stops.
    const settled = net.calls.length;
    await new Promise((r) => setTimeout(r, 120));
    expect(net.calls.length).toBe(settled);
  });

  it("shows queued as in progress, waiting to start", async () => {
    const queued = run({ status: "queued", finding: null, started_at: null, ended_at: null, steps: [] });
    mount({ runs: [queued.row], details: [queued.detail] });

    await until(() =>
      expect(within(shownRun()).getByRole("status").textContent).toMatch(/waiting to start/),
    );
  });
});

/* -------------------------------------------------------------------------- */
/* asking                                                                     */
/* -------------------------------------------------------------------------- */

describe("asking for an Investigation", () => {
  it("posts the one switched-on Investigator to THIS Case and shows the new run as the latest", async () => {
    const queued = run({
      id: "aaaaaaaa-1111-4222-8333-444455556666",
      status: "queued",
      finding: null,
      started_at: null,
      ended_at: null,
      steps: [],
    });
    const net = mount({
      investigators: [investigator(), investigator({ id: "11111111-2222-4333-8444-555566667777", name: "off", enabled: false })],
    });
    net.on(`POST ${LIST}`, () => {
      // The server's truth after the request, for the refetch it triggers.
      net.on(`GET ${LIST}`, () => ({ json: list([queued.row]) }));
      return { status: 202, json: item(queued.detail) };
    });
    net.on(`GET /api/v1/investigations/${queued.row.id}`, () => ({ json: item(queued.detail) }));

    const investigate = await button("Investigate");
    expect(screen.getByText("No Investigation of this firing has been asked for.")).toBeTruthy();
    fireEvent.click(investigate);

    await until(() => expect(net.calls.filter((c) => c.method === "POST")).toHaveLength(1));
    const post = net.calls.find((c) => c.method === "POST")!;
    expect(post.path).toBe(LIST);
    expect(post.body).toEqual({ investigator_id: investigator().id });

    await until(() => expect(shownRun().dataset["investigation"]).toBe(queued.row.id));
    expect(within(shownRun()).getByRole("status")).toBeTruthy();
  });

  it("with more than one switched on, asks which — and offers none that is off", async () => {
    const other = investigator({
      id: "22222222-3333-4444-8555-666677778888",
      name: "deep",
      current_version: { ...investigator().current_version, version: 1 },
    });
    const net = mount({
      investigators: [
        investigator(),
        other,
        investigator({ id: "11111111-2222-4333-8444-555566667777", name: "off", enabled: false }),
      ],
    });
    net.on(`POST ${LIST}`, () => ({
      status: 202,
      json: item(run({ status: "queued", finding: null, ended_at: null, steps: [] }).detail),
    }));
    net.on(`GET /api/v1/investigations/${investigation().id}`, () => ({
      json: item(run({ status: "queued", finding: null, ended_at: null, steps: [] }).detail),
    }));

    fireEvent.click(await button("Investigate…"));
    expect(screen.getByRole("button", { name: "firstlook v3" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^off v/ })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "deep v1" }));
    await until(() => expect(net.calls.filter((c) => c.method === "POST")).toHaveLength(1));
    expect(net.calls.find((c) => c.method === "POST")!.body).toEqual({ investigator_id: other.id });
  });

  it("shows the server's refusal when the request is refused", async () => {
    const net = mount();
    net.on(`POST ${LIST}`, () =>
      problem(403, "forbidden", { detail: "asking for an Investigation requires a human actor" }),
    );

    fireEvent.click(await button("Investigate"));
    await until(() =>
      expect(screen.getByRole("alert").textContent).toContain("requires a human actor"),
    );
  });

  it("⛔ says so when no Investigator is configured, and offers nothing to press", async () => {
    mount({ investigators: [] });
    await until(() =>
      expect(document.body.textContent).toContain("No Investigator is configured in this organisation"),
    );
    expect(screen.queryByRole("button", { name: /^Investigate/ })).toBeNull();
  });

  it("says so when every Investigator is switched off", async () => {
    mount({ investigators: [investigator({ enabled: false })] });
    await until(() =>
      expect(document.body.textContent).toContain("Every Investigator in this organisation is switched off"),
    );
    expect(screen.queryByRole("button", { name: /^Investigate/ })).toBeNull();
  });
});

/* -------------------------------------------------------------------------- */
/* earlier Investigations                                                     */
/* -------------------------------------------------------------------------- */

describe("earlier Investigations of the same Case", () => {
  it("⭐ shows the latest by default, and an earlier one with ITS Finding and ITS instant", async () => {
    const latest = run();
    const earlier = run({
      id: "bbbbbbbb-1111-4222-8333-444455556666",
      investigator_version: 2,
      finding: "Nothing conclusive; the pods had not restarted yet.",
      requested_at: "2026-08-09T09:03:00.000Z",
      started_at: "2026-08-09T09:03:01.000Z",
      ended_at: "2026-08-09T09:04:10.000Z",
    });
    mount({ runs: [latest.row, earlier.row], details: [latest.detail, earlier.detail] });

    await until(() => expect(finding().textContent).toContain("crash-looping"));
    const history = screen.getByRole("list", { name: /Every Investigation of this firing/ });
    const entries = within(history).getAllByRole("button");
    expect(entries).toHaveLength(2);
    expect(entries[0]!.getAttribute("aria-pressed")).toBe("true");
    expect(entries[0]!.textContent).toContain("latest");

    fireEvent.click(entries[1]!);
    await until(() => expect(finding().textContent).toContain("Nothing conclusive"));
    const caption = finding().querySelector("figcaption")!;
    expect(caption.textContent).toContain("firstlook v2");
    expect(caption.querySelector("time")!.getAttribute("datetime")).toBe(earlier.detail.ended_at);
    expect(within(history).getAllByRole("button")[1]!.getAttribute("aria-pressed")).toBe("true");
    expect(shownRun().textContent).toContain("earlier");
  });
});

/* -------------------------------------------------------------------------- */
/* an Incident as a whole (git-bug 74ea849)                                   */
/* -------------------------------------------------------------------------- */

describe("an Incident's Investigations", () => {
  const INCIDENT = 4;
  const INCIDENT_LIST = `/api/v1/incidents/${INCIDENT}/investigations`;

  function mountIncident(runs: readonly InvestigationDetail[]): FetchStub {
    const net = stubFetch({
      [`GET ${INCIDENT_LIST}`]: () => ({
        json: list(runs.map(({ steps: _s, ...row }) => (void _s, row))),
      }),
      "GET /api/v1/investigators": () => ({ json: list([investigator()]) }),
    });
    for (const d of runs) net.on(`GET /api/v1/investigations/${d.id}`, () => ({ json: item(d) }));
    renderScreen(() => <InvestigationPanel subject={{ kind: "incident", number: INCIDENT }} pollMs={20} />);
    return net;
  }

  it("⭐ reads the Incident's own runs by its NUMBER, and shows the Finding with its instant", async () => {
    const whole = investigationDetail({
      subject_kind: "incident",
      subject_id: "5a0e9c8e-3c1f-4b8e-9a51-6f0d2c7b1e44",
      requested_by_label: "oto: Incident #4 was drawn",
      finding: "One deploy at 09:02 explains every Case in the storm.",
    });
    const net = mountIncident([whole]);

    await until(() => expect(finding().textContent).toContain("explains every Case in the storm"));
    expect(net.calls.some((c) => c.path === INCIDENT_LIST)).toBe(true);
    expect(net.calls.some((c) => c.path.startsWith("/api/v1/cases/"))).toBe(false);
    const caption = finding().querySelector("figcaption")!;
    expect(caption.querySelector("time")!.getAttribute("datetime")).toBe(whole.ended_at);
    expect(shownRun().textContent).toContain("asked by oto: Incident #4 was drawn");
    // It says why the member Cases show no runs of their own.
    expect(document.querySelector("[data-incident-coverage]")?.textContent).toMatch(
      /Its Cases start none of their own while they are in it/,
    );
  });

  it("asks about THIS Incident, by its number, and says so when nothing has been asked", async () => {
    const net = mountIncident([]);
    const queued = investigationDetail({
      subject_kind: "incident",
      status: "queued",
      finding: null,
      started_at: null,
      ended_at: null,
      steps: [],
    });
    net.on(`POST ${INCIDENT_LIST}`, () => ({ status: 202, json: item(queued) }));
    net.on(`GET /api/v1/investigations/${queued.id}`, () => ({ json: item(queued) }));

    const investigate = await button("Investigate");
    expect(screen.getByText("No Investigation of this Incident has been asked for.")).toBeTruthy();
    fireEvent.click(investigate);

    await until(() => expect(net.calls.filter((c) => c.method === "POST")).toHaveLength(1));
    const post = net.calls.find((c) => c.method === "POST")!;
    expect(post.path).toBe(INCIDENT_LIST);
    expect(post.body).toEqual({ investigator_id: investigator().id });
  });

  it("is never shown the coverage note on a Case", async () => {
    mount({ runs: [] });
    await until(() =>
      expect(screen.getByText("No Investigation of this firing has been asked for.")).toBeTruthy(),
    );
    expect(document.querySelector("[data-incident-coverage]")).toBeNull();
  });
});

/* -------------------------------------------------------------------------- */
/* Suggestions (ADR 0053 §2, git-bug 8327c00)                                 */
/* -------------------------------------------------------------------------- */

function suggestion(patch: Partial<Suggestion> = {}): Suggestion {
  return {
    id: "5f0c3a3e-6d1a-4b8e-9a7e-1c2d3e4f5a6b",
    investigation_id: "",
    kind: "policy_count_condition",
    state: "open",
    why: "It flaps on every deploy and recovers within a minute.",
    proposed_at: "2026-08-09T09:12:00.000Z",
    lapses_at: "2026-08-16T09:12:00.000Z",
    applied_at: null,
    applied_by_label: null,
    count_condition: {
      policy_id: "8d1f6a3e-1b2c-4d5e-8f90-a1b2c3d4e5f6",
      policy_name: "crashloops → #platform",
      count_min: 3,
      count_window_seconds: 600,
      was_count_min: null,
      was_count_window_seconds: null,
    },
    membership: null,
    ...patch,
  };
}

const moveSuggestion = (): Suggestion =>
  suggestion({
    id: "6a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d",
    kind: "incident_membership",
    why: "Same namespace, same minute.",
    count_condition: null,
    membership: {
      incident_id: "7b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e",
      incident_number: 4,
      case_id: CASE,
      case_number: 9,
      moves_from_incident_number: 2,
    },
  });

function suggestions(): HTMLElement {
  const el = shownRun().querySelector<HTMLElement>("[data-suggestions]");
  expect(el, "the run shows no Suggestions").not.toBeNull();
  return el!;
}

describe("a Finding's Suggestions", () => {
  it("⭐ offers apply and nothing else, and applying is the POST with an empty body", async () => {
    const { row, detail } = run();
    const s = suggestion({ investigation_id: detail.id });
    const net = mount({ runs: [row], details: [detail], suggestions: { [detail.id]: [s] } });
    net.on(`POST /api/v1/suggestions/${s.id}/apply`, () => ({
      json: item({ ...s, state: "applied", applied_at: "2026-08-09T10:00:00.000Z", applied_by_label: "Priya R." }),
    }));

    await until(() => expect(suggestions()).toBeTruthy());
    expect(suggestions().textContent).toContain("crashloops → #platform");
    expect(suggestions().textContent).toContain("3 Cases have happened within 10m");
    expect(suggestions().textContent).toContain("in the model's words");
    expect(suggestions().textContent).toMatch(/lapses after seven days/);
    // ⛔ There is no other verb: nothing declines, rejects or hides a Suggestion.
    const buttons = within(suggestions()).getAllByRole("button");
    expect(buttons.map((b) => b.textContent)).toEqual(["Apply"]);
    for (const verb of [/declin/i, /reject/i, /dismiss/i, /ignore/i, /hide/i]) {
      expect(within(suggestions()).queryByRole("button", { name: verb })).toBeNull();
    }
    expectNoUndefined(suggestions());

    fireEvent.click(await button("Apply", suggestions));
    await until(() => expect(net.to(`/suggestions/${s.id}/apply`)).toHaveLength(1));
    expect(net.to(`/suggestions/${s.id}/apply`)[0]!.body).toEqual({});
  });

  it("⭐ says a membership Suggestion MOVES its Case before it is applied, and names the source in the request", async () => {
    const { row, detail } = run();
    const s = moveSuggestion();
    const net = mount({ runs: [row], details: [detail], suggestions: { [detail.id]: [s] } });
    net.on(`POST /api/v1/suggestions/${s.id}/apply`, () => ({ json: item({ ...s, state: "applied" }) }));

    await until(() => expect(suggestions().querySelector("[data-moves-from]")).not.toBeNull());
    expect(suggestions().querySelector("[data-moves-from]")!.textContent).toMatch(
      /This will move Case #9 from Incident #2 to\s+Incident #4/,
    );
    fireEvent.click(await button("Move to Incident #4", suggestions));
    await until(() => expect(net.to(`/suggestions/${s.id}/apply`)).toHaveLength(1));
    expect(net.to(`/suggestions/${s.id}/apply`)[0]!.body).toEqual({ moves_from_incident_number: 2 });
  });

  it("shows an applied Suggestion with who applied it, and no control", async () => {
    const { row, detail } = run();
    const s = suggestion({ state: "applied", applied_at: "2026-08-09T10:00:00.000Z", applied_by_label: "Priya R." });
    mount({ runs: [row], details: [detail], suggestions: { [detail.id]: [s] } });

    await until(() => expect(suggestions().querySelector("[data-applied]")).not.toBeNull());
    expect(suggestions().textContent).toContain("Applied by Priya R.");
    expect(within(suggestions()).queryByRole("button")).toBeNull();
  });

  it("says a refused apply in the open", async () => {
    const { row, detail } = run();
    const s = suggestion();
    const net = mount({ runs: [row], details: [detail], suggestions: { [detail.id]: [s] } });
    net.on(`POST /api/v1/suggestions/${s.id}/apply`, () =>
      problem(409, "suggestion_lapsed", { detail: "this Suggestion lapsed unapplied" }),
    );

    fireEvent.click(await button("Apply", suggestions));
    await until(() => expect(suggestions().textContent).toMatch(/lapsed unapplied/));
  });

  it("renders nothing when the Finding suggested nothing", async () => {
    const { row, detail } = run();
    mount({ runs: [row], details: [detail] });
    await until(() => expect(finding()).toBeTruthy());
    await until(() => expect(shownRun().querySelector("[data-suggestions]")).toBeNull());
  });
});

