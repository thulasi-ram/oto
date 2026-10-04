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
 */
import { fireEvent, screen, within } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { RemediesView } from "./RemediesView";
import type { Remedy } from "~/api/types";
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
    target: "node pool eu-1",
    description: "Add a node: the pool is out of memory.",
  });

function mount(rows: readonly Remedy[]) {
  const net = stubFetch({
    [`GET /api/v1/investigations/${RUN}/remedies`]: () => ({ json: list(rows) }),
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

  it("renders nothing when the Finding proposed nothing", async () => {
    const net = mount([]);
    await until(() => expect(net.to(`/investigations/${RUN}/remedies`).length).toBeGreaterThan(0));
    expect(document.querySelector("[data-remedies]")).toBeNull();
  });
});
