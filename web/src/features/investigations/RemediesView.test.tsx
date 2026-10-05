/**
 * A Finding's Remedies (ADR 0054, git-bug 4148256).
 *
 * ⛔ THE EXACT COMMAND IS ASSERTED FIRST, ABOVE THE DESCRIPTION. The failure this suite
 * exists to catch is an approver reading the model's sentence and approving a command they
 * never saw — so every test that finds a Remedy with a Tool finds its arguments, exactly as
 * the server sent them, before its description.
 *
 * ⛔ A REMEDY WITH NO TOOL OFFERS NO APPROVE CONTROL, and neither does one whose Tool was
 * removed since it was proposed: both say why instead.
 *
 * ⭐ WHAT SET THE TIER IS SAID UNDER THE COMMAND (git-bug eb4f21b): the rule by name, "no
 * rule matched", "could not parse", or "raised by the risk model" — above the description.
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { RemediesView } from "./RemediesView";
import type { Notification, Remedy } from "~/api/types";
import { expectNoUndefined, item, list, problem, renderScreen, stubFetch, until } from "~/test/harness";

const RUN = "3a1f0c2e-5b6d-4e7f-8a9b-0c1d2e3f4a5b";
const ARGS = '{"namespace":"checkout","deployment":"api","generation":12345678901234567890}';
const SHA = "5d41402abc4b2a76b9719d911017c592ae2e3f4a5b6c7d8e9f00112233445566";

function remedy(patch: Partial<Remedy> = {}): Remedy {
  return {
    id: "8c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f",
    investigation_id: RUN,
    subject_kind: "incident",
    subject_id: "7b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e",
    state: "proposed",
    tool: {
      tool_server_id: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
      tool_server_name: "k8s-write",
      tool_name: "rollout_restart",
    },
    no_tool: null,
    blocked: null,
    arguments: ARGS,
    arguments_display: ARGS,
    arguments_sha256: SHA,
    target: "Deployment checkout/api",
    description: "Restart the api to pick up the reverted config.",
    proposed_by_label: "Investigator firstlook v2",
    required_approvals: 2,
    risk: {
      set_by: "no_rule",
      rule: null,
      detail: null,
      risk_model_check: "unset",
      risk_model: null,
      risk_model_tokens: null,
    },
    approvals: [],
    proposed_at: "2026-08-09T09:12:00.000Z",
    expires_at: "2026-08-09T10:12:00.000Z",
    approved_at: null,
    executing_at: null,
    ended_at: null,
    failure_reason: null,
    detail: null,
    result: null,
    transitions: [],
    ...patch,
  };
}

const noTool = (): Remedy =>
  remedy({
    id: "9d3e4f5a-6b7c-4d8e-9f0a-1b2c3d4e5f6a",
    tool: null,
    no_tool: "no configured Tool can carry this out",
    blocked: "no configured Tool can carry this out",
    arguments: null,
    arguments_display: null,
    arguments_sha256: null,
    risk: null,
    target: "node pool eu-1",
    description: "Add a node: the pool is out of memory.",
  });

function mount(rows: readonly Remedy[], facts: readonly Notification[] = []) {
  const net = stubFetch({
    [`GET /api/v1/investigations/${RUN}/remedies`]: () => ({ json: list(rows) }),
    "GET /api/v1/notifications": () => ({ json: list(facts) }),
  });
  renderScreen(() => <RemediesView investigationId={RUN} pollMs={20} />);
  return net;
}

function shown(id: string): HTMLElement {
  const el = document.querySelector<HTMLElement>(`[data-remedy="${id}"]`);
  expect(el, `Remedy ${id} is not on screen`).not.toBeNull();
  return el!;
}

describe("a Finding's Remedies", () => {
  it("⭐ shows the exact command first, above the description, and approves by its hash", async () => {
    const r = remedy({ approvals: [{ user_id: null, label: "Ada Lovelace", approved_at: "2026-08-09T09:20:00.000Z" }] });
    const net = mount([r]);
    net.on(`POST /api/v1/remedies/${r.id}/approve`, () => ({ json: item({ ...r, state: "approved" }) }));

    await until(() => expect(shown(r.id)).toBeTruthy());
    const el = shown(r.id);
    const text = el.textContent ?? "";
    // The arguments are the server's bytes — the 20-digit number un-rounded.
    expect(el.querySelector("[data-arguments]")!.textContent).toBe(ARGS);
    expect(text.indexOf("k8s-write__rollout_restart")).toBeGreaterThanOrEqual(0);
    expect(text.indexOf("12345678901234567890")).toBeLessThan(text.indexOf("Restart the api"));
    expect(el.querySelector("[data-approvals]")!.textContent).toMatch(/1 of 2 approvals: Ada Lovelace/);
    expectNoUndefined(el);

    fireEvent.click(within(el).getByRole("button", { name: "Approve" }));
    await until(() => expect(net.to(`/remedies/${r.id}/approve`)).toHaveLength(1));
    expect(net.to(`/remedies/${r.id}/approve`)[0]!.body).toEqual({ arguments_sha256: SHA });
  });

  it("⭐ says a Remedy no configured Tool can carry out, offers no approve, and declines", async () => {
    const r = noTool();
    const net = mount([r]);
    net.on(`POST /api/v1/remedies/${r.id}/decline`, () => ({ json: item({ ...r, state: "declined" }) }));

    await until(() => expect(shown(r.id)).toBeTruthy());
    const el = shown(r.id);
    expect(el.querySelector("[data-no-tool]")!.textContent).toMatch(/No configured Tool can carry this out/);
    expect(el.querySelector("[data-arguments]")).toBeNull();
    expect(within(el).queryByRole("button", { name: "Approve" })).toBeNull();

    fireEvent.click(within(el).getByRole("button", { name: "Decline" }));
    await until(() => expect(net.to(`/remedies/${r.id}/decline`)).toHaveLength(1));
  });

  it("says why a Remedy whose Tool was removed cannot be approved, and offers no approve", async () => {
    const r = remedy({ blocked: "the ToolServer k8s-write is no longer configured" });
    mount([r]);
    await until(() => expect(shown(r.id).querySelector("[data-blocked]")).not.toBeNull());
    expect(shown(r.id).querySelector("[data-blocked]")!.textContent).toMatch(/no longer configured/);
    expect(within(shown(r.id)).queryByRole("button", { name: "Approve" })).toBeNull();
    expect(within(shown(r.id)).getByRole("button", { name: "Decline" })).toBeTruthy();
  });

  it("offers no control on a Remedy that has ended, and says why it failed", async () => {
    const r = remedy({
      state: "failed",
      failure_reason: "tool_error",
      detail: "deployments.apps is forbidden",
      result: "Error from server (Forbidden)",
      executing_at: "2026-08-09T09:30:00.000Z",
      ended_at: "2026-08-09T09:30:02.000Z",
    });
    mount([r]);
    await until(() => expect(shown(r.id)).toBeTruthy());
    expect(shown(r.id).textContent).toMatch(/never retried/);
    expect(shown(r.id).textContent).toMatch(/the write Tool answered that the call failed/);
    expect(within(shown(r.id)).queryByRole("button")).toBeNull();
  });

  it("says a refused approval in the open", async () => {
    const r = remedy();
    const net = mount([r]);
    net.on(`POST /api/v1/remedies/${r.id}/approve`, () =>
      problem(403, "remedy_approver_required", { detail: "you do not hold the approval grant on k8s-write" }),
    );
    await until(() => expect(shown(r.id)).toBeTruthy());
    fireEvent.click(within(shown(r.id)).getByRole("button", { name: "Approve" }));
    await until(() => expect(screen.getByText(/do not hold the approval grant/)).toBeTruthy());
  });

  it("⭐ says which rule set a one-approval tier, under the command and above the description", async () => {
    const r = remedy({
      required_approvals: 1,
      risk: {
        set_by: "rule",
        rule: "restart-payments",
        detail: null,
        risk_model_check: "kept",
        risk_model: "https://risk.test/v1#risk-m",
        risk_model_tokens: 135,
      },
    });
    mount([r]);
    await until(() => expect(shown(r.id).querySelector("[data-tier]")).not.toBeNull());
    const el = shown(r.id);
    const tier = el.querySelector("[data-tier]")!;
    expect(tier.textContent).toMatch(/Needs one approval — set by the rule restart-payments\./);
    expect(tier.textContent).toMatch(/risk model was asked and kept it/);
    const text = el.textContent ?? "";
    expect(text.indexOf("12345678901234567890")).toBeLessThan(text.indexOf("restart-payments"));
    expect(text.indexOf("restart-payments")).toBeLessThan(text.indexOf("Restart the api"));
    expect(el.querySelector("[data-approvals]")!.textContent).toMatch(/0 of 1 approvals/);
    expectNoUndefined(el);
  });

  it("says no rule matched, an unparseable command, and a raise by the risk model", async () => {
    const base = remedy().risk!;
    const none = remedy({ id: "aaaaaaaa-0000-4000-8000-000000000001" });
    const sh = remedy({
      id: "aaaaaaaa-0000-4000-8000-000000000002",
      risk: { ...base, set_by: "unparseable", detail: "it runs \"sh\", and the rules read only kubectl's command lines" },
    });
    const raised = remedy({
      id: "aaaaaaaa-0000-4000-8000-000000000003",
      risk: { ...base, set_by: "risk_model", rule: "restart-payments", risk_model_check: "raised", detail: "one replica" },
    });
    const failed = remedy({
      id: "aaaaaaaa-0000-4000-8000-000000000004",
      risk: { ...base, set_by: "risk_model_failed", rule: "restart-payments", risk_model_check: "failed", detail: "timeout" },
    });
    const budget = remedy({
      id: "aaaaaaaa-0000-4000-8000-000000000005",
      risk: { ...base, set_by: "risk_model_budget", rule: "restart-payments", risk_model_check: "budget",
        detail: "the risk model was not asked, so it needs two: this org has spent 1000 of its 1000 daily Investigation tokens" },
    });
    mount([none, sh, raised, failed, budget]);
    await until(() => expect(shown(budget.id)).toBeTruthy());
    expect(shown(none.id).querySelector("[data-tier]")!.textContent).toMatch(/Needs two approvals.*no rule matched/);
    expect(shown(sh.id).querySelector("[data-tier]")!.textContent).toMatch(/could not parse this command.*sh/);
    expect(shown(raised.id).querySelector("[data-tier]")!.textContent).toMatch(
      /raised by the risk model \(one replica\); the rule restart-payments said one/,
    );
    expect(shown(failed.id).querySelector("[data-tier]")!.textContent).toMatch(/risk model gave no answer/);
    // Owner ruling 2026-10-05 (git-bug eb4f21b): a spent day asks no model and leaves two.
    expect(shown(budget.id).querySelector("[data-tier]")!.textContent).toMatch(
      /token budget was spent, so the risk model was not asked and it needs two; the rule restart-payments said one/,
    );
  });

  it("⭐ shows the Incident's last three outbound facts above Approve and Decline (ruling F1, 2026-10-05)", async () => {
    const r = remedy({ transitions: [proposal(INCIDENT)] });
    const facts = [
      fact("f1", "remedy_proposed", "delivered", "2026-08-09T09:12:05.000Z"),
      fact("f2", "acked", "failed", "2026-08-09T09:05:00.000Z"),
      fact("f3", "fired", "delivered", "2026-08-09T09:00:00.000Z"),
    ];
    const net = mount([r], facts);
    await until(() => expect(shown(r.id).querySelectorAll("[data-fact]")).toHaveLength(3));
    const asked = net.to("/notifications")[0]!.search;
    expect(asked.get("conversation_id")).toBe(INCIDENT);
    expect(asked.get("limit")).toBe("3");
    expect(asked.get("status")).not.toMatch(/suppressed/);

    const el = shown(r.id);
    const list = el.querySelector("[data-outbound-facts]")!;
    expect(list.textContent).toMatch(/an Investigator proposed a Remedy/);
    expect(list.textContent).toMatch(/nothing landed/);
    // Above the controls: the facts are read before the decision.
    const approve = within(el).getByRole("button", { name: "Approve" });
    expect(list.compareDocumentPosition(approve) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expectNoUndefined(el);
  });

  it("shows no outbound facts for a Remedy on a Case no Incident holds, and asks for none", async () => {
    const r = remedy({ subject_kind: "case", transitions: [proposal(null)] });
    const net = mount([r]);
    await until(() => expect(shown(r.id)).toBeTruthy());
    expect(shown(r.id).querySelector("[data-outbound-facts]")).toBeNull();
    expect(net.to("/notifications")).toHaveLength(0);
  });

  it("renders nothing when the Finding proposed nothing", async () => {
    const net = mount([]);
    await until(() => expect(net.to(`/investigations/${RUN}/remedies`).length).toBeGreaterThan(0));
    expect(document.querySelector("[data-remedies]")).toBeNull();
  });
});

const INCIDENT = "7b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e";

function proposal(declared: string | null): Remedy["transitions"][number] {
  return {
    from: null,
    to: "proposed",
    actor_kind: "investigator",
    actor_label: "Investigator firstlook v2",
    at: "2026-08-09T09:12:00.000Z",
    failure_reason: null,
    detail: null,
    declared_incident_id: declared,
  };
}

function fact(id: string, reason: Notification["reason"], status: Notification["status"], at: string): Notification {
  return {
    id: `0000000${id.slice(1)}-0000-4000-8000-000000000000`,
    subject_kind: "incident",
    subject_id: INCIDENT,
    alert_id: null,
    case_id: null,
    reason,
    policy_id: null,
    state_version: 1,
    status,
    delivery_summary: { total: 1, sent: status === "delivered" ? 1 : 0, failed: 0, dead: 0, skipped: 0, pending: 0 },
    created_at: at,
    updated_at: null,
  };
}
