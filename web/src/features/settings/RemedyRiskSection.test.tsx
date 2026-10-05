/**
 * The Remedy risk screen (ADR 0054 §3, git-bug eb4f21b), judged on what an operator must leave
 * it knowing: oto ships no rule and every Remedy then needs two; the most severe matching rule
 * wins and an unparseable command is two; the risk model may only raise and never reads the
 * Investigation; and a rule that says one lets one person approve alone.
 */
import { fireEvent, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import { RemedyRiskSection } from "./RemedyRiskSection";
import type { RemedyRiskRules } from "~/api/types";
import { item, list, problem, renderScreen, stubFetch, until, type FetchStub } from "~/test/harness";

const PATH = "/api/v1/remedy-risk-rules";
const PROVIDER = "11111111-1111-4111-8111-111111111111";

function rules(patch: Partial<RemedyRiskRules> = {}): RemedyRiskRules {
  return {
    rules: [],
    risk_model_provider_id: null,
    written_by_label: null,
    written_at: null,
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

  it("saves the whole list and the risk model, and shows who wrote them", async () => {
    const net = mount();
    const stored = rules({
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
      ],
      risk_model_provider_id: PROVIDER,
      written_by_label: "Ada Lovelace",
      written_at: "2026-10-05T09:00:00Z",
    });
    net.on(`PUT ${PATH}`, { json: item(stored) });

    await until(() => expect(screen.getByRole("button", { name: "Add a rule" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    fireEvent.input(screen.getByLabelText("Rule 1 name"), { target: { value: "restart-payments" } });
    fireEvent.input(screen.getByLabelText("Rule 1 verbs"), { target: { value: "rollout restart" } });
    fireEvent.input(screen.getByLabelText("Rule 1 kinds"), { target: { value: "deployment" } });
    fireEvent.input(screen.getByLabelText("Rule 1 namespaces"), { target: { value: "payments, " } });
    await until(() => expect(screen.getByRole("button", { name: "risk" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "risk" }));
    fireEvent.click(screen.getByRole("button", { name: "Save rules" }));

    await until(() => expect(net.to(PATH).filter((c) => c.method === "PUT")).toHaveLength(1));
    expect(net.to(PATH).find((c) => c.method === "PUT")!.body).toEqual({
      rules: [
        {
          name: "restart-payments",
          verbs: ["rollout restart"],
          kinds: ["deployment"],
          namespaces: ["payments"],
          approvals: 1,
        },
      ],
      risk_model_provider_id: PROVIDER,
    });
    await until(() => expect(document.querySelector("[data-written-by]")).not.toBeNull());
    expect(document.querySelector("[data-written-by]")!.textContent).toContain("Ada Lovelace");
  });

  it("puts the server's refusal of a rule with no condition on the row that wrote it", async () => {
    const net = mount();
    net.on(`PUT ${PATH}`, () =>
      problem(422, "remedy_risk_rules_invalid", {
        violations: [
          {
            field: "rules/0",
            code: "no_condition",
            message: "a rule says at least one condition — with none it would match every command",
          },
        ],
      }),
    );
    await until(() => expect(screen.getByRole("button", { name: "Add a rule" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    fireEvent.input(screen.getByLabelText("Rule 1 name"), { target: { value: "everything" } });
    fireEvent.click(screen.getByRole("button", { name: "Save rules" }));
    await until(() => expect(screen.getByText(/with none it would match every command/)).toBeTruthy());
  });
});
