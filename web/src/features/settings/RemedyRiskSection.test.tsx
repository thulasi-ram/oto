/**
 * The Remedy risk screen (ADR 0054 §3, git-bug eb4f21b), judged on what an operator must leave
 * it knowing: oto ships no rule and every Remedy then needs two; the most severe matching rule
 * wins and an unparseable command is two; the risk model may only raise and never reads the
 * Investigation; a rule that says one lets one person approve alone; and — owner ruling O3,
 * 2026-10-06 — a change is PROPOSED by one member and CONFIRMED by a different one, so the screen
 * offers a proposal, never a direct write, and never a working Confirm to the proposer.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { RemedyRiskSection } from "./RemedyRiskSection";
import type { RemedyRiskChange, RemedyRiskRules } from "~/api/types";
import { item, list, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const PATH = "/api/v1/remedy-risk-rules";
const PROVIDER = "11111111-1111-4111-8111-111111111111";

function rules(patch: Partial<RemedyRiskRules> = {}): RemedyRiskRules {
  return {
    rules: [],
    risk_model_provider_id: null,
    written_by_label: null,
    written_at: null,
    pending_change: null,
    reversible_verbs: ["cordon", "rollout restart", "scale"],
    ...patch,
  };
}

function mount(initial: RemedyRiskRules = rules()): FetchStub {
  const net = stubFetch({
    [`GET ${PATH}`]: { json: item(initial) },
    "GET /api/v1/model-providers": {
      json: list([
        {
          id: PROVIDER,
          name: "risk",
          base_url: "https://risk.test/v1",
          model: "risk-m",
          has_key: true,
          created_at: "2026-10-01T00:00:00Z",
          updated_at: "2026-10-01T00:00:00Z",
        },
      ]),
    },
  });
  renderScreen(() => <RemedyRiskSection />);
  return net;
}

describe("the Remedy risk screen", () => {
  it("⭐ starts empty — oto ships no rule — and says every Remedy then needs two", async () => {
    mount();
    await until(() => expect(document.querySelector("[data-no-rules]")).not.toBeNull());
    expect(document.querySelector("[data-no-rules]")!.textContent).toContain("Every Remedy needs two approvals");
    expect(document.body.textContent).toContain("oto ships none");
  });

  it("⭐ says the most severe rule wins, unparseable is two, and the model only raises", async () => {
    mount();
    await until(() => expect(document.querySelector("[data-risk-model-help]")).not.toBeNull());
    const semantics = document.querySelector("[data-semantics]")!.textContent ?? "";
    expect(semantics).toContain("most severe matching rule wins");
    expect(semantics).toContain("sh -c");
    expect(semantics).toContain("two whatever the rules say");
    const model = document.querySelector("[data-risk-model-help]")!.textContent ?? "";
    expect(model).toContain("only ever to raise it to two");
    expect(model).toContain("never the Investigation");
    expect(document.querySelector("[data-one-approval-warning]")!.textContent).toContain(
      "A rule that says one lets one person approve alone.",
    );
  });

  it("⭐ says a change takes two people, and offers a proposal and no direct write", async () => {
    const net = mount();
    await until(() => expect(document.querySelector("[data-no-rules]")).not.toBeNull());
    const managed = document.querySelector("[data-managed-by]")!.textContent ?? "";
    expect(managed).toContain("A change takes two people");
    expect(managed).toContain("different");
    expect(managed).toContain("oto remedy-rules apply --org <slug> -f rules.yaml");
    expect(screen.getByRole("button", { name: "Propose a change" })).toBeTruthy();
    // Nothing on the screen writes a rule: the only mutation it can make is a proposal.
    expect(net.to(PATH).every((c) => c.method === "GET")).toBe(true);
  });

  it("shows the applied rules in order, the risk model by name, and who applied them", async () => {
    mount(
      rules({
        rules: [
          {
            name: "restart-payments",
            tool: null,
            verbs: ["rollout restart"],
            kinds: ["deployment"],
            namespaces: ["payments"],
            reversibility: null,
            approvals: 1,
          },
          {
            name: "secrets-need-two",
            tool: "k8s-write__kubectl",
            verbs: [],
            kinds: ["secret"],
            namespaces: [],
            reversibility: "irreversible",
            approvals: 2,
          },
        ],
        risk_model_provider_id: PROVIDER,
        written_by_label: "oto remedy-rules apply",
        written_at: "2026-10-05T09:00:00Z",
      }),
    );
    await until(() => expect(document.querySelectorAll("[data-rule-row]")).toHaveLength(2));
    const [first, second] = [...document.querySelectorAll("[data-rule-row]")];
    expect(first!.textContent).toContain("restart-payments");
    expect(first!.querySelector("[data-approvals]")!.textContent).toBe("One approval");
    expect(first!.textContent).toContain("rollout restart");
    expect(first!.textContent).not.toContain("tool");
    expect(second!.querySelector("[data-approvals]")!.textContent).toBe("Two approvals");
    expect(second!.textContent).toContain("k8s-write__kubectl");
    expect(second!.textContent).toContain("irreversible");
    await until(() => expect(document.querySelector("[data-risk-model]")!.textContent).toContain("risk"));
    expect(document.querySelector("[data-risk-model-help]")!.textContent).toContain("daily token budget");
    expect(document.querySelector("[data-written-by]")!.textContent).toContain("oto remedy-rules apply");
  });

  it("⭐ shows a change waiting for a second person, and the proposer cannot confirm it", async () => {
    mount(rules({ pending_change: change({ proposed_by_you: true }) }));
    await until(() => expect(document.querySelector("[data-pending-change]")).not.toBeNull());
    const pending = document.querySelector("[data-pending-change]")!;
    expect(pending.textContent).toContain("waiting for a second person");
    expect(pending.textContent).toContain("Proposed by you");
    // ⛔ The reason is on the screen, and the button does not work: not a 403 to explain it.
    expect((screen.getByRole("button", { name: "Confirm" }) as HTMLButtonElement).disabled).toBe(true);
    expect(document.querySelector("[data-needs-second-person]")!.textContent).toContain("a colleague has to confirm");
    // The proposer may still withdraw it.
    expect((screen.getByRole("button", { name: "Discard" }) as HTMLButtonElement).disabled).toBe(false);
    // And a one-approval rule in it is called out.
    expect(document.querySelector("[data-pending-single]")!.textContent).toContain("One rule says one approval");
  });

  it("lets a different member confirm, after saying what they are agreeing to", async () => {
    const net = mount(rules({ pending_change: change({ proposed_by_you: false, proposed_by_label: "Ada Lovelace" }) }));
    net.on(`POST ${PATH}/changes/${CHANGE_ID}/confirm`, { json: item(rules()) });
    await until(() => expect(document.querySelector("[data-pending-change]")).not.toBeNull());
    expect(document.querySelector("[data-pending-change]")!.textContent).toContain("Proposed by Ada Lovelace");

    const confirm = screen.getByRole("button", { name: "Confirm" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(false);
    fireEvent.click(confirm);
    await until(() => expect(screen.getByText(/lets one person approve a matching Remedy alone/)).toBeTruthy());
    expect(net.calls.some((c) => c.method === "POST")).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "Confirm the rules" }));
    await until(() => expect(net.calls.some((c) => c.method === "POST" && c.path.endsWith("/confirm"))).toBe(true));
  });

  it("discards a pending change", async () => {
    const net = mount(rules({ pending_change: change({ proposed_by_you: true }) }));
    net.on(`POST ${PATH}/changes/${CHANGE_ID}/discard`, { status: 204 });
    await until(() => expect(screen.getByRole("button", { name: "Discard" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    await until(() => expect(net.calls.some((c) => c.method === "POST" && c.path.endsWith("/discard"))).toBe(true));
  });

  it("⭐ proposes the whole rule set, and says saving proposes and does not apply", async () => {
    const net = mount(
      rules({
        rules: [
          {
            name: "restart-payments",
            tool: "k8s-write__kubectl",
            verbs: ["rollout restart"],
            kinds: ["deployment"],
            namespaces: ["payments"],
            reversibility: null,
            approvals: 1,
          },
        ],
      }),
    );
    net.on(`POST ${PATH}/changes`, { status: 201, json: item(change({ proposed_by_you: true })) });
    await until(() => expect(screen.getByRole("button", { name: "Propose a change" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Propose a change" }));
    await until(() => expect(document.querySelector("[data-draft-rule]")).not.toBeNull());
    expect(document.body.textContent).toContain("Saving proposes it; it takes effect only when a different member confirms it");
    // It starts from the rules that stand, and a one-approval rule is called out before saving.
    expect((document.querySelector("[data-draft-rule] input[id$=-name]") as HTMLInputElement).value).toBe("restart-payments");
    expect(document.querySelector("[data-one-approval-notice]")!.textContent).toContain("One rule says one approval");

    fireEvent.click(screen.getByRole("button", { name: "Propose this change" }));
    await until(() => expect(net.calls.some((c) => c.method === "POST")).toBe(true));
    expect(net.calls.find((c) => c.method === "POST")?.body).toEqual({
      rules: [
        {
          name: "restart-payments",
          tool: "k8s-write__kubectl",
          verbs: ["rollout restart"],
          kinds: ["deployment"],
          namespaces: ["payments"],
          reversibility: null,
          approvals: 1,
        },
      ],
      risk_model_provider_id: null,
    });
  });

  it("⛔ a new rule starts at two approvals, so adding one cannot loosen anything", async () => {
    mount();
    await until(() => expect(screen.getByRole("button", { name: "Propose a change" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Propose a change" }));
    await until(() => expect(screen.getByRole("button", { name: "Add a rule" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    await until(() => expect(document.querySelector("[data-draft-rule]")).not.toBeNull());
    expect(document.querySelector("[data-one-approval-notice]")).toBeNull();
    expect(document.querySelector("[data-draft-rule]")!.textContent).toContain("Two approvals");
  });

  it("shows the server's refusal against the rule and field it names", async () => {
    const net = mount(
      rules({
        rules: [
          { name: "loose", tool: null, verbs: ["delete"], kinds: [], namespaces: [], reversibility: null, approvals: 1 },
        ],
      }),
    );
    net.on(`POST ${PATH}/changes`, {
      status: 422,
      json: {
        type: "about:blank",
        title: "Unprocessable",
        status: 422,
        code: "validation_failed",
        detail: "the rules are not valid",
        violations: [{ field: "rules/0/tool", code: "single_needs_tool", message: "a rule that says one names its Tool" }],
      },
    });
    await until(() => expect(screen.getByRole("button", { name: "Propose a change" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Propose a change" }));
    await until(() => expect(screen.getByRole("button", { name: "Propose this change" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Propose this change" }));
    await until(() => expect(screen.getByText("a rule that says one names its Tool")).toBeTruthy());
  });
});

const CHANGE_ID = "33333333-3333-4333-8333-333333333333";

function change(patch: Partial<RemedyRiskChange> = {}): RemedyRiskChange {
  return {
    id: CHANGE_ID,
    rules: [
      {
        name: "restart-payments",
        tool: "k8s-write__kubectl",
        verbs: ["rollout restart"],
        kinds: ["deployment"],
        namespaces: ["payments"],
        reversibility: null,
        approvals: 1,
      },
    ],
    risk_model_provider_id: null,
    status: "pending",
    proposed_by_label: "Grace Hopper",
    proposed_at: "2026-10-06T09:00:00Z",
    proposed_by_you: false,
    ...patch,
  };
}
